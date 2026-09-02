// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// linuxAmd64 is the platform every image in the bundle is pulled for.
var linuxAmd64 = v1.Platform{OS: "linux", Architecture: targetArch}

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

// digestRef turns a tag reference and a manifest digest into the reference the
// image is actually pulled by, so a moved tag cannot change what is fetched.
func digestRef(tagRef, digest string) (name.Digest, error) {
	ref, err := name.ParseReference(tagRef)
	if err != nil {
		return name.Digest{}, fmt.Errorf("parse image reference %q: %w", tagRef, err)
	}
	pinned, err := name.NewDigest(ref.Context().Name() + "@" + digest)
	if err != nil {
		return name.Digest{}, fmt.Errorf("pin %s to %s: %w", tagRef, digest, err)
	}
	return pinned, nil
}

// resolveImageDigests reports the linux/amd64 manifest digest behind each tag.
// This is what `make lock` records, and it is the only place a tag is trusted.
func resolveImageDigests(ctx context.Context, images []string) ([]lockImage, error) {
	resolved := make([]lockImage, 0, len(images))
	for _, img := range images {
		ref, err := name.ParseReference(img)
		if err != nil {
			return nil, fmt.Errorf("parse image reference %q: %w", img, err)
		}
		remoteImage, err := remote.Image(ref, remote.WithContext(ctx), remote.WithPlatform(linuxAmd64))
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Name(), err)
		}
		digest, err := remoteImage.Digest()
		if err != nil {
			return nil, fmt.Errorf("digest of %s: %w", ref.Name(), err)
		}
		slog.Info("resolved image", "image", img, "digest", digest.String())
		resolved = append(resolved, lockImage{Ref: img, Digest: digest.String()})
	}
	return resolved, nil
}

// writeBaseImagesTarball pulls every locked image by digest and writes them as
// one gzipped docker-archive tarball at dest, plus its .sha256 sidecar. Each
// image is stored under its tag, because that is the name kubelet looks up.
func writeBaseImagesTarball(ctx context.Context, images []lockImage, dest string) error {
	// Same cache rule as the downloaded artifacts: a tarball that still matches
	// its sidecar is reused, so a re-run does not pull every image again.
	if _, err := bundle.VerifyFileAgainstSidecar(dest, dest+".sha256"); err == nil {
		slog.Info("reusing verified base image tarball", "file", dest)
		return nil
	}

	refToImage := make(map[name.Reference]v1.Image, len(images))
	for _, img := range images {
		tag, err := name.ParseReference(img.Ref)
		if err != nil {
			return fmt.Errorf("parse image reference %q: %w", img.Ref, err)
		}
		pinned, err := digestRef(img.Ref, img.Digest)
		if err != nil {
			return err
		}
		slog.Info("pulling base image", "image", img.Ref, "digest", img.Digest)
		remoteImage, err := remote.Image(pinned, remote.WithContext(ctx), remote.WithPlatform(linuxAmd64))
		if err != nil {
			return fmt.Errorf("pull %s: %w", pinned, err)
		}
		refToImage[tag] = remoteImage
	}

	// Written beside the destination and renamed once complete, so a failed or
	// interrupted pull cannot leave a truncated archive under the name postinst
	// feeds to ctr.
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	tmp := f.Name()

	if err := writeImageArchive(refToImage, f); err != nil {
		return errors.Join(fmt.Errorf("write docker-archive %s: %w", tmp, err), removeIfPresent(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return errors.Join(fmt.Errorf("install %s: %w", dest, err), removeIfPresent(tmp))
	}
	if _, err := bundle.WriteSHA256Sidecar(dest); err != nil {
		return err
	}
	return nil
}

// writeImageArchive gzips a docker-archive of every image into f, and closes f.
func writeImageArchive(refToImage map[name.Reference]v1.Image, f *os.File) error {
	gz := gzip.NewWriter(f)
	if err := tarball.MultiRefWrite(refToImage, gz); err != nil {
		gz.Close()
		f.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return fmt.Errorf("finish the gzip stream: %w", err)
	}
	return f.Close()
}

// removeIfPresent deletes path, reporting anything other than its absence.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
