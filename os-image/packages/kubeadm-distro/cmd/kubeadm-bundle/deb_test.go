// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// TestBuildDeb runs packaging/build-deb.sh over a stubbed bundle tree — tiny
// files standing in for the real binaries — and inspects the resulting package.
// Nothing is downloaded.
// stubBundle writes a bundle tree of tiny files standing in for the real
// artifacts, and returns the bundle directory and the stub installer path.
// withImages controls whether the container image tarball is present.
func stubBundle(t *testing.T, version string, withImages bool) (src, installer string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "dist", version)
	archDir := filepath.Join(src, targetArch)
	require.NoError(t, os.MkdirAll(archDir, 0o755))

	stubs := []string{
		"kubeadm", "kubectl", "kubelet", "runc.amd64",
		"containerd-2.3.0-linux-amd64.tar.gz",
		"cni-plugins-linux-amd64-v1.6.0.tgz",
		"crictl-v1.36.0-linux-amd64.tar.gz",
		"helm-v4.1.4-linux-amd64.tar.gz",
	}
	if withImages {
		stubs = append(stubs, baseImagesTarballName(version))
	}
	for _, name := range stubs {
		path := filepath.Join(archDir, name)
		require.NoError(t, os.WriteFile(path, []byte("stub "+name+"\n"), 0o644))
		_, err := bundle.WriteSHA256Sidecar(path)
		require.NoError(t, err)
	}

	require.NoError(t, writeManifest(filepath.Join(src, "manifest.json"), manifestDoc{
		Version:       version,
		Architectures: map[string]map[string]manifestComponent{targetArch: {"kubeadm": {File: "kubeadm"}}},
	}))

	installer = filepath.Join(dir, "nke-k8s-installer")
	require.NoError(t, os.WriteFile(installer, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return src, installer
}

// buildDebScript is the packaging script under test, relative to this package.
var buildDebScript = filepath.Join("..", "..", "packaging", "build-deb.sh")

func TestBuildDeb(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	const version = "v1.36.3"
	src, installer := stubBundle(t, version, true)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS=v1.36.3 v1.35.3 v1.34.2", "DEB_REVISION=1")
	combined, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "build-deb.sh failed: %s", combined)

	deb := filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb")
	require.FileExists(t, deb)

	fields := map[string]string{
		"Package":      "nke-kubeadm-distro-1.36.3",
		"Version":      "1",
		"Architecture": "amd64",
		"Provides":     "nke-kubeadm-distro",
		"Depends":      "iptables, iproute2, conntrack, socat, ethtool",
	}
	for field, want := range fields {
		got, err := exec.Command("dpkg-deb", "-f", deb, field).Output()
		require.NoError(t, err)
		assert.Equal(t, want, strings.TrimSpace(string(got)), field)
	}

	size, err := exec.Command("dpkg-deb", "-f", deb, "Installed-Size").Output()
	require.NoError(t, err)
	installedSize, err := strconv.Atoi(strings.TrimSpace(string(size)))
	require.NoError(t, err, "Installed-Size must be an integer number of KiB")
	assert.Positive(t, installedSize, "Installed-Size must cover the staged payload")

	conflicts, err := exec.Command("dpkg-deb", "-f", deb, "Conflicts").Output()
	require.NoError(t, err)
	for _, want := range []string{"kubeadm", "runc", "nke-kubeadm-distro", "nke-kubeadm-distro-1.35.3", "nke-kubeadm-distro-1.34.2"} {
		assert.Contains(t, string(conflicts), want)
	}
	assert.NotContains(t, string(conflicts), "nke-kubeadm-distro-1.36.3", "a package must not conflict with itself")

	// /etc/default/kubelet must be a conffile so operator edits survive an upgrade.
	ctrlDir := filepath.Join(t.TempDir(), "control")
	require.NoError(t, exec.Command("dpkg-deb", "-e", deb, ctrlDir).Run())
	conffiles, err := os.ReadFile(filepath.Join(ctrlDir, "conffiles"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/default/kubelet\n", string(conffiles))

	listing, err := exec.Command("dpkg-deb", "-c", deb).Output()
	require.NoError(t, err)
	contents := string(listing)
	// kubeadm's DirAvailable preflight check counts a hidden file as content, so
	// these directories have to ship genuinely empty.
	assert.NotContains(t, contents, ".kubelet-keep")
	// The package root comes from the staging directory's own mode; mktemp -d
	// makes that 0700, which would install an unreadable /.
	assert.Regexp(t, `drwxr-xr-x[^\n]*\./\n`, contents, "the package root must be world-readable")
	for _, want := range []string{
		"/usr/bin/kubeadm",
		"/usr/bin/kubectl",
		"/usr/bin/kubelet",
		"/usr/sbin/runc",
		"/usr/bin/nke-k8s-installer -> ../lib/nke/k8s-matrix/v1.36.3/installer",
		"/lib/systemd/system/kubelet.service",
		"/lib/systemd/system/kubelet.service.d/10-kubeadm.conf",
		"/etc/default/kubelet",
		"/etc/kubernetes/manifests/",
		"/var/lib/kubelet/",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json.sha256",
		"/usr/lib/nke/k8s-matrix/v1.36.3/installer",
		"/usr/lib/nke/k8s-matrix/v1.36.3/installer.sha256",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/" + baseImagesTarballName(version),
		"/usr/share/doc/nke-kubeadm-distro-1.36.3/README.Debian",
	} {
		assert.Contains(t, contents, want)
	}
}

// TestBuildDebRefusesBundleWithoutImages guards the offline contract: a package
// with no container images cannot bring a node up without a registry, so it is
// only buildable when someone asks for it explicitly.
func TestBuildDebRefusesBundleWithoutImages(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	const version = "v1.36.3"
	src, installer := stubBundle(t, version, false)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1")
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no container images")
	assert.Contains(t, string(combined), "could not bring up a node offline")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))

	// ALLOW_NO_IMAGES=1 is the deliberate development escape hatch.
	cmd = exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "ALLOW_NO_IMAGES=1")
	combined, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "build-deb.sh with ALLOW_NO_IMAGES=1 failed: %s", combined)
	assert.Contains(t, string(combined), "DEVELOPMENT ONLY")
	assert.FileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}
