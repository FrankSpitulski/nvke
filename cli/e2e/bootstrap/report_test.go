// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package bootstrap_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapPreparationFailureReport(t *testing.T) {
	f := newArtifactFixture(t, "exit 19\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\nprintf 'candidate\\n'\n")
	report := filepath.Join(filepath.Dir(f.cli), "report.json")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate", "NVKECTL_E2E_REPORT="+report)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 19 {
		t.Fatalf("primary outcome lost: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("missing report: %v\n%s", err, output)
	}
	var result struct {
		APIVersion, Phase, Cleanup string
		ExitCode                   int
		Completed                  bool
		Before                     string `json:"artifactSha256Before"`
		After                      string `json:"artifactSha256After"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.APIVersion != "nvke.dev/bootstrap-e2e/v1" || result.Completed || result.Phase != "preparation" || result.ExitCode != 19 || result.Cleanup != "unknown" || result.Before == "" || result.Before != result.After {
		t.Fatalf("unsupported outcome: %s", raw)
	}
	assertArtifactFixtureClean(t, f)
}

func TestBootstrapReportWriter(t *testing.T) {
	helper, err := filepath.Abs("report.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, completed, phase, status, cleanup string }{
		{name: "terminal assertion completion", completed: "true", phase: "complete", status: "0", cleanup: "unknown"},
		{name: "failure", completed: "false", phase: "bootstrap", status: "7", cleanup: "failed"},
		{name: "cancellation", completed: "false", phase: "bootstrap", status: "143", cleanup: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "terminal.json")
			hash := strings.Repeat("a", 64)
			cmd := exec.Command("bash", "-c", `source "$1"; write_bootstrap_report "$2" "$3" "$4" "$5" "$6" "$3"`, "fixture", helper, tc.status, hash, tc.cleanup, tc.completed, tc.phase)
			cmd.Env = append(os.Environ(), "NVKECTL_E2E_REPORT="+report)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("report: %v %s", err, output)
			}
			raw, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["completed"] != (tc.completed == "true") || decoded["phase"] != tc.phase || decoded["cleanup"] != tc.cleanup {
				t.Fatalf("%s", raw)
			}
			info, err := os.Stat(report)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("report permissions: %v %v", info, err)
			}
		})
	}
}

func TestBootstrapExistingReportIsNotReused(t *testing.T) {
	f := newArtifactFixture(t, "exit 19\n")
	report := filepath.Join(filepath.Dir(f.cli), "report.json")
	if err := os.WriteFile(report, []byte("stale result"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Env = append(f.env, "NVKECTL=absent", "NVKECTL_EXPECTED_VERSION=candidate", "NVKECTL_E2E_REPORT="+report)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "new report path") {
		t.Fatalf("stale report admitted: %v %s", err, output)
	}
	raw, err := os.ReadFile(report)
	if err != nil || string(raw) != "stale result" {
		t.Fatalf("caller report changed: %s %v", raw, err)
	}
	assertArtifactFixtureClean(t, f)
}

func TestBootstrapPreflightFailureReport(t *testing.T) {
	f := newArtifactFixture(t, "exit 19\n")
	binary := filepath.Join(f.cli, "broken supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\nexit 7\n")
	report := filepath.Join(filepath.Dir(f.cli), "report.json")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate", "NVKECTL_E2E_REPORT="+report)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("preflight outcome: %v %s", err, output)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Completed      bool
		Phase, Cleanup string
		ExitCode       int
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Completed || result.Phase != "preflight" || result.ExitCode != 7 || result.Cleanup != "not-started" {
		t.Fatalf("preflight report: %s", raw)
	}
	assertArtifactFixtureClean(t, f)
}
