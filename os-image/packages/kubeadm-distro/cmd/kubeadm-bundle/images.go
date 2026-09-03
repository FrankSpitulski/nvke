// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// baseImagesTarballName is the file the .deb postinst feeds to
// `ctr -n k8s.io images import`; the name is part of that contract.
func baseImagesTarballName(k8sVersion string) string {
	return fmt.Sprintf("k8s-base-container-images_%s_linux_%s.tgz", strings.TrimPrefix(k8sVersion, "v"), targetArch)
}

// kubeadmImageList runs the downloaded kubeadm binary to list the control-plane
// images for its own Kubernetes version. kubeadm is a Linux binary, so this only
// works on a Linux build host.
func kubeadmImageList(kubeadmPath, k8sVersion string) ([]string, error) {
	out, err := exec.Command(kubeadmPath, "config", "images", "list", "--kubernetes-version", k8sVersion).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s config images list: %w: %s", kubeadmPath, err, strings.TrimSpace(string(out)))
	}
	images := parseImageList(string(out))
	if len(images) == 0 {
		return nil, fmt.Errorf("%s config images list returned no images", kubeadmPath)
	}
	return images, nil
}

// parseImageList extracts image references from `kubeadm config images list`
// output, dropping blank lines and any log lines kubeadm writes to the same stream.
func parseImageList(out string) []string {
	var images []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// kubeadm can emit warnings (for example about a missing config) on the
		// same stream; an image reference is a single field containing a tag or
		// digest and no whitespace.
		if len(strings.Fields(line)) != 1 {
			continue
		}
		if !strings.Contains(line, ":") && !strings.Contains(line, "@") {
			continue
		}
		images = append(images, line)
	}
	return images
}

// repoName strips the tag (if any) off an image reference, leaving the bare
// repository name a digest can be appended to. A reference already pinned by
// digest (containing "@") is returned unchanged up to the "@".
func repoName(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty image reference")
	}
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], nil
	}
	lastSlash := strings.LastIndex(ref, "/")
	rest := ref
	if lastSlash >= 0 {
		rest = ref[lastSlash+1:]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		if lastSlash >= 0 {
			return ref[:lastSlash+1+i], nil
		}
		return ref[:i], nil
	}
	return ref, nil
}

// digestRef turns a tag reference and a manifest digest into the reference the
// image is actually pulled by, so a moved tag cannot change what is fetched.
func digestRef(tagRef, digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("pin %s to %s: not a sha256 digest", tagRef, digest)
	}
	repo, err := repoName(tagRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", tagRef, err)
	}
	return repo + "@" + digest, nil
}

// runCrane runs the crane binary and returns its stdout. Any non-zero exit
// carries the combined stderr in the returned error.
func runCrane(ctx context.Context, craneBin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, craneBin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", craneBin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// craneDigest resolves ref to its linux/<targetArch> manifest digest with
// `crane digest`. This is what `make lock` records, and what a rebuild uses to
// confirm a pulled image still matches what the lock pinned.
func craneDigest(ctx context.Context, craneBin, ref string) (string, error) {
	out, err := runCrane(ctx, craneBin, "digest", "--platform", "linux/"+targetArch, ref)
	if err != nil {
		return "", fmt.Errorf("crane digest %s: %w", ref, err)
	}
	digest := strings.TrimSpace(out)
	if digest == "" {
		return "", fmt.Errorf("crane digest %s returned no digest", ref)
	}
	return digest, nil
}

// resolveImageDigests reports the linux/<targetArch> manifest digest behind
// each tag. This is what `make lock` records, and it is the only place a tag
// is trusted.
func resolveImageDigests(ctx context.Context, craneBin string, images []string) ([]lockImage, error) {
	resolved := make([]lockImage, 0, len(images))
	for _, img := range images {
		digest, err := craneDigest(ctx, craneBin, img)
		if err != nil {
			return nil, err
		}
		slog.Info("resolved image", "image", img, "digest", digest)
		resolved = append(resolved, lockImage{Ref: img, Digest: digest})
	}
	return resolved, nil
}

// imageDigestsFingerprint hashes the sorted set of locked image references and
// digests, so the base image tarball's cache can be invalidated the moment the
// lock changes at the same Kubernetes version, even though the tarball's own
// name does not change.
func imageDigestsFingerprint(images []lockImage) string {
	sorted := append([]lockImage(nil), images...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })
	h := sha256.New()
	for _, img := range sorted {
		fmt.Fprintf(h, "%s@%s\n", img.Ref, img.Digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeBaseImagesTarball pulls every locked image with crane and merges the
// per-image tarballs into one gzipped docker-archive tarball at dest, plus its
// .sha256 sidecar. Each image is stored under its tag, because that is the
// name kubelet looks up.
func writeBaseImagesTarball(ctx context.Context, craneBin string, images []lockImage, dest string) error {
	fingerprint := imageDigestsFingerprint(images)
	sidecarPath := dest + ".sha256"
	fingerprintPath := dest + ".lock-digests"

	// Same cache rule as the downloaded artifacts: a tarball that still matches
	// its sidecar, and whose images are still the ones the lock pins, is
	// reused, so a re-run does not pull every image again. Keying reuse on the
	// fingerprint (rather than dest's mere presence) means a lock change at the
	// same Kubernetes version invalidates the cache even though dest's name
	// does not change.
	reusable := false
	if _, err := bundle.VerifyFileAgainstSidecar(dest, sidecarPath); err == nil {
		if cached, err := os.ReadFile(fingerprintPath); err == nil && strings.TrimSpace(string(cached)) == fingerprint {
			reusable = true
		}
	}
	if reusable {
		slog.Info("reusing verified base image tarball", "file", dest)
		return nil
	}
	// Either nothing was cached, or the lock has moved past what's on disk.
	// Clear it now: if the re-pull below fails partway through, a stale but
	// internally consistent tarball/sidecar/fingerprint set must not survive
	// for a later run to trust — in particular a `-skip-images` run, which
	// only checks that these files exist.
	if err := errors.Join(removeIfPresent(dest), removeIfPresent(sidecarPath), removeIfPresent(fingerprintPath)); err != nil {
		return err
	}

	work, err := os.MkdirTemp(filepath.Dir(dest), filepath.Base(dest)+".work-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	blobDir := filepath.Join(work, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return err
	}

	blobSHA := make(map[string]string, len(images)*2)
	manifest := make([]dockerArchiveManifestEntry, 0, len(images))
	for i, img := range images {
		entry, err := pullAndMergeImage(ctx, craneBin, work, blobDir, blobSHA, i, img)
		if err != nil {
			return err
		}
		manifest = append(manifest, entry)
	}

	// Written beside the destination and renamed once complete, so a failed or
	// interrupted pull cannot leave a truncated archive under the name postinst
	// feeds to ctr.
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	tmp := f.Name()

	writeErr := writeMergedArchive(f, blobDir, manifest)
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return errors.Join(fmt.Errorf("write docker-archive %s: %w", tmp, writeErr), removeIfPresent(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return errors.Join(fmt.Errorf("install %s: %w", dest, err), removeIfPresent(tmp))
	}
	if _, err := bundle.WriteSHA256Sidecar(dest); err != nil {
		return err
	}
	if err := os.WriteFile(fingerprintPath, []byte(fingerprint+"\n"), 0o644); err != nil {
		return fmt.Errorf("write lock-digests sidecar %s: %w", fingerprintPath, err)
	}
	return nil
}

// pullAndMergeImage pulls one locked image with crane and folds its blobs
// into the shared blob store.
func pullAndMergeImage(ctx context.Context, craneBin, work, blobDir string, blobSHA map[string]string, index int, img lockImage) (dockerArchiveManifestEntry, error) {
	slog.Info("pulling base image", "image", img.Ref, "digest", img.Digest)
	pinnedRef, err := digestRef(img.Ref, img.Digest)
	if err != nil {
		return dockerArchiveManifestEntry{}, err
	}

	pullDest := filepath.Join(work, fmt.Sprintf("image-%d.tar", index))
	if _, err := runCrane(ctx, craneBin, "pull", "--platform", "linux/"+targetArch, "--format", "tarball", pinnedRef, pullDest); err != nil {
		return dockerArchiveManifestEntry{}, fmt.Errorf("pull %s: %w", pinnedRef, err)
	}

	// crane already validated the manifest it fetched against the digest in
	// pinnedRef before writing this tarball, so re-resolving that same digest
	// here would only ask the registry to confirm what it just said; crane
	// itself already verifies every layer's bytes against its digest as they
	// come off the wire (go-containerregistry's verify.ReadCloser). What that
	// does not cover is anything that happens after crane wrote pullDest: disk
	// corruption between crane's write and our read, or a `-crane` binary that
	// is not really crane. mergeImageTarball (through internBlob) re-hashes
	// every blob from what is actually on disk and checks it against the name
	// crane wrote it under, closing that gap.
	entry, err := mergeImageTarball(pullDest, blobDir, blobSHA, img.Ref)
	if err != nil {
		return dockerArchiveManifestEntry{}, err
	}
	if err := os.Remove(pullDest); err != nil {
		return dockerArchiveManifestEntry{}, err
	}
	return entry, nil
}

// dockerArchiveManifestEntry is one entry of a docker-archive manifest.json:
// the shape `docker save`, crane's tarball writer, and `ctr images import`
// all agree on. LayerSources is deliberately omitted: crane's writer only
// populates it for foreign (non-registry) layers, and no registry.k8s.io
// image has one.
type dockerArchiveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// mergeImageTarball extracts the docker-archive tarball crane pulled at
// tarPath, interns its config and layer blobs into blobDir keyed by their own
// sha256 (so a blob shared with another image is stored once), and returns the
// manifest entry to carry into the merged tarball, with RepoTags overridden to
// tag: the tarball crane wrote it under was pulled by digest, not by tag.
func mergeImageTarball(tarPath, blobDir string, blobSHA map[string]string, tag string) (dockerArchiveManifestEntry, error) {
	extractDir, err := os.MkdirTemp(filepath.Dir(blobDir), "extract-*")
	if err != nil {
		return dockerArchiveManifestEntry{}, err
	}
	defer os.RemoveAll(extractDir)

	if err := extractTar(tarPath, extractDir); err != nil {
		return dockerArchiveManifestEntry{}, fmt.Errorf("extract %s: %w", tarPath, err)
	}

	raw, err := os.ReadFile(filepath.Join(extractDir, "manifest.json"))
	if err != nil {
		return dockerArchiveManifestEntry{}, fmt.Errorf("read manifest.json from %s: %w", tarPath, err)
	}
	var entries []dockerArchiveManifestEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return dockerArchiveManifestEntry{}, fmt.Errorf("parse manifest.json from %s: %w", tarPath, err)
	}
	if len(entries) != 1 {
		return dockerArchiveManifestEntry{}, fmt.Errorf("%s: expected exactly one image in the pulled tarball, got %d", tarPath, len(entries))
	}
	entry := entries[0]

	newConfig, err := internBlob(extractDir, blobDir, blobSHA, entry.Config)
	if err != nil {
		return dockerArchiveManifestEntry{}, err
	}
	newLayers := make([]string, 0, len(entry.Layers))
	for _, layer := range entry.Layers {
		newLayer, err := internBlob(extractDir, blobDir, blobSHA, layer)
		if err != nil {
			return dockerArchiveManifestEntry{}, err
		}
		newLayers = append(newLayers, newLayer)
	}

	return dockerArchiveManifestEntry{
		Config:   newConfig,
		RepoTags: []string{tag},
		Layers:   newLayers,
	}, nil
}

// internBlob hashes the blob at extractDir/name and copies it into blobDir
// under a name keyed by its own sha256, unless a blob with that content is
// already there. It returns the path the merged manifest.json should reference.
//
// crane's tarball writer names every blob by its own content digest: the
// image config as "sha256:<hex>", each layer as "<hex><ext>". crane already
// verifies each layer's bytes against its digest as they come off the wire
// (go-containerregistry's verify.ReadCloser); what this check adds is
// catching corruption between crane's write of pullDest and this read of it,
// or a `-crane` binary that is not really crane — neither of which crane's
// own wire-level verification can see.
func internBlob(extractDir, blobDir string, blobSHA map[string]string, name string) (string, error) {
	clean := filepath.Clean(name)
	if clean != name || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("unsafe blob path %q in manifest.json", name)
	}
	src := filepath.Join(extractDir, clean)
	digest, err := bundle.FileSHA256(src)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", name, err)
	}
	if want, ok := expectedBlobDigest(name); ok && !strings.EqualFold(want, digest) {
		return "", fmt.Errorf("blob %q hashes to %s, want %s: the pulled tarball does not match its own manifest", name, digest, want)
	}
	if blobName, ok := blobSHA[digest]; ok {
		return "blobs/" + blobName, nil
	}
	blobName := digest + blobExt(name)
	if err := copyFile(src, filepath.Join(blobDir, blobName)); err != nil {
		return "", err
	}
	blobSHA[digest] = blobName
	return "blobs/" + blobName, nil
}

// expectedBlobDigest extracts the hex sha256 crane's tarball writer encodes in
// a blob's own filename ("sha256:<hex>" for the image config, "<hex><ext>"
// for a layer), or reports ok=false when name is not shaped that way.
func expectedBlobDigest(name string) (digest string, ok bool) {
	if hex, found := strings.CutPrefix(name, "sha256:"); found {
		return hex, isSHA256Hex(hex)
	}
	base := filepath.Base(name)
	hex := base
	if i := strings.Index(base, "."); i >= 0 {
		hex = base[:i]
	}
	return hex, isSHA256Hex(hex)
}

// blobExt returns the extension of a blob's original filename (".tar.gz",
// ".json", ...), so a renamed blob still carries a recognisable suffix.
func blobExt(name string) string {
	base := filepath.Base(name)
	if i := strings.Index(base, "."); i >= 0 {
		return base[i:]
	}
	return ""
}

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// extractTar writes every regular file in the tar at tarPath under destDir,
// rejecting any entry that would escape it.
func extractTar(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe tar entry %q", hdr.Name)
		}
		out := filepath.Join(destDir, name)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
}

// writeMergedArchive writes the merged docker-archive to w: every blob in
// blobDir under "blobs/<name>", sorted and streamed from disk, followed by
// manifest.json, as a deterministic USTAR tar (mtime 0, uid/gid 0) gzipped
// with no timestamp header — the same bytes for the same inputs, run to run.
// It asserts no tar member is written twice before starting. The caller owns
// w and closes it.
//
// USTAR caps a single member at 8 GiB, and archive/tar's WriteHeader returns
// an error rather than silently upgrading to PAX for a header that does not
// fit the format requested. That is fine here: no registry.k8s.io image layer
// comes close to 8 GiB, so a member that size would itself be a sign
// something has gone wrong upstream, not a legitimate bundle to widen the
// format for.
func writeMergedArchive(w io.Writer, blobDir string, manifest []dockerArchiveManifestEntry) error {
	sort.Slice(manifest, func(i, j int) bool {
		return strings.Join(manifest[i].RepoTags, ",") < strings.Join(manifest[j].RepoTags, ",")
	})
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(blobDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	const manifestName = "manifest.json"
	members := make([]string, 0, len(names)+1)
	seen := make(map[string]struct{}, len(names)+1)
	for _, n := range names {
		name := "blobs/" + n
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate tar member %s", name)
		}
		seen[name] = struct{}{}
		members = append(members, name)
	}
	if _, dup := seen[manifestName]; dup {
		return fmt.Errorf("duplicate tar member %s", manifestName)
	}
	members = append(members, manifestName)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, name := range members {
		var writeErr error
		if name == manifestName {
			writeErr = writeTarMember(tw, name, int64(len(manifestJSON)), bytes.NewReader(manifestJSON))
		} else {
			writeErr = writeTarMemberFromFile(tw, name, filepath.Join(blobDir, strings.TrimPrefix(name, "blobs/")))
		}
		if writeErr != nil {
			tw.Close()
			gz.Close()
			return writeErr
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return fmt.Errorf("finish the tar stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish the gzip stream: %w", err)
	}
	return nil
}

// writeTarMemberFromFile streams path into tw under name, sized from the
// file's own stat rather than a prior read, so a blob's bytes never pass
// through an intermediate in-memory copy.
func writeTarMemberFromFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return writeTarMember(tw, name, info.Size(), f)
}

// writeTarMember writes one deterministic, USTAR-format tar entry named name,
// streaming size bytes from r.
func writeTarMember(tw *tar.Writer, name string, size int64, r io.Reader) error {
	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     size,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.Copy(tw, r)
	return err
}

// removeIfPresent deletes path, reporting anything other than its absence.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
