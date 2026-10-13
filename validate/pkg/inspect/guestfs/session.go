// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package guestfs implements the validator's guestfs inspection tier: it
// opens a node image read-only with libguestfs and serves filesystem facts to
// checks. It follows os-image/packer/scripts/extract-image-sbom.sh, the
// repository's shipped precedent for introspecting these images:
//
//   - guestfish runs as a --listen daemon driven over --remote, because that
//     is the only mode in which --keys-from-stdin reliably supplies LUKS
//     passphrases (a -f script file does not deliver them);
//   - the keys file repeats the passphrase, one line per prompt;
//   - LIBGUESTFS_BACKEND defaults to direct, so no libvirt is needed;
//   - the root filesystem is identified by what is on it (/var/lib/dpkg),
//     not by its device path.
//
// Everything is read-only: drives are added readonly and mounts are mount-ro.
package guestfs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Options configure Open.
type Options struct {
	// LUKSPassphrase unlocks LUKS containers found on the image. Empty is
	// correct for non-FDE images; an image that carries LUKS but gets no
	// passphrase fails Open rather than inspecting only the boot partition.
	LUKSPassphrase string
	// Guestfish overrides the binary name, for tests. Default "guestfish".
	Guestfish string
}

// startTimeout bounds the wait for the daemon's PID announcement.
const startTimeout = 60 * time.Second

// Session is one open image: a running guestfish daemon with the image's
// root filesystem mounted read-only. Always Close it.
type Session struct {
	guestfish string
	pid       string
	hasLUKS   bool
}

// Available reports whether the guestfish binary is on PATH — the cheap
// pre-check for `--inspect auto`.
func Available() bool {
	_, err := exec.LookPath("guestfish")
	return err == nil
}

// Open boots a libguestfs appliance over the image, opens any LUKS container,
// activates LVM, and mounts the root filesystem read-only.
func Open(ctx context.Context, imagePath string, opts Options) (*Session, error) {
	bin := opts.Guestfish
	if bin == "" {
		bin = "guestfish"
	}
	s := &Session{guestfish: bin}

	args := []string{"--listen"}
	var stdin strings.Builder
	if opts.LUKSPassphrase != "" {
		args = append(args, "--keys-from-stdin")
		// One line is consumed per prompt; repeat so the file holds more
		// than can be asked for (same construction as extract-image-sbom.sh).
		for range 16 {
			stdin.WriteString(opts.LUKSPassphrase + "\n")
		}
	}
	// The daemon guestfish forks can inherit the parent's stdout, so waiting
	// for EOF (cmd.Output) can hang for the daemon's whole lifetime. Read
	// line-wise until the PID announcement instead, and never wait for EOF.
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin.String())
	cmd.Env = append(os.Environ(), "LIBGUESTFS_BACKEND="+backend())
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("starting guestfish daemon: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting guestfish daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap the parent whenever it exits

	pidCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if pid, perr := parseListenOutput(scanner.Text()); perr == nil {
				pidCh <- pid
				return
			}
		}
		errCh <- fmt.Errorf("guestfish --listen ended without a GUESTFISH_PID: %s", strings.TrimSpace(stderrBuf.String()))
	}()
	select {
	case s.pid = <-pidCh:
	case err := <-errCh:
		_ = cmd.Process.Kill()
		return nil, err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("guestfish --listen: %w: %s", ctx.Err(), strings.TrimSpace(stderrBuf.String()))
	case <-time.After(startTimeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("guestfish --listen printed no GUESTFISH_PID within %s: %s", startTimeout, strings.TrimSpace(stderrBuf.String()))
	}
	if err := s.open(ctx, imagePath, opts.LUKSPassphrase != ""); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func backend() string {
	if b := os.Getenv("LIBGUESTFS_BACKEND"); b != "" {
		return b
	}
	return "direct"
}

func (s *Session) open(ctx context.Context, imagePath string, havePassphrase bool) error {
	if _, err := s.remote(ctx, "add-drive", imagePath, "readonly:true"); err != nil {
		return err
	}
	if _, err := s.remote(ctx, "run"); err != nil {
		return err
	}

	// Open every LUKS container. On the FDE layout the root volume group only
	// appears once the container is open.
	devices, err := s.remoteLines(ctx, "list-devices")
	if err != nil {
		return err
	}
	partitions, err := s.remoteLines(ctx, "list-partitions")
	if err != nil {
		return err
	}
	opened := 0
	for _, dev := range append(devices, partitions...) {
		vfs, err := s.remote(ctx, "vfs-type", dev)
		if err != nil {
			continue // an unformatted device has no vfs-type; not an error
		}
		if strings.TrimSpace(vfs) != "crypto_LUKS" {
			continue
		}
		s.hasLUKS = true
		if !havePassphrase {
			return fmt.Errorf("image carries a LUKS container on %s but no passphrase was supplied", dev)
		}
		if _, err := s.remote(ctx, "cryptsetup-open", dev, fmt.Sprintf("nvkeluks%d", opened)); err != nil {
			return fmt.Errorf("opening LUKS container on %s: %w", dev, err)
		}
		opened++
	}
	if opened > 0 {
		if _, err := s.remote(ctx, "vgscan"); err != nil {
			return err
		}
		if _, err := s.remote(ctx, "vg-activate-all", "true"); err != nil {
			return err
		}
	}

	// The root filesystem is identified by what is on it: mount candidates
	// read-only until one holds a dpkg database.
	filesystems, err := s.remoteLines(ctx, "list-filesystems")
	if err != nil {
		return err
	}
	for _, line := range filesystems {
		dev, kind, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kind = strings.TrimSpace(kind)
		if kind == "" || kind == "unknown" || kind == "swap" || kind == "crypto_LUKS" {
			continue
		}
		dev = strings.TrimSpace(dev)
		if _, err := s.remote(ctx, "mount-ro", dev, "/"); err != nil {
			continue
		}
		if exists, err := s.Exists("/var/lib/dpkg/status"); err == nil && exists {
			return nil
		}
		if _, err := s.remote(ctx, "umount-all"); err != nil {
			return err
		}
	}
	return fmt.Errorf("no filesystem on %s holds a dpkg database; not a node root image", imagePath)
}

// Close shuts the daemon down. Safe to call more than once.
func (s *Session) Close() {
	if s.pid == "" {
		return
	}
	cmd := exec.Command(s.guestfish, "--remote="+s.pid, "--", "exit")
	_ = cmd.Run()
	s.pid = ""
}

func (s *Session) remote(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--remote=" + s.pid, "--"}, args...)
	cmd := exec.CommandContext(ctx, s.guestfish, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("guestfish %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (s *Session) remoteLines(ctx context.Context, args ...string) ([]string, error) {
	out, err := s.remote(ctx, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// parseListenOutput extracts the daemon pid from `guestfish --listen` output,
// which is shell to eval: `GUESTFISH_PID=1234; export GUESTFISH_PID`.
func parseListenOutput(out string) (string, error) {
	for _, field := range strings.FieldsFunc(out, func(r rune) bool { return r == ';' || r == '\n' || r == ' ' }) {
		if pid, ok := strings.CutPrefix(field, "GUESTFISH_PID="); ok && pid != "" {
			return pid, nil
		}
	}
	return "", fmt.Errorf("guestfish --listen printed no GUESTFISH_PID: %q", out)
}
