// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleLock is a lock with the fields in a deliberately unsorted order, so the
// formatting tests have something to normalise.
func sampleLock() lockFile {
	return lockFile{
		Kubernetes: "v1.36.3",
		Artifacts: map[string]lockArtifact{
			"kubelet": {URL: "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubelet", Sha256: strings.Repeat("a", 64)},
			"kubeadm": {URL: "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm", Sha256: strings.Repeat("b", 64)},
		},
		Images: []lockImage{
			{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:" + strings.Repeat("c", 64)},
			{Ref: "registry.k8s.io/etcd:3.6.6-0", Digest: "sha256:" + strings.Repeat("d", 64)},
		},
	}
}

func TestLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleLock()
	require.NoError(t, writeLock(dir, want))

	got, err := readLock(dir, "v1.36.3")
	require.NoError(t, err)
	assert.Equal(t, want.Kubernetes, got.Kubernetes)
	assert.Equal(t, want.Artifacts, got.Artifacts)
	assert.ElementsMatch(t, want.Images, got.Images)
}

func TestMarshalLockIsStable(t *testing.T) {
	first, err := marshalLock(sampleLock())
	require.NoError(t, err)

	// Same content, different input order: the bytes must not move.
	shuffled := sampleLock()
	shuffled.Images[0], shuffled.Images[1] = shuffled.Images[1], shuffled.Images[0]
	second, err := marshalLock(shuffled)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))

	text := string(first)
	assert.True(t, strings.HasSuffix(text, "}\n"), "exactly one trailing newline")
	assert.False(t, strings.HasSuffix(text, "\n\n"))
	assert.Contains(t, text, "\n  \"kubernetes\": \"v1.36.3\",", "two-space indent")
	// Object keys come out sorted, so a regenerated lock diffs cleanly.
	assert.Less(t, strings.Index(text, `"kubeadm"`), strings.Index(text, `"kubelet"`))
	assert.Less(t, strings.Index(text, "etcd"), strings.Index(text, "pause"))
	assert.Less(t, strings.Index(text, `"kubernetes"`), strings.Index(text, `"artifacts"`))
	assert.Less(t, strings.Index(text, `"artifacts"`), strings.Index(text, `"images"`))
}

func TestReadLockMissing(t *testing.T) {
	_, err := readLock(t.TempDir(), "v1.36.3")
	require.ErrorContains(t, err, "no lock for v1.36.3: run make lock and review the diff")
}

func TestReadLockRejectsMismatchedContent(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "v1.36.3.json"), []byte("{ not json"), 0o644))
	_, err := readLock(dir, "v1.36.3")
	require.ErrorContains(t, err, "parse")

	wrongVersion := sampleLock()
	wrongVersion.Kubernetes = "v1.35.3"
	raw, err := marshalLock(wrongVersion)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "v1.36.3.json"), raw, 0o644))
	_, err = readLock(dir, "v1.36.3")
	require.ErrorContains(t, err, `locks "v1.35.3", not v1.36.3`)

	empty, err := marshalLock(lockFile{Kubernetes: "v1.36.3"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "v1.36.3.json"), empty, 0o644))
	_, err = readLock(dir, "v1.36.3")
	require.ErrorContains(t, err, "locks no artifacts")
}

func TestCheckArtifactAgainstLock(t *testing.T) {
	lock := sampleLock()
	const kubeadmURL = "https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm"
	kubeadmDigest := strings.Repeat("b", 64)

	t.Run("match", func(t *testing.T) {
		require.NoError(t, checkArtifactAgainstLock(lock, "kubeadm", kubeadmURL, kubeadmDigest))
	})

	t.Run("digest case is not significant", func(t *testing.T) {
		require.NoError(t, checkArtifactAgainstLock(lock, "kubeadm", kubeadmURL, strings.ToUpper(kubeadmDigest)))
	})

	t.Run("content changed under the same URL", func(t *testing.T) {
		err := checkArtifactAgainstLock(lock, "kubeadm", kubeadmURL, strings.Repeat("f", 64))
		require.ErrorContains(t, err, "upstream changed the bytes behind this URL")
	})

	t.Run("pins moved without a lock refresh", func(t *testing.T) {
		err := checkArtifactAgainstLock(lock, "kubeadm", "https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubeadm", kubeadmDigest)
		require.ErrorContains(t, err, "run make lock and review the diff")
	})

	t.Run("component absent from the lock", func(t *testing.T) {
		err := checkArtifactAgainstLock(lock, "helm", "https://get.helm.sh/helm-v4.1.4-linux-amd64.tar.gz", kubeadmDigest)
		require.ErrorContains(t, err, "helm is not in the lock")
	})
}

func TestCheckImageListAgainstLock(t *testing.T) {
	lock := sampleLock()
	locked := []string{"registry.k8s.io/pause:3.10", "registry.k8s.io/etcd:3.6.6-0"}

	t.Run("exact match in any order", func(t *testing.T) {
		require.NoError(t, checkImageListAgainstLock(lock, locked))
		require.NoError(t, checkImageListAgainstLock(lock, []string{locked[1], locked[0]}))
	})

	t.Run("kubeadm wants an image the lock does not have", func(t *testing.T) {
		err := checkImageListAgainstLock(lock, append(locked, "registry.k8s.io/kube-proxy:v1.36.3"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "+ registry.k8s.io/kube-proxy:v1.36.3 (requested by kubeadm, not in the lock)")
		assert.Contains(t, err.Error(), "run make lock and review the diff")
	})

	t.Run("the lock has an image kubeadm no longer wants", func(t *testing.T) {
		err := checkImageListAgainstLock(lock, locked[:1])
		require.Error(t, err)
		assert.Contains(t, err.Error(), "- registry.k8s.io/etcd:3.6.6-0 (in the lock, not requested by kubeadm)")
	})

	t.Run("a retagged image is both a removal and an addition", func(t *testing.T) {
		err := checkImageListAgainstLock(lock, []string{"registry.k8s.io/pause:3.10", "registry.k8s.io/etcd:3.6.7-0"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "- registry.k8s.io/etcd:3.6.6-0")
		assert.Contains(t, err.Error(), "+ registry.k8s.io/etcd:3.6.7-0")
	})
}

func TestDigestRef(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"tagged", "registry.k8s.io/pause:3.10", "registry.k8s.io/pause@" + digest},
		{"nested repository", "registry.k8s.io/coredns/coredns:v1.13.1", "registry.k8s.io/coredns/coredns@" + digest},
		{"no tag", "registry.k8s.io/pause", "registry.k8s.io/pause@" + digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := digestRef(tt.ref, digest)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("a malformed digest is rejected", func(t *testing.T) {
		_, err := digestRef("registry.k8s.io/pause:3.10", "not-a-digest")
		require.Error(t, err)
	})
}

func TestSummarizeLockChange(t *testing.T) {
	old := sampleLock()
	updated := sampleLock()
	updated.Artifacts["kubeadm"] = lockArtifact{
		URL:    "https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubeadm",
		Sha256: strings.Repeat("b", 64),
	}
	updated.Artifacts["helm"] = lockArtifact{URL: "https://get.helm.sh/helm-v4.1.4-linux-amd64.tar.gz", Sha256: strings.Repeat("e", 64)}
	delete(updated.Artifacts, "kubelet")
	updated.Images = []lockImage{{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:" + strings.Repeat("9", 64)}}

	assert.Equal(t, []string{
		"+ artifact helm https://get.helm.sh/helm-v4.1.4-linux-amd64.tar.gz",
		"~ artifact kubeadm url https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm -> https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubeadm",
		"- artifact kubelet",
		"~ image registry.k8s.io/pause:3.10 sha256:" + strings.Repeat("c", 64) + " -> sha256:" + strings.Repeat("9", 64),
		"- image registry.k8s.io/etcd:3.6.6-0",
	}, summarizeLockChange(old, updated))

	assert.Empty(t, summarizeLockChange(sampleLock(), sampleLock()), "an unchanged lock reports nothing")
}

// TestSummarizeLockChangeReportsURLAndSha256Independently proves that when a
// pin bump moves both the URL and the bytes behind it, both are reported
// rather than only the first one noticed.
func TestSummarizeLockChangeReportsURLAndSha256Independently(t *testing.T) {
	old := sampleLock()
	updated := sampleLock()
	updated.Artifacts["kubeadm"] = lockArtifact{
		URL:    "https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubeadm",
		Sha256: strings.Repeat("f", 64),
	}

	got := summarizeLockChange(old, updated)
	assert.Contains(t, got, "~ artifact kubeadm url https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm -> https://dl.k8s.io/release/v1.36.4/bin/linux/amd64/kubeadm")
	assert.Contains(t, got, "~ artifact kubeadm sha256 "+strings.Repeat("b", 64)+" -> "+strings.Repeat("f", 64))
}
