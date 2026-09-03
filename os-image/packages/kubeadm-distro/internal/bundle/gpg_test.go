// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireGPG skips the test when gpg is not on PATH. This is the only
// allowed skip in this file: every other case must run.
func requireGPG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH; skipping GPG-dependent test")
	}
}

// gpgHome creates a fresh, isolated GNUPGHOME for the duration of the test
// and kills everything gpg started against it (agent, keyboxd, dirmngr) on
// cleanup so subtests don't leak daemons.
//
// It deliberately does not use t.TempDir(): that nests under a path made
// from the test name, which routinely exceeds the ~104-byte limit on
// AF_UNIX socket paths (macOS and some Linux libc). gpg-agent's socket
// lives under GNUPGHOME, so a long homedir makes gpg fail to start its
// agent with an opaque "General error" rather than a path-length error.
func gpgHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "gpg")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(home, 0o700))
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	})
	return home
}

// runGPG runs gpg against the given homedir and fails the test on error.
func runGPG(t *testing.T, home string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("gpg", append([]string{"--homedir", home}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "gpg %v: %s", args, out)
	return out
}

// quickGenKey generates an unattended, passphrase-less key for uid in home,
// with the given quick-gen-key EXPIRE argument (for example "0" for no
// expiration).
func quickGenKey(t *testing.T, home, uid, expire string) {
	t.Helper()
	runGPG(t, home,
		"--batch", "--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-gen-key", uid, "default", "default", expire,
	)
}

// clearsign clearsigns text as uid in home and returns the armored document.
func clearsign(t *testing.T, home, uid, text string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "msg.txt")
	require.NoError(t, os.WriteFile(src, []byte(text), 0o644))
	out := filepath.Join(dir, "msg.asc")
	runGPG(t, home,
		"--local-user", uid, "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
		"--clearsign", "-o", out, src,
	)
	body, err := os.ReadFile(out)
	require.NoError(t, err)
	return string(body)
}

// exportKeyring exports uid's public key from home as a binary keyring file
// under a fresh temp dir and returns its path.
func exportKeyring(t *testing.T, home, uid string) string {
	t.Helper()
	out := runGPG(t, home, "--export", uid)
	path := filepath.Join(t.TempDir(), "keyring.gpg")
	require.NoError(t, os.WriteFile(path, out, 0o644))
	return path
}

func TestVerifyClearsignedChecksumDoc(t *testing.T) {
	requireGPG(t)

	t.Run("valid signature verifies and cleartext round-trips", func(t *testing.T) {
		home := gpgHome(t)
		const uid = "good@example.com"
		quickGenKey(t, home, "Good Key <"+uid+">", "0")
		const text = "hello world"
		doc := clearsign(t, home, uid, text)
		keyring := exportKeyring(t, home, uid)

		err := VerifyClearsignedChecksumDoc(doc, keyring)
		require.NoError(t, err)
		assert.Equal(t, text, ExtractPGPSignedClearText(doc))
	})

	t.Run("tampered body fails with BADSIG", func(t *testing.T) {
		home := gpgHome(t)
		const uid = "tamper@example.com"
		quickGenKey(t, home, "Tamper Key <"+uid+">", "0")
		doc := clearsign(t, home, uid, "hello world")
		keyring := exportKeyring(t, home, uid)

		tampered := strings.Replace(doc, "hello world", "bye world!!", 1)

		err := VerifyClearsignedChecksumDoc(tampered, keyring)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "BADSIG")
	})

	t.Run("unsigned plain text is rejected before gpg runs", func(t *testing.T) {
		home := gpgHome(t)
		const uid = "unused@example.com"
		quickGenKey(t, home, "Unused Key <"+uid+">", "0")
		keyring := exportKeyring(t, home, uid)

		err := VerifyClearsignedChecksumDoc("just some plain text\n", keyring)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not PGP clearsigned")
	})

	t.Run("signed by a key not in the keyring fails with NO_PUBKEY", func(t *testing.T) {
		signerHome := gpgHome(t)
		const signerUID = "signer@example.com"
		quickGenKey(t, signerHome, "Signer Key <"+signerUID+">", "0")
		doc := clearsign(t, signerHome, signerUID, "hello world")

		// A different key's keyring: valid gpg keyring, but it doesn't hold
		// the signer's public key.
		otherHome := gpgHome(t)
		const otherUID = "other@example.com"
		quickGenKey(t, otherHome, "Other Key <"+otherUID+">", "0")
		otherKeyring := exportKeyring(t, otherHome, otherUID)

		err := VerifyClearsignedChecksumDoc(doc, otherKeyring)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NO_PUBKEY")
	})

	// The next two cases are regression tests for a real bug: gpg resolves a
	// signer from the caller's ambient GNUPGHOME whenever it isn't told
	// otherwise, so a key that happens to live in the *caller's* default
	// keybox -- not in the pinned keyring passed to
	// VerifyClearsignedChecksumDoc -- could verify clean instead of failing
	// with NO_PUBKEY. VerifyClearsignedChecksumDoc now always runs gpg
	// against a scratch homedir it creates itself, so setting GNUPGHOME in
	// the calling process must not change the outcome, with or without
	// use-keyboxd enabled in that ambient home.

	t.Run("signer key present only in the caller's ambient GNUPGHOME still fails with NO_PUBKEY", func(t *testing.T) {
		ambientHome := gpgHome(t)
		const uid = "ambient@example.com"
		quickGenKey(t, ambientHome, "Ambient Key <"+uid+">", "0")
		doc := clearsign(t, ambientHome, uid, "hello world")

		otherHome := gpgHome(t)
		const otherUID = "other2@example.com"
		quickGenKey(t, otherHome, "Other Key <"+otherUID+">", "0")
		otherKeyring := exportKeyring(t, otherHome, otherUID)

		t.Setenv("GNUPGHOME", ambientHome)
		err := VerifyClearsignedChecksumDoc(doc, otherKeyring)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NO_PUBKEY")
	})

	t.Run("ambient GNUPGHOME with use-keyboxd enabled still fails with NO_PUBKEY", func(t *testing.T) {
		ambientHome := gpgHome(t)
		require.NoError(t, os.WriteFile(filepath.Join(ambientHome, "common.conf"), []byte("use-keyboxd\n"), 0o600))
		const uid = "keyboxd@example.com"
		quickGenKey(t, ambientHome, "Keyboxd Key <"+uid+">", "0")
		doc := clearsign(t, ambientHome, uid, "hello world")

		otherHome := gpgHome(t)
		const otherUID = "other3@example.com"
		quickGenKey(t, otherHome, "Other Key <"+otherUID+">", "0")
		otherKeyring := exportKeyring(t, otherHome, otherUID)

		t.Setenv("GNUPGHOME", ambientHome)
		err := VerifyClearsignedChecksumDoc(doc, otherKeyring)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NO_PUBKEY")
	})
}

// TestCheckGpgStatus covers checkGpgStatus directly against fixed
// `--status-fd` transcripts. This is also where the expired/revoked-key
// branches are exercised: constructing a live key with an expiry short
// enough for a fast unit test, but long enough for keygen and clearsign to
// finish first, is possible but flaky and adds real wall-clock time; the
// status-line contract is what matters and is stable to pin directly here.
func TestCheckGpgStatus(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr string
	}{
		{
			name: "good and valid",
			out: "gpg: Signature made ...\n" +
				"[GNUPG:] NEWSIG\n" +
				"[GNUPG:] GOODSIG D5AE4FE860E37A67 Good Key <good@example.com>\n" +
				"[GNUPG:] VALIDSIG F5829075E7D73EEA09E70DC6D5AE4FE860E37A67 2026-09-03 1788451112 0 4 0 22 10 01 F5829075E7D73EEA09E70DC6D5AE4FE860E37A67\n",
		},
		{
			name:    "goodsig without validsig",
			out:     "[GNUPG:] GOODSIG D5AE4FE860E37A67 Good Key <good@example.com>\n",
			wantErr: "no VALIDSIG",
		},
		{
			name:    "exit 0 but no goodsig at all",
			out:     "gpg: nothing of interest happened\n",
			wantErr: "no GOODSIG",
		},
		{
			name:    "expired key",
			out:     "[GNUPG:] EXPKEYSIG DF695848A14D36E3 Test Key <test@example.com>\n",
			wantErr: "EXPKEYSIG",
		},
		{
			name:    "revoked key",
			out:     "[GNUPG:] REVKEYSIG DF695848A14D36E3 Test Key <test@example.com>\n",
			wantErr: "REVKEYSIG",
		},
		{
			name:    "expired signature",
			out:     "[GNUPG:] EXPSIG DF695848A14D36E3 Test Key <test@example.com>\n",
			wantErr: "EXPSIG",
		},
		{
			name:    "bad signature",
			out:     "[GNUPG:] BADSIG D5AE4FE860E37A67 Good Key <good@example.com>\n",
			wantErr: "BADSIG",
		},
		{
			name:    "missing public key",
			out:     "[GNUPG:] NO_PUBKEY D5AE4FE860E37A67\n",
			wantErr: "NO_PUBKEY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGpgStatus([]byte(tt.out))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestFinishVerifyRejectsIgnoredKeyring covers the tripwire directly: a
// GOODSIG/VALIDSIG status stream must still be refused when gpg reports that
// it ignored the --keyring option, because the signature was then checked
// against some other key store.
func TestFinishVerifyRejectsIgnoredKeyring(t *testing.T) {
	good := []byte("[GNUPG:] GOODSIG D5AE4FE860E37A67 Good Key <good@example.com>\n" +
		"[GNUPG:] VALIDSIG F5829075E7D73EEA09E70DC6D5AE4FE860E37A67 2026-09-03 1788451112 0 4 0 22 10 01 F5829075E7D73EEA09E70DC6D5AE4FE860E37A67\n")

	require.NoError(t, finishVerify(good, "", nil))

	err := finishVerify(good, "gpg: Note: Specified keyrings are ignored due to option \"use-keyboxd\"\n", nil)
	require.ErrorContains(t, err, "ignored --keyring")
}
