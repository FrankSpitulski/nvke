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

// containerdConfigV2 is the shape `containerd config default` emits for
// containerd 1.7 (config version 2): the CRI plugin owns sandbox_image, and the
// cgroup driver lives in the runc options table.
const containerdConfigV2 = `version = 2

[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "registry.k8s.io/pause:3.8"
    [plugins."io.containerd.grpc.v1.cri".containerd]
      default_runtime_name = "runc"
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
            BinaryName = ""
            SystemdCgroup = false
`

// containerdConfigV3 is the shape containerd 2.x emits (config version 3): the
// sandbox image moved to the CRI image service's pinned_images table, is spelled
// "sandbox", and is single-quoted.
const containerdConfigV3 = `version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    snapshotter = 'overlayfs'
    [plugins.'io.containerd.cri.v1.images'.pinned_images]
      sandbox = 'registry.k8s.io/pause:3.10'
  [plugins.'io.containerd.cri.v1.runtime']
    [plugins.'io.containerd.cri.v1.runtime'.containerd]
      default_runtime_name = 'runc'
      [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
        runtime_type = 'io.containerd.runc.v2'
        sandboxer = 'podsandbox'
        [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
          BinaryName = ''
          SystemdCgroup = false
`

func TestRewriteContainerdConfig(t *testing.T) {
	const pause = "registry.k8s.io/pause:3.11"

	t.Run("config version 2", func(t *testing.T) {
		got, err := rewriteContainerdConfig(containerdConfigV2, pause)
		require.NoError(t, err)
		assert.Contains(t, got, `sandbox_image = "`+pause+`"`)
		assert.NotContains(t, got, "pause:3.8")
		assert.Contains(t, got, "SystemdCgroup = true")
		assert.NotContains(t, got, "SystemdCgroup = false")
	})

	t.Run("config version 3", func(t *testing.T) {
		got, err := rewriteContainerdConfig(containerdConfigV3, pause)
		require.NoError(t, err)
		assert.Contains(t, got, `sandbox = '`+pause+`'`)
		assert.NotContains(t, got, "pause:3.10")
		assert.Contains(t, got, "SystemdCgroup = true")
		assert.NotContains(t, got, "SystemdCgroup = false")
		// sandboxer is a different key and must survive untouched.
		assert.Contains(t, got, "sandboxer = 'podsandbox'")
	})

	t.Run("SystemdCgroup absent is added to the runc options table", func(t *testing.T) {
		trimmed := `version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images'.pinned_images]
    sandbox = 'registry.k8s.io/pause:3.10'
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]
    BinaryName = ''
`
		got, err := rewriteContainerdConfig(trimmed, pause)
		require.NoError(t, err)
		assert.Contains(t, got, "SystemdCgroup = true")
		assert.Contains(t,
			got,
			"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]\n    SystemdCgroup = true",
			"the key must land inside the runc options table")
	})

	t.Run("no sandbox image setting fails", func(t *testing.T) {
		_, err := rewriteContainerdConfig("version = 3\n[plugins]\n", pause)
		require.ErrorContains(t, err, "no sandbox image setting")
	})

	t.Run("no runc options table to fix up fails", func(t *testing.T) {
		_, err := rewriteContainerdConfig("version = 3\nsandbox = 'x'\n", pause)
		require.ErrorContains(t, err, "no runc options table")
	})
}

func TestParsePauseImage(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "kubeadm output",
			in: "registry.k8s.io/kube-apiserver:v1.36.3\n" +
				"registry.k8s.io/coredns/coredns:v1.13.1\n" +
				"registry.k8s.io/pause:3.10\n" +
				"registry.k8s.io/etcd:3.6.6-0\n",
			want: "registry.k8s.io/pause:3.10",
		},
		{
			name: "a warning mentioning pause is not an image",
			in: "W0102 03:04:05.000000 1 x.go:1] could not resolve registry.k8s.io/pause:3.10\n" +
				"registry.k8s.io/pause:3.9\n",
			want: "registry.k8s.io/pause:3.9",
		},
		{"no pause image", "registry.k8s.io/etcd:3.6.6-0\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePauseImage(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCommentOutFstabSwap(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "swap entry commented out",
			in: "UUID=abc / ext4 defaults 0 1\n" +
				"/swap.img none swap sw 0 0\n" +
				"UUID=def /boot ext4 defaults 0 2\n",
			want: "UUID=abc / ext4 defaults 0 1\n" +
				"#/swap.img none swap sw 0 0\n" +
				"UUID=def /boot ext4 defaults 0 2\n",
		},
		{
			name: "already commented and blank lines are left alone",
			in:   "# /swap.img none swap sw 0 0\n\nUUID=abc / ext4 defaults 0 1\n",
			want: "# /swap.img none swap sw 0 0\n\nUUID=abc / ext4 defaults 0 1\n",
		},
		{
			name: "a device named swap in another column is not a swap mount",
			in:   "/dev/swapdisk /mnt/swap ext4 defaults 0 0\n",
			want: "/dev/swapdisk /mnt/swap ext4 defaults 0 0\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fstab")
			require.NoError(t, os.WriteFile(path, []byte(tt.in), 0o644))
			require.NoError(t, commentOutFstabSwap(path))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}

	t.Run("missing file is not an error", func(t *testing.T) {
		require.NoError(t, commentOutFstabSwap(filepath.Join(t.TempDir(), "absent")))
	})
}

func TestNormalizeVersion(t *testing.T) {
	assert.Equal(t, "v1.36.3", normalizeVersion("v1.36.3"))
	assert.Equal(t, "v1.36.3", normalizeVersion("1.36.3"))
	assert.Equal(t, "v1.36.3", normalizeVersion("  v1.36.3  "))
	assert.Empty(t, normalizeVersion("  "))
}

func TestTargetPath(t *testing.T) {
	tests := map[string]string{
		"kubelet":     "/usr/bin/kubelet",
		"kubeadm":     "/usr/bin/kubeadm",
		"kubectl":     "/usr/bin/kubectl",
		"helm":        "/usr/bin/helm",
		"crictl":      "/usr/bin/crictl",
		"runc":        "/usr/sbin/runc",
		"containerd":  "",
		"cni-plugins": "",
	}
	for name, want := range tests {
		assert.Equal(t, want, targetPath(name), name)
	}
}

func TestRunRequiresBundleRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; this test asserts the pre-root-check argument validation")
	}
	require.ErrorContains(t, run("v1.36.3"), "must run as root")
}
