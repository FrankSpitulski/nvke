// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGolden writes a manifest text file so Check has something to compare
// against; it does not need to correspond to a real .deb.
func writeGolden(t *testing.T, goldenDir, version string, m Manifest) {
	t.Helper()
	m.normalize()
	require.NoError(t, os.WriteFile(filepath.Join(goldenDir, "s3-"+version+".manifest"), []byte(m.String()), 0o644))
}

func TestCheckAdvisoryModeWarnsButDoesNotFail(t *testing.T) {
	deb, _ := buildFixtureDeb(t)
	outDir := t.TempDir()
	require.NoError(t, os.Rename(deb, filepath.Join(outDir, "nke-kubeadm-distro-1.36.3_1_amd64.deb")))

	goldenDir := t.TempDir()
	// A golden that will not match the fixture .deb (different Package),
	// forcing a real diff.
	writeGolden(t, goldenDir, "1.36.3", Manifest{Control: map[string][]string{"Package": {"not-fixture"}}})

	deviationsPath := filepath.Join(t.TempDir(), "deviations.txt")
	require.NoError(t, os.WriteFile(deviationsPath, []byte("# empty, no directives yet\n"), 0o644))

	report, err := Check(outDir, goldenDir, deviationsPath, nil)
	require.NoError(t, err)
	assert.False(t, report.Strict)
	assert.False(t, report.Failed(), "advisory mode must not fail the build even with a real diff")
	assert.Contains(t, report.Checked, "nke-kubeadm-distro-1.36.3_1_amd64.deb")
	assert.NotEmpty(t, report.Diffs["nke-kubeadm-distro-1.36.3_1_amd64.deb"])
}

func TestCheckStrictModeFails(t *testing.T) {
	deb, _ := buildFixtureDeb(t)
	outDir := t.TempDir()
	require.NoError(t, os.Rename(deb, filepath.Join(outDir, "nke-kubeadm-distro-1.36.3_1_amd64.deb")))

	goldenDir := t.TempDir()
	writeGolden(t, goldenDir, "1.36.3", Manifest{Control: map[string][]string{"Package": {"not-fixture"}}})

	deviationsPath := filepath.Join(t.TempDir(), "deviations.txt")
	require.NoError(t, os.WriteFile(deviationsPath, []byte("field Package = not-fixture-but-wrong  # deliberately does not fix it\n"), 0o644))

	report, err := Check(outDir, goldenDir, deviationsPath, nil)
	require.NoError(t, err)
	assert.True(t, report.Strict)
	assert.True(t, report.Failed(), "strict mode must fail on a real diff")
}

func TestCheckStrictModePassesWithMatchingDeviation(t *testing.T) {
	deb, _ := buildFixtureDeb(t)

	// Build the golden from the fixture's own real manifest, with only
	// Package changed, so the deviation is the *only* difference.
	actual, err := Generate(deb)
	require.NoError(t, err)
	golden := cloneManifest(actual)
	golden.Control["Package"] = []string{"not-fixture"}

	outDir := t.TempDir()
	require.NoError(t, os.Rename(deb, filepath.Join(outDir, "nke-kubeadm-distro-1.36.3_1_amd64.deb")))
	goldenDir := t.TempDir()
	writeGolden(t, goldenDir, "1.36.3", golden)

	deviationsPath := filepath.Join(t.TempDir(), "deviations.txt")
	require.NoError(t, os.WriteFile(deviationsPath, []byte("field Package = fixture  # renamed\n"), 0o644))

	report, err := Check(outDir, goldenDir, deviationsPath, nil)
	require.NoError(t, err)
	assert.True(t, report.Strict)
	assert.False(t, report.Failed())
	assert.Empty(t, report.Diffs)
}

func TestCheckSkipsVersionsWithNoGolden(t *testing.T) {
	deb, _ := buildFixtureDeb(t)
	outDir := t.TempDir()
	require.NoError(t, os.Rename(deb, filepath.Join(outDir, "nke-kubeadm-distro-9.9.9_1_amd64.deb")))

	goldenDir := t.TempDir() // no goldens at all

	deviationsPath := filepath.Join(t.TempDir(), "deviations.txt")
	require.NoError(t, os.WriteFile(deviationsPath, []byte("# empty\n"), 0o644))

	report, err := Check(outDir, goldenDir, deviationsPath, nil)
	require.NoError(t, err)
	assert.Empty(t, report.Checked)
	assert.Contains(t, report.Skipped, "nke-kubeadm-distro-9.9.9_1_amd64.deb")
	assert.False(t, report.Failed())
}

func TestCheckIgnoresNonMatchingFiles(t *testing.T) {
	outDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "Packages"), []byte("not a deb"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "README.txt"), []byte("not a deb"), 0o644))

	goldenDir := t.TempDir()
	deviationsPath := filepath.Join(t.TempDir(), "deviations.txt")
	require.NoError(t, os.WriteFile(deviationsPath, []byte("# empty\n"), 0o644))

	report, err := Check(outDir, goldenDir, deviationsPath, nil)
	require.NoError(t, err)
	assert.Empty(t, report.Checked)
	assert.Empty(t, report.Skipped)
}

func TestHasDirective(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"only comments", "# a\n  # b  \n", false},
		{"blank lines", "\n\n   \n", false},
		{"one directive", "# header\nremoved /x  # reason\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasDirective(tc.text))
		})
	}
}
