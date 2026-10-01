// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMetadataReadsActiveAndDeprecatedProfiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "osProfiles")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	metadata := `profiles:
  nke-1.31.12-251119:
    kubernetes_version: "1.31.12"
    architecture: amd64
    build_date: "2025-11-19"
  nke-1.31.9-251124-24.04-arm64:
    kubernetes_version: "1.31.9"
    architecture: arm64
    build_date: "2025-11-24"
    ubuntu_version: "24.04"
`
	deprecated := `profiles:
  nke-deprecated-profile:
    kubernetes_version: "1.31.9"
    architecture: amd64
    build_date: "2025-11-19"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.yaml"), []byte(metadata), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deprecated.yaml"), []byte(deprecated), 0o644))

	index, err := loadMetadata(root)

	require.NoError(t, err)
	assert.Len(t, index.Profiles, 2)
	assert.Equal(t, "1.31.12", index.Profiles["nke-1.31.12-251119"].KubernetesVersion)
	assert.Equal(t, "24.04", index.Profiles["nke-1.31.9-251124-24.04-arm64"].UbuntuVersion)
	assert.Len(t, index.DeprecatedProfiles, 1)
	assert.Contains(t, index.DeprecatedProfiles, "nke-deprecated-profile")
	assert.NotContains(t, index.Profiles, "nke-deprecated-profile")
}

func TestLoadMetadataWithoutDeprecatedFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "osProfiles")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.yaml"), []byte("profiles: {}\n"), 0o644))

	index, err := loadMetadata(root)

	require.NoError(t, err)
	assert.Empty(t, index.DeprecatedProfiles)
}

func TestLoadMetadataMissingFile(t *testing.T) {
	_, err := loadMetadata(t.TempDir())

	require.ErrorContains(t, err, "failed to read metadata file")
}
