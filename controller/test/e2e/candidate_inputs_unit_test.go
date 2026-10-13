// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	ginkgotypes "github.com/onsi/ginkgo/v2/types"
)

// This exercises the existing shell consumer with real local files. It does
// not stand up a controller or claim external runtime acceptance.
func TestUnit_CandidateCLISmokeNeverBuildsSuppliedBinary(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("Linux SHA256 utility is required by the runtime entrypoint")
	}
	library, err := filepath.Abs("../../hack/ci/nvkectl-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"supplied", "wrong digest", "missing digest", "symlink", "not executable", "missing candidate CLI"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, "nvkectl")
			raw := []byte("#!/bin/sh\nprintf supplied\\n\n")
			if err := os.WriteFile(binary, raw, 0o700); err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			switch scenario {
			case "wrong digest":
				digest = "sha256:" + strings.Repeat("a", 64)
			case "missing digest":
				digest = ""
			case "symlink":
				link := filepath.Join(root, "link")
				if err := os.Symlink(binary, link); err != nil {
					t.Fatal(err)
				}
				binary = link
			case "not executable":
				if err := os.Chmod(binary, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing candidate CLI":
				binary = ""
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; log(){ printf '%s\n' "$*" >&2; }; nvkectl_smoke_build arm64; printf '%s' "$NVKECTL_BIN"`, "fixture", library)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NVKECTL_BIN=/must-not-build", "NKE_E2E_CANDIDATE_RELEASE=/verified-release.json", "NVKECTL_SMOKE_PREBUILT=" + binary, "NVKECTL_SMOKE_SHA256=" + digest}
			out, runErr := cmd.CombinedOutput()
			if scenario == "supplied" {
				if runErr != nil || string(out) != binary {
					t.Fatalf("supplied binary selection: %s %v", out, runErr)
				}
			} else if runErr == nil || strings.Contains(string(out), "compiling nvkectl") {
				t.Fatalf("invalid candidate reached source build: %s %v", out, runErr)
			}
		})
	}
}

func candidateLibrary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../hack/ci/nico-vms-candidate.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The current product CLI is built only as a contract-test fixture. Runtime
// candidate admission receives an already verified and supplied CLI; it never
// invokes this build or turns metadata inspection into authentication.
func TestUnit_CandidateReleaseInputsUseCurrentCLIOwner(t *testing.T) {
	cliDir, err := filepath.Abs("../../../cli")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "nvkectl")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/nvkectl")
	build.Dir = cliDir
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build the current CLI contract fixture: %s %v", out, buildErr)
	}
	fixture, err := os.ReadFile("../../../deploy/testdata/nvkr-release/valid.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		wantOK bool
	}{
		{name: "complete current metadata", wantOK: true},
		{name: "lifecycle v4", mutate: func(doc map[string]any) {
			doc["schemaVersion"] = "nvkr.dev/release/v4"
			doc["artifacts"] = doc["outputs"]
			delete(doc, "outputs")
		}},
		{name: "verification wrapper", mutate: func(doc map[string]any) {
			wrapped := make(map[string]any, len(doc))
			for key, value := range doc {
				wrapped[key] = value
				delete(doc, key)
			}
			doc["release"] = wrapped
		}},
		{name: "filtered output selection", mutate: func(doc map[string]any) {
			doc["inspectionDetails"] = map[string]any{"outputSelection": "filtered"}
		}},
		{name: "selected output", mutate: func(doc map[string]any) { doc["selectedOutput"] = "cwe" }},
		{name: "incomplete output count", mutate: func(doc map[string]any) { doc["totalOutputs"] = 99 }},
		{name: "duplicate named output", mutate: func(doc map[string]any) {
			outputs := doc["outputs"].([]any)
			doc["outputs"] = append(outputs, outputs[0])
			doc["totalOutputs"] = len(outputs) + 1
		}},
		{name: "missing required role returns empty owner selection", mutate: func(doc map[string]any) {
			outputs := doc["outputs"].([]any)
			var retained []any
			for _, output := range outputs {
				if output.(map[string]any)["name"] != "temporal" {
					retained = append(retained, output)
				}
			}
			doc["outputs"] = retained
			doc["totalOutputs"] = len(retained)
		}},
		{name: "required role has wrong platform", mutate: func(doc map[string]any) {
			for _, output := range doc["outputs"].([]any) {
				artifact := output.(map[string]any)
				if artifact["name"] == "controller-profiles" {
					artifact["os"], artifact["arch"] = "linux", "amd64"
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if decodeErr := json.Unmarshal(fixture, &doc); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if tc.mutate != nil {
				tc.mutate(doc)
			}
			raw, encodeErr := json.Marshal(doc)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			dir := t.TempDir()
			release := filepath.Join(dir, "release.json")
			if writeErr := os.WriteFile(release, raw, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; fail(){ printf '%s\n' "$*" >&2; exit 1; }; candidate_release_inputs`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NKE_E2E_CANDIDATE_NVKECTL=" + binary, "NKE_E2E_CANDIDATE_RELEASE=" + release}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.wantOK {
				t.Fatalf("metadata/platform admission: %s %v; want success=%v", out, runErr, tc.wantOK)
			}
			if tc.name == "missing required role returns empty owner selection" && !strings.Contains(string(out), "omits required role temporal") {
				t.Fatalf("absent role must fail after empty owner selection: %s", out)
			}
			unchanged, readErr := os.ReadFile(release)
			if readErr != nil || !bytes.Equal(unchanged, raw) {
				t.Fatalf("admission modified the description: %v", readErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("admission wrote output before runtime: %v, entries=%v", readErr, entries)
			}
		})
	}
}

func TestUnit_CandidateNativeReportsRejectEmptyAndSkippedRuns(t *testing.T) {
	for _, scenario := range []string{"passed", "empty", "skipped", "failed", "incomplete", "suite failed"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			report := ginkgotypes.Report{SuiteSucceeded: true, PreRunStats: ginkgotypes.PreRunStats{SpecsThatWillRun: 1}, SpecReports: ginkgotypes.SpecReports{
				{LeafNodeType: ginkgotypes.NodeTypeBeforeSuite, State: ginkgotypes.SpecStatePassed},
				{LeafNodeType: ginkgotypes.NodeTypeIt, State: ginkgotypes.SpecStatePassed},
			}}
			switch scenario {
			case "empty":
				report.PreRunStats.SpecsThatWillRun = 0
			case "skipped":
				report.SpecReports[1].State = ginkgotypes.SpecStateSkipped
			case "failed":
				report.SpecReports[1].State = ginkgotypes.SpecStateFailed
			case "incomplete":
				report.PreRunStats.SpecsThatWillRun = 2
			case "suite failed":
				report.SuiteSucceeded = false
			}
			raw, err := json.Marshal([]ginkgotypes.Report{report})
			if err != nil {
				t.Fatal(err)
			}
			for name, bytes := range map[string][]byte{"ginkgo.json": raw, "junit.xml": []byte("native fixture")} {
				if writeErr := os.WriteFile(filepath.Join(dir, name), bytes, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; fail(){ exit 1; }; candidate_verify_reports`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "passed") {
				t.Fatalf("native %s report result: %s %v", scenario, out, err)
			}
		})
	}
}

// Commands below are contract doubles only. They prove cleanup selection and
// terminal status propagation; real kind, libvirt and guest acceptance belongs
// to the nightly runner.
const candidateCleanupContract = `
source "$1"
timeout(){ shift; "$@"; }
candidate_stop_group(){ printf 'stop %s\n' "$1" >> "$STATE_DIR/actions"; }
redact_diagnostics(){ [[ "$CASE" != redaction ]]; }
virsh(){
  if [[ "$*" == *'list --all --name'* ]]; then
    [[ "$CASE" != foreign ]] || printf 'foreign-domain\n'
    return 0
  fi
  printf 'inspect %s\n' "$*" >> "$STATE_DIR/actions"
  return 1
}
kind(){
  [[ "$1" != delete || "$CASE" != delete ]] || return 1
  return 0
}
kubectl(){ return 0; }
sudo(){ shift; "$@"; }
setcap(){ return 0; }
candidate_api(){
  [[ "$CASE" != unavailable ]] || return 1
  if [[ "$*" == *DELETE* ]]; then
    printf 'delete %s\n' "$*" >> "$STATE_DIR/actions"
    touch "$STATE_DIR/deleted"
    printf '{}'
  elif [[ "$CASE" == live && ! -f "$STATE_DIR/deleted" ]]; then
    printf '[{"id":"owned-live"}]'
  else
    printf '[]'
  fi
}
candidate_state_owned=true
candidate_test_exit=0
candidate_sim_pid=123
candidate_monitor_pid='' candidate_suite_pid='' candidate_spire_pid='' candidate_oidc_pid='' candidate_dhcp_pid=''
candidate_endpoint=http://owned-fixture
NICO_VMS_ORG_ID=fixture NICO_VMS_TOKEN=fixture
LIBVIRT_URI=qemu:///session KIND_CLUSTER_OWNED=true KIND_CLUSTER_NAME=owned-kind
E2E_BIN="$STATE_DIR/suite" E2E_CONFIG="$STATE_DIR/config" E2E_KUBECONFIG="$STATE_DIR/kubeconfig"
DIAGNOSTICS_MARKER="$STATE_DIR/SAFE_TO_UPLOAD"
NKE_E2E_CANDIDATE_OS_PROFILE=nvke-local-26-04-arm64-plain NKE_E2E_CANDIDATE_KUBERNETES_VERSION=1.34.2
mkdir "$STATE_DIR/sim"
printf 'owned-historical\n' > "$STATE_DIR/owned-instance-ids"
trap candidate_cleanup EXIT
if [[ "$CASE" == cancel ]]; then candidate_test_exit=null; exit 143; fi
exit 0
`

func TestUnit_CandidateCleanupPreservesFailureAndOwnsExactInstances(t *testing.T) {
	for _, scenario := range []string{"clean", "live", "delete", "foreign", "unavailable", "redaction", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", candidateCleanupContract, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "CASE=" + scenario}
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "clean" || scenario == "live") {
				t.Fatalf("cleanup %s result: %s %v", scenario, out, err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "candidate-outcome.json"))
			if err != nil {
				t.Fatalf("missing cleanup outcome: %s %v", out, err)
			}
			var outcome struct {
				TestExit    *int `json:"test_exit"`
				CleanupExit int  `json:"cleanup_exit"`
				EntryExit   int  `json:"entry_exit"`
			}
			if decodeErr := json.Unmarshal(raw, &outcome); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			failedCleanup := scenario != "clean" && scenario != "live" && scenario != "cancel"
			if (outcome.CleanupExit != 0) != failedCleanup {
				t.Fatalf("incorrect cleanup outcome: %s", raw)
			}
			if scenario == "cancel" && (outcome.EntryExit != 143 || outcome.TestExit != nil) {
				t.Fatalf("cancellation was converted into success: %s", raw)
			}
			actions, err := os.ReadFile(filepath.Join(dir, "actions"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(actions), "delete -X DELETE http://owned-fixture/v2/org/fixture/forge/instance/owned-historical") || strings.Contains(string(actions), "destroy") {
				t.Fatalf("cleanup attempted unowned or already deleted resources: %s", actions)
			}
			if scenario == "live" && !strings.Contains(string(actions), "/instance/owned-live") {
				t.Fatalf("cleanup omitted its currently live instance: %s", actions)
			}
		})
	}
}

func TestUnit_CandidateImagePinsAndContainerdReadback(t *testing.T) {
	for _, scenario := range []string{"valid", "archive changed", "wrong reference", "duplicate", "missing values", "wrong readback"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "image.tar")
			payload := []byte("transport fixture; containerd media validation belongs to the native importer")
			if writeErr := os.WriteFile(archive, payload, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			manifest := "sha256:" + strings.Repeat("a", 64)
			reference := "registry.example/cwe@" + manifest
			image := map[string]string{"name": "cwe", "reference": reference, "manifestDigest": manifest, "archive": archive, "archiveDigest": fmt.Sprintf("sha256:%x", sha256.Sum256(payload))}
			if scenario == "archive changed" {
				image["archiveDigest"] = "sha256:" + strings.Repeat("b", 64)
			}
			if scenario == "wrong reference" {
				image["reference"] = "registry.example/cwe:latest"
			}
			images := []map[string]string{image}
			if scenario == "duplicate" {
				images = append(images, image)
			}
			raw, err := json.Marshal(images)
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(dir, "images.json")
			if writeErr := os.WriteFile(input, raw, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			for _, role := range []string{"spire", "postgresql", "temporal"} {
				if scenario == "missing values" && role == "spire" {
					continue
				}
				if writeErr := os.WriteFile(filepath.Join(dir, role+".json"), []byte(`{"image":{"digest":"selected"}}`), 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			readback := reference
			if scenario == "wrong readback" {
				readback += "-other"
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-ec", `
source "$1"
fail(){ exit 1; }
timeout(){ shift; "$@"; }
docker(){
  if [[ "$*" == *'images import'* ]]; then
    cat >/dev/null
    printf '%s\n' "$*" >> "$STATE_DIR/actions"
  else
    printf '%s' "$READBACK"
  fi
}

candidate_image_inputs
candidate_import_images
`, "contract", candidateLibrary(t))
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "STATE_DIR=" + dir, "NKE_E2E_CANDIDATE_IMAGE_INPUTS=" + input, "NKE_E2E_CANDIDATE_VALUES=" + dir, "KIND_CLUSTER_NAME=owned", "READBACK=" + readback}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != (scenario == "valid") {
				t.Fatalf("image %s result: %s %v", scenario, out, runErr)
			}
			if scenario == "valid" {
				actions, readErr := os.ReadFile(filepath.Join(dir, "actions"))
				if readErr != nil || !strings.Contains(string(actions), "--platform linux/arm64 --base-name registry.example/cwe -") {
					t.Fatalf("did not invoke native selected-platform importer: %s %v", actions, readErr)
				}
			}
		})
	}
}

func TestUnit_CandidateCompletedLeaderStillStopsOwnedGroup(t *testing.T) {
	// Real kernel process groups: the leader exits successfully while another
	// owned member remains. Leader Wait success is insufficient cleanup proof.
	leader := exec.CommandContext(t.Context(), "sh", "-c", "read -r signal")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := leader.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if startErr := leader.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	t.Cleanup(func() {
		_ = leader.Process.Kill()
		_ = stdin.Close()
		if leader.ProcessState == nil {
			_ = leader.Wait()
		}
	})
	member := exec.CommandContext(t.Context(), "sleep", "120")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if startErr := member.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	joined := make(chan error, 1)
	go func() { joined <- member.Wait() }()
	t.Cleanup(func() {
		_ = member.Process.Kill()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned group member was not joined")
		}
	})
	if _, writeErr := stdin.Write([]byte("complete\n")); writeErr != nil {
		t.Fatal(writeErr)
	}
	if waitErr := leader.Wait(); waitErr != nil {
		t.Fatalf("leader did not succeed: %v", waitErr)
	}
	if probeErr := syscall.Kill(-leader.Process.Pid, 0); probeErr != nil {
		t.Fatalf("owned group unexpectedly disappeared with leader: %v", probeErr)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-ec", `source "$1"; candidate_stop_group "$2"`, "contract", candidateLibrary(t), fmt.Sprint(leader.Process.Pid))
	if out, stopErr := cmd.CombinedOutput(); stopErr != nil {
		t.Fatalf("actual owned group was not stopped/joined: %s %v", out, stopErr)
	}
	if probeErr := syscall.Kill(-leader.Process.Pid, 0); probeErr != syscall.ESRCH {
		t.Fatalf("group remained after cleanup: %v", probeErr)
	}
}
