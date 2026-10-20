// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package bootstrap_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSuppliedArtifact runs the real Make target and unchanged entrypoint in a
// scratch tree. Only supporting tools and preparation are fixtures; no cluster runs.
func TestSuppliedArtifact(t *testing.T) {
	for _, tc := range []struct {
		name, binary, expected, prepare string
		want                            string
		preflight                       bool
	}{
		{name: "missing executable", expected: "candidate", want: "NVKECTL is required"},
		{name: "missing expected version", binary: "valid", want: "NVKECTL_EXPECTED_VERSION is required"},
		{name: "absent executable", binary: "absent", expected: "candidate", want: "executable regular file"},
		{name: "broken executable", binary: "broken", expected: "candidate", want: "supplied executable:", preflight: true},
		{name: "wrong version", binary: "valid", expected: "other", want: "version does not match", preflight: true},
		{name: "support preparation fails", binary: "valid", expected: "candidate", want: "supplied executable after cleanup:", preflight: true},
		{name: "changed bytes and preparation failure", binary: "valid", expected: "candidate", prepare: "printf '# changed\\n' >> \"$NVKECTL\"\n", want: "bytes changed or became unavailable", preflight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.preflight {
				requireArtifactTools(t)
			}
			f := newArtifactFixture(t, tc.prepare+"exit 19\n")
			binary := ""
			if tc.binary != "" {
				binary = filepath.Join(f.cli, "supplied cli")
			}
			if tc.binary != "absent" && binary != "" {
				body := "printf 'candidate\\n'\n"
				if tc.binary == "broken" {
					body = "exit 7\n"
				}
				writeArtifactFixture(t, binary, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROBE\"\n"+body)
			}
			makeTool := "make"
			if path, err := exec.LookPath("gmake"); err == nil {
				makeTool = path
			}
			cmd := exec.Command(makeTool, "--no-print-directory", "test-e2e-bootstrap-artifact", "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION="+tc.expected)
			cmd.Dir = f.cli
			cmd.Env = f.env
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("got %v\n%s; want failure containing %q", err, output, tc.want)
			}
			probe, _ := os.ReadFile(f.probe)
			if tc.preflight && string(probe) != "version -o short\n" {
				t.Fatalf("supplied executable was not consumed exactly once: %q", probe)
			}
			if !tc.preflight && len(probe) != 0 {
				t.Fatalf("executable ran before input admission: %q", probe)
			}
			assertArtifactFixtureClean(t, f)
		})
	}
}

func TestArtifactPreparationPreservesPrimaryStatus(t *testing.T) {
	requireArtifactTools(t)
	f := newArtifactFixture(t, "printf '# changed\\n' >> \"$NVKECTL\"\nexit 19\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\nprintf 'candidate\\n'\n")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	ok := errors.As(err, &exit)
	if !ok || exit.ExitCode() != 19 || !strings.Contains(string(output), "bytes changed") {
		t.Fatalf("primary preparation failure lost: %v\n%s", err, output)
	}
	assertArtifactFixtureClean(t, f)
}

func TestArtifactPreparationGroupCancellation(t *testing.T) {
	requireArtifactTools(t)
	f := newArtifactFixture(t, "touch \"$PREPARING\"\nexec sleep 30\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\nprintf 'candidate\\n'\n")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(f.preparing); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preparation did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not join")
	}
	if !strings.Contains(output.String(), "supplied executable after cleanup:") {
		t.Fatalf("integrity observation missing after cancellation: %s", output.String())
	}
	assertArtifactFixtureClean(t, f)
}

func TestArtifactPreflightCancellation(t *testing.T) {
	requireArtifactTools(t)
	f := newArtifactFixture(t, "exit 19\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\ntouch \"$PREPARING\"\nexec sleep 60\n")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(f.preparing); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("version did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 143 {
			t.Fatalf("preflight cancellation lost: %v\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("version group was not stopped and joined")
	}
	assertArtifactFixtureClean(t, f)
}

// TestArtifactSuiteFailure reaches the real site-spec invocation, then fails the
// supplied process. Preparation and teardown commands are controlled doubles.
func TestArtifactSuiteFailure(t *testing.T) {
	requireArtifactTools(t)
	f := newArtifactFixture(t, `touch "$1/nvke-cni-ovn-charts-2.4.2_2.4.2_arm64.deb" "$1/nvke-cni-ovn-images-2.4.2_2.4.2_arm64.deb" "$1/nke-e2e-helm.deb"
`)
	root := filepath.Dir(f.cli)
	tools := filepath.Join(root, "tools")
	writeArtifactFixture(t, filepath.Join(tools, "jq"), "#!/bin/sh\nexit 0\n")
	writeArtifactFixture(t, filepath.Join(tools, "id"), "#!/bin/sh\necho 0\n")
	writeArtifactFixture(t, filepath.Join(tools, "make"), `#!/bin/sh
case "$*" in
 *nodes-hosts*) echo NVKECTL_E2E_HOSTS_CSV=fixture.csv ;;
 *nodes-down*) touch "$PREPARING" ;;
esac
`)
	writeArtifactFixture(t, filepath.Join(tools, "nerdctl"), "#!/bin/sh\nexit 0\n")
	writeArtifactFixture(t, filepath.Join(root, "controller/hack/ensure-vtpm-proxy.sh"), "#!/bin/sh\nexit 0\n")
	writeArtifactFixture(t, filepath.Join(root, "controller/hack/backend-e2e-lib.sh"), "dockerhub_build_args() { :; }\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, `#!/bin/sh
printf '%s\n' "$*" >> "$PROBE"
if [ "$1" = version ]; then echo candidate; else exit 7; fi
`)
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate",
		"NVKECTL_E2E_CONTROL_PLANE_VIP=172.26.1.1", "NVKECTL_E2E_SPIRE_CA_CERT=fixture.crt", "NVKECTL_E2E_SPIRE_CA_KEY=fixture.key")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	ok := errors.As(err, &exit)
	if !ok || exit.ExitCode() != 7 {
		t.Fatalf("suite process failure lost: %v\n%s", err, output)
	}
	probe, _ := os.ReadFile(f.probe)
	if !strings.HasPrefix(string(probe), "version -o short\ngenerate sitespec ") {
		t.Fatalf("supplied executable not used by suite: %s", probe)
	}
	if _, err := os.Stat(f.preparing); err != nil {
		t.Fatalf("node teardown was not called: %v\n%s", err, output)
	}
	assertArtifactFixtureClean(t, f)
}

// TestArtifactVersionTimeout uses the real timeout utility and its fixed bound.
func TestArtifactVersionTimeout(t *testing.T) {
	requireArtifactTools(t)
	if testing.Short() {
		t.Skip("real 30-second preflight deadline")
	}
	f := newArtifactFixture(t, "touch \"$PREPARING\"\nexit 19\n")
	binary := filepath.Join(f.cli, "supplied cli")
	writeArtifactFixture(t, binary, "#!/bin/sh\nexec sleep 60\n")
	cmd := exec.Command("bash", filepath.Join(f.cli, "e2e/bootstrap/run.sh"))
	cmd.Dir = f.cli
	cmd.Env = append(f.env, "NVKECTL="+binary, "NVKECTL_EXPECTED_VERSION=candidate")
	start := time.Now()
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	ok := errors.As(err, &exit)
	if !ok || exit.ExitCode() != 124 || time.Since(start) > 40*time.Second {
		t.Fatalf("preflight not bounded: %v\n%s", err, output)
	}
	if _, err := os.Stat(f.preparing); !os.IsNotExist(err) {
		t.Fatal("preparation ran after preflight timeout")
	}
	assertArtifactFixtureClean(t, f)
}

type artifactFixture struct {
	cli, probe, preparing, scratch string
	env                            []string
}

func newArtifactFixture(t *testing.T, prepare string) artifactFixture {
	t.Helper()
	root := t.TempDir()
	cli := filepath.Join(root, "cli")
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Makefile", "e2e/bootstrap/run.sh", "e2e/bootstrap/process.sh"} {
		raw, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		writeArtifactFixture(t, filepath.Join(cli, name), string(raw))
	}
	if err := os.Symlink(filepath.Join(source, "../make"), filepath.Join(root, "make")); err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(root, "tools")
	// Git metadata is irrelevant here; avoid macOS xcrun creating a cache in TMPDIR.
	writeArtifactFixture(t, filepath.Join(tools, "git"), "#!/bin/sh\nexit 1\n")
	writeArtifactFixture(t, filepath.Join(tools, "go"), "#!/bin/sh\nif [ \"$1\" = env ]; then echo arm64; else touch \"$BUILT\"; exit 91; fi\n")
	writeArtifactFixture(t, filepath.Join(root, "controller/hack/ci/prepare-capd-bundles.sh"), "#!/bin/sh\n"+prepare)
	// A working default exists, but may never mask a supplied executable failure.
	writeArtifactFixture(t, filepath.Join(cli, "bin/nvkectl"), "#!/bin/sh\ntouch \"$FALLBACK\"\nexit 0\n")
	f := artifactFixture{cli: cli, probe: filepath.Join(root, "probe"), preparing: filepath.Join(root, "preparing"), scratch: filepath.Join(root, "scratch")}
	if err := os.Mkdir(f.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if key == "PATH" || key == "TMPDIR" || strings.HasPrefix(key, "NVKECTL") || strings.HasPrefix(key, "MAKE") {
			continue
		}
		f.env = append(f.env, v)
	}
	f.env = append(f.env, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+f.scratch,
		"PROBE="+f.probe, "PREPARING="+f.preparing, "BUILT="+filepath.Join(root, "built"), "FALLBACK="+filepath.Join(root, "fallback"),
		"NVKECTL_NVKE_CONTROLLER_IMAGE=caller-controller:fixture", "NVKECTL_E2E_NODES=1", "NVKECTL_E2E_CONTROL_PLANE_VIP=")
	return f
}

func assertArtifactFixtureClean(t *testing.T, f artifactFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch not cleaned: %v %v", entries, err)
	}
	for _, name := range []string{"built", "fallback"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(f.cli), name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected %s: %v", name, err)
		}
	}
}

func writeArtifactFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestStopBootstrapGroup exercises the Linux setsid interface used by run.sh.
func TestStopBootstrapGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("Linux setsid required")
	}
	helper, err := filepath.Abs("process.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, handler string }{
		{name: "cooperative", handler: "trap 'exit 0' TERM"},
		{name: "ignores termination", handler: "trap '' TERM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			cmd := exec.Command("bash", "-c", `set -euo pipefail
source "$1"
setsid bash -c "$3; touch \"\$1\"; sleep 60 & wait" fixture "$2" &
bootstrap_pid=$!
trap 'kill -KILL -- "-${bootstrap_pid}" 2>/dev/null || true' EXIT
for ((i=0; i<100; i++)); do [ ! -f "$2" ] || break; sleep 0.01; done
[ -f "$2" ]
owned_pid=$bootstrap_pid
stop_owned_group "$bootstrap_pid"
for ((i=0; i<200; i++)); do
  kill -0 -- "-$owned_pid" 2>/dev/null || break
  sleep 0.01
done
! kill -0 -- "-$owned_pid" 2>/dev/null
trap - EXIT
`, "fixture", helper, ready, tc.handler)
			start := time.Now()
			output, err := cmd.CombinedOutput()
			if err != nil || time.Since(start) > 9*time.Second {
				t.Fatalf("owned group not stopped and joined: %v\n%s", err, output)
			}
		})
	}
}

func requireArtifactTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "timeout", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required by run.sh", tool)
		}
	}
}
