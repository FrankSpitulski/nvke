// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vendored fixture is the real os-image/v0.2.0-rc.1 release evidence:
// its published checksums.txt, the sigstore bundle GitHub Actions attested it
// with, and the trusted root pinned at vendoring time. Everything verifies
// offline — a test that reaches the network is a defect.
const fixture = "../../testdata/release-v0.2.0-rc.1"

// releaseIdentity matches the workflow that signs os-image release evidence.
func releaseIdentity() Identity {
	return Identity{
		Issuer:   "https://token.actions.githubusercontent.com",
		SANRegex: `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`,
	}
}

func TestLoadParsesChecksums(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)
	assert.Len(t, b.Checksums, 17, "the fixture release ships 8 qcow2s + 8 SBOMs + 1 checker binary")
	assert.Contains(t, b.Checksums, "nke-node-fde-ubuntu-24.04.5-amd64.qcow2")
}

func TestVerifyEvidence(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)

	ev, err := b.VerifyEvidence(releaseIdentity())
	require.NoError(t, err)
	assert.Contains(t, ev.SignerSAN, "os-image-artifact.yml@refs/tags/os-image/v0.2.0-rc.1",
		"the verified signer is the release workflow at the release tag")
}

func TestVerifyEvidenceRejectsWrongIdentity(t *testing.T) {
	b, err := Load(fixture)
	require.NoError(t, err)

	tests := []struct {
		name string
		id   Identity
	}{
		{name: "wrong workflow", id: Identity{Issuer: "https://token.actions.githubusercontent.com", SANRegex: `^https://github\.com/someone-else/.*$`}},
		{name: "wrong issuer", id: Identity{Issuer: "https://accounts.example.com", SANRegex: `.*`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := b.VerifyEvidence(tt.id)
			require.Error(t, err)
		})
	}
}

// copyFixture clones the fixture into a temp dir so a test can tamper with one
// member without touching the vendored evidence.
func copyFixture(t *testing.T, mutate func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{ChecksumsName, AttestationName, TrustedRootName} {
		data, err := os.ReadFile(filepath.Join(fixture, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	mutate(dir)
	return dir
}

func TestVerifyEvidenceRejectsTamperedChecksums(t *testing.T) {
	dir := copyFixture(t, func(dir string) {
		path := filepath.Join(dir, ChecksumsName)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		// Swap one digest hex char: still parses, no longer the attested bytes.
		data[0] = map[byte]byte{'0': '1'}[data[0]] | 'a'
		require.NoError(t, os.WriteFile(path, data, 0o644))
	})
	b, err := Load(dir)
	require.NoError(t, err, "a tampered checksums file still parses; only verification catches it")
	_, err = b.VerifyEvidence(releaseIdentity())
	require.Error(t, err)
	assert.ErrorContains(t, err, "evidence verification")
}

func TestVerifyEvidenceFailsClosedOnMissingPieces(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		want   string
	}{
		{name: "no attestation", remove: AttestationName, want: "no readable attestation"},
		{name: "no trusted root", remove: TrustedRootName, want: "no readable trusted root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := copyFixture(t, func(dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, tt.remove)))
			})
			b, err := Load(dir)
			require.NoError(t, err)
			_, err = b.VerifyEvidence(releaseIdentity())
			require.ErrorContains(t, err, tt.want)
		})
	}
}
