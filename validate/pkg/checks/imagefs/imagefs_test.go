// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package imagefs

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// fakeFS is a map-backed checks.ImageFS, so every check is unit-testable
// without libguestfs; the real Session is exercised by the tagged
// integration suite in pkg/inspect/guestfs.
type fakeFS struct {
	files map[string]string
	links map[string]string
	stats map[string]checks.FileStat
	luks  bool
}

func (f *fakeFS) Exists(path string) (bool, error) {
	_, okF := f.files[path]
	_, okS := f.stats[path]
	return okF || okS, nil
}

func (f *fakeFS) Cat(path string) (string, error) {
	if c, ok := f.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("%s: no such file", path)
}

func (f *fakeFS) ReadLink(path string) (string, error) {
	if t, ok := f.links[path]; ok {
		return t, nil
	}
	return "", fmt.Errorf("%s: not a symlink", path)
}

func (f *fakeFS) Stat(path string) (checks.FileStat, error) {
	if st, ok := f.stats[path]; ok {
		return st, nil
	}
	return checks.FileStat{}, fmt.Errorf("%s: no such file", path)
}

func (f *fakeFS) HasLUKS() bool { return f.luks }

const dpkgStatus = `Package: linux-nvidia
Status: install ok installed
Version: 24.04.1
Architecture: amd64

Package: nke-cni-calico-manifests-3.30.1
Status: install ok installed
Version: 1
Architecture: all

Package: removed-pkg
Status: deinstall ok config-files
Version: 1.0
Architecture: amd64

Package: dash
Status: install ok installed
Version: 0.5.12-6ubuntu5
Architecture: amd64
`

func goodFS() *fakeFS {
	return &fakeFS{
		files: map[string]string{
			"/var/lib/dpkg/status": dpkgStatus,
			"/etc/passwd":          "root:x:0:0:root:/root:/bin/bash\netcd:x:998:997::/var/lib/etcd:/usr/sbin/nologin\n",
			"/etc/group":           "root:x:0:\netcd:x:997:\n",
		},
		links: map[string]string{
			"/etc/systemd/system/apt-daily.timer":             "/dev/null",
			"/etc/systemd/system/apt-daily-upgrade.timer":     "/dev/null",
			"/etc/systemd/system/unattended-upgrades.service": "/dev/null",
			"/etc/systemd/system/multipathd.service":          "/dev/null",
			"/etc/systemd/system/multipathd.socket":           "/dev/null",
		},
		stats: map[string]checks.FileStat{
			"/var/lib/etcd": {Perm: 0o700, UID: 998, GID: 997},
		},
		luks: true,
	}
}

func sbomMatching() map[string]string {
	return map[string]string{
		"linux-nvidia":                    "24.04.1",
		"nke-cni-calico-manifests-3.30.1": "1",
		"dash":                            "0.5.12-6ubuntu5",
	}
}

func run(t *testing.T, id string, target checks.Target, in checks.Inputs) checks.Outcome {
	t.Helper()
	impl, ok := Impls()[id]
	require.True(t, ok, "no implementation for %s", id)
	out, err := impl(context.Background(), target, in)
	require.NoError(t, err)
	return out
}

func TestParseDpkgStatus(t *testing.T) {
	pkgs, err := ParseDpkgStatus(dpkgStatus)
	require.NoError(t, err)
	assert.Len(t, pkgs, 3, "the deinstalled package must not count")
	assert.Equal(t, "24.04.1", pkgs["linux-nvidia"].Version)
	assert.NotContains(t, pkgs, "removed-pkg")

	_, err = ParseDpkgStatus("not a status file")
	require.Error(t, err)
}

func TestCheckDpkgMatchesSBOM(t *testing.T) {
	target := checks.Target{}
	tests := []struct {
		name     string
		sbom     map[string]string
		want     checks.Status
		contains string
	}{
		{name: "exact match passes", sbom: sbomMatching(), want: checks.StatusPass, contains: "3 installed packages"},
		{name: "nil SBOM is a visible skip", sbom: nil, want: checks.StatusSkip, contains: "no SBOM"},
		{
			name: "version drift fails naming the package",
			sbom: func() map[string]string { s := sbomMatching(); s["dash"] = "9.9"; return s }(),
			want: checks.StatusFail, contains: "~dash installed=0.5.12-6ubuntu5 sbom=9.9",
		},
		{
			name: "package only in SBOM fails",
			sbom: func() map[string]string { s := sbomMatching(); s["ghost"] = "1"; return s }(),
			want: checks.StatusFail, contains: "-ghost=1",
		},
		{
			name: "installed package missing from SBOM fails",
			sbom: func() map[string]string { s := sbomMatching(); delete(s, "dash"); return s }(),
			want: checks.StatusFail, contains: "+dash=0.5.12-6ubuntu5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := run(t, "INT-004", target, checks.Inputs{FS: goodFS(), SBOMPackages: tt.sbom})
			assert.Equal(t, tt.want, out.Status)
			assert.Contains(t, out.Evidence, tt.contains)
		})
	}
}

func TestMaskedUnitChecks(t *testing.T) {
	in := checks.Inputs{FS: goodFS()}
	assert.Equal(t, checks.StatusPass, run(t, "HARD-001", checks.Target{}, in).Status)
	assert.Equal(t, checks.StatusPass, run(t, "HARD-002", checks.Target{}, in).Status)

	fs := goodFS()
	delete(fs.links, "/etc/systemd/system/multipathd.socket")
	out := run(t, "HARD-002", checks.Target{}, checks.Inputs{FS: fs})
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "multipathd.socket")
	assert.NotContains(t, out.Evidence, "multipathd.service,", "only the unmasked unit is named")

	fs = goodFS()
	fs.links["/etc/systemd/system/apt-daily.timer"] = "/usr/lib/systemd/system/apt-daily.timer"
	out = run(t, "HARD-001", checks.Target{}, checks.Inputs{FS: fs})
	assert.Equal(t, checks.StatusFail, out.Status, "a non-/dev/null link is not masked")
}

func TestCheckEtcdDir(t *testing.T) {
	assert.Equal(t, checks.StatusPass, run(t, "HARD-003", checks.Target{}, checks.Inputs{FS: goodFS()}).Status)

	tests := []struct {
		name   string
		mutate func(*fakeFS)
	}{
		{name: "wrong mode", mutate: func(f *fakeFS) { f.stats["/var/lib/etcd"] = checks.FileStat{Perm: 0o755, UID: 998, GID: 997} }},
		{name: "wrong owner", mutate: func(f *fakeFS) { f.stats["/var/lib/etcd"] = checks.FileStat{Perm: 0o700, UID: 0, GID: 0} }},
		{name: "missing directory", mutate: func(f *fakeFS) { delete(f.stats, "/var/lib/etcd") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := goodFS()
			tt.mutate(fs)
			assert.Equal(t, checks.StatusFail, run(t, "HARD-003", checks.Target{}, checks.Inputs{FS: fs}).Status)
		})
	}
}

func TestCheckKernelFlavor(t *testing.T) {
	amd := checks.Target{Arch: "amd64"}
	assert.Equal(t, checks.StatusPass, run(t, "HARD-004", amd, checks.Inputs{FS: goodFS()}).Status)

	out := run(t, "HARD-004", checks.Target{Arch: "arm64"}, checks.Inputs{FS: goodFS()})
	assert.Equal(t, checks.StatusFail, out.Status, "amd64 flavor does not satisfy an arm64 target")
	assert.Contains(t, out.Evidence, "linux-nvidia-64k")
}

func TestCheckCalicoManifests(t *testing.T) {
	assert.Equal(t, checks.StatusPass, run(t, "SPEC-004", checks.Target{}, checks.Inputs{FS: goodFS()}).Status)

	fs := goodFS()
	fs.files["/var/lib/dpkg/status"] = "Package: nvke-cni-calico-manifests-3.30.1\nStatus: install ok installed\nVersion: 1\nArchitecture: amd64\n"
	assert.Equal(t, checks.StatusPass, run(t, "SPEC-004", checks.Target{}, checks.Inputs{FS: fs}).Status,
		"the renamed nvke-cni package satisfies the check")

	fs = goodFS()
	fs.files["/var/lib/dpkg/status"] = "Package: dash\nStatus: install ok installed\nVersion: 1\nArchitecture: amd64\n"
	assert.Equal(t, checks.StatusFail, run(t, "SPEC-004", checks.Target{}, checks.Inputs{FS: fs}).Status)
}

func TestCheckLUKSPresent(t *testing.T) {
	assert.Equal(t, checks.StatusPass, run(t, "FDE-001", checks.Target{Variant: checks.VariantFDE}, checks.Inputs{FS: goodFS()}).Status)

	fs := goodFS()
	fs.luks = false
	assert.Equal(t, checks.StatusFail, run(t, "FDE-001", checks.Target{Variant: checks.VariantFDE}, checks.Inputs{FS: fs}).Status)
}

func TestMergeRejectsDuplicates(t *testing.T) {
	assert.Panics(t, func() {
		Merge(Impls(), Impls())
	})
}
