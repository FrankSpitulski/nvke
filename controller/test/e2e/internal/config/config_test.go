// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Strict parsing has to keep accepting what the repo already ships, or the
// lane stops running for a reason that has nothing to do with the lane.
func TestLoadAcceptsShippedConfigs(t *testing.T) {
	paths, err := filepath.Glob("../../*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no candidate config files found; this test is checking nothing")
	}
	for _, p := range paths {
		if _, err := Load(p); err != nil {
			t.Errorf("%s no longer loads: %v", filepath.Base(p), err)
		}
	}
}

// The document qualify-e2e.yml generates. It is written by a
// heredoc in the workflow rather than from this struct, so nothing but a test
// keeps the two in step.
func TestLoadAcceptsTheGeneratedSiteConfig(t *testing.T) {
	doc := `node:
  user: ubuntu
nvkectl:
  binary: /w/.site-inputs/nvkectl/nvkectl-linux-amd64
site:
  images:
    controller:
      repository: nvcr.io/example-org/nvke-controller
      archive: /w/.site-inputs/controller.tar
    capi-provisioned:
      repository: nvcr.io/example-org/cluster-api-provider-provisioned
      archive: /w/.site-inputs/capi-provisioned.tar
    profiles:
      repository: nke-e2e-profiles
      tag: local
      archive: /w/.site-inputs/profiles.tar
`
	cfg, err := Load(write(t, doc))
	if err != nil {
		t.Fatalf("the config the workflow writes no longer loads: %v", err)
	}
	for _, image := range []struct {
		name string
		got  SiteImage
		want SiteImage
	}{
		{"controller", cfg.Site.Images.Controller, SiteImage{Repository: "nvcr.io/example-org/nvke-controller", Archive: "/w/.site-inputs/controller.tar"}},
		{"capi-provisioned", cfg.Site.Images.CAPIProvisioned, SiteImage{Repository: "nvcr.io/example-org/cluster-api-provider-provisioned", Archive: "/w/.site-inputs/capi-provisioned.tar"}},
		{"profiles", cfg.Site.Images.Profiles, SiteImage{Repository: "nke-e2e-profiles", Tag: "local", Archive: "/w/.site-inputs/profiles.tar"}},
	} {
		if image.got != image.want {
			t.Errorf("%s = %+v, want %+v", image.name, image.got, image.want)
		}
	}
	if cfg.Site.ChartOverrides != nil || cfg.Site.DeployPathRef != "" {
		t.Errorf("legacy config acquired deploy overrides: %+v", cfg.Site)
	}
	if cfg.Node.User != "ubuntu" || cfg.Nvkectl.Binary != "/w/.site-inputs/nvkectl/nvkectl-linux-amd64" {
		t.Errorf("workflow node/CLI inputs changed: node=%+v nvkectl=%+v", cfg.Node, cfg.Nvkectl)
	}
}

func TestLoadRejectsAnUnknownKey(t *testing.T) {
	if _, err := Load(write(t, "site:\n  deployPathReff: \"typo\"\n")); err == nil {
		t.Fatal("an unknown key was accepted, so the run would proceed on the default")
	}
}

// The keys this block used to carry. Each now lives in test code, in
// -e2e.versions, in the environment file or under another block, so a config
// still naming one is out of date and has to say so rather than be half
// obeyed. `repo` is the one with no replacement: the deploy path is always
// exported from the clone the suite runs from.
func TestLoadRejectsARetiredSiteKey(t *testing.T) {
	for _, key := range []string{
		"name: x", "cni: ovn-kubernetes", "disableMTLS: true",
		"controllerRepository: x", "profilesImage: x:y", "registryServer: ghcr.io",
		"bootstrapEnv: {}", "jumpHost: x", "teleportLogin: root",
		"images: [a.tar]", "repo: /a/clone", "teleportIdentityFile: /id",
	} {
		if _, err := Load(write(t, "site:\n  "+key+"\n")); err == nil {
			t.Errorf("site.%s was accepted", key)
		}
	}
}

// The other half of rejecting site.teleportIdentityFile: the key still binds,
// one block up.
func TestRunnerTakesTheTeleportIdentityFile(t *testing.T) {
	cfg, err := Load(write(t, "runner:\n  teleportIdentityFile: /tmp/identity\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runner.TeleportIdentityFile != "/tmp/identity" {
		t.Errorf("Runner.TeleportIdentityFile = %q, want the one written", cfg.Runner.TeleportIdentityFile)
	}
}

func TestDefaultsNeedNoSiteBlock(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		missing   bool
	}{
		{"missing config", "", true},
		{"empty config", "", false},
		{"empty site", "site: {}\n", false},
		{"unrelated settings", "kube:\n  context: local\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "absent.yaml")
			if !tc.missing {
				p = write(t, tc.doc)
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("defaults do not validate: %v", err)
			}
			want := SiteImages{
				Controller:      SiteImage{Repository: "ghcr.io/dsx-ai-factory/nvke/nke-controller", Archive: DefaultSiteInputs + "/controller.tar"},
				CAPIProvisioned: SiteImage{Repository: "ghcr.io/dsx-ai-factory/nvke/cluster-api-provider-provisioned", Archive: DefaultSiteInputs + "/capi-provisioned.tar"},
				Profiles:        SiteImage{Repository: "nke-e2e-profiles", Tag: "local", Archive: DefaultSiteInputs + "/profiles.tar"},
			}
			if cfg.Site.Images != want {
				t.Errorf("default images = %+v, want %+v", cfg.Site.Images, want)
			}
			if cfg.Site.ChartOverrides != nil || cfg.Site.DeployPathRef != "" || cfg.Nvkectl.Binary != "" {
				t.Errorf("legacy defaults acquired overrides: site=%+v nvkectl=%+v", cfg.Site, cfg.Nvkectl)
			}
			if cfg.Cluster.CAPDNodeImage != "cwe-capd-kindest-node:local" {
				t.Errorf("default CAPD image = %q", cfg.Cluster.CAPDNodeImage)
			}
		})
	}
}

func TestImagesValidate(t *testing.T) {
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"controller tag", "site:\n  images:\n    controller:\n      tag: v1\n", "-e2e.versions=controller="},
		{"capi-provisioned tag", "site:\n  images:\n    capi-provisioned:\n      tag: v1\n", "-e2e.versions=capi-provisioned="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

func TestLoadStagedArtifactReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	doc := fmt.Sprintf(`site:
  chartOverrides:
    controller: /staged/controller.tgz
  images:
    controller:
      digest: %s
    capi-provisioned:
      digest: %s
    profiles:
      digest: %s
`, digest, digest, digest)
	cfg, err := Load(write(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ChartOverrides["controller"] != "/staged/controller.tgz" {
		t.Fatalf("chart override lost: %v", cfg.Site.ChartOverrides)
	}
	if cfg.Site.Images.Controller.Digest != digest || cfg.Site.Images.CAPIProvisioned.Digest != digest || cfg.Site.Images.Profiles.Digest != digest {
		t.Fatal("image digest was dropped")
	}
	if cfg.Site.Images.Profiles.Tag != "" {
		t.Fatal("a staged digest acquired a default profiles tag")
	}
}

// The limit of that, recorded because it looks like a gap and is not one.
// sigs.k8s.io/yaml converts to JSON and decodes with encoding/json, whose
// field matching is case-insensitive, so a key differing only in case binds
// to the field rather than counting as unknown. Nothing is dropped, which is
// the property this file is about.
func TestLoadStillAcceptsAKeyDifferingOnlyInCase(t *testing.T) {
	cfg, err := Load(write(t, "site:\n  deploypathref: \"abc123\"\n"))
	if err != nil {
		t.Fatalf("a case variant was rejected: %v", err)
	}
	if cfg.Site.DeployPathRef != "abc123" {
		t.Errorf("Site.DeployPathRef = %q, want it bound from the case variant", cfg.Site.DeployPathRef)
	}
}

func write(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e2e-config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
