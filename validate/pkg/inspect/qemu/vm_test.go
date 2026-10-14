// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	_, err := defaults(Options{Arch: "amd64"})
	require.ErrorContains(t, err, "SeedISO and SSHKeyPath are required")

	_, err = defaults(Options{Arch: "riscv", SeedISO: "s", SSHKeyPath: "k"})
	require.ErrorContains(t, err, "unsupported architecture")

	opts, err := defaults(Options{Arch: "amd64", SeedISO: "s", SSHKeyPath: "k", FirmwarePath: "/fw"})
	require.NoError(t, err)
	assert.Equal(t, "qemu-system-x86_64", opts.QEMUSystem)
	assert.Equal(t, 2048, opts.MemMB)
	assert.Equal(t, "nvkeprobe", opts.SSHUser)
	assert.Equal(t, 10*time.Minute, opts.BootTimeout)
}

func TestSSHArgsAreNonInteractive(t *testing.T) {
	vm := &VM{sshPort: 2222, sshKey: "/k", sshUser: "u"}
	joined := strings.Join(vm.sshArgs("systemctl is-enabled ssh"), " ")
	assert.Contains(t, joined, "BatchMode=yes")
	assert.Contains(t, joined, "StrictHostKeyChecking=no")
	assert.Contains(t, joined, "-p 2222")
	assert.Contains(t, joined, "u@127.0.0.1")
	assert.Contains(t, joined, "-- systemctl is-enabled ssh")
}

func TestAccelFallsBackToTCGForForeignArch(t *testing.T) {
	foreign := map[string]string{"arm64": "amd64", "amd64": "arm64"}[archSelf()]
	assert.Equal(t, "tcg", accel(foreign))
}

func archSelf() string {
	opts, _ := defaults(Options{SeedISO: "s", SSHKeyPath: "k", FirmwarePath: "/fw"})
	return opts.Arch
}
