// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle holds the manifest and artifact integrity helpers shared by
// kubeadm-bundle (build host) and nke-k8s-installer (node).
package bundle

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ComponentNames are the only logical components allowed in manifest.json.
var ComponentNames = map[string]struct{}{
	"kubelet":     {},
	"kubeadm":     {},
	"kubectl":     {},
	"helm":        {},
	"containerd":  {},
	"cni-plugins": {},
	"crictl":      {},
	"runc":        {},
}

// ValidateComponentName returns an error if name is not a known matrix component.
func ValidateComponentName(name string) error {
	if _, ok := ComponentNames[name]; !ok {
		return fmt.Errorf("unknown component %q", name)
	}
	return nil
}

// artifactBasenamePattern limits manifest "file" values to safe single-segment names.
var artifactBasenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}$`)

// ValidateArtifactBasename rejects path traversal and odd filenames in manifest.json.
func ValidateArtifactBasename(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty artifact filename")
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("artifact filename must be a single path segment, got %q", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("artifact filename must not contain .., got %q", name)
	}
	if !artifactBasenamePattern.MatchString(name) {
		return fmt.Errorf("artifact filename %q has invalid characters", name)
	}
	return nil
}

// JoinUnderRoot returns an absolute path under root after Clean; it rejects escapes.
func JoinUnderRoot(root string, elems ...string) (string, error) {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return "", fmt.Errorf("invalid root directory")
	}
	all := append([]string{root}, elems...)
	joined := filepath.Clean(filepath.Join(all...))
	rootPrefix := root
	if !strings.HasSuffix(rootPrefix, string(filepath.Separator)) {
		rootPrefix += string(filepath.Separator)
	}
	if joined != root && !strings.HasPrefix(joined, rootPrefix) {
		return "", fmt.Errorf("path escapes bundle root: %s", joined)
	}
	return joined, nil
}
