// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// kubeadm-bundle downloads the upstream Kubernetes node stack for one
// linux/<arch>, verifies every artifact against its publisher's checksum,
// pulls the matching control-plane container images, and writes the
// per-version bundle tree that packaging/build-deb.sh turns into an
// nke-kubeadm-distro-<x.y.z> Debian package.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// targetArch is the architecture this run of the bundler produces, set once in
// run() from the -arch flag before any goroutine reads it. NKE nodes are
// amd64. The flag and the dist path/tarball name/crane --platform plumbing
// already carry an arbitrary arch; only the lock file schema is amd64-only
// (one lock per Kubernetes version, not per arch), so run() rejects anything
// else until the lock is split per architecture.
var targetArch = "amd64"

// supportedArches are the architectures -arch accepts. Only amd64 is enabled
// until the lock file is split per architecture; see run().
var supportedArches = map[string]struct{}{"amd64": {}}

// manifestDoc is the per-version manifest.json the node-side installer reads.
type manifestDoc struct {
	Version           string                                  `json:"version"`
	HelmVersion       string                                  `json:"helm_version"`
	ContainerdVersion string                                  `json:"containerd_version"`
	RuncVersion       string                                  `json:"runc_version"`
	CriToolsVersion   string                                  `json:"cri_tools_version"`
	Architectures     map[string]map[string]manifestComponent `json:"architectures"`
	// Images records what went into the container image tarball, by digest, so
	// the bundle on disk says which images it carries.
	Images []lockImage `json:"images,omitempty"`
}

// manifestComponent names one artifact file and its verified digest.
type manifestComponent struct {
	File   string `json:"file"`
	Sha256 string `json:"sha256"`
}

func main() {
	versionsFlag := flag.String("versions", "", "Kubernetes versions to bundle, comma- or space-separated (for example \"v1.36.3 v1.35.3\").")
	distFlag := flag.String("dist", "dist", "Bundle root directory; each version lands in <dist>/<version>/<arch>/.")
	keyringFlag := flag.String("runc-keyring", "", "Path to the runc release keyring (default: packaging/keyrings/runc-release.gpg next to this package).")
	lockDirFlag := flag.String("lock", "lock", "Directory holding the committed lock files, one <version>.json per Kubernetes version.")
	writeLockFlag := flag.Bool("write-lock", false, "Regenerate the lock files from upstream instead of building a bundle. Review the diff and commit it.")
	skipImages := flag.Bool("skip-images", false, "Skip the kubeadm image list and container image tarball. Development only: the .deb then has no images to import.")
	workers := flag.Int("download-workers", 8, "Maximum concurrent downloads per Kubernetes version.")
	archFlag := flag.String("arch", "amd64", "Target architecture for the bundle. Only amd64 is supported today; the lock file is not yet per-architecture.")
	craneFlag := flag.String("crane", "crane", "Path to the crane binary (from go-containerregistry), used to resolve and pull container images.")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	opts := runOptions{
		versions:   *versionsFlag,
		dist:       *distFlag,
		lockDir:    *lockDirFlag,
		keyring:    *keyringFlag,
		writeLock:  *writeLockFlag,
		skipImages: *skipImages,
		workers:    *workers,
		arch:       *archFlag,
		crane:      *craneFlag,
	}
	if err := run(opts); err != nil {
		slog.Error("kubeadm-bundle failed", "err", err)
		os.Exit(1)
	}
}

// runOptions is the command line, resolved.
type runOptions struct {
	versions   string
	dist       string
	lockDir    string
	keyring    string
	writeLock  bool
	skipImages bool
	workers    int
	arch       string
	crane      string
}

func run(opts runOptions) error {
	versions, err := parseVersions(opts.versions)
	if err != nil {
		return err
	}
	if opts.workers < 1 {
		return fmt.Errorf("-download-workers must be at least 1, got %d", opts.workers)
	}
	if opts.writeLock && opts.skipImages {
		return fmt.Errorf("-write-lock and -skip-images are contradictory: a lock records the images")
	}
	if _, ok := supportedArches[opts.arch]; !ok {
		return fmt.Errorf("-arch %q: the lock and the .deb are amd64-only for now; another arch needs a per-arch lock", opts.arch)
	}
	// Read by every goroutine fetchAllArtifacts starts below, so it must be set
	// before any of them can run.
	targetArch = opts.arch
	needsKubeadm := !opts.skipImages
	if needsKubeadm && runtime.GOOS != "linux" {
		return fmt.Errorf("the container images are listed by the downloaded linux kubeadm binary, "+
			"so this build host (%s) cannot produce a complete bundle; run on Linux or pass -skip-images", runtime.GOOS)
	}
	if needsKubeadm {
		// Fail before a ~1 GiB run of downloads rather than partway through the
		// first image pull.
		if _, err := exec.LookPath(opts.crane); err != nil {
			return fmt.Errorf("crane not found at %q: %w", opts.crane, err)
		}
	}
	keyring := opts.keyring
	if keyring == "" {
		keyring, err = defaultRuncKeyring()
		if err != nil {
			return err
		}
	}
	if st, err := os.Stat(keyring); err != nil || st.IsDir() {
		return fmt.Errorf("runc release keyring missing at %s (required for the PGP-signed runc checksums)", keyring)
	}

	dist, err := filepath.Abs(opts.dist)
	if err != nil {
		return err
	}
	lockDir, err := filepath.Abs(opts.lockDir)
	if err != nil {
		return err
	}

	ctx := context.Background()
	for _, v := range versions {
		build := buildVersion
		if opts.writeLock {
			build = writeLockForVersion
		}
		if err := build(ctx, versionBuild{
			dist:       dist,
			lockDir:    lockDir,
			k8sVersion: v,
			keyring:    keyring,
			skipImages: opts.skipImages,
			workers:    opts.workers,
			crane:      opts.crane,
		}); err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
	}
	return nil
}

// versionBuild is one Kubernetes version's worth of work.
type versionBuild struct {
	dist       string
	lockDir    string
	k8sVersion string
	keyring    string
	skipImages bool
	workers    int
	crane      string
}

// defaultRuncKeyring returns packaging/keyrings/runc-release.gpg relative to
// the working directory, which is this package root under `make debs` and
// `make lock` (both of which pass -runc-keyring explicitly anyway). This is
// only the fallback for running the binary directly from this source tree.
func defaultRuncKeyring() (string, error) {
	candidate := filepath.Join("packaging", "keyrings", "runc-release.gpg")
	if st, err := os.Stat(candidate); err != nil || st.IsDir() {
		return "", fmt.Errorf("no runc keyring at %s; run from packages/kubeadm-distro or pass -runc-keyring", candidate)
	}
	return candidate, nil
}

// buildVersion downloads, verifies and records every artifact for one Kubernetes
// release, then writes manifest.json and its digest sidecar. Everything it
// fetches is checked against the committed lock as well as against the
// publisher's checksum: the lock is what a human reviewed, the checksum only
// says the CDN agreed with itself.
func buildVersion(ctx context.Context, b versionBuild) error {
	lock, err := readLock(b.lockDir, b.k8sVersion)
	if err != nil {
		return err
	}
	p, err := pinsFor(b.k8sVersion)
	if err != nil {
		return err
	}
	slog.Info("bundling Kubernetes release",
		"version", b.k8sVersion, "containerd", p.Containerd, "runc", p.Runc, "helm", p.Helm, "cri-tools", p.CriTools)

	manifest, archDir, err := fetchAllArtifacts(ctx, b, p, &lock)
	if err != nil {
		return err
	}

	if !b.skipImages {
		// Safe to execute now, and only now: kubeadm's bytes matched the lock.
		kubeadmPath := filepath.Join(archDir, manifest.Architectures[targetArch]["kubeadm"].File)
		images, err := kubeadmImageList(kubeadmPath, b.k8sVersion)
		if err != nil {
			return err
		}
		if err := checkImageListAgainstLock(lock, images); err != nil {
			return err
		}
		tarballPath := filepath.Join(archDir, baseImagesTarballName(b.k8sVersion))
		if err := writeBaseImagesTarball(ctx, b.crane, lock.Images, tarballPath); err != nil {
			return err
		}
		manifest.Images = lock.Images
		slog.Info("wrote base container images", "file", tarballPath, "images", len(lock.Images))
	}

	return writeManifest(filepath.Join(b.dist, b.k8sVersion, "manifest.json"), manifest)
}

// writeLockForVersion regenerates one lock file from upstream: the same download
// and checksum verification as a build, then kubeadm's image list resolved to
// manifest digests. It writes no bundle; `make debs` does that from the lock.
func writeLockForVersion(ctx context.Context, b versionBuild) error {
	p, err := pinsFor(b.k8sVersion)
	if err != nil {
		return err
	}
	slog.Info("locking Kubernetes release",
		"version", b.k8sVersion, "containerd", p.Containerd, "runc", p.Runc, "helm", p.Helm, "cri-tools", p.CriTools)

	manifest, archDir, err := fetchAllArtifacts(ctx, b, p, nil)
	if err != nil {
		return err
	}

	updated := lockFile{Kubernetes: b.k8sVersion, Artifacts: map[string]lockArtifact{}}
	urls := componentURLs(b.k8sVersion, p)
	for component, info := range manifest.Architectures[targetArch] {
		updated.Artifacts[component] = lockArtifact{URL: urls[component], Sha256: info.Sha256}
	}

	kubeadmPath := filepath.Join(archDir, manifest.Architectures[targetArch]["kubeadm"].File)
	images, err := kubeadmImageList(kubeadmPath, b.k8sVersion)
	if err != nil {
		return err
	}
	if updated.Images, err = resolveImageDigests(ctx, b.crane, images); err != nil {
		return err
	}

	// The previous lock is only read to describe the change; a missing one is
	// simply a new version.
	previous, err := readLock(b.lockDir, b.k8sVersion)
	if err != nil {
		previous = lockFile{}
	}
	changes := summarizeLockChange(previous, updated)
	if err := writeLock(b.lockDir, updated); err != nil {
		return err
	}
	path := lockPath(b.lockDir, b.k8sVersion)
	if len(changes) == 0 {
		slog.Info("lock unchanged", "file", path)
		return nil
	}
	slog.Info("lock updated; review the diff before committing", "file", path)
	for _, line := range changes {
		fmt.Fprintf(os.Stderr, "  %s\n", line)
	}
	return nil
}

// fetchAllArtifacts downloads and verifies every component for one release and
// returns the manifest it built plus the directory the files landed in. When
// lock is non-nil every artifact must also match it.
func fetchAllArtifacts(ctx context.Context, b versionBuild, p pins, lock *lockFile) (manifestDoc, string, error) {
	archDir := filepath.Join(b.dist, b.k8sVersion, targetArch)
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		return manifestDoc{}, "", err
	}

	manifest := manifestDoc{
		Version:           b.k8sVersion,
		HelmVersion:       p.Helm,
		ContainerdVersion: p.Containerd,
		RuncVersion:       p.Runc,
		CriToolsVersion:   p.CriTools,
		Architectures:     map[string]map[string]manifestComponent{targetArch: {}},
	}

	urls := componentURLs(b.k8sVersion, p)
	components := make([]string, 0, len(urls))
	for component := range urls {
		components = append(components, component)
	}
	sort.Strings(components)

	var mu sync.Mutex
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.workers)
	for _, component := range components {
		url := urls[component]
		g.Go(func() error {
			fname := filepath.Base(url)
			if err := bundle.ValidateArtifactBasename(fname); err != nil {
				return fmt.Errorf("%s: %w", component, err)
			}
			dest := filepath.Join(archDir, fname)
			var lockDigest string
			if lock != nil {
				lockDigest = lock.Artifacts[component].Sha256
			}
			digest, err := fetchAndVerify(ctx, component, url, dest, fname, b.keyring, lockDigest)
			if err != nil {
				return fmt.Errorf("%s: %w", component, err)
			}
			if lock != nil {
				if err := checkArtifactAgainstLock(*lock, component, url, digest); err != nil {
					return err
				}
			}
			mu.Lock()
			defer mu.Unlock()
			manifest.Architectures[targetArch][component] = manifestComponent{File: fname, Sha256: digest}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return manifestDoc{}, "", err
	}
	return manifest, archDir, nil
}

// fetchAndVerify downloads url unless dest already verifies, and returns the
// verified digest. A cached file that fails verification is re-downloaded
// once, which is what a pin bump looks like.
//
// When lockDigest is non-empty and the cached file's own sha256 already
// matches it, the publisher's checksum document is not fetched at all: the
// lock is what a human reviewed, so a cache hit against it needs no second
// opinion from the CDN. The publisher document is still consulted at
// `-write-lock` time (lockDigest is always empty there) and whenever the
// cache misses or drifts from the lock.
func fetchAndVerify(ctx context.Context, component, url, dest, fname, keyring, lockDigest string) (string, error) {
	sidecar := dest + ".sha256"
	if _, err := os.Stat(dest); err == nil {
		if lockDigest != "" {
			if digest, err := bundle.VerifyFileAgainstSidecar(dest, sidecar); err == nil && strings.EqualFold(digest, lockDigest) {
				slog.Info("reusing artifact that matches the lock", "component", component, "file", fname)
				return finishArtifact(component, dest, digest)
			}
		}
		checksumURL := upstreamChecksumURL(component, url)
		if digest, err := verifyUpstreamSHA256(ctx, dest, checksumURL, fname, component, keyring); err == nil {
			slog.Info("reusing verified artifact", "component", component, "file", fname)
			return finishArtifact(component, dest, digest)
		}
		slog.Warn("cached artifact failed upstream checksum; re-downloading", "component", component, "file", fname)
		// Drop the cached bytes and their sidecar now. If the re-download fails,
		// what is left behind must not look like a verified artifact to the next
		// run, or to packaging.
		if err := errors.Join(removeIfPresent(dest), removeIfPresent(sidecar)); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	slog.Info("downloading", "component", component, "url", url)
	tmp, err := downloadToTemp(ctx, url, dest)
	if err != nil {
		return "", err
	}
	digest, err := verifyUpstreamSHA256(ctx, tmp, upstreamChecksumURL(component, url), fname, component, keyring)
	if err != nil {
		// An unverified download must not survive under any name.
		return "", errors.Join(err, removeIfPresent(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", errors.Join(fmt.Errorf("install %s: %w", dest, err), removeIfPresent(tmp))
	}
	return finishArtifact(component, dest, digest)
}

// finishArtifact makes bare binaries executable and records the publisher's
// digest beside the artifact, now that the artifact is in its final place.
func finishArtifact(component, dest, digest string) (string, error) {
	if err := ensureExecutableComponent(component, dest); err != nil {
		return "", err
	}
	sidecar := dest + ".sha256"
	if err := os.WriteFile(sidecar, []byte(digest+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write checksum sidecar %s: %w", sidecar, err)
	}
	return digest, nil
}

// writeManifest writes manifest.json and its manifest.json.sha256 sidecar.
func writeManifest(path string, manifest manifestDoc) error {
	out, err := json.MarshalIndent(manifest, "", "    ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := bundle.WriteSHA256Sidecar(path); err != nil {
		return err
	}
	return nil
}
