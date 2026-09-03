// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// stageComponents extracts the containerd, CNI plugins, crictl and helm
// tarballs into an FHS-shaped tree under stageDir, and renders
// /etc/containerd/config.toml and /etc/crictl.yaml for it. This is the
// payload nfpm.yaml pulls in for everything dpkg has to own beyond the bare
// kubeadm/kubectl/kubelet/runc binaries it already installs straight from
// archDir, and the container image tarball packaged alongside it.
func stageComponents(archDir, stageDir string, manifest manifestDoc, lockImages []lockImage) error {
	if err := requireContainerdV2(manifest.ContainerdVersion); err != nil {
		return err
	}
	if err := os.RemoveAll(stageDir); err != nil {
		return fmt.Errorf("clean %s: %w", stageDir, err)
	}
	components := manifest.Architectures[targetArch]

	localBin := filepath.Join(stageDir, "usr", "local", "bin")
	cniBin := filepath.Join(stageDir, "opt", "cni", "bin")
	usrBin := filepath.Join(stageDir, "usr", "bin")
	etcContainerd := filepath.Join(stageDir, "etc", "containerd")
	for _, d := range []string{localBin, cniBin, usrBin, etcContainerd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	containerdTgz := filepath.Join(archDir, components["containerd"].File)
	if err := extractTarGzSelected(containerdTgz, map[string]string{
		"bin/containerd":              filepath.Join(localBin, "containerd"),
		"bin/containerd-shim-runc-v2": filepath.Join(localBin, "containerd-shim-runc-v2"),
		"bin/ctr":                     filepath.Join(localBin, "ctr"),
	}, 0o755); err != nil {
		return fmt.Errorf("stage containerd: %w", err)
	}

	cniTgz := filepath.Join(archDir, components["cni-plugins"].File)
	if err := extractTarGzAll(cniTgz, cniBin, 0o755); err != nil {
		return fmt.Errorf("stage cni-plugins: %w", err)
	}

	crictlTgz := filepath.Join(archDir, components["crictl"].File)
	if err := extractTarGzSelected(crictlTgz, map[string]string{
		"crictl": filepath.Join(usrBin, "crictl"),
	}, 0o755); err != nil {
		return fmt.Errorf("stage crictl: %w", err)
	}

	helmTgz := filepath.Join(archDir, components["helm"].File)
	if err := extractTarGzSelected(helmTgz, map[string]string{
		"linux-" + targetArch + "/helm": filepath.Join(usrBin, "helm"),
	}, 0o755); err != nil {
		return fmt.Errorf("stage helm: %w", err)
	}

	pauseRef, err := pauseImageRef(lockImages)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(etcContainerd, "config.toml"), []byte(renderContainerdConfig(pauseRef)), 0o644); err != nil {
		return fmt.Errorf("write containerd config.toml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "etc", "crictl.yaml"), []byte(crictlConfig), 0o644); err != nil {
		return fmt.Errorf("write crictl.yaml: %w", err)
	}
	return nil
}

// requireContainerdV2 rejects a containerd pin below v2.0.0. renderContainerdConfig
// always writes `version = 3`, which containerd's config loader only accepts
// from containerd 2.x -- 1.x (still pinned in pins.go for Kubernetes 1.29-1.31)
// reads `version = 2` and a different plugin-id shape entirely
// ([plugins."io.containerd.grpc.v1.cri"], not
// [plugins.'io.containerd.cri.v1.runtime']). Staging one of those rows today
// would silently ship a config.toml containerd refuses to parse.
func requireContainerdV2(containerdVersion string) error {
	v := strings.TrimPrefix(strings.TrimSpace(containerdVersion), "v")
	major := v
	if i := strings.Index(v, "."); i >= 0 {
		major = v[:i]
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return fmt.Errorf("cannot read a major version from containerd pin %q", containerdVersion)
	}
	if n < 2 {
		return fmt.Errorf("stage: containerd pin %q is below v2.0.0: config version 3 needs containerd 2.x; "+
			"add a v2 pin or a v2 config template", containerdVersion)
	}
	return nil
}

// pauseImageRef returns the pause (sandbox) image reference from the locked
// image list, so containerd's rendered configuration can never skew from the
// bundled control-plane images.
func pauseImageRef(images []lockImage) (string, error) {
	var matches []string
	for _, img := range images {
		if strings.Contains(img.Ref, "/pause:") {
			matches = append(matches, img.Ref)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no pause image in the locked image list; containerd has no sandbox image to pin")
	}
	sort.Strings(matches)
	return matches[0], nil
}

// crictlConfig is /etc/crictl.yaml, fixed for every version: the containerd
// CRI socket never changes between the Kubernetes releases this package bundles.
const crictlConfig = `runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
`

// renderContainerdConfig renders /etc/containerd/config.toml for containerd
// 2.x (config version 3), pinning the sandbox image to pauseRef and the runc
// handler to the systemd cgroup driver kubelet requires. Unset keys keep each
// plugin's own defaults, so only what has to differ from stock is written.
//
// config.toml and crictl.yaml are rewritten by every install, so they are not
// dpkg conffiles: the sandbox image has to track the bundled images on every
// matrix-row switch, which a preserved local edit would defeat. The supported
// override path is the `imports` line below: containerd's config loader
// treats `imports` as a list of additional files to merge over the main one
// (simple fields overwritten, array/map fields appended) and documents glob
// patterns in its own example
// (https://github.com/containerd/containerd/blob/v2.3.0/docs/man/containerd-config.toml.5.md,
// "imports": `imports = ["/etc/containerd/runtime_*.toml", "./debug.toml"]`),
// so an operator drops a fragment under /etc/containerd/conf.d/ instead of
// editing this file.
func renderContainerdConfig(pauseRef string) string {
	return "# Managed by nke-kubeadm-distro. Rendered at package build time from the\n" +
		"# locked image list; the sandbox image is the pause image this Kubernetes\n" +
		"# release asks for, so it can never skew from the bundled images. This file\n" +
		"# is rewritten on every install -- put local overrides in\n" +
		"# /etc/containerd/conf.d/ instead of editing it directly.\n" +
		"imports = ['/etc/containerd/conf.d/*.toml']\n" +
		"version = 3\n" +
		"\n" +
		"[plugins]\n" +
		"  [plugins.'io.containerd.cri.v1.images']\n" +
		"    [plugins.'io.containerd.cri.v1.images'.pinned_images]\n" +
		"      sandbox = '" + pauseRef + "'\n" +
		"  [plugins.'io.containerd.cri.v1.runtime']\n" +
		"    [plugins.'io.containerd.cri.v1.runtime'.cni]\n" +
		"      bin_dir = '/opt/cni/bin'\n" +
		"      conf_dir = '/etc/cni/net.d'\n" +
		"    [plugins.'io.containerd.cri.v1.runtime'.containerd]\n" +
		"      default_runtime_name = 'runc'\n" +
		"      [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes]\n" +
		"        [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]\n" +
		"          runtime_type = 'io.containerd.runc.v2'\n" +
		"          [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc.options]\n" +
		"            BinaryName = '/usr/sbin/runc'\n" +
		"            SystemdCgroup = true\n"
}

// extractTarGzSelected extracts specific members from a gzip-compressed tar
// archive to fixed destinations, chmod'd to mode. Every entry in want must be
// found, or the archive does not match what pins.go expects and the build
// should fail loudly rather than ship a package missing a binary.
func extractTarGzSelected(archivePath string, want map[string]string, mode os.FileMode) error {
	remaining := make(map[string]string, len(want))
	for k, v := range want {
		remaining[k] = v
	}
	err := walkTarGz(archivePath, func(name string, r io.Reader) error {
		dest, ok := remaining[name]
		if !ok {
			return nil
		}
		if err := writeExtractedFile(dest, r, mode); err != nil {
			return err
		}
		delete(remaining, name)
		return nil
	})
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("%s: archive is missing expected member(s) %s", archivePath, strings.Join(missing, ", "))
	}
	return nil
}

// extractTarGzAll extracts every regular file in a gzip-compressed tar
// archive into destDir, chmod'd to mode. Used for the CNI plugins tarball,
// whose members are a flat list of binaries with no subdirectory -- a member
// name containing a path separator does not match that shape and is rejected
// rather than silently flattened to its basename, which could otherwise
// let two differently-pathed members collide under one destination name.
func extractTarGzAll(archivePath, destDir string, mode os.FileMode) error {
	found := false
	err := walkTarGz(archivePath, func(name string, r io.Reader) error {
		if strings.ContainsRune(name, '/') {
			return fmt.Errorf("%s: member %q is not a flat top-level file", archivePath, name)
		}
		found = true
		return writeExtractedFile(filepath.Join(destDir, name), r, mode)
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s: no regular files found", archivePath)
	}
	return nil
}

// walkTarGz calls fn for every regular file in a gzip-compressed tar archive,
// with its cleaned, traversal-checked name. Non-regular members (directories,
// symlinks, hardlinks, ...) are deliberately ignored: every archive this
// package stages from is a flat set of binaries, so the only entries that
// matter are the regular files carrying their bytes.
func walkTarGz(archivePath string, fn func(name string, r io.Reader) error) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archivePath, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archivePath, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) {
			return fmt.Errorf("%s: unsafe tar entry %q", archivePath, hdr.Name)
		}
		if err := fn(name, tr); err != nil {
			return err
		}
	}
}

// writeExtractedFile writes r to dest, creating parent directories, with mode.
func writeExtractedFile(dest string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
