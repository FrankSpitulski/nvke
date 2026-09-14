// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nke/controller/nico-sim/internal/vm/toolpath"
)

// swtpmPaths holds the on-disk locations for one instance's software TPM.
type swtpmPaths struct {
	StateDir   string // <instanceDir>/tpm — the only recovery-critical artifact for an FDE guest
	CtrlSock   string // control socket, under os.TempDir() (macOS sun_path cap, same scheme as QMPSock)
	PIDFile    string // <instanceDir>/swtpm.pid — swtpm's own, separate from QEMU's
	EKCertPath string // <instanceDir>/tpm-ek.crt — base64-encoded DER, written once at seed time
	LogPath    string // <instanceDir>/swtpm.log
}

func swtpmPathsForInstance(instanceDir, instanceID string) swtpmPaths {
	return swtpmPaths{
		StateDir:   filepath.Join(instanceDir, "tpm"),
		CtrlSock:   filepath.Join(os.TempDir(), swtpmSocketName(instanceID)),
		PIDFile:    filepath.Join(instanceDir, "swtpm.pid"),
		EKCertPath: filepath.Join(instanceDir, "tpm-ek.crt"),
		LogPath:    filepath.Join(instanceDir, "swtpm.log"),
	}
}

// swtpmSocketName mirrors qmpSocketName's truncation: macOS caps
// sockaddr_un.sun_path at 104 bytes, so the ctrl socket cannot live under a
// deep instancesDir path the way the TPM state directory does.
func swtpmSocketName(instanceID string) string {
	shortID := instanceID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("nvm-tpm-%s.sock", shortID)
}

// swtpmAvailable reports whether swtpm and swtpm_setup are on PATH, or
// configured via NICO_VMS_SWTPM_PATH / NICO_VMS_SWTPM_SETUP_PATH. A host
// without them skips TPM support entirely — same degrade-silently contract
// as the libvirt backend's setupTPM.
func swtpmAvailable() (swtpmBin, swtpmSetupBin string, ok bool) {
	swtpmBin = toolpath.Find("swtpm", "NICO_VMS_SWTPM_PATH")
	swtpmSetupBin = toolpath.Find("swtpm_setup", "NICO_VMS_SWTPM_SETUP_PATH")
	return swtpmBin, swtpmSetupBin, swtpmBin != "" && swtpmSetupBin != ""
}

// tpmStateSeeded reports whether swtpm_setup has already run for this
// instance. swtpm_setup must never run twice against a bound FDE guest: a
// second run mints a different EK and permanently locks it out of its own
// disk once the passphrase slot has been removed.
func tpmStateSeeded(paths swtpmPaths) bool {
	_, err := os.Stat(filepath.Join(paths.StateDir, "tpm2-00.permall"))
	return err == nil
}

// ensureSwtpmState seeds swtpm state for the instance exactly once and
// returns the base64-encoded DER EK certificate. If state already exists —
// from a prior seed, or nico-sim adopting an instance across a restart — it
// returns the cached cert instead of reseeding.
func ensureSwtpmState(ctx context.Context, swtpmSetupBin string, paths swtpmPaths) (string, error) {
	if tpmStateSeeded(paths) {
		certData, err := os.ReadFile(paths.EKCertPath)
		if err != nil {
			return "", fmt.Errorf("swtpm state exists but EK cert is missing at %s: %w", paths.EKCertPath, err)
		}
		return strings.TrimSpace(string(certData)), nil
	}

	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		return "", fmt.Errorf("create swtpm state dir %s: %w", paths.StateDir, err)
	}

	instanceDir := filepath.Dir(paths.StateDir)
	certDir, err := os.MkdirTemp(instanceDir, "tpm-certs-")
	if err != nil {
		_ = os.RemoveAll(paths.StateDir)
		return "", fmt.Errorf("create swtpm cert scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(certDir) }()

	setupConf, err := writeSwtpmSetupConfig(instanceDir)
	if err != nil {
		_ = os.RemoveAll(paths.StateDir)
		return "", fmt.Errorf("write swtpm_setup config: %w", err)
	}

	cmd := exec.CommandContext(ctx, swtpmSetupBin,
		"--tpm2",
		"--tpmstate", paths.StateDir,
		"--config", setupConf,
		"--create-ek-cert",
		"--create-platform-cert",
		"--write-ek-cert-files", certDir,
	)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		_ = os.RemoveAll(paths.StateDir)
		return "", fmt.Errorf("swtpm_setup failed: %w: %s", runErr, string(output))
	}

	// Read the EK certificate (DER format) written by swtpm_setup. Same
	// candidate order as the libvirt backend; rsa2048 is always produced by
	// default so it matches first.
	var certData []byte
	for _, name := range []string{"ek-rsa2048.crt", "ek-rsa3072.crt", "ek-ecc384.crt"} {
		if data, readErr := os.ReadFile(filepath.Join(certDir, name)); readErr == nil {
			certData = data
			break
		}
	}
	if certData == nil {
		_ = os.RemoveAll(paths.StateDir)
		return "", fmt.Errorf("swtpm_setup produced no EK certificate in %s", certDir)
	}

	encoded := base64.StdEncoding.EncodeToString(certData)
	if err := os.WriteFile(paths.EKCertPath, []byte(encoded), 0o644); err != nil {
		return "", fmt.Errorf("write EK cert %s: %w", paths.EKCertPath, err)
	}
	return encoded, nil
}

// writeSwtpmSetupConfig writes a swtpm_setup config that points
// swtpm_localca at a writable state directory under instanceDir.
//
// This is necessary: verified against the flox-packaged swtpm 0.11.0 build,
// swtpm_setup's bundled default config (baked into the package at build
// time) points swtpm_localca at /var/lib/swtpm-localca, which an
// unprivileged user cannot write — swtpm_localca fails with "Could not
// create directory for 'statedir': Permission denied" and swtpm_setup exits
// 0 anyway, leaving no state and no cert. A Homebrew build has the same
// problem with a different hardcoded path. Resolving swtpm_localca via
// exec.LookPath (rather than trusting either package's baked-in default)
// works across both installs.
func writeSwtpmSetupConfig(instanceDir string) (string, error) {
	localca, err := exec.LookPath("swtpm_localca")
	if err != nil {
		return "", fmt.Errorf("swtpm_localca not found: %w", err)
	}

	localcaDir := filepath.Join(instanceDir, "swtpm-localca")
	if err := os.MkdirAll(localcaDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", localcaDir, err)
	}

	localcaConf := filepath.Join(instanceDir, "swtpm-localca.conf")
	localcaContent := fmt.Sprintf("statedir = %s\nsigningkey = %s\nissuercert = %s\ncertserial = %s\n",
		localcaDir,
		filepath.Join(localcaDir, "signkey.pem"),
		filepath.Join(localcaDir, "issuercert.pem"),
		filepath.Join(localcaDir, "certserial"),
	)
	if err := os.WriteFile(localcaConf, []byte(localcaContent), 0o644); err != nil {
		return "", err
	}

	localcaOpts := filepath.Join(instanceDir, "swtpm-localca.options")
	optsContent := "--platform-manufacturer NVIDIA\n--platform-version 2.1\n--platform-model nico-sim\n"
	if err := os.WriteFile(localcaOpts, []byte(optsContent), 0o644); err != nil {
		return "", err
	}

	setupConf := filepath.Join(instanceDir, "swtpm_setup.conf")
	setupContent := fmt.Sprintf("create_certs_tool = %s\ncreate_certs_tool_config = %s\ncreate_certs_tool_options = %s\n",
		localca, localcaConf, localcaOpts,
	)
	if err := os.WriteFile(setupConf, []byte(setupContent), 0o644); err != nil {
		return "", err
	}
	return setupConf, nil
}

// startSwtpm launches swtpm as a self-daemonizing process bound to the
// instance's ctrl socket, and waits for the socket to appear so QEMU's
// eager -chardev connect (verified: it fails immediately against a missing
// socket) does not race it. swtpm writes its own PID file natively.
func startSwtpm(ctx context.Context, swtpmBin string, paths swtpmPaths) error {
	_ = os.Remove(paths.CtrlSock)
	_ = os.Remove(paths.PIDFile)

	cmd := exec.CommandContext(ctx, swtpmBin, "socket",
		"--tpm2",
		"--tpmstate", "dir="+paths.StateDir,
		"--ctrl", "type=unixio,path="+paths.CtrlSock,
		"--pid", "file="+paths.PIDFile,
		"--log", "file="+paths.LogPath+",level=1",
		"--daemon",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("swtpm socket failed to start: %w: %s", err, string(output))
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(paths.CtrlSock); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("swtpm ctrl socket %s did not appear within 10s", paths.CtrlSock)
}

// stopSwtpm stops a running swtpm process using its own PID file. Callers
// must invoke this only after QEMU has exited: swtpm is deliberately not in
// QEMU/vmnet-run's process group (it must survive a vmnet-run teardown), so
// it needs its own, separate shutdown.
func stopSwtpm(paths swtpmPaths) {
	_ = stopPIDFileProcess(paths.PIDFile)
	_ = os.Remove(paths.PIDFile)
	_ = os.Remove(paths.CtrlSock)
}

// tpmQEMUArgs wires the swtpm ctrl socket up as a QEMU TPM device. The
// device name differs by architecture: aarch64 uses tpm-tis-device, x86_64
// uses tpm-tis (libvirt's domain_xml.go makes the same distinction). On
// HVF+aarch64, QEMU 11's PPI RAM region cannot be mapped by HVF, so ppi=off
// is appended — but only when the running QEMU's tpm-tis-device actually
// exposes that property, so older QEMU builds are not broken by an unknown
// property.
func tpmQEMUArgs(ctx context.Context, qemu string, paths swtpmPaths) []string {
	device := "tpm-tis"
	if goruntime.GOARCH == "arm64" {
		device = "tpm-tis-device"
		if tpmTisHasPPI(ctx, qemu) {
			device += ",ppi=off"
		}
	}
	return []string{
		"-chardev", "socket,id=chrtpm,path=" + paths.CtrlSock,
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", device + ",tpmdev=tpm0",
	}
}

func tpmTisHasPPI(ctx context.Context, qemu string) bool {
	out, err := exec.CommandContext(ctx, qemu, "-device", "tpm-tis-device,help").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ppi=") {
			return true
		}
	}
	return false
}
