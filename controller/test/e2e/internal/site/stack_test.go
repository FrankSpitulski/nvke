// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

const spireOverlay = `
spiffe-oidc-discovery-provider:
  config:
    additionalDomains:
      - "nke.site-controller.internal"
      - "localhost"
      - "127.0.0.1"
`

var sbomTarget = DeployTarget{
	Node:        testNode,
	SiteName:    "e2e-site",
	TrustDomain: "e2e-site.nke.local",
	Cluster:     Cluster{NodeKubeconfig: kubeconfigPath},
}

// stackCheckout is a tree with the two paths the stack stages, written once
// for the package rather than per test: Stack.validate stats it, and nothing
// mutates it.
var stackCheckout = func() string {
	dir, err := os.MkdirTemp("", "nke-stack-assets-")
	if err != nil {
		panic(err)
	}
	for _, rel := range []string{"deploy/nvke-deploy.sh", "deploy/nico/release.yaml"} {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			panic(err)
		}
	}
	items := "items:\n"
	for _, item := range []string{"controller", "postgresql", "temporal", "spire", "spire-crds", "nktp"} {
		items += "  - {name: " + item + ", type: helm}\n"
		directory := filepath.Join(dir, "deploy", "nico", item)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			panic(err)
		}
		metadata := "chart:\n  name: " + item + "\n  version: \"1.2.3\"\n  repository: https://charts.example\nrelease:\n  name: " + item + "\n  namespace: " + item + "\nvalues:\n  - values.yaml\n"
		if err := os.WriteFile(filepath.Join(directory, "release.yaml"), []byte(metadata), 0o644); err != nil {
			panic(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "nico", "nvke-deploy.yaml"), []byte(items), 0o644); err != nil {
		panic(err)
	}
	// Session unit tests model the executor admission output. Separate native
	// executor tests exercise real current and historical deployment refs.
	executor := "#!/usr/bin/env bash\nprintf '%s\\n' 'helm upgrade --install nvke-e2e-chart-admission /nvke-e2e-chart-admission.tgz --namespace fixture'\n"
	if err := os.WriteFile(filepath.Join(dir, "deploy", "nvke-deploy.sh"), []byte(executor), 0o644); err != nil {
		panic(err)
	}
	return dir
}()

func testSBOM() Stack {
	layout := CurrentLayout()
	layout.Export = []string{"deploy"}
	return Stack{
		Assets:     DeployAssets{Dir: stackCheckout, Layout: layout},
		Controller: Image{Repository: "nvcr.io/nvke/cwe", Tag: "v1.2.3"},
		Profiles:   Image{Repository: "nvcr.io/nvke/profiles", Tag: "run-42"},
		Provider:   NewNico("https://forge.example", "org", "tok"),
		Tenant:     Tenant{User: "admin", Password: "p"},
		Arch:       "arm64",
	}
}

func overlayPath() string {
	return path.Join(assetsDir, "deploy", "nico", "spire", "overlay.yaml")
}

func sbomSession() *fakeSession {
	s := newFakeSession()
	s.fetch[overlayPath()] = []byte(spireOverlay)
	return s
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dig(t *testing.T, doc any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := doc.(map[string]any)
		if !ok {
			t.Fatalf("%s: not a map at %q", strings.Join(keys, "."), k)
		}
		doc = m[k]
	}
	return doc
}

func TestSBOMRender(t *testing.T) {
	d := testSBOM().withDefaults()
	d.DatabasePassword = "db-secret"
	d.ControllerEnv = map[string]string{"DGXC_REGION": "us-west"}
	files, err := d.render(sbomTarget, []byte(spireOverlay))
	if err != nil {
		t.Fatal(err)
	}

	controllerValues := decode(t, files["controller.yaml"])
	for _, keys := range [][]string{{"image"}, {"profiles", "image"}} {
		image, ok := dig(t, controllerValues, keys...).(map[string]any)
		if !ok {
			t.Fatalf("%v is not an image map", keys)
		}
		if _, exists := image["digest"]; exists {
			t.Errorf("legacy %v overlay acquired a digest override", keys)
		}
	}
	for keys, want := range map[string]string{
		"image.repository":                 "nvcr.io/nvke/cwe",
		"image.tag":                        "v1.2.3",
		"appDb.postgresPassword":           "db-secret",
		"nvkeOIDCAuth.discoveryURL":        "https://10.0.0.5:30443/.well-known/openid-configuration",
		"spiffe.trustDomain":               "e2e-site.nke.local",
		"spiffe.spire.serverAddress":       "10.0.0.5",
		"nodeAgent.spireAttestationMethod": "tpm",
		"profiles.image.repository":        "nvcr.io/nvke/profiles",
		"profiles.image.tag":               "run-42",
	} {
		if got := dig(t, controllerValues, strings.Split(keys, ".")...); got != want {
			t.Errorf("controller %s = %v, want %s", keys, got, want)
		}
	}
	env := mustJSON(t, controllerValues["env"])
	for _, want := range []string{
		`{"name":"NODE_AGENT_PORT","value":"15005"}`,
		`{"name":"SPIRE_SERVER_API_ADDRESS","value":"spire-server.spire-server.svc.cluster.local:443"}`,
		`{"name":"DGXC_REGION","value":"us-west"}`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("controller env %s lacks %s", env, want)
		}
	}

	spire := decode(t, files["spire.yaml"])
	if got := dig(t, spire, "spire-server", "nodeAttestor", "tpmDirect", "checksum"); got != mustChecksum(t, "arm64") {
		t.Errorf("TPM checksum = %v", got)
	}
	// Follows the name the controller is installed under rather than asserting one, for
	// the reason spireAdminPath gives: the deploy path chooses it and this
	// file does not. TestSBOMAdminIDFollowsTheControllerName covers the
	// following itself.
	wantAdminIDs := fmt.Sprintf(`["spiffe://e2e-site.nke.local/nke-service/%s"]`, controller.Service())
	if got := mustJSON(t, dig(t, spire, "spire-server", "adminIDs")); got != wantAdminIDs {
		t.Errorf("adminIDs = %s, want %s", got, wantAdminIDs)
	}
	if got := mustJSON(t, dig(t, spire, "spiffe-oidc-discovery-provider", "config", "additionalDomains")); got !=
		`["nke.site-controller.internal","localhost","127.0.0.1","10.0.0.5"]` {
		t.Errorf("additionalDomains = %s", got)
	}
	if got := dig(t, spire, "global", "spire", "clusterName"); got != "e2e-site" {
		t.Errorf("clusterName = %v", got)
	}

	// Both Temporal datastores, not just one: setting the password on only
	// one leaves the schema job crash-looping on password authentication.
	for _, want := range []struct {
		file string
		keys []string
	}{
		{"postgresql.yaml", []string{"auth", "postgresPassword"}},
		{"temporal.yaml", []string{"server", "config", "persistence", "default", "sql", "password"}},
		{"temporal.yaml", []string{"server", "config", "persistence", "visibility", "sql", "password"}},
	} {
		if got := dig(t, decode(t, files[want.file]), want.keys...); got != "db-secret" {
			t.Errorf("%s %v = %v", want.file, want.keys, got)
		}
	}

	var config struct {
		Nico        map[string]string `json:"Nico"`
		Tenant      map[string]string `json:"Tenant"`
		ClusterName string            `json:"ClusterName"`
	}
	if err := json.Unmarshal(files["bootstrap-config.json"], &config); err != nil {
		t.Fatal(err)
	}
	if config.Nico["APIName"] != "forge" || config.Nico["Token"] != "tok" ||
		config.Tenant["User"] != "admin" || config.ClusterName != "e2e-site" {
		t.Errorf("bootstrap config = %+v", config)
	}
}

func TestOIDCDomains(t *testing.T) {
	got, err := oidcDomains([]byte(spireOverlay), "127.0.0.1")
	if err != nil || len(got) != 3 {
		t.Errorf("a VIP already listed was added again: %v, %v", got, err)
	}
	if _, err := oidcDomains([]byte("spiffe-oidc-discovery-provider: {}\n"), "10.0.0.5"); err == nil {
		t.Error("an overlay without additionalDomains was accepted")
	}
}

func TestSBOMDeploy(t *testing.T) {
	s := sbomSession()
	if err := testSBOM().Deploy(context.Background(), s, sbomTarget); err != nil {
		t.Fatal(err)
	}

	valuesDir := path.Join(workDir, "values")
	for _, name := range []string{"controller.yaml", "spire.yaml", "postgresql.yaml", "temporal.yaml", "bootstrap-config.json"} {
		p, ok := s.puts[path.Join(valuesDir, name)]
		if !ok {
			t.Errorf("%s not staged", name)
			continue
		}
		if p.mode != 0o600 {
			t.Errorf("%s mode %o, want 600", name, p.mode)
		}
	}
	pg := decode(t, s.puts[path.Join(valuesDir, "postgresql.yaml")].data)
	if pw, _ := dig(t, pg, "auth", "postgresPassword").(string); len(pw) < 32 {
		t.Errorf("generated password %q is too short", pw)
	}

	order := []string{
		// The deploy path is staged first: every step after it runs from the
		// tree it unpacks.
		"assets.tar.gz",
		"node-role.kubernetes.io/control-plane-",
		"clusterctl init",
		"local-path-storage.yaml",
		"nvke-deploy.sh",
		"apply-capi-spiffe-exec-auth.sh",
	}
	last := -1
	for _, want := range order {
		i := s.ran(want)
		if i <= last {
			t.Fatalf("%q ran at %d, want after %d:\n%s", want, i, last, strings.Join(s.runs, "\n"))
		}
		last = i
	}
	// The rendered values hold credentials, so removing them is the last thing
	// that happens. Asserted on the final command rather than by searching for
	// "rm -rf", which the staging step also runs.
	if final := s.runs[len(s.runs)-1]; !strings.Contains(final, valuesDir) || !strings.Contains(final, "rm -rf") {
		t.Errorf("last command should remove the values directory, got %s", final)
	}
	install := s.cmd(t, "nvke-deploy.sh")
	if strings.Contains(install, "--chart-override") {
		t.Errorf("legacy install acquired a chart override: %s", install)
	}
	for staged := range s.puts {
		if strings.HasPrefix(staged, path.Join(workDir, "charts")+"/") {
			t.Errorf("legacy install staged an unrequested chart: %s", staged)
		}
	}
	for _, want := range []string{
		"nvke-deploy.sh install ",
		"--deploy-path /var/lib/nke-e2e/assets/deploy/nico",
		"--config",
		"controller=/var/lib/nke-e2e/values/controller.yaml",
		"temporal=/var/lib/nke-e2e/values/temporal.yaml",
		"--kubeconfig",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("install lacks %s: %s", want, install)
		}
	}
	for _, cmd := range s.runs {
		if strings.Contains(cmd, "tok") {
			t.Errorf("a credential reached a command line: %s", cmd)
		}
	}
}

func TestSBOMDeployCleansUpOnFailure(t *testing.T) {
	s := sbomSession()
	s.fail = func(cmd string) (string, error) {
		if strings.Contains(cmd, "nvke-deploy.sh") {
			return "helm: release temporal failed", errors.New("exit status 1")
		}
		return "", nil
	}
	err := testSBOM().Deploy(context.Background(), s, sbomTarget)
	if err == nil || !strings.Contains(err.Error(), "install deploy/nico") || !strings.Contains(err.Error(), "release temporal failed") {
		t.Fatalf("err = %v", err)
	}
	if s.ran("apply-capi-spiffe-exec-auth.sh") >= 0 {
		t.Error("ran the post-install step after a failed install")
	}
	if s.ran("rm -rf") < 0 {
		t.Error("left the values, which hold credentials, on the node")
	}
}

func TestStackRemovesStagedCharts(t *testing.T) {
	for _, tc := range []struct{ name, failure string }{
		{name: "success"},
		{name: "overlay missing", failure: "overlay"},
		{name: "install failed", failure: "install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testSBOM()
			chart := filepath.Join(t.TempDir(), "controller.tgz")
			writeFile(t, chart, "packaged chart payload")
			d.ChartOverrides = map[string]string{"controller": chart}
			s := sbomSession()
			if tc.failure == "overlay" {
				s.fetch = map[string][]byte{}
			}
			s.fail = func(cmd string) (string, error) {
				if tc.failure == "install" && strings.Contains(cmd, "nvke-deploy.sh") {
					return "failed install", errors.New("install failed")
				}
				return "", nil
			}
			err := d.Deploy(context.Background(), s, sbomTarget)
			if (err != nil) != (tc.failure != "") {
				t.Fatalf("Deploy = %v, failure = %s", err, tc.failure)
			}
			if _, staged := s.puts[stagedChart("controller")]; !staged {
				t.Fatal("test never staged the chart")
			}
			last := unquoted(s.runs[len(s.runs)-1])
			if !strings.Contains(last, "rm -rf "+path.Join(workDir, "values")+" "+path.Join(workDir, "charts")) {
				t.Fatalf("staged charts not removed: %s", last)
			}
		})
	}
}

type chartFailureSession struct {
	*fakeSession
	cancel             context.CancelFunc
	cleanupWasDetached bool
}

func (s *chartFailureSession) Put(ctx context.Context, name string, data []byte, mode uint32) error {
	if err := s.fakeSession.Put(ctx, name, data, mode); err != nil {
		return err
	}
	if name == stagedChart("postgresql") {
		s.cancel()
		return errors.New("partial chart write")
	}
	return nil
}

func (s *chartFailureSession) Run(ctx context.Context, cmd string) (string, error) {
	if strings.Contains(unquoted(cmd), "rm -rf "+path.Join(workDir, "values")) {
		s.cleanupWasDetached = ctx.Err() == nil
	}
	return s.fakeSession.Run(ctx, cmd)
}

func TestStackRemovesPartialChartsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := testSBOM()
	chart := filepath.Join(t.TempDir(), "chart.tgz")
	writeFile(t, chart, "chart payload")
	d.ChartOverrides = map[string]string{"controller": chart, "postgresql": chart}
	s := &chartFailureSession{fakeSession: sbomSession(), cancel: cancel}
	err := d.Deploy(ctx, s, sbomTarget)
	if err == nil || !strings.Contains(err.Error(), "partial chart write") {
		t.Fatalf("Deploy = %v, want failed chart staging", err)
	}
	if _, ok := s.puts[stagedChart("controller")]; !ok {
		t.Fatal("first chart was not staged before failure")
	}
	if !s.cleanupWasDetached || !strings.Contains(unquoted(s.runs[len(s.runs)-1]), path.Join(workDir, "charts")) {
		t.Fatal("canceled chart staging did not clean charts with a detached context")
	}
	if s.ran("nvke-deploy.sh") >= 0 {
		t.Fatal("deploy continued after chart staging failed")
	}
}

func TestStackValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Stack){
		"arch":          func(d *Stack) { d.Arch = "riscv64" },
		"controller":    func(d *Stack) { d.Controller.Tag = "" },
		"profiles":      func(d *Stack) { d.Profiles = Image{} },
		"provider":      func(d *Stack) { d.Provider = NewNico("https://forge.example", "org", "") },
		"no provider":   func(d *Stack) { d.Provider = nil },
		"storage class": func(d *Stack) { d.StorageClassManifest = "https://example/sc.yaml" },
	} {
		t.Run(name, func(t *testing.T) {
			d := testSBOM()
			mutate(&d)
			s := sbomSession()
			if err := d.Deploy(context.Background(), s, sbomTarget); err == nil {
				t.Fatal("expected a validation error")
			}
			if len(s.runs) != 0 || len(s.puts) != 0 {
				t.Error("touched the node before validating")
			}
		})
	}
}

func TestStagedChartOverrides(t *testing.T) {
	d := testSBOM()
	chart := filepath.Join(t.TempDir(), "controller.tgz")
	data := []byte("packaged chart payload")
	if err := os.WriteFile(chart, data, 0o600); err != nil {
		t.Fatal(err)
	}
	d.ChartOverrides = map[string]string{"controller": chart}
	s := sbomSession()
	if err := d.Deploy(context.Background(), s, sbomTarget); err != nil {
		t.Fatal(err)
	}
	if got := s.puts[stagedChart("controller")]; !bytes.Equal(got.data, data) || got.mode != 0o600 {
		t.Fatalf("staged chart = %+v, want exact payload with mode 0600", got)
	}
	commands := strings.Join(s.runs, "\n")
	if strings.Contains(commands, "--chart-override") {
		t.Fatalf("installer interface was changed: %s", commands)
	}
	metadata := decode(t, s.puts[path.Join(assetsDir, d.Assets.Layout.DeployPath, "controller", "release.yaml")].data)
	if got := dig(t, metadata, "chart", "path"); got != stagedChart("controller") {
		t.Fatalf("release chart path = %v, want staged archive", got)
	}
	if _, repository := metadata["chart"].(map[string]any)["repository"]; repository {
		t.Fatal("projected release has two chart sources")
	}
	for _, tc := range []struct{ name, item, file string }{
		{"unknown item", "unexpected", chart},
		{"absent chart", "controller", chart + ".absent"},
		{"directory", "controller", filepath.Dir(chart)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.ChartOverrides = map[string]string{tc.item: tc.file}
			s := sbomSession()
			if err := d.Deploy(context.Background(), s, sbomTarget); err == nil {
				t.Fatal("invalid chart override was accepted")
			}
			if len(s.runs) != 0 || len(s.puts) != 0 {
				t.Fatal("invalid chart touched the node")
			}
		})
	}
}

func TestImageDigestOverridesTag(t *testing.T) {
	d := testSBOM()
	digest := "sha256:" + strings.Repeat("a", 64)
	d.Controller.Digest = digest
	d.Profiles.Digest = digest
	files, err := d.render(sbomTarget, []byte(spireOverlay))
	if err != nil {
		t.Fatal(err)
	}
	values := decode(t, files[valuesFile(releaseController)])
	for _, keys := range [][]string{{"image"}, {"profiles", "image"}} {
		if got := dig(t, values, append(keys, "digest")...); got != digest {
			t.Errorf("%v digest = %v, want %s", keys, got, digest)
		}
		wantTag := d.Controller.Tag
		if keys[0] == "profiles" {
			wantTag = d.Profiles.Tag
		}
		if got := dig(t, values, append(keys, "tag")...); got != wantTag {
			t.Errorf("%v informational tag = %v, want %s", keys, got, wantTag)
		}
	}
	if got := d.Controller.Ref(); got != d.Controller.Repository+"@"+digest {
		t.Errorf("Ref = %s, want digest reference", got)
	}
}

func TestStackRejectsInvalidImageDigestsBeforeRemoteWork(t *testing.T) {
	for _, image := range []string{"controller", "profiles"} {
		for _, tc := range []struct{ name, digest string }{
			{"short value", "sha256:abc"},
			{"uppercase value", "sha256:" + strings.Repeat("A", 64)},
			{"unsupported algorithm", "sha999:" + strings.Repeat("a", 64)},
		} {
			t.Run(image+"/"+tc.name, func(t *testing.T) {
				d := testSBOM()
				if image == "controller" {
					d.Controller.Digest = tc.digest
				} else {
					d.Profiles.Digest = tc.digest
				}
				s := sbomSession()
				err := d.Deploy(context.Background(), s, sbomTarget)
				if err == nil || !strings.Contains(err.Error(), image) || !strings.Contains(err.Error(), "digest") {
					t.Fatalf("Deploy = %v, want an invalid digest error naming %s", err, image)
				}
				if len(s.runs) != 0 || len(s.puts) != 0 || len(s.forwards) != 0 {
					t.Fatal("invalid image digest touched the node")
				}
			})
		}
	}
}

func TestValidateChartOverrides(t *testing.T) {
	dir := t.TempDir()
	chart := filepath.Join(dir, "controller.tgz")
	empty := filepath.Join(dir, "empty.tgz")
	for file, data := range map[string][]byte{chart: []byte("packaged chart payload"), empty: nil} {
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"nil overrides", nil, ""},
		{"empty overrides", map[string]string{}, ""},
		{"known items", map[string]string{"controller": chart, "postgresql": chart, "temporal": chart, "spire": chart, "spire-crds": chart, "nktp": chart}, ""},
		{"role validated against assets later", map[string]string{"future-role": chart}, ""},
		{"empty path", map[string]string{"controller": ""}, "controller"},
		{"absent chart", map[string]string{"controller": chart + ".absent"}, "controller"},
		{"empty chart", map[string]string{"controller": empty}, "nonempty regular file"},
		{"directory", map[string]string{"controller": dir}, "nonempty regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChartOverrides(tc.overrides)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateChartOverrides = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustChecksum(t *testing.T, arch string) string {
	t.Helper()
	sum, ok := tpmServerChecksum(arch)
	if !ok {
		t.Fatalf("no checksum for %s", arch)
	}
	return sum
}

// assetsInstalling is a deploy path whose controller HelmRelease installs
// under name, which is all CheckControllerName reads.
func assetsInstalling(t *testing.T, deployPath, name string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ControllerReleasePath(deployPath)),
		"chart:\n  name: nke-controller\nrelease:\n  name: c\n  namespace: "+name+"\n")
	return dir
}

// TestCheckControllerName covers the guard that runs before a machine is
// taken: a deploy path installing the controller under a name the suite does
// not look for fails on the runner, naming both and the variables to set.
func TestCheckControllerName(t *testing.T) {
	deployPath := CurrentLayout().DeployPath
	for _, env := range []string{
		controller.EnvNamespace, controller.LegacyEnvNamespace,
		controller.EnvService, controller.LegacyEnvService,
	} {
		t.Setenv(env, "")
	}

	t.Run("the suite default matches", func(t *testing.T) {
		if err := CheckControllerName(assetsInstalling(t, deployPath, controller.ServiceNamespace), deployPath); err != nil {
			t.Fatalf("a matching name was rejected: %v", err)
		}
	})

	t.Run("a different name fails", func(t *testing.T) {
		const deployed = "other-controller"
		err := CheckControllerName(assetsInstalling(t, deployPath, deployed), deployPath)
		if err == nil {
			t.Fatal("a deploy path installing a different name was accepted")
		}
		for _, want := range []string{deployed, controller.ServiceNamespace, controller.EnvNamespace, controller.EnvService} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %s: %v", want, err)
			}
		}
	})
}
