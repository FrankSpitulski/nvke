// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"fmt"
	"io"
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
//
// gpg is run against a scratch GNUPGHOME created and destroyed for this call
// alone. Without that, gpg inherits the caller's ambient GNUPGHOME, and if
// that home has "use-keyboxd" set in common.conf (an opt-in in GnuPG 2.4,
// the default in 2.5+), gpg silently ignores --keyring and --no-default-keyring
// and resolves the signer from the caller's own keybox instead of the pinned
// runc-release.gpg — so a key we never vetted can verify clean.
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

	// A scratch homedir, not the caller's ambient GNUPGHOME: see the doc
	// comment above for why that matters.
	scratchHome, err := os.MkdirTemp("", "nke-gpg-verify-")
	if err != nil {
		return fmt.Errorf("create scratch GNUPGHOME: %w", err)
	}
	if err := os.Chmod(scratchHome, 0o700); err != nil {
		os.RemoveAll(scratchHome)
		return fmt.Errorf("chmod scratch GNUPGHOME: %w", err)
	}
	defer func() {
		_ = exec.Command("gpgconf", "--homedir", scratchHome, "--kill", "all").Run()
		_ = os.RemoveAll(scratchHome)
	}()

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create status-fd pipe: %w", err)
	}
	defer statusR.Close()

	cmd := exec.Command("gpg",
		"--homedir", scratchHome,
		"--no-options",
		"--batch",
		"--no-tty",
		"--status-fd", "3",
		"--verify",
		"--no-default-keyring",
		"--keyring", keyringPath,
		tmpPath,
	)
	cmd.ExtraFiles = []*os.File{statusW}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	var statusOut []byte
	runErr := cmd.Start()
	// Close our end of the write side. gpg holds the other end (via
	// ExtraFiles); once gpg closes or exits, reading statusR hits EOF.
	statusW.Close()
	if runErr == nil {
		var readErr error
		statusOut, readErr = io.ReadAll(statusR)
		waitErr := cmd.Wait()
		runErr = readErr
		if runErr == nil {
			runErr = waitErr
		}
	}
	return finishVerify(statusOut, stderr.String(), runErr)
}

// finishVerify interprets the status-fd transcript and stderr from a gpg
// --verify invocation, in that priority order.
func finishVerify(statusOut []byte, stderrOut string, runErr error) error {
	stderrOut = strings.TrimSpace(stderrOut)
	// A hard tripwire: if gpg tells us it ignored our --keyring option (for
	// example because the homedir has use-keyboxd set), the verification
	// that follows means nothing, no matter what the status stream says.
	if strings.Contains(stderrOut, "Specified keyrings are ignored") {
		return fmt.Errorf("gpg ignored --keyring (%s): refusing to trust the result", strings.TrimSpace(stderrOut))
	}
	if err := checkGpgStatus(statusOut); err != nil {
		if runErr != nil {
			return fmt.Errorf("%w (gpg --verify: %w: %s)", err, runErr, stderrOut)
		}
		return err
	}
	if runErr != nil {
		return fmt.Errorf("gpg --verify failed: %w: %s", runErr, stderrOut)
	}
	return nil
}

// checkGpgStatus inspects gpg's `--status-fd` machine-readable output and
// reports whether the signature is trustworthy. It requires both a GOODSIG
// and a VALIDSIG line, and treats EXPKEYSIG, REVKEYSIG, EXPSIG, BADSIG, and
// NO_PUBKEY as hard failures even if gpg's exit status was 0.
func checkGpgStatus(out []byte) error {
	var haveGoodsig, haveValidsig bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[GNUPG:] ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "[GNUPG:] "))
		if len(fields) == 0 {
			continue
		}
		token := fields[0]
		switch token {
		case "GOODSIG":
			haveGoodsig = true
		case "VALIDSIG":
			haveValidsig = true
		case "EXPKEYSIG", "REVKEYSIG", "EXPSIG", "BADSIG", "NO_PUBKEY":
			return fmt.Errorf("gpg status %s: signature rejected", token)
		}
	}
	if !haveGoodsig {
		return fmt.Errorf("gpg status: no GOODSIG reported")
	}
	if !haveValidsig {
		return fmt.Errorf("gpg status: no VALIDSIG reported")
	}
	return nil
}
