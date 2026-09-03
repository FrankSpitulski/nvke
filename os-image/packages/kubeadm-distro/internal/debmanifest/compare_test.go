// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseGolden() Manifest {
	m := Manifest{
		Control: map[string][]string{
			"Package":      {"nke-kubeadm-distro-1.36.3"},
			"Architecture": {"amd64"},
			"Depends":      {"iptables", "iproute2"},
			"Conflicts":    {"kubeadm", "runc"},
		},
		Scripts: []string{"postinst", "postrm", "prerm"},
		Files: []FileEntry{
			{Type: "d", Mode: "rwxr-xr-x", Path: "/etc/"},
			{Type: "f", Mode: "rw-r--r--", Path: "/etc/kubernetes/manifests/.kubelet-keep"},
			{Type: "f", Mode: "rwxr-xr-x", Path: "/usr/bin/kubeadm", SHA256: "aaaa"},
			{Type: "f", Mode: "rw-r--r--", Path: "/var/lib/kubelet/.kubelet-keep"},
		},
	}
	m.normalize()
	return m
}

func TestCompareIdentical(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()

	diff, err := Compare(golden, actual, nil)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareRemovedFile(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	// The new layout ships /etc/kubernetes/manifests/ empty: no placeholder.
	actual.Files = removeByPath(actual.Files, "/etc/kubernetes/manifests/.kubelet-keep")

	// Without the deviation, they must not match.
	diff, err := Compare(golden, actual, nil)
	require.Error(t, err)
	assert.Contains(t, diff, "-f rw-r--r-- /etc/kubernetes/manifests/.kubelet-keep")

	devs, err := ParseDeviations("removed /etc/kubernetes/manifests/.kubelet-keep  # dirs ship empty now\n")
	require.NoError(t, err)
	diff, err = Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareAddedFile(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	actual.Files = append(actual.Files, FileEntry{Type: "f", Mode: "rwxr-xr-x", Path: "/usr/local/bin/containerd", SHA256: "bbbb"})

	diff, err := Compare(golden, actual, nil)
	require.Error(t, err)
	assert.Contains(t, diff, "+f rwxr-xr-x /usr/local/bin/containerd sha256=bbbb")

	devs, err := ParseDeviations("added /usr/local/bin/containerd  # unpacked by the installer\n")
	require.NoError(t, err)
	diff, err = Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareFieldChanged(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	actual.Control["Depends"] = []string{"iptables", "iproute2", "conntrack", "socat", "ethtool"}

	diff, err := Compare(golden, actual, nil)
	require.Error(t, err)
	assert.NotEmpty(t, diff)

	devs, err := ParseDeviations("field Depends += conntrack, socat, ethtool  # kubeadm preflight hard requirement\n")
	require.NoError(t, err)
	diff, err = Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareUnusedDeviation(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()

	// This deviation does not correspond to any actual difference, so it
	// must be reported as unused even though the manifests already match
	// without it — an allowlist entry that does nothing is exactly the rot
	// the parser is supposed to catch.
	devs, err := ParseDeviations("removed /usr/bin/does-not-exist  # stale entry\n")
	require.NoError(t, err)

	diff, err := Compare(golden, actual, devs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "had no effect")
	assert.Empty(t, diff, "manifests still match; only the deviation is unused")
}

func TestCompareUnusedAlongsideRealMismatch(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	actual.Files = append(actual.Files, FileEntry{Type: "f", Mode: "rwxr-xr-x", Path: "/usr/local/bin/containerd", SHA256: "bbbb"})

	devs, err := ParseDeviations("removed /usr/bin/does-not-exist  # stale entry\n")
	require.NoError(t, err)

	diff, err := Compare(golden, actual, devs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "had no effect")
	assert.Contains(t, diff, "+f rwxr-xr-x /usr/local/bin/containerd sha256=bbbb")
}

func TestCompareModeChanged(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	for i, f := range actual.Files {
		if f.Path == "/usr/bin/kubeadm" {
			actual.Files[i].Mode = "rwsr-xr-x"
		}
	}

	diff, err := Compare(golden, actual, nil)
	require.Error(t, err)
	assert.Contains(t, diff, "-f rwxr-xr-x /usr/bin/kubeadm sha256=aaaa")
	assert.Contains(t, diff, "+f rwsr-xr-x /usr/bin/kubeadm sha256=aaaa")

	devs, err := ParseDeviations("mode /usr/bin/kubeadm rwsr-xr-x  # setuid kubeadm, reviewed\n")
	require.NoError(t, err)
	diff, err = Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareScriptsReplace(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	actual.Scripts = []string{"postinst"}

	devs, err := ParseDeviations("scripts = postinst  # prerm/postrm dropped\n")
	require.NoError(t, err)
	diff, err := Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCompareDoesNotMutateInputsOrDeviations(t *testing.T) {
	golden := baseGolden()
	actual := baseGolden()
	actual.Files = removeByPath(actual.Files, "/etc/kubernetes/manifests/.kubelet-keep")
	goldenFilesBefore := len(golden.Files)

	devs, err := ParseDeviations("removed /etc/kubernetes/manifests/.kubelet-keep  # dirs ship empty now\n")
	require.NoError(t, err)

	diff, err := Compare(golden, actual, devs)
	require.NoError(t, err)
	assert.Empty(t, diff)
	assert.Len(t, golden.Files, goldenFilesBefore, "Compare must not mutate the golden it was given")

	// The same []*Deviation, reused against a pair where the deviation does
	// NOT apply, must be reported unused again rather than staying "used"
	// from the previous call — that would happen if Compare tracked used-ness
	// on the shared *Deviation instead of locally.
	golden2 := baseGolden()
	golden2.Files = removeByPath(golden2.Files, "/etc/kubernetes/manifests/.kubelet-keep")
	actual2 := baseGolden()
	actual2.Files = removeByPath(actual2.Files, "/etc/kubernetes/manifests/.kubelet-keep")
	// Both sides already agree, so "removed" has nothing left to remove.
	_, err = Compare(golden2, actual2, devs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "had no effect")
}

func TestCompareDoesNotReorderActualFilesInPlace(t *testing.T) {
	// Deliberately unsorted and unnormalized, holding on to the slice
	// Compare is handed.
	actual := Manifest{
		Control: map[string][]string{"Package": {"x"}},
		Files: []FileEntry{
			{Type: "f", Mode: "rw-r--r--", Path: "/zzz"},
			{Type: "f", Mode: "rw-r--r--", Path: "/aaa"},
		},
	}
	before := append([]FileEntry(nil), actual.Files...)

	golden := Manifest{Control: map[string][]string{"Package": {"x"}}}
	_, _ = Compare(golden, actual, nil)

	assert.Equal(t, before, actual.Files, "Compare must not sort actual.Files in place")
}

func TestParseDeviationsRejectsMissingReason(t *testing.T) {
	_, err := ParseDeviations("removed /usr/bin/nke-k8s-installer\n")
	require.Error(t, err)
}

func TestParseDeviationsIgnoresCommentsAndBlankLines(t *testing.T) {
	devs, err := ParseDeviations("# header comment\n\n# another comment\n")
	require.NoError(t, err)
	assert.Empty(t, devs)
}

func TestParseDeviationsRejectsUnknownKind(t *testing.T) {
	_, err := ParseDeviations("frobnicate /usr/bin/kubeadm  # not a real kind\n")
	require.Error(t, err)
}

func TestParseDeviationsRequiresSpaceHashSeparator(t *testing.T) {
	// A bare "#" inside the path must not be mistaken for the reason
	// separator: this line has no " #" anywhere, so it must be rejected.
	_, err := ParseDeviations("removed /usr/bin/weird#path\n")
	require.Error(t, err)
}

func TestParseDeviationsAllowsHashInPathBeforeReason(t *testing.T) {
	devs, err := ParseDeviations("removed /usr/bin/weird#path  # reason after real separator\n")
	require.NoError(t, err)
	require.Len(t, devs, 1)
	assert.Equal(t, "/usr/bin/weird#path", devs[0].Values[0])
	assert.Equal(t, "reason after real separator", devs[0].Reason)
}

func TestParseDeviationsRejectsAddSubtractOnScalarField(t *testing.T) {
	_, err := ParseDeviations("field Package += extra  # not allowed on a scalar field\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scalar")
}

func TestParseDeviationsMode(t *testing.T) {
	devs, err := ParseDeviations("mode /usr/bin/kubeadm rwsr-xr-x  # setuid\n")
	require.NoError(t, err)
	require.Len(t, devs, 1)
	assert.Equal(t, "mode", devs[0].Kind)
	assert.Equal(t, []string{"/usr/bin/kubeadm", "rwsr-xr-x"}, devs[0].Values)
}

func TestParseDeviationsRejectsInvalidModeString(t *testing.T) {
	cases := []string{
		"755",        // not the "rwxr-xr-x" shape at all
		"rwxr-xr-xx", // too long
		"rwxr-xr",    // too short
		"rwzr-xr-x",  // 'z' is not a valid permission character
	}
	for _, newMode := range cases {
		t.Run(newMode, func(t *testing.T) {
			_, err := ParseDeviations("mode /usr/bin/kubeadm " + newMode + "  # bad mode\n")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "permission string")
		})
	}
}

func TestParseDeviationsRejectsUnknownField(t *testing.T) {
	_, err := ParseDeviations("field Depend += conntrack  # typo, missing the 's'\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not part of the contract")
}

func removeByPath(files []FileEntry, path string) []FileEntry {
	out := make([]FileEntry, 0, len(files))
	for _, f := range files {
		if f.Path != path {
			out = append(out, f)
		}
	}
	return out
}
