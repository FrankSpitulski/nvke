// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

func TestBaseImagesTarballName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"v1.36.3", "k8s-base-container-images_1.36.3_linux_amd64.tgz"},
		{"1.34.2", "k8s-base-container-images_1.34.2_linux_amd64.tgz"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, baseImagesTarballName(tt.in), tt.in)
	}
}

func TestParseImageList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "kubeadm output",
			in: "registry.k8s.io/kube-apiserver:v1.36.3\n" +
				"registry.k8s.io/kube-controller-manager:v1.36.3\n" +
				"registry.k8s.io/kube-scheduler:v1.36.3\n" +
				"registry.k8s.io/kube-proxy:v1.36.3\n" +
				"registry.k8s.io/coredns/coredns:v1.13.1\n" +
				"registry.k8s.io/pause:3.10\n" +
				"registry.k8s.io/etcd:3.6.6-0\n",
			want: []string{
				"registry.k8s.io/kube-apiserver:v1.36.3",
				"registry.k8s.io/kube-controller-manager:v1.36.3",
				"registry.k8s.io/kube-scheduler:v1.36.3",
				"registry.k8s.io/kube-proxy:v1.36.3",
				"registry.k8s.io/coredns/coredns:v1.13.1",
				"registry.k8s.io/pause:3.10",
				"registry.k8s.io/etcd:3.6.6-0",
			},
		},
		{
			name: "warnings and blank lines are dropped",
			in: "W0102 03:04:05.000000    1 common.go:1] WARNING: this is a warning\n" +
				"\n" +
				"registry.k8s.io/pause:3.10\n" +
				"   \n",
			want: []string{"registry.k8s.io/pause:3.10"},
		},
		{
			name: "digest reference",
			in:   "registry.k8s.io/pause@sha256:aaaa\n",
			want: []string{"registry.k8s.io/pause@sha256:aaaa"},
		},
		{
			name: "no images",
			in:   "\n\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseImageList(tt.in))
		})
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct {
		name, ref, want string
	}{
		{"tagged", "registry.k8s.io/pause:3.10", "registry.k8s.io/pause"},
		{"nested repository", "registry.k8s.io/coredns/coredns:v1.13.1", "registry.k8s.io/coredns/coredns"},
		{"no tag", "registry.k8s.io/pause", "registry.k8s.io/pause"},
		{"already a digest", "registry.k8s.io/pause@sha256:" + strings.Repeat("a", 64), "registry.k8s.io/pause"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repoName(tt.ref)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("empty reference is rejected", func(t *testing.T) {
		_, err := repoName("")
		require.Error(t, err)
	})
}

func TestImageDigestsFingerprint(t *testing.T) {
	a := []lockImage{
		{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:" + strings.Repeat("a", 64)},
		{Ref: "registry.k8s.io/etcd:3.6.6-0", Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	// Same content, different order: the fingerprint must not move.
	b := []lockImage{a[1], a[0]}
	assert.Equal(t, imageDigestsFingerprint(a), imageDigestsFingerprint(b))

	changed := []lockImage{a[0], {Ref: a[1].Ref, Digest: "sha256:" + strings.Repeat("c", 64)}}
	assert.NotEqual(t, imageDigestsFingerprint(a), imageDigestsFingerprint(changed),
		"a digest change at the same Kubernetes version must change the fingerprint")
}

// sha256Hex returns the lowercase hex sha256 of data, for building
// crane-shaped, content-addressed blob names in tests.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeSyntheticImageTarball writes a docker-archive tarball shaped like what
// `crane pull --format tarball` actually produces: a manifest.json naming a
// config file as "sha256:<hex>" and a layer file as "<hex>.tar.gz", both
// content-addressed by their own bytes, and both present in the tar — so
// mergeImageTarball's on-disk digest check (internBlob/expectedBlobDigest)
// runs the same path here that it runs against a real pull.
func writeSyntheticImageTarball(t *testing.T, path string, configBytes, layerBytes []byte, repoTag string) {
	t.Helper()
	configName := "sha256:" + sha256Hex(configBytes)
	layerName := sha256Hex(layerBytes) + ".tar.gz"

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	writeEntry := func(name string, data []byte) {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}))
		_, err := tw.Write(data)
		require.NoError(t, err)
	}
	writeEntry(configName, configBytes)
	writeEntry(layerName, layerBytes)

	manifest := []dockerArchiveManifestEntry{{Config: configName, Layers: []string{layerName}, RepoTags: []string{repoTag}}}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	writeEntry("manifest.json", manifestJSON)
}

func TestExpectedBlobDigest(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"config, crane shape", "sha256:" + hex64, hex64, true},
		{"layer, .tar.gz", hex64 + ".tar.gz", hex64, true},
		{"layer, bare .tar", hex64 + ".tar", hex64, true},
		{"layer, .tar.zst", hex64 + ".tar.zst", hex64, true},
		{"not crane-shaped, arbitrary json name", "config1.json", "", false},
		{"empty", "", "", false},
		{"sha256 prefix but too short", "sha256:short", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := expectedBlobDigest(tt.in)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// TestInternBlobRejectsAMismatchedName proves internBlob refuses a blob whose
// content does not hash to what its own crane-shaped filename claims —
// standing in for disk corruption between crane's write and this read, or a
// `-crane` binary that is not really crane, since crane's own wire-level
// verification cannot see either of those.
func TestInternBlobRejectsAMismatchedName(t *testing.T) {
	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	require.NoError(t, os.MkdirAll(extractDir, 0o755))
	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))

	actual := []byte("actual layer bytes on disk")
	name := sha256Hex([]byte("bytes crane claimed to have written")) + ".tar.gz"
	require.NoError(t, os.WriteFile(filepath.Join(extractDir, name), actual, 0o644))

	_, err := internBlob(extractDir, blobDir, map[string]string{}, name)
	require.ErrorContains(t, err, "does not match its own manifest")

	entries, err := os.ReadDir(blobDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a mismatched blob must not be interned")
}

// TestMergeImageTarballDedupesSharedLayer proves two synthetic per-image
// tarballs that share a layer intern that layer's blob once, and that the
// merged manifest.json still resolves both images to it.
func TestMergeImageTarballDedupesSharedLayer(t *testing.T) {
	dir := t.TempDir()
	sharedLayer := []byte("shared layer bytes")

	tar1 := filepath.Join(dir, "image1.tar")
	writeSyntheticImageTarball(t, tar1, []byte("config-one"), sharedLayer, "original/one:pulled-by-digest")

	tar2 := filepath.Join(dir, "image2.tar")
	writeSyntheticImageTarball(t, tar2, []byte("config-two"), sharedLayer, "original/two:pulled-by-digest")

	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	blobSHA := map[string]string{}

	entry1, err := mergeImageTarball(tar1, blobDir, blobSHA, "registry.k8s.io/one:v1")
	require.NoError(t, err)
	entry2, err := mergeImageTarball(tar2, blobDir, blobSHA, "registry.k8s.io/two:v1")
	require.NoError(t, err)

	// RepoTags is overridden to the original tag, not the digest ref pulled.
	assert.Equal(t, []string{"registry.k8s.io/one:v1"}, entry1.RepoTags)
	assert.Equal(t, []string{"registry.k8s.io/two:v1"}, entry2.RepoTags)
	assert.Equal(t, entry1.Layers, entry2.Layers, "the shared layer must resolve to the same blob path in both entries")
	assert.NotEqual(t, entry1.Config, entry2.Config, "distinct config blobs must not collide")

	entries, err := os.ReadDir(blobDir)
	require.NoError(t, err)
	assert.Len(t, entries, 3, "two distinct configs plus one shared layer, not four blobs")

	archivePath := filepath.Join(dir, "merged.tgz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)
	require.NoError(t, writeMergedArchive(f, blobDir, []dockerArchiveManifestEntry{entry1, entry2}))
	require.NoError(t, f.Close())

	members, manifestBytes := readTarMembers(t, archivePath)
	assert.Len(t, members, 4, "three blobs plus manifest.json")
	for name, hdr := range members {
		assert.Equal(t, int64(0), hdr.ModTime.Unix(), "tar entry %s must have a deterministic mtime", name)
		assert.Zero(t, hdr.Uid)
		assert.Zero(t, hdr.Gid)
		assert.Equal(t, tar.FormatUSTAR, hdr.Format, "tar entry %s must be USTAR", name)
	}

	var merged []dockerArchiveManifestEntry
	require.NoError(t, json.Unmarshal(manifestBytes, &merged))
	require.Len(t, merged, 2)
	var tags []string
	for _, e := range merged {
		tags = append(tags, e.RepoTags...)
		// containerd's docker-archive importer requires every Config and Layers
		// path an entry names to actually be a member of the tar; a merge that
		// silently dropped or renamed a blob would produce an archive nothing
		// could import.
		assert.Contains(t, members, e.Config, "manifest entry %v names a Config not present in the tar", e.RepoTags)
		for _, layer := range e.Layers {
			assert.Contains(t, members, layer, "manifest entry %v names a layer not present in the tar", e.RepoTags)
		}
	}
	assert.ElementsMatch(t, []string{"registry.k8s.io/one:v1", "registry.k8s.io/two:v1"}, tags)
}

// readTarMembers gunzips and reads every member of a merged archive, checking
// along the way that no member name repeats. It returns each member's header
// keyed by name, plus the raw bytes of manifest.json.
func readTarMembers(t *testing.T, path string) (map[string]*tar.Header, []byte) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)

	members := map[string]*tar.Header{}
	var manifestBytes []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		_, dup := members[hdr.Name]
		require.False(t, dup, "duplicate tar member %s", hdr.Name)
		members[hdr.Name] = hdr
		if hdr.Name == "manifest.json" {
			manifestBytes, err = io.ReadAll(tr)
			require.NoError(t, err)
		}
	}
	return members, manifestBytes
}

// TestWriteMergedArchiveIsByteIdentical proves writeMergedArchive is
// deterministic: running it twice over the same blob directory and manifest
// produces the exact same bytes, which is what makes the tarball's own
// .sha256 sidecar and lock-digests fingerprint meaningful as a cache key.
func TestWriteMergedArchiveIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, strings.Repeat("a", 64)+".json"), []byte("config"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(blobDir, strings.Repeat("b", 64)+".tar.gz"), []byte("layer"), 0o644))
	manifest := []dockerArchiveManifestEntry{{
		Config:   "blobs/" + strings.Repeat("a", 64) + ".json",
		Layers:   []string{"blobs/" + strings.Repeat("b", 64) + ".tar.gz"},
		RepoTags: []string{"registry.k8s.io/pause:3.10"},
	}}

	run := func() []byte {
		var buf bytes.Buffer
		require.NoError(t, writeMergedArchive(&buf, blobDir, append([]dockerArchiveManifestEntry(nil), manifest...)))
		return buf.Bytes()
	}

	first := run()
	second := run()
	assert.Equal(t, first, second, "the same inputs must produce byte-identical output")
	assert.NotEmpty(t, first)
}

// TestWriteBaseImagesTarballReusesOnMatchingFingerprint proves a cached
// tarball whose sidecar and lock-digests fingerprint both still match is
// reused without invoking crane at all, and that a lock change (a different
// image digest) invalidates it.
func TestWriteBaseImagesTarballReusesOnMatchingFingerprint(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "images.tgz")
	const missingCrane = "/nonexistent/crane-must-not-run"

	images := []lockImage{{Ref: "registry.k8s.io/pause:3.10", Digest: "sha256:" + strings.Repeat("a", 64)}}
	require.NoError(t, os.WriteFile(dest, []byte("cached bytes"), 0o644))
	_, err := bundle.WriteSHA256Sidecar(dest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dest+".lock-digests", []byte(imageDigestsFingerprint(images)+"\n"), 0o644))

	require.NoError(t, writeBaseImagesTarball(t.Context(), missingCrane, images, dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "cached bytes", string(got), "a matching fingerprint must not touch the cached tarball")

	changed := []lockImage{{Ref: images[0].Ref, Digest: "sha256:" + strings.Repeat("b", 64)}}
	err = writeBaseImagesTarball(t.Context(), missingCrane, changed, dest)
	require.Error(t, err, "a lock change must invalidate the cache and attempt a pull, which fails without a real crane binary")

	// The stale tarball, its sidecar and its fingerprint must all be gone, not
	// left behind as an internally consistent but outdated set that a later
	// -skip-images run (which only checks that these files exist) could trust.
	for _, path := range []string{dest, dest + ".sha256", dest + ".lock-digests"} {
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "%s must not survive a failed re-pull", path)
	}
}
