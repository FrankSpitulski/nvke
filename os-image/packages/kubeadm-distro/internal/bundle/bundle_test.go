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

func TestValidateArtifactBasename(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain binary", "kubeadm", false},
		{"tarball", "containerd-2.1.7-linux-amd64.tar.gz", false},
		{"runc asset", "runc.amd64", false},
		{"traversal", "../../../etc/passwd", true},
		{"slash", "foo/bar", true},
		{"empty", "", true},
		{"leading dot", ".hidden", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArtifactBasename(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestParseSHA256Sidecar(t *testing.T) {
	const digest = "6a2346006132f6e1ed0b5248e518098cf5abbce25bf11b8926fb1073091b83f4"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare", digest + "\n", digest, false},
		{"gnu form", digest + "  kubeadm\n", digest, false},
		{"uppercase", "6A2346006132F6E1ED0B5248E518098CF5ABBCE25BF11B8926FB1073091B83F4\n", digest, false},
		{"comment then digest", "# generated\n" + digest + "\n", digest, false},
		{"too short", digest[:63] + "\n", "", true},
		{"not hex", "zz" + digest[2:] + "\n", "", true},
		{"empty", "\n\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSHA256Sidecar([]byte(tt.in))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestVerifyFileAgainstSidecar(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(artifact, []byte("hello"), 0o644))

	digest, err := WriteSHA256Sidecar(artifact)
	require.NoError(t, err)

	sidecar := artifact + ".sha256"
	got, err := VerifyFileAgainstSidecar(artifact, sidecar)
	require.NoError(t, err)
	assert.Equal(t, digest, got)

	require.NoError(t, os.WriteFile(sidecar, []byte("0"+digest[1:]+"\n"), 0o644))
	_, err = VerifyFileAgainstSidecar(artifact, sidecar)
	require.Error(t, err)
}

func TestExtractPGPSignedClearText(t *testing.T) {
	const signed = `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  runc.amd64
bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  runc.arm64
-----BEGIN PGP SIGNATURE-----
abcdef
-----END PGP SIGNATURE-----
`
	want := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  runc.amd64\n" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  runc.arm64"
	assert.Equal(t, want, ExtractPGPSignedClearText(signed))
	assert.Equal(t, "plain text only", ExtractPGPSignedClearText("plain text only"))
	assert.True(t, IsPGPSignedMessage(signed))
	assert.False(t, IsPGPSignedMessage("plain text only"))
}
