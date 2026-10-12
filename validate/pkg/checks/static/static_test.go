// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package static

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

const artifact = "nke-node-fde-ubuntu-24.04.5-amd64.qcow2"

// writeFixture writes content to dir/name and returns its path and sha256.
func writeFixture(t *testing.T, dir, name string, content []byte) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0o644))
	sum := sha256.Sum256(content)
	return path, hex.EncodeToString(sum[:])
}

func TestParseChecksums(t *testing.T) {
	dir := t.TempDir()
	good := fmt.Sprintf("%064d  a.qcow2\n%064d  b.spdx.json\n", 1, 2)
	tests := []struct {
		name    string
		content string
		wantErr string
		wantLen int
	}{
		{name: "two well-formed entries", content: good, wantLen: 2},
		{name: "binary-mode asterisk is tolerated", content: fmt.Sprintf("%064d  *a.qcow2\n", 1), wantLen: 1},
		{name: "short digest is rejected", content: "abc123  a.qcow2\n", wantErr: "not sha256sum format"},
		{name: "missing separator is rejected", content: fmt.Sprintf("%064d a.qcow2\n", 1), wantErr: "not sha256sum format"},
		{name: "conflicting duplicate is rejected", content: fmt.Sprintf("%064d  a.qcow2\n%064d  a.qcow2\n", 1, 2), wantErr: "conflicting digests"},
		{name: "empty file is rejected", content: "\n", wantErr: "no entries"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("sums-%d.txt", i))
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))
			sums, err := ParseChecksums(path)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, sums, tt.wantLen)
		})
	}
}

func TestIdentityFromName(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
		want     Identity
		wantErr  bool
	}{
		{
			name:     "fde amd64",
			artifact: "nke-node-fde-ubuntu-24.04.5-amd64.qcow2",
			want:     Identity{Variant: checks.VariantFDE, Ubuntu: "24.04.5", Arch: "amd64"},
		},
		{
			name:     "non-fde arm64",
			artifact: "nke-node-non-fde-ubuntu-26.04.1-arm64.qcow2",
			want:     Identity{Variant: checks.VariantNonFDE, Ubuntu: "26.04.1", Arch: "arm64"},
		},
		{
			name:     "renamed nvke spelling",
			artifact: "nvke-node-fde-ubuntu-26.04.1-amd64.qcow2",
			want:     Identity{Variant: checks.VariantFDE, Ubuntu: "26.04.1", Arch: "amd64"},
		},
		{name: "not an image name", artifact: "nke-os-image-check", wantErr: true},
		{name: "unknown arch", artifact: "nke-node-fde-ubuntu-24.04.5-riscv.qcow2", wantErr: true},
		{name: "neither prefix generation", artifact: "nvvke-node-fde-ubuntu-24.04.5-amd64.qcow2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IdentityFromName(tt.artifact)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckImageDigest(t *testing.T) {
	dir := t.TempDir()
	_, digest := writeFixture(t, dir, artifact, []byte("image bytes"))
	target := checks.Target{ImageName: artifact}

	tests := []struct {
		name     string
		inputs   checks.Inputs
		want     checks.Status
		evidence string
	}{
		{
			name:   "digest matches",
			inputs: checks.Inputs{ImageSHA256: digest, Checksums: map[string]string{artifact: digest}},
			want:   checks.StatusPass,
		},
		{
			name:     "tampered image",
			inputs:   checks.Inputs{ImageSHA256: "deadbeef", Checksums: map[string]string{artifact: digest}},
			want:     checks.StatusFail,
			evidence: "!= recorded",
		},
		{
			name:     "artifact missing from checksums",
			inputs:   checks.Inputs{ImageSHA256: digest, Checksums: map[string]string{"other.qcow2": digest}},
			want:     checks.StatusFail,
			evidence: "not listed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := checkImageDigest(context.Background(), target, tt.inputs)
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Status)
			assert.Contains(t, out.Evidence, tt.evidence)
		})
	}
}

func TestCheckArtifactIdentity(t *testing.T) {
	tests := []struct {
		name   string
		target checks.Target
		want   checks.Status
	}{
		{
			name:   "identity matches",
			target: checks.Target{ImageName: artifact, Variant: checks.VariantFDE, Arch: "amd64", Ubuntu: "24.04.5"},
			want:   checks.StatusPass,
		},
		{
			name:   "variant mismatch",
			target: checks.Target{ImageName: artifact, Variant: checks.VariantNonFDE, Arch: "amd64", Ubuntu: "24.04.5"},
			want:   checks.StatusFail,
		},
		{
			name:   "arch mismatch",
			target: checks.Target{ImageName: artifact, Variant: checks.VariantFDE, Arch: "arm64", Ubuntu: "24.04.5"},
			want:   checks.StatusFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := checkArtifactIdentity(context.Background(), tt.target, checks.Inputs{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Status)
		})
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path, want := writeFixture(t, dir, "blob", []byte("some bytes"))
	got, err := HashFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	_, err = HashFile(filepath.Join(dir, "missing"))
	require.Error(t, err)
}
