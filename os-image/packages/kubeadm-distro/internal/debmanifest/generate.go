// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
)

// scriptNames are the maintainer script members debmanifest looks for in the
// control tarball.
var scriptNames = map[string]bool{"preinst": true, "postinst": true, "prerm": true, "postrm": true}

// Generate builds a normalized Manifest for the .deb at debPath by shelling
// out to dpkg-deb: "-f" for the control fields, "--ctrl-tarfile" for
// conffiles and the maintainer scripts, and "--fsys-tarfile" for the file
// listing. It streams both tarballs rather than extracting them to disk,
// since a bundled kubeadm-distro .deb runs several hundred megabytes.
func Generate(debPath string) (Manifest, error) {
	m := Manifest{Control: map[string][]string{}}

	if err := readControlFields(debPath, &m); err != nil {
		return Manifest{}, err
	}
	if err := readCtrlTarfile(debPath, &m); err != nil {
		return Manifest{}, err
	}
	if err := readFsysTarfile(debPath, &m); err != nil {
		return Manifest{}, err
	}

	m.normalize()
	return m, nil
}

// dpkgErr wraps err from running dpkg-deb, appending the trimmed stderr to
// the message when err is an *exec.ExitError. stderr is used if non-empty
// (the streaming callers pipe stdout but capture stderr into their own
// buffer); otherwise it falls back to the ExitError's own Stderr field,
// which exec.Cmd.Output populates automatically.
func dpkgErr(err error, desc string, stderr []byte) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("%s: %w", desc, err)
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(exitErr.Stderr))
	}
	if msg == "" {
		return fmt.Errorf("%s: %w", desc, err)
	}
	return fmt.Errorf("%s: %w: %s", desc, err, msg)
}

// readControlFields runs "dpkg-deb -f" for the fields this contract covers.
// Fields absent from the package are simply missing from the output; dpkg-deb
// does not error for them.
func readControlFields(debPath string, m *Manifest) error {
	args := append([]string{"-f", debPath}, controlOrder...)
	out, err := exec.Command("dpkg-deb", args...).Output()
	if err != nil {
		return dpkgErr(err, fmt.Sprintf("dpkg-deb -f %s", debPath), nil)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		field, value, ok := strings.Cut(line, ": ")
		if !ok {
			return fmt.Errorf("dpkg-deb -f %s: unparseable control line %q", debPath, line)
		}
		if listFields[field] {
			vals := splitCommaList(value)
			if field == "Conflicts" || field == "Replaces" {
				vals = dropPeerFieldValues(vals)
			}
			m.Control[field] = vals
		} else {
			m.Control[field] = []string{value}
		}
	}
	return nil
}

// splitCommaList splits a Debian comma-separated field value, trimming the
// whitespace dpkg-deb -f already inserts after each comma.
func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readCtrlTarfile streams "dpkg-deb --ctrl-tarfile" to find DEBIAN/conffiles
// and which maintainer scripts are present.
func readCtrlTarfile(debPath string, m *Manifest) error {
	desc := fmt.Sprintf("dpkg-deb --ctrl-tarfile %s", debPath)
	cmd := exec.Command("dpkg-deb", "--ctrl-tarfile", debPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}
	if err := cmd.Start(); err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}

	tr := tar.NewReader(stdout)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = cmd.Wait()
			return fmt.Errorf("reading ctrl-tarfile of %s: %w", debPath, err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		switch {
		case name == "conffiles":
			data, err := io.ReadAll(tr)
			if err != nil {
				_ = cmd.Wait()
				return fmt.Errorf("reading conffiles in %s: %w", debPath, err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				if line != "" {
					m.Conffiles = append(m.Conffiles, line)
				}
			}
		case scriptNames[name]:
			m.Scripts = append(m.Scripts, name)
		}
	}
	if err := cmd.Wait(); err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}
	return nil
}

// readFsysTarfile streams "dpkg-deb --fsys-tarfile" to build the file
// listing, hashing regular files under binDirs as it goes.
func readFsysTarfile(debPath string, m *Manifest) error {
	desc := fmt.Sprintf("dpkg-deb --fsys-tarfile %s", debPath)
	cmd := exec.Command("dpkg-deb", "--fsys-tarfile", debPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}
	if err := cmd.Start(); err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}

	tr := tar.NewReader(stdout)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = cmd.Wait()
			return fmt.Errorf("reading fsys-tarfile of %s: %w", debPath, err)
		}

		path := "/" + strings.TrimPrefix(hdr.Name, "./")
		if path == "/" {
			continue
		}

		entry := FileEntry{Path: path, Mode: permString(hdr.FileInfo().Mode())}
		switch hdr.Typeflag {
		case tar.TypeDir:
			entry.Type = "d"
		case tar.TypeReg:
			entry.Type = "f"
			if isBinPath(path) {
				h := sha256.New()
				if _, err := io.Copy(h, tr); err != nil {
					_ = cmd.Wait()
					return fmt.Errorf("hashing %s in %s: %w", path, debPath, err)
				}
				entry.SHA256 = hex.EncodeToString(h.Sum(nil))
			}
		case tar.TypeSymlink:
			entry.Type = "l"
			entry.LinkTarget = hdr.Linkname
		case tar.TypeLink:
			// Unlike a symlink target (an arbitrary filesystem-relative
			// string), a hardlink's Linkname names another archive member,
			// so it comes back "./usr/..." the same way hdr.Name does; give
			// it the same absolute-path normalization as every Path.
			entry.Type = "h"
			entry.LinkTarget = "/" + strings.TrimPrefix(hdr.Linkname, "./")
		case tar.TypeChar:
			entry.Type = "c"
		case tar.TypeBlock:
			entry.Type = "b"
		case tar.TypeFifo:
			entry.Type = "p"
		default:
			_ = cmd.Wait()
			return fmt.Errorf("%s: unsupported tar entry type %q for %s", debPath, string(hdr.Typeflag), path)
		}
		m.Files = append(m.Files, entry)
	}
	if err := cmd.Wait(); err != nil {
		return dpkgErr(err, desc, stderr.Bytes())
	}
	return nil
}

// permString renders mode as a "rwxr-xr-x" style string, matching the second
// field of `tar -tv`, including the setuid/setgid/sticky bits in their usual
// s/s/t (or S/S/T when the underlying execute bit is unset) positions.
func permString(mode fs.FileMode) string {
	const rwx = "rwxrwxrwx"
	perm := mode.Perm()
	b := []byte(rwx)
	for i := 0; i < 9; i++ {
		if perm&(1<<uint(8-i)) == 0 {
			b[i] = '-'
		}
	}
	if mode&fs.ModeSetuid != 0 {
		if b[2] == 'x' {
			b[2] = 's'
		} else {
			b[2] = 'S'
		}
	}
	if mode&fs.ModeSetgid != 0 {
		if b[5] == 'x' {
			b[5] = 's'
		} else {
			b[5] = 'S'
		}
	}
	if mode&fs.ModeSticky != 0 {
		if b[8] == 'x' {
			b[8] = 't'
		} else {
			b[8] = 'T'
		}
	}
	return string(b)
}
