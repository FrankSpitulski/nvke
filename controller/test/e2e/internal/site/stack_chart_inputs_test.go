// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/pkg/deploypath"
)

const chartInputRelease = `chart:
  name: fixture-chart
  version: "1.2.3"
  repository: https://charts.example
  artifact: pinned-chart
  imageArtifact: pinned-image
  profilesArtifact: pinned-profiles
  setFile:
    - credentials.file=/etc/nvke/credential
release:
  name: fixture-release
  namespace: fixture-namespace
values:
  - values.yaml
  - overlay.yaml
requires:
  - {kind: certificate, name: identity, namespace: fixture-namespace, condition: Ready}
`

func chartInputAssets(t *testing.T) DeployAssets {
	t.Helper()
	assets := DeployAssets{Dir: t.TempDir(), Layout: CurrentLayout()}
	items := "items:\n  - {name: controller, type: helm}\n  - {name: future-chart, type: helm}\n  - {name: raw, type: manifest}\n"
	for file, data := range map[string]string{
		assets.Layout.Executor: "#!/usr/bin/env bash\nprintf '%s\\n' 'helm upgrade --install nvke-e2e-chart-admission /nvke-e2e-chart-admission.tgz --namespace fixture'\n",
		filepath.Join(assets.Layout.DeployPath, "nvke-deploy.yaml"):             items,
		filepath.Join(assets.Layout.DeployPath, "controller", "release.yaml"):   chartInputRelease,
		filepath.Join(assets.Layout.DeployPath, "future-chart", "release.yaml"): chartInputRelease,
	} {
		target := filepath.Join(assets.Dir, file)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, target, data)
	}
	return assets
}

func TestChartProjectionPreservesSourceReleaseAndUsesExportedRoles(t *testing.T) {
	assets := chartInputAssets(t)
	chart := filepath.Join(t.TempDir(), "chart.tgz")
	writeFile(t, chart, "fixture chart bytes")
	originalFile := filepath.Join(assets.Dir, assets.Layout.DeployPath, "future-chart", "release.yaml")
	original, err := deploypath.ReadRelease(filepath.Dir(originalFile))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := chartOverrideReleases(assets, map[string]string{"future-chart": chart})
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "release.yaml"), string(projected["future-chart"]))
	actual, err := deploypath.ReadRelease(local)
	if err != nil {
		t.Fatal(err)
	}
	expected := *original
	expectedChart := *original.Chart
	expected.Chart = &expectedChart
	expected.Chart.Repository = ""
	expected.Chart.Path = stagedChart("future-chart")
	if !reflect.DeepEqual(*actual, expected) {
		t.Fatalf("release semantics changed:\ngot %+v\nwant %+v", actual, expected)
	}
	unchanged, err := os.ReadFile(originalFile)
	if err != nil || string(unchanged) != chartInputRelease {
		t.Fatal("source release was modified", err)
	}
	if len(projected) != 1 {
		t.Fatalf("projected unrelated releases: %v", projected)
	}
}

func TestChartProjectionRejectsUnsafeExportBeforeRemoteWork(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *DeployAssets)
		role   string
	}{
		{name: "unknown role", role: "absent"},
		{name: "nonhelm role", role: "raw"},
		{name: "layout traversal", role: "controller", change: func(_ *testing.T, a *DeployAssets) { a.Layout.DeployPath = "../outside" }},
		{name: "item traversal", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "nvke-deploy.yaml"), "items:\n  - {name: ../controller, type: helm}\n")
		}},
		{name: "duplicate item", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "nvke-deploy.yaml"), "items:\n  - {name: controller, type: helm}\n  - {name: controller, type: helm}\n")
		}},
		{name: "unknown metadata field", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), chartInputRelease+"unrecognized: true\n")
		}},
		{name: "duplicate YAML key", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), strings.Replace(chartInputRelease, "  name: fixture-chart\n", "  name: fixture-chart\n  name: other\n", 1))
		}},
		{name: "second release document", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), chartInputRelease+"---\n"+chartInputRelease)
		}},
		{name: "second item document", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "nvke-deploy.yaml"), "items:\n  - {name: controller, type: helm}\n---\nitems:\n  - {name: controller, type: manifest}\n")
		}},
		{name: "missing release", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			if err := os.Rename(filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), filepath.Join(a.Dir, "saved.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no chart section", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), "release:\n  name: r\n  namespace: n\n")
		}},
		{name: "metadata symlink", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			original := filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml")
			saved := filepath.Join(t.TempDir(), "release.yaml")
			writeFile(t, saved, chartInputRelease)
			if err := os.Rename(original, filepath.Join(a.Dir, "saved.yaml")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(saved, original); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "item directory escapes export", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			original := filepath.Join(a.Dir, a.Layout.DeployPath, "controller")
			if err := os.Rename(original, filepath.Join(a.Dir, "saved-controller")); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, "release.yaml"), chartInputRelease)
			if err := os.Symlink(outside, original); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized release", role: "controller", change: func(t *testing.T, a *DeployAssets) {
			t.Helper()
			writeFile(t, filepath.Join(a.Dir, a.Layout.DeployPath, "controller", "release.yaml"), strings.Repeat("#", (1<<20)+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assets := chartInputAssets(t)
			if test.change != nil {
				test.change(t, &assets)
			}
			chart := filepath.Join(t.TempDir(), "chart.tgz")
			writeFile(t, chart, "fixture bytes")
			d := testSBOM()
			d.Assets = assets
			d.ChartOverrides = map[string]string{test.role: chart}
			session := sbomSession()
			if err := d.Deploy(t.Context(), session, sbomTarget); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if len(session.puts) != 0 || len(session.runs) != 0 {
				t.Fatal("invalid metadata touched the node")
			}
		})
	}
}

func TestChartProjectionUsesOriginalExecutorAcrossRefs(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the native deploy executor")
	}
	for _, tool := range []string{"git", "bash", "python3", "helm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	top, err := exec.CommandContext(t.Context(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	repo := strings.TrimSpace(string(top))
	for _, ref := range []string{"origin/main", "HEAD", "controller/v0.2.0-rc.1"} {
		t.Run(ref, func(t *testing.T) {
			if err := exec.CommandContext(t.Context(), "git", "-C", repo, "cat-file", "-e", ref+"^{commit}").Run(); err != nil {
				t.Skipf("ref unavailable: %v", err)
			}
			assets, cleanup, err := ExportDeployAssets(t.Context(), repo, ref)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			executor := filepath.Join(assets.Dir, assets.Layout.Executor)
			before, err := os.ReadFile(executor)
			if err != nil {
				t.Fatal(err)
			}
			values := renderValues(t, assets.Dir)
			args := []string{executor, "install", "--deploy-path", filepath.Join(assets.Dir, assets.Layout.DeployPath), "--config", filepath.Join(values, "bootstrap-config.json"), "--dry-run"}
			ordinary, err := runTool(t, "bash", args...)
			if err != nil {
				t.Fatal(err)
			}
			chart := filepath.Join(t.TempDir(), "controller.tgz")
			writeFile(t, chart, "fixture bytes")
			admissionErr := ValidateChartOverridesForAssets(t.Context(), assets, map[string]string{"controller": chart})
			if ref == "controller/v0.2.0-rc.1" {
				if admissionErr == nil || !strings.Contains(admissionErr.Error(), "absolute packaged chart.path") {
					t.Fatalf("legacy executor admission = %v, want early unsupported-input failure", admissionErr)
				}
				after, readErr := os.ReadFile(executor)
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal("legacy executor changed", readErr)
				}
				return
			}
			if admissionErr != nil {
				t.Fatal(admissionErr)
			}
			projected, err := chartOverrideReleases(assets, map[string]string{"controller": chart})
			if err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(assets.Dir, assets.Layout.DeployPath, "controller", "release.yaml")
			local := bytes.ReplaceAll(projected["controller"], []byte(stagedChart("controller")), []byte(chart))
			writeFile(t, metadata, string(local))
			selected, err := runTool(t, "bash", args...)
			if err != nil {
				t.Fatal(err)
			}
			base, actual := helmInstalls(t, ordinary), helmInstalls(t, selected)
			name, err := controllerNameFromDeployPath(assets.Dir, assets.Layout.DeployPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(base) != len(actual) || !strings.Contains(actual[name], " "+chart+" ") {
				t.Fatalf("packaged chart not selected:\n%s", selected)
			}
			for release, command := range base {
				if release != name && actual[release] != command {
					t.Fatalf("unrelated %s changed", release)
				}
			}
			after, err := os.ReadFile(executor)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("exported executor changed", err)
			}
			if strings.Contains(selected, "helm dependency build "+chart) {
				t.Fatal("packaged chart triggered dependency build")
			}
		})
	}
}

func TestChartAssetPreflightDoesNotMutateInput(t *testing.T) {
	assets := chartInputAssets(t)
	chart := filepath.Join(t.TempDir(), "chart.tgz")
	writeFile(t, chart, "fixture bytes")
	if err := ValidateChartOverridesForAssets(t.Context(), assets, map[string]string{"controller": chart}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(assets.Dir, assets.Layout.DeployPath, "controller", "release.yaml"))
	if err != nil || string(raw) != chartInputRelease {
		t.Fatal("preflight modified its source", err)
	}
	if err := ValidateChartOverridesForAssets(t.Context(), DeployAssets{}, nil); err != nil {
		t.Fatal("legacy path required unused metadata", err)
	}
}
