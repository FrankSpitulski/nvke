// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

func TestUpstreamChecksumURL(t *testing.T) {
	tests := []struct {
		component, artifactURL, want string
	}{
		{
			"kubeadm",
			"https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm",
			"https://dl.k8s.io/release/v1.36.3/bin/linux/amd64/kubeadm.sha256",
		},
		{
			"containerd",
			"https://github.com/containerd/containerd/releases/download/v2.3.0/containerd-2.3.0-linux-amd64.tar.gz",
			"https://github.com/containerd/containerd/releases/download/v2.3.0/containerd-2.3.0-linux-amd64.tar.gz.sha256sum",
		},
		{
			"runc",
			"https://github.com/opencontainers/runc/releases/download/v1.4.2/runc.amd64",
			"https://github.com/opencontainers/runc/releases/download/v1.4.2/runc.sha256sum",
		},
		{"unknown", "https://example.com/x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.component, func(t *testing.T) {
			assert.Equal(t, tt.want, upstreamChecksumURL(tt.component, tt.artifactURL))
		})
	}
}

func TestIsSHA256Hex(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"", false},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isSHA256Hex(tt.in), tt.in)
	}
}

func TestParseExpectedSHA256FromChecksumDoc(t *testing.T) {
	const wantHex = "6a2346006132f6e1ed0b5248e518098cf5abbce25bf11b8926fb1073091b83f4"
	const otherHex = "1111111111111111111111111111111111111111111111111111111111111111"

	tests := []struct {
		name      string
		body      string
		artifact  string
		want      string
		errSubstr string
	}{
		{
			name:     "bare digest, dl.k8s.io shape",
			body:     wantHex + "\n",
			artifact: "kubeadm",
			want:     wantHex,
		},
		{
			name:     "gnu name match",
			body:     wantHex + "  containerd-2.3.0-linux-amd64.tar.gz\n",
			artifact: "containerd-2.3.0-linux-amd64.tar.gz",
			want:     wantHex,
		},
		{
			name:     "gnu path in the second field",
			body:     wantHex + "  dist/out/cni-plugins-linux-amd64-v1.6.0.tgz\n",
			artifact: "cni-plugins-linux-amd64-v1.6.0.tgz",
			want:     wantHex,
		},
		{
			name: "runc clearsigned list",
			body: "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
				wantHex + "  runc.amd64\n" + otherHex + "  runc.arm64\n" +
				"-----BEGIN PGP SIGNATURE-----\nx\n",
			artifact: "runc.amd64",
			want:     wantHex,
		},
		{
			name:      "multiple bare digests",
			body:      wantHex + "\n" + otherHex + "\n",
			artifact:  "kubeadm",
			errSubstr: "multiple bare SHA-256",
		},
		{
			name:      "no match",
			body:      otherHex + "  other.tgz\n",
			artifact:  "missing.bin",
			errSubstr: "no SHA-256 digest",
		},
		{
			name:      "two digests for one name",
			body:      wantHex + "  foo.bin\n" + otherHex + "  foo.bin\n",
			artifact:  "foo.bin",
			errSubstr: "ambiguous",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExpectedSHA256FromChecksumDoc(tt.body, tt.artifact)
			if tt.errSubstr != "" {
				require.ErrorContains(t, err, tt.errSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestVerifyUpstreamSHA256(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "blob")
	require.NoError(t, os.WriteFile(artifact, []byte("upstream-check-test-payload\n"), 0o644))
	wantHash, err := bundle.FileSHA256(artifact)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s\n", wantHash)
	}))
	defer srv.Close()

	digest, err := verifyUpstreamSHA256(t.Context(), artifact, srv.URL, "blob", "kubelet", "")
	require.NoError(t, err)
	assert.Equal(t, wantHash, digest)
	// Verification reports; it does not write. The caller owns the sidecar.
	_, statErr := os.Stat(artifact + ".sha256")
	assert.True(t, os.IsNotExist(statErr))

	other := filepath.Join(dir, "other")
	require.NoError(t, os.WriteFile(other, []byte("different bytes"), 0o644))
	_, err = verifyUpstreamSHA256(t.Context(), other, srv.URL, "blob", "kubelet", "")
	require.ErrorContains(t, err, "upstream checksum mismatch")
}

func TestFetchAndVerify(t *testing.T) {
	payload := []byte("#!/bin/sh\necho kubeadm\n")
	digest := sha256.Sum256(payload)
	want := hex.EncodeToString(digest[:])

	var artifactHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s\n", want)
			return
		}
		artifactHits++
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kubeadm")
	url := srv.URL + "/kubeadm"

	got, err := fetchAndVerify(t.Context(), "kubeadm", url, dest, "kubeadm", "", "")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, artifactHits)

	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "binaries must be executable in the bundle")

	// A cached artifact that still verifies is not downloaded again.
	got, err = fetchAndVerify(t.Context(), "kubeadm", url, dest, "kubeadm", "", "")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, artifactHits)

	// A cached artifact that no longer matches the publisher is replaced.
	require.NoError(t, os.WriteFile(dest, []byte("stale"), 0o644))
	got, err = fetchAndVerify(t.Context(), "kubeadm", url, dest, "kubeadm", "", "")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 2, artifactHits)
}

func TestVerifyUpstreamSHA256RejectsUnsignedRunc(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "runc.amd64")
	require.NoError(t, os.WriteFile(artifact, []byte("runc\n"), 0o644))
	digest, err := bundle.FileSHA256(artifact)
	require.NoError(t, err)

	// A plain checksum list, exactly as it would look if the signature were
	// stripped in transit. No gpg needed: the document never reaches it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  runc.amd64\n", digest)
	}))
	defer srv.Close()

	_, err = verifyUpstreamSHA256(t.Context(), artifact, srv.URL, "runc.amd64", "runc", "")
	require.ErrorContains(t, err, "not OpenPGP clearsigned")
}

func TestVerifyUpstreamSHA256RejectsUnexpectedSignature(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(artifact, []byte("kubeadm\n"), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nnope\n")
	}))
	defer srv.Close()

	_, err := verifyUpstreamSHA256(t.Context(), artifact, srv.URL, "kubeadm", "kubeadm", "")
	require.ErrorContains(t, err, "unexpected PGP-signed checksum")
}

func TestFetchAndVerifyLeavesNothingBehindOnFailure(t *testing.T) {
	// The server serves a body that does not match the checksum it publishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s\n", strings.Repeat("a", 64))
			return
		}
		_, _ = w.Write([]byte("bytes that hash to something else"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kubeadm")
	_, err := fetchAndVerify(t.Context(), "kubeadm", srv.URL+"/kubeadm", dest, "kubeadm", "", "")
	require.ErrorContains(t, err, "upstream checksum mismatch")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a failed download must leave neither the artifact, a .part file, nor a sidecar")
}

func TestFetchAndVerifyDropsAStaleCacheWhenTheRedownloadFails(t *testing.T) {
	// Upstream publishes a checksum that matches neither the cached bytes nor
	// what it now serves: a pin bump against a broken mirror.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s\n", strings.Repeat("a", 64))
			return
		}
		_, _ = w.Write([]byte("replacement bytes that also do not match"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(dest, []byte("stale cached bytes"), 0o755))
	_, err := bundle.WriteSHA256Sidecar(dest)
	require.NoError(t, err)

	_, err = fetchAndVerify(t.Context(), "kubeadm", srv.URL+"/kubeadm", dest, "kubeadm", "", "")
	require.ErrorContains(t, err, "upstream checksum mismatch")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries,
		"the stale artifact and its sidecar must be gone, so the next run cannot mistake them for verified")
}

func TestEnsureExecutableComponent(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o644))
	require.NoError(t, ensureExecutableComponent("kubeadm", binary))
	info, err := os.Stat(binary)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	archive := filepath.Join(dir, "helm.tar.gz")
	require.NoError(t, os.WriteFile(archive, []byte("x"), 0o644))
	require.NoError(t, ensureExecutableComponent("helm", archive))
	info, err = os.Stat(archive)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	require.NoError(t, writeManifest(path, manifestDoc{
		Version:       "v1.36.3",
		Architectures: map[string]map[string]manifestComponent{"amd64": {"kubeadm": {File: "kubeadm", Sha256: "abc"}}},
	}))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var round manifestDoc
	require.NoError(t, json.Unmarshal(raw, &round))
	assert.Equal(t, "kubeadm", round.Architectures["amd64"]["kubeadm"].File)

	_, err = bundle.VerifyFileAgainstSidecar(path, path+".sha256")
	require.NoError(t, err)
}

// withFastRetryBackoff shrinks retryBackoff to near-zero for the duration of
// a test, so a test that exercises real retries does not spend real seconds
// asleep, and restores it afterward.
func withFastRetryBackoff(t *testing.T) {
	t.Helper()
	prev := retryBackoff
	retryBackoff = time.Microsecond
	t.Cleanup(func() { retryBackoff = prev })
}

// TestDoWithRetrySucceedsAfterTransientServerErrors proves a 500 is retried
// and a later success is returned, not treated as a terminal failure.
func TestDoWithRetrySucceedsAfterTransientServerErrors(t *testing.T) {
	withFastRetryBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	resp, err := doWithRetry(t.Context(), srv.URL, time.Second)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.EqualValues(t, 3, hits.Load(), "must retry the two 5xx responses before the third succeeds")
}

// TestDoWithRetryDoesNotRetryClientErrors proves a 404 fails immediately: a
// retry cannot change what a nonexistent object returns.
func TestDoWithRetryDoesNotRetryClientErrors(t *testing.T) {
	withFastRetryBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := doWithRetry(t.Context(), srv.URL, time.Second)
	require.Error(t, err)
	assert.EqualValues(t, 1, hits.Load(), "a 404 must not be retried")
}

// TestDoWithRetryGivesUpAfterMaxAttempts proves a persistently failing server
// is retried exactly maxDownloadAttempts times, then reports the failure.
func TestDoWithRetryGivesUpAfterMaxAttempts(t *testing.T) {
	withFastRetryBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := doWithRetry(t.Context(), srv.URL, time.Second)
	require.Error(t, err)
	assert.EqualValues(t, maxDownloadAttempts, hits.Load())
}

// TestDoWithRetryRetriesTooManyRequests proves a 429 is retried like a 5xx,
// and that a small Retry-After is honored instead of the exponential backoff.
func TestDoWithRetryRetriesTooManyRequests(t *testing.T) {
	withFastRetryBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	resp, err := doWithRetry(t.Context(), srv.URL, time.Second)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.EqualValues(t, 2, hits.Load())
}

// TestRetryAfterDuration is a table test over the values doWithRetry can see
// on the wire: an honorable small delay, and everything that must fall back
// to the exponential backoff instead — no header, non-numeric, negative,
// zero (retry now, not "wait zero"), over the ceiling, and a value large
// enough that converting it to a time.Duration before bounding it would
// overflow and wrap negative, which used to slip past the ceiling check.
func TestRetryAfterDuration(t *testing.T) {
	tests := []struct {
		name string
		v    string
		want time.Duration
		ok   bool
	}{
		{"absent", "", 0, false},
		{"honored", "5", 5 * time.Second, true},
		{"at the ceiling", "30", 30 * time.Second, true},
		{"just over the ceiling", "31", 0, false},
		{"zero falls back to the backoff", "0", 0, false},
		{"negative", "-1", 0, false},
		{"not a number", "soon", 0, false},
		{"large enough to overflow a naive seconds-to-Duration conversion", "10000000000", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.v != "" {
				h.Set("Retry-After", tt.v)
			}
			got, ok := retryAfterDuration(h)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestFetchAndVerifyReusesLockMatchingCacheWithoutNetwork proves a cached
// artifact whose sha256 already matches the lock is trusted without
// consulting the publisher's checksum document at all.
func TestFetchAndVerifyReusesLockMatchingCacheWithoutNetwork(t *testing.T) {
	payload := []byte("#!/bin/sh\necho kubeadm\n")
	digest := sha256.Sum256(payload)
	want := hex.EncodeToString(digest[:])

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(dest, payload, 0o644))
	_, err := bundle.WriteSHA256Sidecar(dest)
	require.NoError(t, err)

	got, err := fetchAndVerify(t.Context(), "kubeadm", srv.URL+"/kubeadm", dest, "kubeadm", "", want)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.EqualValues(t, 0, hits.Load(), "a cached artifact matching the lock must not touch the network")
}

// TestFetchAndVerifyFallsBackToUpstreamWhenLockDoesNotMatchCache proves a
// stale cache (sha256 no longer matching the lock) still falls back to the
// publisher's checksum document rather than trusting the lock digest blindly.
func TestFetchAndVerifyFallsBackToUpstreamWhenLockDoesNotMatchCache(t *testing.T) {
	cachedPayload := []byte("stale cached bytes")
	freshPayload := []byte("#!/bin/sh\necho kubeadm\n")
	freshDigest := sha256.Sum256(freshPayload)
	freshWant := hex.EncodeToString(freshDigest[:])

	var artifactHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s\n", freshWant)
			return
		}
		artifactHits++
		_, _ = w.Write(freshPayload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "kubeadm")
	require.NoError(t, os.WriteFile(dest, cachedPayload, 0o644))
	_, err := bundle.WriteSHA256Sidecar(dest)
	require.NoError(t, err)

	got, err := fetchAndVerify(t.Context(), "kubeadm", srv.URL+"/kubeadm", dest, "kubeadm", "", freshWant)
	require.NoError(t, err)
	assert.Equal(t, freshWant, got)
	assert.Equal(t, 1, artifactHits, "a lock digest that does not match the cache must fall back to a real download")
}
