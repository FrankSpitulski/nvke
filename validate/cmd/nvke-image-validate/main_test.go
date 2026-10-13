// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
)

const artifact = "nke-node-fde-ubuntu-24.04.5-amd64.qcow2"

// fixture builds an image file + checksums.txt recording digest (tampered
// flips a byte of the recorded digest) and returns the common CLI args.
func fixture(t *testing.T, tampered bool) []string {
	t.Helper()
	dir := t.TempDir()
	content := []byte("not a real qcow2, and that is fine for the static tier")
	image := filepath.Join(dir, artifact)
	require.NoError(t, os.WriteFile(image, content, 0o644))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	if tampered {
		digest = "0" + digest[1:]
		if digest == hex.EncodeToString(sum[:]) {
			digest = "1" + digest[1:]
		}
	}
	checksums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(checksums, []byte(fmt.Sprintf("%s  %s\n", digest, artifact)), 0o644))
	return []string{
		"--image", image,
		"--checksums", checksums,
		"--checks", filepath.Join("..", "..", "checks.yaml"),
		"--target-variant", "fde",
		"--target-arch", "amd64",
		"--target-ubuntu", "24.04.5",
	}
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestRun(t *testing.T) {
	tests := []struct {
		name string
		args func(t *testing.T) []string
		want int
	}{
		{
			name: "valid image passes",
			args: func(t *testing.T) []string { return fixture(t, false) },
			want: report.ExitPass,
		},
		{
			name: "tampered checksums fail blocking",
			args: func(t *testing.T) []string { return fixture(t, true) },
			want: report.ExitFail,
		},
		{
			name: "identity mismatch fails blocking",
			args: func(t *testing.T) []string {
				args := fixture(t, false)
				for i, a := range args {
					if a == "fde" {
						args[i] = "non-fde"
					}
				}
				return args
			},
			want: report.ExitFail,
		},
		{
			name: "missing required flag is a usage error",
			args: func(_ *testing.T) []string { return []string{"--image", "x.qcow2"} },
			want: report.ExitError,
		},
		{
			name: "unreadable image is an error, not a fail",
			args: func(t *testing.T) []string {
				args := fixture(t, false)
				args[1] = filepath.Join(t.TempDir(), "missing.qcow2")
				return args
			},
			want: report.ExitError,
		},
		{
			name: "version flag exits 0",
			args: func(_ *testing.T) []string { return []string{"--version"} },
			want: report.ExitPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := devNull(t)
			assert.Equal(t, tt.want, run(tt.args(t), sink, sink))
		})
	}
}

func TestRunJSONFormat(t *testing.T) {
	args := append(fixture(t, false), "--format", "json")
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	assert.Equal(t, report.ExitPass, run(args, f, devNull(t)))
	require.NoError(t, f.Close())
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"schemaVersion": "1"`)
	assert.Contains(t, string(data), `"INT-001"`)
}

const realBundle = "../../testdata/release-v0.2.0-rc.1"

// TestRunBundleMode exercises the evidence-verifying path against the vendored
// real release bundle: the evidence verifies (INT-002 pass), while a wrong
// file posing as the image fails INT-001 — so the run exits 1 for the right
// reason, which the JSON report distinguishes.
func TestRunBundleMode(t *testing.T) {
	image := filepath.Join(t.TempDir(), artifact)
	require.NoError(t, os.WriteFile(image, []byte("wrong bytes"), 0o644))
	out := filepath.Join(t.TempDir(), "out.json")
	f, err := os.Create(out)
	require.NoError(t, err)
	code := run([]string{
		"--image", image,
		"--bundle", realBundle,
		"--checks", filepath.Join("..", "..", "checks.yaml"),
		"--target-variant", "fde", "--target-arch", "amd64", "--target-ubuntu", "24.04.5",
		"--format", "json",
	}, f, devNull(t))
	require.NoError(t, f.Close())
	assert.Equal(t, report.ExitFail, code)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Regexp(t, `"id": "INT-002",\s*"status": "pass"`, string(data))
	assert.Regexp(t, `"id": "INT-001",\s*"status": "fail"`, string(data))
}

func TestRunRequiresExactlyOneEvidenceSource(t *testing.T) {
	base := fixture(t, false)
	both := append(append([]string{}, base...), "--bundle", realBundle)
	neither := make([]string, 0, len(base)-2)
	for i := 0; i < len(base); i++ {
		if base[i] == "--checksums" {
			i++
			continue
		}
		neither = append(neither, base[i])
	}
	sink := devNull(t)
	assert.Equal(t, report.ExitError, run(both, sink, sink))
	assert.Equal(t, report.ExitError, run(neither, sink, sink))
}
