// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwtpmPathsForInstance(t *testing.T) {
	paths := swtpmPathsForInstance("/instances/abc123def456ghi789", "abc123def456ghi789")
	require.Equal(t, "/instances/abc123def456ghi789/tpm", paths.StateDir)
	require.Equal(t, "/instances/abc123def456ghi789/swtpm.pid", paths.PIDFile)
	require.Equal(t, "/instances/abc123def456ghi789/tpm-ek.crt", paths.EKCertPath)
	require.Equal(t, filepath.Join(os.TempDir(), "nvm-tpm-abc123def456.sock"), paths.CtrlSock)
	require.LessOrEqual(t, len(paths.CtrlSock), 104, "macOS sockaddr_un.sun_path is capped at 104 bytes")
}

func TestTpmQEMUArgsDeviceNamePerArch(t *testing.T) {
	paths := swtpmPaths{CtrlSock: "/tmp/x.sock"}
	args := tpmQEMUArgs(context.Background(), "not-invoked-missing-qemu", paths)
	require.Equal(t, []string{
		"-chardev", "socket,id=chrtpm,path=/tmp/x.sock",
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", deviceForArch() + ",tpmdev=tpm0",
	}, args)
}

// deviceForArch mirrors tpmQEMUArgs's arch switch for the assertion above.
// A missing/failing qemu binary means tpmTisHasPPI returns false, so no
// ppi=off is appended regardless of arch.
func deviceForArch() string {
	if goruntime.GOARCH == "arm64" {
		return "tpm-tis-device"
	}
	return "tpm-tis"
}

func TestTpmTisHasPPI(t *testing.T) {
	require.False(t, tpmTisHasPPI(context.Background(), "not-a-real-qemu-binary"))
}

func TestEnsureSwtpmStateIdempotent(t *testing.T) {
	_, swtpmSetupBin, ok := swtpmAvailable()
	if !ok {
		t.Skip("swtpm_setup not installed; set NICO_VMS_SWTPM_SETUP_PATH to exercise this test")
	}
	if _, err := exec.LookPath("swtpm_localca"); err != nil {
		t.Skip("swtpm_localca not installed")
	}

	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-idempotent")
	require.False(t, tpmStateSeeded(paths))

	cert1, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)
	require.NotEmpty(t, cert1)
	require.True(t, tpmStateSeeded(paths))

	// A second call must not reseed: swtpm_setup run twice mints a
	// different EK, which would permanently brick an already-bound FDE
	// guest. Assert the state file's mtime and the cert are unchanged.
	stateFile := paths.StateDir + "/tpm2-00.permall"
	before, statErr := os.Stat(stateFile)
	require.NoError(t, statErr)

	cert2, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)
	require.Equal(t, cert1, cert2, "second seed must return the byte-identical cached EK cert")

	after, statErr := os.Stat(stateFile)
	require.NoError(t, statErr)
	require.Equal(t, before.ModTime(), after.ModTime(), "second call must not touch swtpm state")
}

func TestStopSwtpmLeavesStateDirIntact(t *testing.T) {
	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-stop")
	require.NoError(t, os.MkdirAll(paths.StateDir, 0o755))
	require.NoError(t, os.WriteFile(paths.StateDir+"/tpm2-00.permall", []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(paths.PIDFile, []byte("999999"), 0o644))

	stopSwtpm(paths)

	require.NoFileExists(t, paths.PIDFile)
	_, err := os.Stat(paths.StateDir + "/tpm2-00.permall")
	require.NoError(t, err, "stopSwtpm must never remove TPM state; only Delete does, via the instance dir cleanup")
}

func TestStartSwtpmThenStop(t *testing.T) {
	swtpmBin, swtpmSetupBin, ok := swtpmAvailable()
	if !ok {
		t.Skip("swtpm not installed; set NICO_VMS_SWTPM_PATH to exercise this test")
	}
	if _, err := exec.LookPath("swtpm_localca"); err != nil {
		t.Skip("swtpm_localca not installed")
	}

	dir := t.TempDir()
	paths := swtpmPathsForInstance(dir, "test-instance-start-stop")

	_, err := ensureSwtpmState(context.Background(), swtpmSetupBin, paths)
	require.NoError(t, err)

	require.NoError(t, startSwtpm(context.Background(), swtpmBin, paths))
	require.FileExists(t, paths.CtrlSock)
	require.FileExists(t, paths.PIDFile)

	stopSwtpm(paths)
	require.NoFileExists(t, paths.PIDFile)
	require.NoFileExists(t, paths.CtrlSock)
	// State survives the stop — it is only ever removed by the caller
	// deleting the whole instance directory.
	require.True(t, tpmStateSeeded(paths))
}

func TestSwtpmAvailableRespectsEnvOverride(t *testing.T) {
	t.Setenv("NICO_VMS_SWTPM_PATH", "/bin/does-not-exist-swtpm")
	t.Setenv("NICO_VMS_SWTPM_SETUP_PATH", "/bin/does-not-exist-swtpm-setup")
	swtpmBin, swtpmSetupBin, ok := swtpmAvailable()
	require.True(t, ok)
	require.Equal(t, "/bin/does-not-exist-swtpm", swtpmBin)
	require.Equal(t, "/bin/does-not-exist-swtpm-setup", swtpmSetupBin)
}
