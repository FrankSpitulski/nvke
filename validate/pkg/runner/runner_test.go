// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runner

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

	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
)

// fixtureOptions builds a digest-only run over a crafted image whose digest
// matches its checksums entry.
func fixtureOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	const artifact = "nke-node-fde-ubuntu-24.04.5-amd64.qcow2"
	content := []byte("runner fixture bytes")
	image := filepath.Join(dir, artifact)
	require.NoError(t, os.WriteFile(image, content, 0o600))
	sum := sha256.Sum256(content)
	checksums := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(checksums, fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(sum[:]), artifact), 0o600))
	return Options{
		ImagePath:     image,
		ChecksumsPath: checksums,
		RegistryPath:  filepath.Join("..", "..", "checks.yaml"),
		Variant:       "fde",
		Arch:          "amd64",
		Ubuntu:        "24.04.5",
		Inspect:       "off",
	}
}

func sink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestUnknownFormatRejectsBeforeSideEffects pins the round-3 regression: an
// invalid format must fail before any verdict is signed or written.
func TestUnknownFormatRejectsBeforeSideEffects(t *testing.T) {
	opts := fixtureOptions(t)
	opts.Format = "xml"
	opts.EmitVerdictPath = filepath.Join(t.TempDir(), "verdict.json")
	opts.SignKeyPath = filepath.Join(t.TempDir(), "missing.key")

	exit, err := Run(context.Background(), opts, sink(t))
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "unknown report format")
	_, statErr := os.Stat(opts.EmitVerdictPath)
	assert.True(t, os.IsNotExist(statErr), "no verdict may exist behind an exit-2 usage error")
}

// TestVerdictPathMustNotAliasTheReportStream pins destination separation:
// both documents interleaved in one sink is neither a report nor a verdict.
func TestVerdictPathMustNotAliasTheReportStream(t *testing.T) {
	opts := fixtureOptions(t)
	out := sink(t)
	opts.EmitVerdictPath = out.Name()
	opts.SignKeyPath = filepath.Join(t.TempDir(), "missing.key")

	exit, err := Run(context.Background(), opts, out)
	assert.Equal(t, report.ExitError, exit)
	require.ErrorContains(t, err, "aliases the report output")
}

// TestRunPassesOnTheFixture keeps the happy path pinned at this level too.
func TestRunPassesOnTheFixture(t *testing.T) {
	exit, err := Run(context.Background(), fixtureOptions(t), sink(t))
	require.NoError(t, err)
	assert.Equal(t, report.ExitPass, exit)
}
