// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/md5" //nolint:gosec // dpkg's own conffile-integrity mechanism, not a security boundary.
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	// The four matrix binaries download.go chmods 0755 in the real bundle;
	// stubbed the same way here so a regression in that chmod shows up as a
	// mode mismatch in TestBuildDeb's dpkg-deb -c listing instead of passing
	// silently.
	executables := map[string]bool{"kubeadm": true, "kubectl": true, "kubelet": true, "runc.amd64": true}
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
		mode := os.FileMode(0o644)
		if executables[name] {
			mode = 0o755
		}
		require.NoError(t, os.WriteFile(path, []byte("stub "+name+"\n"), mode))
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

// nfpmBin resolves the nfpm binary build-deb.sh must run against: NFPM_BIN
// (set by `make test`, which downloads the pinned version), or nfpm on PATH.
// Unlike dpkg-deb, which is only used here to inspect the result, nfpm builds
// it — so its absence fails the test outright rather than skipping it.
func nfpmBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("NFPM_BIN"); bin != "" {
		return bin
	}
	if bin, err := exec.LookPath("nfpm"); err == nil {
		return bin
	}
	t.Fatal("nfpm not found: set NFPM_BIN, or put nfpm on PATH. Run 'make test' from the package root, which builds the pinned nfpm first.")
	return ""
}

func TestBuildDeb(t *testing.T) {
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	const version = "v1.36.3"
	src, installer := stubBundle(t, version, true)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS=v1.36.3 v1.35.3 v1.34.2", "DEB_REVISION=1", "NFPM_BIN="+nfpm)
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
	for _, want := range []string{
		"kubeadm", "runc", "containerd", "containerd.io",
		"nke-kubeadm-distro", "nke-kubeadm-distro-1.35.3", "nke-kubeadm-distro-1.34.2",
	} {
		assert.Contains(t, string(conflicts), want)
	}
	assert.NotContains(t, string(conflicts), "nke-kubeadm-distro-1.36.3", "a package must not conflict with itself")

	// /etc/default/kubelet must be a conffile so operator edits survive an upgrade.
	ctrlDir := filepath.Join(t.TempDir(), "control")
	require.NoError(t, exec.Command("dpkg-deb", "-e", deb, ctrlDir).Run())
	conffiles, err := os.ReadFile(filepath.Join(ctrlDir, "conffiles"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/default/kubelet\n", string(conffiles))

	// DEBIAN/md5sums is what dpkg uses to tell a locally modified conffile from
	// an untouched one; a missing or stale entry defeats that on every payload
	// file, not just conffiles.
	md5sums, err := os.ReadFile(filepath.Join(ctrlDir, "md5sums"))
	require.NoError(t, err)
	require.NotEmpty(t, md5sums, "md5sums must not be empty")
	payloadDir := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, exec.Command("dpkg-deb", "-x", deb, payloadDir).Run())
	for _, line := range strings.Split(strings.TrimSpace(string(md5sums)), "\n") {
		md5sumsFields := strings.SplitN(line, "  ", 2)
		require.Lenf(t, md5sumsFields, 2, "md5sums line: %q", line)
		wantSum, relPath := md5sumsFields[0], md5sumsFields[1]
		data, err := os.ReadFile(filepath.Join(payloadDir, relPath))
		require.NoErrorf(t, err, "payload file for md5sums entry %q", relPath)
		gotSum := fmt.Sprintf("%x", md5.Sum(data)) //nolint:gosec // see the import comment.
		assert.Equal(t, wantSum, gotSum, relPath)
	}

	listing, err := exec.Command("dpkg-deb", "-c", deb).Output()
	require.NoError(t, err)
	contents := string(listing)
	// kubeadm's DirAvailable preflight check counts a hidden file as content, so
	// these directories have to ship genuinely empty.
	assert.NotContains(t, contents, ".kubelet-keep")
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

	// /usr/bin/* and /usr/sbin/runc get 0755 from nfpm.yaml's own file_info,
	// which would mask a regression in download.go's chmod. The copies under
	// k8s-matrix/<version>/amd64/, pulled in verbatim by the `type: tree`
	// entry, have no file_info override -- they only end up 0755 because
	// stubBundle wrote them that way, mimicking what download.go does to the
	// real bundle -- so checking those actually exercises that chmod.
	for _, want := range []string{
		"/usr/bin/kubeadm",
		"/usr/bin/kubectl",
		"/usr/bin/kubelet",
		"/usr/sbin/runc",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubeadm",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubectl",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubelet",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/runc.amd64",
	} {
		line := findListingLine(t, contents, want)
		assert.Truef(t, strings.HasPrefix(line, "-rwxr-xr-x"), "expected 0755 for %s, got %q", want, line)
	}
}

// findListingLine returns the single `dpkg-deb -c` line for path (dpkg-deb
// lists paths as "./usr/bin/kubeadm", not absolute), or fails the test.
func findListingLine(t *testing.T, listing, path string) string {
	t.Helper()
	want := "." + path
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == want {
			return line
		}
	}
	t.Fatalf("no dpkg-deb -c entry for %s", path)
	return ""
}

// chtimesAll sets every regular file under root to the given time. Used to
// prove the reproducibility fix actually normalises mtimes, rather than the
// two builds trivially agreeing because they read identical, untouched files.
func chtimesAll(t *testing.T, root string, when time.Time) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			return os.Chtimes(path, when, when)
		}
		return nil
	}))
}

// TestBuildDebReproducible guards nfpm.yaml's fixed mtime: building the same
// input twice, with every source file at a different real mtime between the
// two builds, must still produce byte-identical .debs. Without the fix in
// build-deb.sh (touching the arch directory copy before nfpm reads it), the
// `type: tree` content entry would leak each source file's own mtime into the
// package, and this test would fail.
func TestBuildDebReproducible(t *testing.T) {
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	const version = "v1.36.3"
	src, installer := stubBundle(t, version, true)

	build := func(outName string, srcTime time.Time) []byte {
		chtimesAll(t, src, srcTime)
		out := filepath.Join(t.TempDir(), outName)
		cmd := exec.Command(buildDebScript, version, src, installer, out)
		cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
		combined, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "build-deb.sh failed: %s", combined)
		data, err := os.ReadFile(filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
		require.NoError(t, err)
		return data
	}

	first := build("out1", time.Date(2020, 1, 1, 1, 1, 0, 0, time.UTC))
	second := build("out2", time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC))
	assert.Equal(t, first, second, "building the same input twice must yield a byte-identical .deb")
}

// TestBuildDebRefusesBundleWithoutImages guards the offline contract: a package
// with no container images cannot bring a node up without a registry, so it is
// only buildable when someone asks for it explicitly.
func TestBuildDebRefusesBundleWithoutImages(t *testing.T) {
	nfpm := nfpmBin(t)

	const version = "v1.36.3"
	src, installer := stubBundle(t, version, false)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no container images")
	assert.Contains(t, string(combined), "could not bring up a node offline")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))

	// ALLOW_NO_IMAGES=1 is the deliberate development escape hatch.
	cmd = exec.Command(buildDebScript, version, src, installer, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "ALLOW_NO_IMAGES=1", "NFPM_BIN="+nfpm)
	combined, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "build-deb.sh with ALLOW_NO_IMAGES=1 failed: %s", combined)
	assert.Contains(t, string(combined), "DEVELOPMENT ONLY")
	assert.FileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}
