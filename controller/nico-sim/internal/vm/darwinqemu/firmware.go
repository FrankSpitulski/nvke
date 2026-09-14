// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func platformArgs(ctx context.Context, qemu, dir string) (string, string, []string, error) {
	machine, codeName, varsName := "q35,accel=hvf", "edk2-x86_64-code.fd", "edk2-i386-vars.fd"
	arm := strings.Contains(filepath.Base(qemu), "aarch64") || strings.Contains(filepath.Base(qemu), "arm64")
	if arm != (runtime.GOARCH == "arm64") {
		return "", "", nil, fmt.Errorf("QEMU %s does not match native %s architecture required by HVF; override with NICO_VMS_QEMU_SYSTEM_PATH", qemu, runtime.GOARCH)
	}
	if arm {
		machine, codeName, varsName = "virt,accel=hvf", "edk2-aarch64-code.fd", "edk2-arm-vars.fd"
	}
	code, vars, err := firmwarePair(ctx, qemu, codeName, varsName)
	if err != nil {
		return "", "", nil, err
	}
	privateVars := filepath.Join(dir, "efi-vars.fd")
	if err := copyVariables(vars, privateVars); err != nil {
		return "", "", nil, err
	}
	return machine, "host", []string{
		"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
		"-drive", "if=pflash,format=raw,file=" + privateVars,
	}, nil
}

func firmwarePair(ctx context.Context, qemu, codeName, varsName string) (string, string, error) {
	code, vars := os.Getenv("NICO_VMS_EFI_CODE"), os.Getenv("NICO_VMS_EFI_VARS_TEMPLATE")
	if code != "" || vars != "" {
		if code == "" || vars == "" {
			return "", "", fmt.Errorf("set both NICO_VMS_EFI_CODE and NICO_VMS_EFI_VARS_TEMPLATE")
		}
		for _, p := range []string{code, vars} {
			f, err := os.Open(p)
			if err != nil {
				return "", "", fmt.Errorf("open firmware %s: %w", p, err)
			}
			info, err := f.Stat()
			f.Close()
			if err != nil {
				return "", "", err
			}
			if !info.Mode().IsRegular() {
				return "", "", fmt.Errorf("firmware %s is not a regular file", p)
			}
		}
		return code, vars, nil
	}
	// Ask the selected QEMU first. Its paths work for Flox, Nix and Homebrew alike.
	out, err := exec.CommandContext(ctx, qemu, "-L", "help").Output()
	if err != nil {
		return "", "", fmt.Errorf("query QEMU firmware directories: %w", err)
	}
	dirs := strings.Split(string(out), "\n")
	dirs = append(dirs, "/opt/homebrew/share/qemu", "/usr/local/share/qemu", "/usr/share/qemu")
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		c, v := filepath.Join(dir, codeName), filepath.Join(dir, varsName)
		ci, ce := os.Stat(c)
		vi, ve := os.Stat(v)
		if ce == nil && ve == nil && ci.Mode().IsRegular() && vi.Mode().IsRegular() {
			return c, v, nil
		}
	}
	return "", "", fmt.Errorf("no matching UEFI firmware pair found for %s; set NICO_VMS_EFI_CODE and NICO_VMS_EFI_VARS_TEMPLATE", qemu)
}

func copyVariables(src, dst string) error {
	// Preserve NVRAM when reconciling an existing instance.
	if info, err := os.Stat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("EFI vars %s exists but is not a regular file", dst)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read EFI variables: %w", err)
	}
	// Write through a temp sibling and rename into place. A crash mid-write
	// must not leave a truncated dst: copyVariables's "exists means done"
	// check above would treat a partial file as already-copied forever,
	// giving QEMU invalid pflash on every subsequent boot.
	tmp, err := os.OpenFile(dst+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
