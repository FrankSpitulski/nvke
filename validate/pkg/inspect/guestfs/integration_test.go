// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build guestfs

// Integration suite for the guestfs tier: runs the real Session against the
// crafted LUKS2+LVM+ext4 fixture that hack/gen-guestfs-fixture.sh builds.
// Needs libguestfs and a Linux kernel; driven locally by
// hack/test-guestfs-container.sh and by the nightly CI lane.
package guestfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/imagefs"
)

// fixtureDir returns the generated-fixture directory or skips.
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("VALIDATE_GUESTFS_FIXTURE_DIR")
	if dir == "" {
		t.Skip("VALIDATE_GUESTFS_FIXTURE_DIR not set; run hack/test-guestfs-container.sh")
	}
	return dir
}

func openFDE(t *testing.T) *Session {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(fixtureDir(t), "disk-fde.qcow2"), Options{LUKSPassphrase: "changeit"})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func TestGuestfsOpenFDE(t *testing.T) {
	s := openFDE(t)
	assert.True(t, s.HasLUKS())

	content, err := s.Cat("/var/lib/dpkg/status")
	require.NoError(t, err)
	assert.Contains(t, content, "Package: linux-nvidia")

	target, err := s.ReadLink("/etc/systemd/system/multipathd.service")
	require.NoError(t, err)
	assert.Equal(t, "/dev/null", target)

	st, err := s.Stat("/var/lib/etcd")
	require.NoError(t, err)
	assert.Equal(t, uint32(0o700), st.Perm)

	exists, err := s.Exists("/no/such/path")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestGuestfsOpenNonFDE(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(fixtureDir(t), "disk-nonfde.qcow2"), Options{})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	assert.False(t, s.HasLUKS())
	ok, err := s.Exists("/var/lib/dpkg/status")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestGuestfsWrongPassphraseFailsClosed(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(fixtureDir(t), "disk-fde.qcow2"), Options{LUKSPassphrase: "wrongpass"})
	require.Error(t, err)
}

func TestGuestfsFDEWithoutPassphraseFailsClosed(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(fixtureDir(t), "disk-fde.qcow2"), Options{})
	require.ErrorContains(t, err, "no passphrase was supplied")
}

// TestGuestfsChecksEndToEnd runs every guestfs-tier check implementation
// against the real session over the fixture — the same code path the CLI
// takes, minus the evidence layer.
func TestGuestfsChecksEndToEnd(t *testing.T) {
	s := openFDE(t)
	in := checks.Inputs{FS: s}
	target := checks.Target{Variant: checks.VariantFDE, Arch: "amd64"}

	want := map[string]checks.Status{
		"HARD-001": checks.StatusPass,
		"HARD-002": checks.StatusPass,
		"HARD-003": checks.StatusPass,
		"HARD-004": checks.StatusPass,
		"SPEC-004": checks.StatusPass,
		"FDE-001":  checks.StatusPass,
		"INT-004":  checks.StatusSkip, // no SBOM supplied in this run
	}
	for id, impl := range imagefs.Impls() {
		out, err := impl(context.Background(), target, in)
		require.NoError(t, err, id)
		assert.Equal(t, want[id], out.Status, "%s: %s", id, out.Evidence)
	}
}
