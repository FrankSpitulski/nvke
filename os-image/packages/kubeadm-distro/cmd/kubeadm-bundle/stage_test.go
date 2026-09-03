// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSyntheticTarGz writes a gzip-compressed tar archive at path holding
// members (archive-internal name -> content), so stage_test.go needs no real
// upstream tarball to exercise stageComponents.
func writeSyntheticTarGz(t *testing.T, path string, members map[string]string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range members {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(content)),
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
}

func TestStageComponents(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "amd64")
	stageDir := filepath.Join(dir, "stage")

	manifest := manifestDoc{
		Version:           "v1.36.3",
		ContainerdVersion: "v2.3.0",
		Architectures: map[string]map[string]manifestComponent{
			targetArch: {
				"containerd":  {File: "containerd-2.3.0-linux-amd64.tar.gz"},
				"cni-plugins": {File: "cni-plugins-linux-amd64-v1.6.0.tgz"},
				"crictl":      {File: "crictl-v1.36.0-linux-amd64.tar.gz"},
				"helm":        {File: "helm-v4.1.4-linux-amd64.tar.gz"},
			},
		},
	}

	writeSyntheticTarGz(t, filepath.Join(archDir, "containerd-2.3.0-linux-amd64.tar.gz"), map[string]string{
		"bin/containerd":              "stub containerd",
		"bin/containerd-shim-runc-v2": "stub containerd-shim-runc-v2",
		"bin/ctr":                     "stub ctr",
		"bin/containerd-stress":       "stub containerd-stress", // not selected; must be ignored
	})
	writeSyntheticTarGz(t, filepath.Join(archDir, "cni-plugins-linux-amd64-v1.6.0.tgz"), map[string]string{
		"bridge":   "stub bridge",
		"loopback": "stub loopback",
	})
	writeSyntheticTarGz(t, filepath.Join(archDir, "crictl-v1.36.0-linux-amd64.tar.gz"), map[string]string{
		"crictl": "stub crictl",
	})
	writeSyntheticTarGz(t, filepath.Join(archDir, "helm-v4.1.4-linux-amd64.tar.gz"), map[string]string{
		"linux-amd64/helm":      "stub helm",
		"linux-amd64/LICENSE":   "stub license", // not selected; must be ignored
		"linux-amd64/README.md": "stub readme",
	})

	// A fixture lock image list, the shape make lock writes: several
	// control-plane images plus the pause (sandbox) image the rendered
	// containerd config must pin.
	fixtureImages := []lockImage{
		{Ref: "registry.k8s.io/coredns/coredns:v1.14.2", Digest: "sha256:aaaa"},
		{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:bbbb"},
		{Ref: "registry.k8s.io/kube-apiserver:v1.36.3", Digest: "sha256:cccc"},
	}

	require.NoError(t, stageComponents(archDir, stageDir, manifest, fixtureImages))

	for path, wantMode := range map[string]os.FileMode{
		filepath.Join(stageDir, "usr", "local", "bin", "containerd"):              0o755,
		filepath.Join(stageDir, "usr", "local", "bin", "containerd-shim-runc-v2"): 0o755,
		filepath.Join(stageDir, "usr", "local", "bin", "ctr"):                     0o755,
		filepath.Join(stageDir, "usr", "bin", "crictl"):                           0o755,
		filepath.Join(stageDir, "usr", "bin", "helm"):                             0o755,
		filepath.Join(stageDir, "opt", "cni", "bin", "bridge"):                    0o755,
		filepath.Join(stageDir, "opt", "cni", "bin", "loopback"):                  0o755,
	} {
		st, err := os.Stat(path)
		require.NoErrorf(t, err, "expected %s to exist", path)
		assert.Equal(t, wantMode, st.Mode().Perm(), path)
	}

	// containerd-stress, LICENSE and README.md were not asked for and must not
	// leak into the staged tree.
	for _, unwanted := range []string{
		filepath.Join(stageDir, "usr", "local", "bin", "containerd-stress"),
		filepath.Join(stageDir, "usr", "bin", "LICENSE"),
		filepath.Join(stageDir, "usr", "bin", "README.md"),
	} {
		_, err := os.Stat(unwanted)
		assert.Truef(t, os.IsNotExist(err), "did not expect %s to exist", unwanted)
	}

	config, err := os.ReadFile(filepath.Join(stageDir, "etc", "containerd", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "sandbox = 'registry.k8s.io/pause:3.10'")
	assert.Contains(t, string(config), "SystemdCgroup = true")
	assert.Contains(t, string(config), "BinaryName = '/usr/sbin/runc'")
	assert.Contains(t, string(config), "imports = ['/etc/containerd/conf.d/*.toml']")

	crictlYAML, err := os.ReadFile(filepath.Join(stageDir, "etc", "crictl.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(crictlYAML), "unix:///run/containerd/containerd.sock")
}

func TestStageComponentsNoPauseImage(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "amd64")
	stageDir := filepath.Join(dir, "stage")

	manifest := manifestDoc{
		ContainerdVersion: "v2.3.0",
		Architectures: map[string]map[string]manifestComponent{
			targetArch: {
				"containerd":  {File: "containerd.tar.gz"},
				"cni-plugins": {File: "cni.tgz"},
				"crictl":      {File: "crictl.tar.gz"},
				"helm":        {File: "helm.tar.gz"},
			},
		},
	}
	writeSyntheticTarGz(t, filepath.Join(archDir, "containerd.tar.gz"), map[string]string{
		"bin/containerd": "x", "bin/containerd-shim-runc-v2": "x", "bin/ctr": "x",
	})
	writeSyntheticTarGz(t, filepath.Join(archDir, "cni.tgz"), map[string]string{"bridge": "x"})
	writeSyntheticTarGz(t, filepath.Join(archDir, "crictl.tar.gz"), map[string]string{"crictl": "x"})
	writeSyntheticTarGz(t, filepath.Join(archDir, "helm.tar.gz"), map[string]string{"linux-amd64/helm": "x"})

	err := stageComponents(archDir, stageDir, manifest, []lockImage{
		{Ref: "registry.k8s.io/coredns/coredns:v1.14.2", Digest: "sha256:aaaa"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pause image")
}

func TestStageComponentsMissingArchiveMember(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "amd64")
	stageDir := filepath.Join(dir, "stage")

	manifest := manifestDoc{
		ContainerdVersion: "v2.3.0",
		Architectures: map[string]map[string]manifestComponent{
			targetArch: {
				"containerd":  {File: "containerd.tar.gz"},
				"cni-plugins": {File: "cni.tgz"},
				"crictl":      {File: "crictl.tar.gz"},
				"helm":        {File: "helm.tar.gz"},
			},
		},
	}
	// containerd tarball is missing ctr.
	writeSyntheticTarGz(t, filepath.Join(archDir, "containerd.tar.gz"), map[string]string{
		"bin/containerd": "x", "bin/containerd-shim-runc-v2": "x",
	})
	writeSyntheticTarGz(t, filepath.Join(archDir, "cni.tgz"), map[string]string{"bridge": "x"})
	writeSyntheticTarGz(t, filepath.Join(archDir, "crictl.tar.gz"), map[string]string{"crictl": "x"})
	writeSyntheticTarGz(t, filepath.Join(archDir, "helm.tar.gz"), map[string]string{"linux-amd64/helm": "x"})

	err := stageComponents(archDir, stageDir, manifest, []lockImage{{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:bbbb"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bin/ctr")
}

// TestStageComponentsRejectsContainerd1 guards the config-schema mismatch: a
// containerd 1.x pin (still bundled for Kubernetes 1.29-1.31 in pins.go)
// reads a `version = 2` config, not the `version = 3` renderContainerdConfig
// always writes. stageComponents must refuse before it stages anything for
// such a pin, rather than ship a config.toml containerd cannot parse.
func TestStageComponentsRejectsContainerd1(t *testing.T) {
	dir := t.TempDir()
	archDir := filepath.Join(dir, "amd64")
	stageDir := filepath.Join(dir, "stage")

	manifest := manifestDoc{
		ContainerdVersion: "v1.7.31",
		Architectures: map[string]map[string]manifestComponent{
			targetArch: {
				"containerd":  {File: "containerd.tar.gz"},
				"cni-plugins": {File: "cni.tgz"},
				"crictl":      {File: "crictl.tar.gz"},
				"helm":        {File: "helm.tar.gz"},
			},
		},
	}
	// No archives written at all: requireContainerdV2 must fail before
	// stageComponents tries to read any of them.

	err := stageComponents(archDir, stageDir, manifest, []lockImage{{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:bbbb"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "below v2.0.0")
	assert.Contains(t, err.Error(), "config version 3 needs containerd 2.x")
	assert.NoDirExists(t, stageDir)
}

func TestRequireContainerdV2(t *testing.T) {
	require.NoError(t, requireContainerdV2("v2.3.0"))
	require.NoError(t, requireContainerdV2("2.0.0"))
	require.Error(t, requireContainerdV2("v1.7.31"))
	require.Error(t, requireContainerdV2("v1.6.36"))
	require.Error(t, requireContainerdV2(""))
	require.Error(t, requireContainerdV2("not-a-version"))
}
