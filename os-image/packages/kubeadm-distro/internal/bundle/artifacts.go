// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle holds the manifest and artifact integrity helpers used by
// kubeadm-bundle to download, verify and stage the node stack it packages.
package bundle

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

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
