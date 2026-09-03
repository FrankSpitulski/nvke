// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFixtureDeb stages a tiny package tree exercising every FileEntry
// shape (directory, regular file outside a bin dir, regular file under
// /usr/bin, symlink, hardlink, setuid regular file, conffile, two
// maintainer scripts) and builds it with dpkg-deb, the same tool Generate
// shells out to. setuidSupported is false when the setuid bit does not
// survive being chmod'd onto a file under t.TempDir() — some tmpfs-backed
// temp directories (observed under macOS's default $TMPDIR) silently drop
// it — so TestGenerate can skip the one assertion that needs it rather than
// fail on an environment quirk unrelated to the code under test.
func buildFixtureDeb(t *testing.T) (deb string, setuidSupported bool) {
	t.Helper()
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}

	stage := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "DEBIAN"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "usr", "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "usr", "share", "doc", "fixture"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(stage, "etc", "default"), 0o755))

	control := "Package: fixture\n" +
		"Version: 1\n" +
		"Section: admin\n" +
		"Priority: optional\n" +
		"Architecture: amd64\n" +
		"Provides: fixture-provider\n" +
		"Depends: iptables, iproute2\n" +
		"Conflicts: old-fixture, older-fixture\n" +
		"Replaces: old-fixture\n" +
		"Maintainer: NKE <nke-dev@exchange.nvidia.com>\n" +
		"Description: fixture package for debmanifest tests\n" +
		" not part of the contract.\n"
	require.NoError(t, os.WriteFile(filepath.Join(stage, "DEBIAN", "control"), []byte(control), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "DEBIAN", "conffiles"), []byte("/etc/default/kubelet\n"), 0o644))
	for _, script := range []string{"postinst", "postrm"} {
		require.NoError(t, os.WriteFile(filepath.Join(stage, "DEBIAN", script), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	}

	require.NoError(t, os.WriteFile(filepath.Join(stage, "etc", "default", "kubelet"), []byte("KUBELET_EXTRA_ARGS=\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "usr", "bin", "kubeadm"), []byte("#!/bin/sh\necho fixture\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "usr", "share", "doc", "fixture", "README.Debian"), []byte("fixture\n"), 0o644))
	require.NoError(t, os.Symlink("../lib/nke/k8s-matrix/fixture/installer", filepath.Join(stage, "usr", "bin", "nke-k8s-installer")))

	// A hardlink: dpkg-deb's underlying tar emits a "1" (TypeLink) entry for
	// the second name once two staged paths share an inode.
	require.NoError(t, os.Link(
		filepath.Join(stage, "usr", "share", "doc", "fixture", "README.Debian"),
		filepath.Join(stage, "usr", "share", "doc", "fixture", "README.Debian.hardlink"),
	))

	// A setuid regular file, to exercise permString's "s" rendering.
	setuidPath := filepath.Join(stage, "usr", "bin", "fixture-setuid")
	require.NoError(t, os.WriteFile(setuidPath, []byte("#!/bin/sh\necho fixture\n"), 0o755))
	require.NoError(t, os.Chmod(setuidPath, 0o4755))
	fi, err := os.Stat(setuidPath)
	require.NoError(t, err)
	setuidSupported = fi.Mode()&os.ModeSetuid != 0

	deb = filepath.Join(t.TempDir(), "fixture_1_amd64.deb")
	cmd := exec.Command("dpkg-deb", "--root-owner-group", "--build", stage, deb)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "dpkg-deb --build: %s", out)
	return deb, setuidSupported
}

func TestGenerate(t *testing.T) {
	deb, setuidSupported := buildFixtureDeb(t)

	m, err := Generate(deb)
	require.NoError(t, err)

	require.Equal(t, []string{"fixture"}, m.Control["Package"])
	require.NotContains(t, m.Control, "Version", "Version is excluded from the contract; it's the packaging revision")
	require.Equal(t, []string{"amd64"}, m.Control["Architecture"])
	require.Equal(t, []string{"fixture-provider"}, m.Control["Provides"])
	// Depends/Conflicts/Replaces must come back sorted, not in control-file order.
	require.Equal(t, []string{"iproute2", "iptables"}, m.Control["Depends"])
	require.Equal(t, []string{"old-fixture", "older-fixture"}, m.Control["Conflicts"])
	require.Equal(t, []string{"old-fixture"}, m.Control["Replaces"])
	require.NotContains(t, m.Control, "Maintainer")
	require.NotContains(t, m.Control, "Description")

	require.Equal(t, []string{"/etc/default/kubelet"}, m.Conffiles)
	require.Equal(t, []string{"postinst", "postrm"}, m.Scripts)

	byPath := map[string]FileEntry{}
	for _, f := range m.Files {
		byPath[f.Path] = f
	}

	dir, ok := byPath["/etc/"]
	require.True(t, ok, "directory entries must be present")
	require.Equal(t, "d", dir.Type)
	require.Equal(t, "rwxr-xr-x", dir.Mode)

	nonBin, ok := byPath["/etc/default/kubelet"]
	require.True(t, ok)
	require.Equal(t, "f", nonBin.Type)
	require.Empty(t, nonBin.SHA256, "files outside a bin dir must not carry a sha256")

	bin, ok := byPath["/usr/bin/kubeadm"]
	require.True(t, ok)
	require.Equal(t, "f", bin.Type)
	require.Len(t, bin.SHA256, 64, "files under /usr/bin must carry a sha256")

	link, ok := byPath["/usr/bin/nke-k8s-installer"]
	require.True(t, ok)
	require.Equal(t, "l", link.Type)
	require.Equal(t, "../lib/nke/k8s-matrix/fixture/installer", link.LinkTarget)
	require.Empty(t, link.SHA256, "symlinks never carry a sha256, even under a bin dir")

	hardlink, ok := byPath["/usr/share/doc/fixture/README.Debian.hardlink"]
	require.True(t, ok, "a hard-linked second name must produce its own \"h\" row, not an error")
	require.Equal(t, "h", hardlink.Type)
	require.Equal(t, "/usr/share/doc/fixture/README.Debian", hardlink.LinkTarget)
	require.Empty(t, hardlink.SHA256, "hardlinks never carry a sha256, even under a bin dir")

	setuid, ok := byPath["/usr/bin/fixture-setuid"]
	require.True(t, ok)
	require.Equal(t, "f", setuid.Type)
	require.Len(t, setuid.SHA256, 64)
	if setuidSupported {
		require.Equal(t, "rwsr-xr-x", setuid.Mode, "setuid renders as 's' in the owner-execute position")
	} else {
		t.Log("this filesystem does not preserve setuid on chmod; skipping the setuid rendering assertion (see TestPermStringSetuid for a filesystem-independent check)")
	}
}

// TestPermStringSetuid exercises the setuid/setgid/sticky rendering
// directly, independent of whether the test filesystem preserves a setuid
// bit set via chmod (TestGenerate's fixture can't rely on that).
func TestPermStringSetuid(t *testing.T) {
	cases := []struct {
		name string
		mode fs.FileMode
		want string
	}{
		{"setuid with owner execute", fs.ModeSetuid | 0o755, "rwsr-xr-x"},
		{"setuid without owner execute", fs.ModeSetuid | 0o655, "rwSr-xr-x"},
		{"setgid with group execute", fs.ModeSetgid | 0o755, "rwxr-sr-x"},
		{"setgid without group execute", fs.ModeSetgid | 0o745, "rwxr-Sr-x"},
		{"sticky with other execute", fs.ModeSticky | 0o755, "rwxr-xr-t"},
		{"sticky without other execute", fs.ModeSticky | 0o754, "rwxr-xr-T"},
		{"plain file", 0o644, "rw-r--r--"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, permString(tc.mode))
		})
	}
}

func TestGenerateMissingFile(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is not on PATH")
	}
	_, err := Generate(filepath.Join(t.TempDir(), "does-not-exist.deb"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "No such file or directory", "the wrapped error should include dpkg-deb's stderr")
}
