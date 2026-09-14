// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// Package toolpath locates optional host VM tools without requiring a package manager.
package toolpath

import (
	"os"
	"os/exec"
	"path/filepath"
)

// VMNet finds an explicitly configured or installed vmnet executable. Every
// candidate — including the env override — is stat'd and must be an
// executable regular file; a typo'd override must not be handed back as if
// it were a resolved path.
func VMNet(name, env string) string {
	if override := os.Getenv(env); override != "" {
		if isExecutableRegularFile(override) {
			return override
		}
		return ""
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, prefix := range []string{"/opt/vmnet-helper/bin", "/opt/homebrew/opt/vmnet-helper/libexec", "/usr/local/opt/vmnet-helper/libexec"} {
		p := filepath.Join(prefix, name)
		if isExecutableRegularFile(p) {
			return p
		}
	}
	return ""
}

func isExecutableRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
