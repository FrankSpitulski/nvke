// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsPGPSignedMessage reports whether body is an OpenPGP clearsigned document.
func IsPGPSignedMessage(body string) bool {
	return strings.Contains(body, "-----BEGIN PGP SIGNED MESSAGE-----")
}

// ExtractPGPSignedClearText returns the signed payload of a clearsigned document,
// or body unchanged when it is not clearsigned.
func ExtractPGPSignedClearText(body string) string {
	const begin = "-----BEGIN PGP SIGNED MESSAGE-----"
	const sigBegin = "-----BEGIN PGP SIGNATURE-----"
	i := strings.Index(body, begin)
	if i < 0 {
		return body
	}
	// The armor header block ends at the first blank line; CRLF documents never
	// contain a bare "\n\n", so checking LF first is unambiguous.
	rest := body[i+len(begin):]
	var payload string
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		payload = rest[j+2:]
	} else if j := strings.Index(rest, "\r\n\r\n"); j >= 0 {
		payload = rest[j+4:]
	} else {
		return body
	}
	if k := strings.Index(payload, sigBegin); k >= 0 {
		payload = payload[:k]
	}
	return strings.TrimSpace(payload)
}

// VerifyClearsignedChecksumDoc runs `gpg --verify` on a clearsigned checksum
// document. keyringPath must be a gpg keyring file, such as the bundled
// packaging/keyrings/runc-release.gpg.
func VerifyClearsignedChecksumDoc(body, keyringPath string) error {
	if !IsPGPSignedMessage(body) {
		return fmt.Errorf("checksum document is not PGP clearsigned")
	}
	keyringPath = filepath.Clean(keyringPath)
	st, err := os.Stat(keyringPath)
	if err != nil {
		return fmt.Errorf("keyring %s: %w", keyringPath, err)
	}
	if st.IsDir() {
		return fmt.Errorf("keyring %s is a directory", keyringPath)
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		return fmt.Errorf("gpg not found in PATH (install gnupg): %w", err)
	}

	tmp, err := os.CreateTemp("", "bundle-checksum-*.asc")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write clearsigned document: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	out, err := exec.Command("gpg", "--verify", "--no-default-keyring", "--keyring", keyringPath, tmpPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gpg --verify failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
