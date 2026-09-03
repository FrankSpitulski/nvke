// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/md5" //nolint:gosec // dpkg's own conffile-integrity mechanism, not a security boundary.
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// fixturePauseImage is the sandbox image the stubbed containerd config.toml
// carries, standing in for what stageComponents renders from the locked
// image list. TestBuildDeb asserts the packaged config.toml still names it.
const fixturePauseImage = "registry.k8s.io/pause:3.10"

// TestBuildDeb runs packaging/build-deb.sh over a stubbed bundle tree — tiny
// files standing in for the real binaries — and inspects the resulting package.
// Nothing is downloaded.
// stubBundle writes a bundle tree of tiny files standing in for the real
// artifacts and the staged FHS runtime tree, and returns the bundle
// directory. withImages controls whether the container image tarball is
// present.
func stubBundle(t *testing.T, version string, withImages bool) (src string) {
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
	stubs := []string{"kubeadm", "kubectl", "kubelet", "runc.amd64"}
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

	if withImages {
		writeStage(t, src)
	}
	return src
}

// writeStage writes the staged FHS runtime tree stageComponents would produce
// from real containerd/CNI/crictl/helm tarballs: the extracted binaries, the
// CNI plugin directory, and the rendered containerd and crictl configs.
func writeStage(t *testing.T, src string) {
	t.Helper()
	stage := filepath.Join(src, "stage")

	binFiles := map[string]string{
		filepath.Join(stage, "usr", "local", "bin", "containerd"):              "stub containerd\n",
		filepath.Join(stage, "usr", "local", "bin", "containerd-shim-runc-v2"): "stub containerd-shim-runc-v2\n",
		filepath.Join(stage, "usr", "local", "bin", "ctr"):                     "stub ctr\n",
		filepath.Join(stage, "usr", "bin", "crictl"):                           "stub crictl\n",
		filepath.Join(stage, "usr", "bin", "helm"):                             "stub helm\n",
		filepath.Join(stage, "opt", "cni", "bin", "bridge"):                    "stub bridge\n",
		filepath.Join(stage, "opt", "cni", "bin", "loopback"):                  "stub loopback\n",
	}
	for path, content := range binFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	}

	config := "version = 3\n\n[plugins]\n  [plugins.'io.containerd.cri.v1.images']\n" +
		"    [plugins.'io.containerd.cri.v1.images'.pinned_images]\n" +
		"      sandbox = '" + fixturePauseImage + "'\n"
	configDir := filepath.Join(stage, "etc", "containerd")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(config), 0o644))

	crictlYAML := "runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\ntimeout: 10\n"
	require.NoError(t, os.WriteFile(filepath.Join(stage, "etc", "crictl.yaml"), []byte(crictlYAML), 0o644))
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
	src := stubBundle(t, version, true)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, out)
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

	// dpkg owns every file in this package: the only maintainer scripts are
	// postinst, prerm and postrm.
	scripts, err := os.ReadDir(ctrlDir)
	require.NoError(t, err)
	var scriptNames []string
	for _, e := range scripts {
		switch e.Name() {
		case "control", "conffiles", "md5sums":
			continue
		default:
			scriptNames = append(scriptNames, e.Name())
		}
	}
	sort.Strings(scriptNames)
	assert.Equal(t, []string{"postinst", "postrm", "prerm"}, scriptNames)

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

	// The rendered containerd config must still carry the pause image the
	// (fixture) locked image list named, so the sandbox image can never skew
	// from the bundled control-plane images.
	config, err := os.ReadFile(filepath.Join(payloadDir, "etc", "containerd", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), fixturePauseImage)

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
		"/usr/bin/crictl",
		"/usr/bin/helm",
		"/usr/sbin/runc",
		"/usr/local/bin/containerd",
		"/usr/local/bin/containerd-shim-runc-v2",
		"/usr/local/bin/ctr",
		"/opt/cni/bin/bridge",
		"/opt/cni/bin/loopback",
		"/etc/containerd/config.toml",
		"/etc/crictl.yaml",
		"/lib/systemd/system/containerd.service",
		"/lib/systemd/system/kubelet.service",
		"/lib/systemd/system/kubelet.service.d/10-kubeadm.conf",
		"/etc/default/kubelet",
		"/etc/kubernetes/manifests/",
		"/var/lib/kubelet/",
		"/etc/cni/net.d/",
		"/etc/containerd/conf.d/",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json.sha256",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/" + baseImagesTarballName(version),
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/" + baseImagesTarballName(version) + ".sha256",
		"/usr/share/doc/nke-kubeadm-distro-1.36.3/README.Debian",
	} {
		assert.Contains(t, contents, want)
	}

	// dpkg owns everything now: the node installer, its symlink and sidecar,
	// and every duplicate binary/tarball that used to be copied under
	// k8s-matrix/<version>/amd64/ are gone.
	for _, notWant := range []string{
		"nke-k8s-installer",
		"/usr/lib/nke/k8s-matrix/v1.36.3/installer",
		"/usr/lib/nke/k8s-matrix/v1.36.3/manifest.json.sha256.sha256",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubeadm",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubectl",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/kubelet",
		"/usr/lib/nke/k8s-matrix/v1.36.3/amd64/runc.amd64",
	} {
		assert.NotContains(t, contents, notWant)
	}

	// /usr/bin/*, /usr/sbin/runc and the staged runtime binaries get 0755 from
	// nfpm.yaml's own file_info, which would mask a regression in download.go's
	// chmod or in stageComponents' extraction mode.
	for _, want := range []string{
		"/usr/bin/kubeadm",
		"/usr/bin/kubectl",
		"/usr/bin/kubelet",
		"/usr/bin/crictl",
		"/usr/bin/helm",
		"/usr/sbin/runc",
		"/usr/local/bin/containerd",
		"/usr/local/bin/containerd-shim-runc-v2",
		"/usr/local/bin/ctr",
		"/opt/cni/bin/bridge",
		"/opt/cni/bin/loopback",
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

// TestBuildDebReproducible guards nfpm.yaml's fixed mtime and build-deb.sh's
// pinning of the one `type: tree` entry (/opt/cni/bin): building the same
// input twice, with every source file at a different real mtime between the
// two builds, must still produce byte-identical .debs.
func TestBuildDebReproducible(t *testing.T) {
	nfpm := nfpmBin(t)
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	const version = "v1.36.3"
	src := stubBundle(t, version, true)

	build := func(outName string, srcTime time.Time) []byte {
		chtimesAll(t, src, srcTime)
		out := filepath.Join(t.TempDir(), outName)
		cmd := exec.Command(buildDebScript, version, src, out)
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

// TestBuildDebRefusesBundleWithoutImages guards the offline contract: a
// package with no container images cannot bring a node up without a
// registry, so a bundle missing them is refused outright. There is no
// development escape hatch anymore -- a bundle with images and a staged
// runtime tree is the only thing this build produces.
func TestBuildDebRefusesBundleWithoutImages(t *testing.T) {
	nfpm := nfpmBin(t)

	const version = "v1.36.3"
	src := stubBundle(t, version, false)
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no container images")
	assert.Contains(t, string(combined), "could not bring up a node offline")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}

// TestBuildDebRefusesBundleWithoutStage guards the same offline contract for
// the staged runtime tree: a bundle whose `stage` step never ran is not
// buildable either.
func TestBuildDebRefusesBundleWithoutStage(t *testing.T) {
	nfpm := nfpmBin(t)

	const version = "v1.36.3"
	src := stubBundle(t, version, true)
	require.NoError(t, os.RemoveAll(filepath.Join(src, "stage")))
	out := filepath.Join(t.TempDir(), "out")

	cmd := exec.Command(buildDebScript, version, src, out)
	cmd.Env = append(os.Environ(), "PEER_VERSIONS="+version, "DEB_REVISION=1", "NFPM_BIN="+nfpm)
	combined, err := cmd.CombinedOutput()
	require.Error(t, err, "build-deb.sh must refuse a bundle with no staged runtime tree")
	assert.Contains(t, string(combined), "missing staged file")
	assert.NoFileExists(t, filepath.Join(out, "nke-kubeadm-distro-1.36.3_1_amd64.deb"))
}

// writeCommandPattern flags a line as a candidate write: it contains a `>` or
// `>>` redirection, or one of the shell commands that can create, overwrite
// or remove a file (`tee`, `install`, `cp`, `mv`, `rm`, `touch`, `mkdir`,
// `ln`), or `sed` combined with `-i`. Word-boundary matched so it does not
// fire on substrings (a hostname containing "cp", "rmdir" is not "rm", ...).
// `sed` alone is not a write -- only `sed -i` rewrites a file in place.
var writeCommandPattern = regexp.MustCompile(`>>?|\b(?:tee|install|cp|mv|rm|touch|mkdir|ln)\b|\bsed\b.*-i\b`)

// absolutePathTokenPattern extracts every absolute-path-looking token from a
// line: a run of path characters starting with "/", optionally wrapped in
// single or double quotes. Applied to every write-command line, not just the
// text immediately after the triggering command, so it catches every path
// argument on the line (for example both paths in
// `rm -f /etc/sysctl.d/k8s.conf /etc/evil2.conf`), not only the first.
var absolutePathTokenPattern = regexp.MustCompile(`["']?(/[A-Za-z0-9_./+-]+)["']?`)

// maintainerScriptWriteAllowlist is every absolute path a maintainer script
// may write to (create, overwrite or remove) outside of what dpkg's own file
// list already owns. Nothing else may appear here without a reviewer
// noticing: postinst and postrm only exist for the node-level state a file
// list cannot express. The postinst log path is never a literal here (it is
// always written through "$LOG"), so it needs no entry.
var maintainerScriptWriteAllowlist = map[string]bool{
	"/etc/fstab":                          true,
	"/etc/modules-load.d/k8s.conf":        true,
	"/etc/sysctl.d/k8s.conf":              true,
	"/etc/systemd/system/kubelet.service": true, // removed, not written
	"/dev/null":                           true, // discarding output, not a filesystem write
}

// findUnallowlistedWrites returns, for each write-command line in data, the
// absolute-path tokens that are not in maintainerScriptWriteAllowlist. Shared
// by the real scripts test and its self-test below, so both exercise the
// identical matching logic.
//
// Paths are only extracted from the text following each write-trigger match
// to the next one (or end of line), not the whole line: an echo message can
// legitimately mention a path in prose before a `| tee "$LOG"` (see postinst's
// "vendor unit in /lib applies." line), and that prose is not itself a write
// target.
func findUnallowlistedWrites(data string) []string {
	var violations []string
	for _, line := range strings.Split(data, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		triggers := writeCommandPattern.FindAllStringIndex(line, -1)
		for i, trig := range triggers {
			segEnd := len(line)
			if i+1 < len(triggers) {
				segEnd = triggers[i+1][0]
			}
			segment := line[trig[1]:segEnd]
			for _, m := range absolutePathTokenPattern.FindAllStringSubmatch(segment, -1) {
				path := m[1]
				if maintainerScriptWriteAllowlist[path] {
					continue
				}
				violations = append(violations, path)
			}
		}
	}
	return violations
}

// TestMaintainerScriptsWriteOnlyAllowlistedPaths greps postinst, prerm and
// postrm for every absolute path a write command touches and asserts each is
// on the allowlist above, so a future edit cannot silently start writing
// somewhere dpkg does not know about.
func TestMaintainerScriptsWriteOnlyAllowlistedPaths(t *testing.T) {
	for _, script := range []string{"postinst", "prerm", "postrm"} {
		script := script
		t.Run(script, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "packaging", script))
			require.NoError(t, err)
			for _, path := range findUnallowlistedWrites(string(data)) {
				t.Errorf("%s writes to %q, which is not on the allowlist", script, path)
			}
		})
	}
}

// TestFindUnallowlistedWritesCatchesInjectedLines is a self-test for the
// matcher above: four lines a reviewer found the previous version of this
// test missed, each writing (or removing) an unallowlisted path through a
// different command shape. If any of these stops being flagged, the matcher
// has rotted and TestMaintainerScriptsWriteOnlyAllowlistedPaths can no longer
// be trusted to catch a real regression.
func TestFindUnallowlistedWritesCatchesInjectedLines(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"tee", `echo bad | tee /etc/evil.conf`, "/etc/evil.conf"},
		{"sed -i", `sed -i "s/a/b/" /etc/shadow`, "/etc/shadow"},
		{"rm with two paths", `rm -f /etc/sysctl.d/k8s.conf /etc/evil2.conf`, "/etc/evil2.conf"},
		{"install", `install -m 0755 x /usr/local/bin/evil`, "/usr/local/bin/evil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findUnallowlistedWrites(tt.line)
			assert.Contains(t, got, tt.want, "matcher must flag %q on line %q", tt.want, tt.line)
		})
	}
}
