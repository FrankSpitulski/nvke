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

func TestManifestStringRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
		want string
	}{
		{
			name: "typical package",
			m: Manifest{
				Control: map[string][]string{
					"Package":      {"nke-kubeadm-distro-1.36.3"},
					"Section":      {"admin"},
					"Priority":     {"optional"},
					"Architecture": {"amd64"},
					"Provides":     {"nke-kubeadm-distro"},
					"Depends":      {"iptables", "iproute2", "conntrack"},
					"Conflicts":    {"kubeadm", "helm"},
					"Replaces":     {"kubeadm"},
				},
				Scripts: []string{"postinst", "prerm", "postrm"},
				Files: []FileEntry{
					{Type: "d", Mode: "rwxr-xr-x", Path: "/etc/"},
					{Type: "f", Mode: "rw-r--r--", Path: "/etc/default/kubelet"},
					{Type: "f", Mode: "rwxr-xr-x", Path: "/usr/bin/kubeadm", SHA256: "abc123"},
					{Type: "l", Mode: "rwxrwxrwx", Path: "/usr/bin/nke-k8s-installer", LinkTarget: "../lib/nke/k8s-matrix/v1.36.3/installer"},
					{Type: "h", Mode: "rw-r--r--", Path: "/usr/share/doc/nke-kubeadm-distro-1.36.3/copyright", LinkTarget: "/usr/share/doc/nke-kubeadm-distro/copyright"},
					{Type: "c", Mode: "rw-------", Path: "/dev/null"},
					{Type: "b", Mode: "rw-rw----", Path: "/dev/sda"},
					{Type: "p", Mode: "rw-r--r--", Path: "/run/fixture.fifo"},
				},
			},
			// Version does not appear anywhere: it is the packaging
			// revision (DEB_REVISION), not part of the contract, and is
			// excluded exactly like Installed-Size/Maintainer/Description.
			want: "## control\n" +
				"# nke-kubeadm-distro* peer rows are excluded from Conflicts/Replaces: computed from K8S_VERSIONS, not part of the contract.\n" +
				"Package: nke-kubeadm-distro-1.36.3\n" +
				"Section: admin\n" +
				"Priority: optional\n" +
				"Architecture: amd64\n" +
				"Provides: nke-kubeadm-distro\n" +
				"Depends: conntrack, iproute2, iptables\n" +
				"Conflicts: helm, kubeadm\n" +
				"Replaces: kubeadm\n" +
				"## conffiles\n" +
				"(none)\n" +
				"## scripts\n" +
				"postinst postrm prerm \n" +
				"## files\n" +
				"c rw------- /dev/null\n" +
				"b rw-rw---- /dev/sda\n" +
				"d rwxr-xr-x /etc/\n" +
				"f rw-r--r-- /etc/default/kubelet\n" +
				"p rw-r--r-- /run/fixture.fifo\n" +
				"f rwxr-xr-x /usr/bin/kubeadm sha256=abc123\n" +
				"l rwxrwxrwx /usr/bin/nke-k8s-installer -> ../lib/nke/k8s-matrix/v1.36.3/installer\n" +
				"h rw-r--r-- /usr/share/doc/nke-kubeadm-distro-1.36.3/copyright -> /usr/share/doc/nke-kubeadm-distro/copyright\n",
		},
		{
			name: "conffiles present, no scripts",
			m: Manifest{
				Control:   map[string][]string{"Package": {"x"}},
				Conffiles: []string{"/etc/default/kubelet"},
			},
			want: "## control\n" +
				"# nke-kubeadm-distro* peer rows are excluded from Conflicts/Replaces: computed from K8S_VERSIONS, not part of the contract.\n" +
				"Package: x\n" +
				"## conffiles\n" +
				"/etc/default/kubelet\n" +
				"## scripts\n" +
				" \n" +
				"## files\n",
		},
		{
			name: "path and link target with spaces",
			m: Manifest{
				Control: map[string][]string{"Package": {"x"}},
				Files: []FileEntry{
					{Type: "f", Mode: "rw-r--r--", Path: "/usr/share/doc/x/read me.txt"},
					{Type: "l", Mode: "rwxrwxrwx", Path: "/usr/share/doc/x/link with spaces", LinkTarget: "../target with spaces"},
					{Type: "f", Mode: "rwxr-xr-x", Path: "/usr/bin/has space", SHA256: "deadbeef"},
				},
			},
			want: "## control\n" +
				"# nke-kubeadm-distro* peer rows are excluded from Conflicts/Replaces: computed from K8S_VERSIONS, not part of the contract.\n" +
				"Package: x\n" +
				"## conffiles\n" +
				"(none)\n" +
				"## scripts\n" +
				" \n" +
				"## files\n" +
				"f rwxr-xr-x /usr/bin/has space sha256=deadbeef\n" +
				"l rwxrwxrwx /usr/share/doc/x/link with spaces -> ../target with spaces\n" +
				"f rw-r--r-- /usr/share/doc/x/read me.txt\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.m.normalize()
			assert.Equal(t, tc.want, tc.m.String())

			// ParseManifest must read back exactly what String wrote.
			parsed, err := ParseManifest(tc.want)
			require.NoError(t, err)
			assert.Equal(t, tc.want, parsed.String())
		})
	}
}

func TestParseManifestRejectsGarbage(t *testing.T) {
	_, err := ParseManifest("not a manifest at all\n")
	require.Error(t, err)
}

// TestGoldenManifestsRoundTrip parses every committed testdata/golden/*.manifest
// and asserts String() reproduces it exactly, so a committed golden is
// guaranteed to already be in the canonical (sorted, Version-free) form
// Compare expects it in.
func TestGoldenManifestsRoundTrip(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "*.manifest"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected at least one committed golden manifest")

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			require.NoError(t, err)

			m, err := ParseManifest(string(data))
			require.NoError(t, err)
			assert.Equal(t, string(data), m.String(), "committed golden is not in canonical form; regenerate it with cmd/debmanifest")
			assert.NotContains(t, string(data), "Version:", "Version is excluded from the contract")
		})
	}
}

// TestParseDeviationsAcceptsCommittedFile guards against the committed
// testdata/golden/deviations.txt drifting out of the grammar ParseDeviations
// actually accepts.
func TestParseDeviationsAcceptsCommittedFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "deviations.txt"))
	require.NoError(t, err)

	_, err = ParseDeviations(string(data))
	require.NoError(t, err)
}
