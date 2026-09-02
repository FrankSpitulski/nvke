// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/bundle"
)

// httpClient is generous: the containerd and kubelet artifacts are large.
var httpClient = &http.Client{Timeout: 2 * time.Hour}

// componentURLs returns the linux/amd64 download URL of every matrix component
// for one Kubernetes release.
func componentURLs(k8sVersion string, p pins) map[string]string {
	containerdBare := strings.TrimPrefix(p.Containerd, "v")
	return map[string]string{
		"kubelet":     fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubelet", k8sVersion, targetArch),
		"kubeadm":     fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubeadm", k8sVersion, targetArch),
		"kubectl":     fmt.Sprintf("https://dl.k8s.io/release/%s/bin/linux/%s/kubectl", k8sVersion, targetArch),
		"helm":        fmt.Sprintf("https://get.helm.sh/helm-%s-linux-%s.tar.gz", p.Helm, targetArch),
		"runc":        fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/%s/runc.%s", p.Runc, targetArch),
		"containerd":  fmt.Sprintf("https://github.com/containerd/containerd/releases/download/%s/containerd-%s-linux-%s.tar.gz", p.Containerd, containerdBare, targetArch),
		"cni-plugins": fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz", cniPluginsVersion, targetArch, cniPluginsVersion),
		"crictl":      fmt.Sprintf("https://github.com/kubernetes-sigs/cri-tools/releases/download/%s/crictl-%s-linux-%s.tar.gz", p.CriTools, p.CriTools, targetArch),
	}
}

// upstreamChecksumURL returns the publisher's checksum document for a component
// artifact, or "" when the component has none.
func upstreamChecksumURL(component, artifactURL string) string {
	switch component {
	case "kubelet", "kubeadm", "kubectl", "helm", "cni-plugins", "crictl":
		return artifactURL + ".sha256"
	case "containerd":
		return artifactURL + ".sha256sum"
	case "runc":
		i := strings.LastIndex(artifactURL, "/")
		if i < 0 {
			return ""
		}
		return artifactURL[:i] + "/runc.sha256sum"
	default:
		return ""
	}
}

// maxArtifactBytes bounds a single download. The largest artifact in the bundle
// is the containerd tarball at a few hundred MiB, so 2 GiB leaves room to grow
// while still refusing to fill the build host's disk with an unexpected body.
const maxArtifactBytes = 2 << 30

// downloadToTemp writes the body of url to a temp file beside dest and returns
// its path. The caller renames it into place once the checksum verifies, so a
// truncated or unverified download never appears under the artifact's name.
// The request is bound to ctx, so one component failing cancels its siblings
// rather than leaving them to finish a download nobody will use.
func downloadToTemp(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	out, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()

	// One byte past the ceiling: reading it means the body was over the limit.
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err == nil && n > maxArtifactBytes {
		err = fmt.Errorf("body of %s exceeds the %d byte limit for a single artifact", url, int64(maxArtifactBytes))
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			return "", fmt.Errorf("%w (and removing %s: %w)", err, tmp, rmErr)
		}
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return tmp, nil
}

// fetchChecksumText downloads a checksum document (bare sha256, sha256sum list,
// or an OpenPGP clearsigned list).
func fetchChecksumText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	return string(body), nil
}

// isSHA256Hex reports whether s is exactly 64 hexadecimal characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// parseExpectedSHA256FromChecksumDoc extracts the published SHA-256 for
// artifactBasename (kubeadm, runc.amd64, containerd-2.1.7-linux-amd64.tar.gz, …)
// from the common upstream checksum-document shapes.
func parseExpectedSHA256FromChecksumDoc(body, artifactBasename string) (string, error) {
	body = bundle.ExtractPGPSignedClearText(strings.TrimSpace(body))
	var loneHex []string
	var matched string
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !isSHA256Hex(fields[0]) {
			continue
		}
		if len(fields) == 1 {
			loneHex = append(loneHex, strings.ToLower(fields[0]))
			continue
		}
		name := fields[1]
		if name == artifactBasename || filepath.Base(name) == artifactBasename {
			h := strings.ToLower(fields[0])
			if matched != "" && matched != h {
				return "", fmt.Errorf("ambiguous checksum entries for %q", artifactBasename)
			}
			matched = h
		}
	}
	if matched != "" {
		return matched, nil
	}
	switch len(loneHex) {
	case 1:
		return loneHex[0], nil
	case 0:
		return "", fmt.Errorf("no SHA-256 digest found for %q in checksum document", artifactBasename)
	default:
		return "", fmt.Errorf("checksum file has multiple bare SHA-256 lines; expected a line naming %q", artifactBasename)
	}
}

// verifyUpstreamSHA256 checks localPath against the publisher's checksum document
// and returns its verified digest. It writes nothing: the caller decides whether
// the file has earned its place under the artifact's name. runc's document must
// be OpenPGP clearsigned, and is verified against the bundled keyring before it
// is parsed.
func verifyUpstreamSHA256(ctx context.Context, localPath, checksumURL, artifactBasename, component, runcKeyring string) (string, error) {
	if checksumURL == "" {
		return "", fmt.Errorf("no upstream checksum URL for %q", artifactBasename)
	}
	doc, err := fetchChecksumText(ctx, checksumURL)
	if err != nil {
		return "", fmt.Errorf("fetch checksum %s: %w", checksumURL, err)
	}
	// runc signs its checksum list, so an unsigned document is a downgrade
	// rather than a shape the bundler should quietly accept.
	if component == "runc" {
		if !bundle.IsPGPSignedMessage(doc) {
			return "", fmt.Errorf("runc checksum document %s is not OpenPGP clearsigned; refusing to trust it", checksumURL)
		}
		if err := bundle.VerifyClearsignedChecksumDoc(doc, runcKeyring); err != nil {
			return "", fmt.Errorf("PGP verify checksum %s: %w", checksumURL, err)
		}
	} else if bundle.IsPGPSignedMessage(doc) {
		return "", fmt.Errorf("unexpected PGP-signed checksum for component %q", component)
	}
	want, err := parseExpectedSHA256FromChecksumDoc(doc, artifactBasename)
	if err != nil {
		return "", fmt.Errorf("parse checksum from %s: %w", checksumURL, err)
	}
	got, err := bundle.FileSHA256(localPath)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("upstream checksum mismatch for %s (artifact %q): got %s want %s (from %s)",
			localPath, artifactBasename, got, want, checksumURL)
	}
	return want, nil
}

// executableComponents are shipped as bare binaries; HTTP downloads land at 0644
// and would not run from the paths the .deb installs them to.
var executableComponents = map[string]struct{}{
	"kubelet": {}, "kubeadm": {}, "kubectl": {}, "runc": {},
}

// ensureExecutableComponent sets 0755 on components installed as bare binaries.
func ensureExecutableComponent(component, dest string) error {
	if _, ok := executableComponents[component]; !ok {
		return nil
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	return nil
}
