// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package static

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func TestCheckEvidenceVerified(t *testing.T) {
	tests := []struct {
		name     string
		evidence checks.EvidenceStatus
		want     checks.Status
		contains string
	}{
		{name: "not attempted is a visible skip", evidence: checks.EvidenceStatus{Checked: false}, want: checks.StatusSkip, contains: "digest-only"},
		{name: "verification failure fails", evidence: checks.EvidenceStatus{Checked: true, Err: errors.New("subject mismatch")}, want: checks.StatusFail, contains: "subject mismatch"},
		{name: "verified passes naming the signer", evidence: checks.EvidenceStatus{Checked: true, SignerSAN: "https://github.com/x/wf@tag"}, want: checks.StatusPass, contains: "wf@tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := checkEvidenceVerified(context.Background(), checks.Target{}, checks.Inputs{Evidence: tt.evidence})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Status)
			assert.Contains(t, out.Evidence, tt.contains)
		})
	}
}

func TestParseBOM(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		wantErr string
		wantLen int
	}{
		{
			name:    "two components",
			content: "components:\n  nvke-agent: 0.4.0\n  spire-agent: 1.14.1+tpm1.9.0-2\n",
			wantLen: 2,
		},
		{name: "empty document is rejected", content: "components: {}\n", wantErr: "no components"},
		{name: "unknown field is rejected", content: "component:\n  nvke-agent: 0.4.0\n", wantErr: "field component not found"},
		{name: "shell metacharacters are not a package name", content: "components:\n  \"nvke-agent; rm -rf /\": 0.4.0\n", wantErr: "not a deb package name"},
		{name: "uppercase is not a package name", content: "components:\n  NVKE-Agent: 0.4.0\n", wantErr: "not a deb package name"},
		{name: "version must start with a digit", content: "components:\n  nvke-agent: latest\n", wantErr: "not a deb version"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("bom-%d.yaml", i))
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))
			bom, err := ParseBOM(path)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, bom, tt.wantLen)
		})
	}
}

func TestCheckBOMMatchesSBOM(t *testing.T) {
	sbom := map[string]string{"nvke-agent": "0.4.0", "spire-agent": "1.14.1+tpm1.9.0-2", "dash": "0.5.12-6ubuntu5"}
	tests := []struct {
		name     string
		in       checks.Inputs
		want     checks.Status
		contains string
		wantErr  string
	}{
		{
			name:     "no BOM is a visible skip",
			in:       checks.Inputs{SBOMPackages: sbom},
			want:     checks.StatusSkip,
			contains: "--target-bom",
		},
		{
			name:    "BOM without an SBOM cannot evaluate",
			in:      checks.Inputs{BOM: map[string]string{"nvke-agent": "0.4.0"}},
			wantErr: "no release SBOM",
		},
		{
			name:     "all components at required versions pass",
			in:       checks.Inputs{BOM: map[string]string{"nvke-agent": "0.4.0", "spire-agent": "1.14.1+tpm1.9.0-2"}, SBOMPackages: sbom},
			want:     checks.StatusPass,
			contains: "2 BOM components",
		},
		{
			name:     "version drift fails naming the component and both versions",
			in:       checks.Inputs{BOM: map[string]string{"nvke-agent": "0.5.0"}, SBOMPackages: sbom},
			want:     checks.StatusFail,
			contains: "nvke-agent: bom 0.5.0, sbom 0.4.0",
		},
		{
			name:     "absent component fails naming it",
			in:       checks.Inputs{BOM: map[string]string{"nvke-gpu-tools": "1.0"}, SBOMPackages: sbom},
			want:     checks.StatusFail,
			contains: "nvke-gpu-tools: bom 1.0, not in SBOM",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := checkBOMMatchesSBOM(context.Background(), checks.Target{}, tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Status)
			assert.Contains(t, out.Evidence, tt.contains)
		})
	}
}
