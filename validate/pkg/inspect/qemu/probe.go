// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// The VM implements checks.Prober over ssh.
var _ checks.Prober = (*VM)(nil)

// sshCommandExitCode mirrors ssh(1): 255 means ssh itself failed (transport),
// anything else is the remote command's exit code.
const sshTransportExit = 255

// Probe runs one command inside the booted image. A non-zero remote exit is
// returned as data; only transport failures are errors.
func (v *VM) Probe(ctx context.Context, command string) (checks.ProbeResult, error) {
	cmd := exec.CommandContext(ctx, "ssh", v.sshArgs(command)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != sshTransportExit {
			return checks.ProbeResult{Stdout: string(out), ExitCode: exitErr.ExitCode()}, nil
		}
		return checks.ProbeResult{}, fmt.Errorf("probe transport: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return checks.ProbeResult{Stdout: string(out), ExitCode: 0}, nil
}

// MakeSeed writes a NoCloud seed ISO carrying a probe user with the given SSH
// public key, and returns its path. It shells to cloud-localds when present
// and genisoimage otherwise — the standard tools for the job; reimplementing
// ISO9660 here would be all risk and no insight.
func MakeSeed(dir, user, sshPublicKey string) (string, error) {
	userData := fmt.Sprintf(`#cloud-config
users:
  - name: %s
    ssh_authorized_keys:
      - %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
ssh_pwauth: false
`, user, strings.TrimSpace(sshPublicKey))
	metaData := "instance-id: nvke-validate-probe\nlocal-hostname: nvke-validate-probe\n"

	udPath := filepath.Join(dir, "user-data")
	mdPath := filepath.Join(dir, "meta-data")
	if err := os.WriteFile(udPath, []byte(userData), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(mdPath, []byte(metaData), 0o600); err != nil {
		return "", err
	}
	seed := filepath.Join(dir, "seed.iso")

	if _, err := exec.LookPath("cloud-localds"); err == nil {
		if out, err := exec.Command("cloud-localds", seed, udPath, mdPath).CombinedOutput(); err != nil {
			return "", fmt.Errorf("cloud-localds: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return seed, nil
	}
	if _, err := exec.LookPath("genisoimage"); err == nil {
		if out, err := exec.Command("genisoimage", "-output", seed, "-volid", "cidata", "-joliet", "-rock", udPath, mdPath).CombinedOutput(); err != nil {
			return "", fmt.Errorf("genisoimage: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return seed, nil
	}
	return "", errors.New("neither cloud-localds nor genisoimage is on PATH; one is required to build the NoCloud probe seed")
}

// GenerateSSHKey writes a throwaway ed25519 keypair into dir and returns the
// private key path and the public key line.
func GenerateSSHKey(dir string) (keyPath, publicKey string, err error) {
	keyPath = filepath.Join(dir, "probe_ed25519")
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", "nvke-validate-probe", "-f", keyPath).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", "", err
	}
	return keyPath, string(pub), nil
}

// Available reports whether the boot tier's host tools exist for the guest
// architecture: the qemu system binary plus a seed builder.
func Available(arch string) bool {
	bin, ok := map[string]string{"arm64": "qemu-system-aarch64", "amd64": "qemu-system-x86_64"}[arch]
	if !ok {
		return false
	}
	if _, err := exec.LookPath(bin); err != nil {
		return false
	}
	_, errA := exec.LookPath("cloud-localds")
	_, errB := exec.LookPath("genisoimage")
	return errA == nil || errB == nil
}
