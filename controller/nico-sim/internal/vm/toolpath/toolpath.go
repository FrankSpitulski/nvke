// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// Package toolpath locates optional host VM tools without requiring a package manager.
package toolpath

import (
	"os"
	"os/exec"
	"path/filepath"
)

// VMNet finds an explicitly configured or installed vmnet executable.
func VMNet(name, env string) string {
	if override := os.Getenv(env); override != "" {
		return override
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, prefix := range []string{"/opt/vmnet-helper/bin", "/opt/homebrew/opt/vmnet-helper/libexec", "/usr/local/opt/vmnet-helper/libexec"} {
		p := filepath.Join(prefix, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
