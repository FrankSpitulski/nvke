// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath"
	"github.com/dsx-ai-factory/nvke/cli/internal/deploypath/stackconfig"
	"github.com/dsx-ai-factory/nvke/cli/internal/stackartifacts"
)

func newRenderCmd() *cobra.Command {
	// Internal: deploy/nvke-deploy.sh runs it with --sitespec, --stack-config,
	// --stack-artifacts and --arch.
	cmd := &cobra.Command{
		Use:    "render",
		Short:  "Write a resolved copy of an input, ready to apply",
		Hidden: true,
	}
	cmd.AddCommand(newRenderDeployPathCmd())
	return cmd
}

type renderDeployPathOptions struct {
	deployPath  string
	siteSpec    string
	stackConfig string
	artifacts   string
	out         string
	arch        string
	plainHTTP   []string
	ephemeral   bool
}

func newRenderDeployPathCmd() *cobra.Command {
	opts := &renderDeployPathOptions{}
	cmd := &cobra.Command{
		Use:   "deploy-path --deploy-path DIR (--sitespec FILE --stack-config FILE | --stack-artifacts FILE | both) --out DIR",
		Short: "Resolve a deploy path for a site and against a stack artifacts file, and write the result",
		Long: `Write a copy of a deploy path under --out, resolved for a site, against a
stack artifacts file, or both. The committed deploy path is never edited.

With --sitespec and --stack-config, the two go together: the site spec
supplies the site name, the control-plane VIP and the SPIRE trust domain;
the stack config supplies what only the stack needs: the NICo API. The
credentials come from
NVKECTL_NICO_TOKEN, NVKECTL_NODE_USER, NVKECTL_NODE_PASSWORD and
NVKECTL_DATABASE_PASSWORD. Items get a site-values.yaml; template items
become manifests. SPIRE gets the CA named by NVKECTL_SPIRE_CA_CERT_FILE and
NVKECTL_SPIRE_CA_KEY_FILE by file reference: helm reads it at install
time. The stack joins the bootstrap's trust domain: spire.trustDomain, set
once before the bootstrap, else the default both commands share, with a
warning.

--arch is the nodes' architecture, linux/amd64 or linux/arm64. It selects
the artifacts and the SPIRE TPM attestor checksum. deploy/nvke-deploy.sh
reads it from the cluster's nodes and passes it; without the flag this
machine's architecture is used, with a warning when a site is rendered.

--stack-artifacts takes the nvke.nvidia.com/stack-artifacts/v1 JSON that
names every artifact the stack installs: written by hand, by make
local-stack-artifacts, or converted from an NVKR release export. Each item's
release.yaml names its artifacts: chart.artifact (default: the item name),
chart.imageArtifact, chart.profilesArtifact, or a manifest item's artifact.
Charts are read by digest into charts/<item>/; images go into
release-images.yaml or a kustomization.yaml; images.txt lists them, each
with its OCI layout when the artifact has one, and stack-artifacts.json is
the file with every layout absolute. An artifact with a layout is read from
it and nothing is pulled; one without is pulled from its reference, with the
Docker credential store. A chart the file lacks stays as committed. The file
decides the version of every chart it carries, with a warning when
release.yaml pins another; the pin decides only for a chart the file lacks.
An artifact no item binds is noted.

--out holds the site's secrets: it is created 0700 and is for one install.
An --out under a shared temp directory is refused; use a directory only you
can read, for example ~/.local/state/nvke/renders/<site>. The command ends
by naming the files under --out that hold a credential; delete --out after
the install. --out gets a .gitignore that ignores everything, so a
repository does not pick it up by accident. Committed chart paths are
written absolute, so --out can live anywhere. Problems are reported all at
once and nothing is left on failure. Then install it:
deploy/nvke-deploy.sh install --deploy-path OUT.`,
		Example:      `  nvkectl render deploy-path --deploy-path deploy/nico --sitespec sitespec.yaml --stack-config stack-config.json --arch linux/arm64 --stack-artifacts dist/local-site/stack-artifacts.json --out ~/.local/state/nvke/renders/my-site`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRenderDeployPath(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.deployPath, "deploy-path", "", "The committed deploy path (required)")
	cmd.Flags().StringVarP(&opts.siteSpec, "sitespec", "f", "", "The site spec: the site name, VIP and trust domain (with --stack-config)")
	cmd.Flags().StringVar(&opts.stackConfig, "stack-config", "", "The stack config file: the stack's input for this site (with --sitespec)")
	cmd.Flags().StringVar(&opts.artifacts, "stack-artifacts", "", "The stack artifacts file, JSON: every image and chart the stack installs")
	cmd.Flags().StringVar(&opts.out, "out", "", "Directory to write; must not exist and must not be under a shared temp directory (required)")
	cmd.Flags().StringVar(&opts.arch, "arch", "linux/"+runtime.GOARCH, "The nodes' architecture, linux/amd64 or linux/arm64; default: this machine's")
	cmd.Flags().StringSliceVar(&opts.plainHTTP, "plain-http-registries", nil, "Registry hosts served over plain HTTP; nothing is probed")
	cmd.Flags().BoolVar(&opts.ephemeral, "ephemeral", false, "The caller removes --out when it is done; the summary does not ask the operator to")
	for _, flag := range []string{"deploy-path", "out"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func runRenderDeployPath(cmd *cobra.Command, opts *renderDeployPathOptions) error {
	if opts.siteSpec != "" && opts.stackConfig == "" {
		return errors.New("--sitespec needs --stack-config: the stack's input is the stack config file")
	}
	if opts.stackConfig != "" && opts.siteSpec == "" {
		return errors.New("--stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec")
	}
	if opts.siteSpec == "" && opts.artifacts == "" {
		return errors.New("pass --sitespec with --stack-config, --stack-artifacts, or both")
	}
	if !slices.Contains(stackartifacts.Archs, opts.arch) {
		return fmt.Errorf("--arch %s is not one of %s", opts.arch, strings.Join(stackartifacts.Archs, ", "))
	}
	ro := deploypath.RenderOptions{DeployPath: opts.deployPath, Out: opts.out, Arch: opts.arch, Log: cmd.ErrOrStderr(), AllowTempOut: os.Getenv("NVKECTL_RENDER_ALLOW_TEMP_OUT") == "1"}
	var inputs []string
	if opts.siteSpec != "" {
		spec, err := sitespec.Load(opts.siteSpec)
		if err != nil {
			return err
		}
		stack, err := stackconfig.Load(opts.stackConfig)
		if err != nil {
			return fmt.Errorf("stack config %s:\n%w", opts.stackConfig, err)
		}
		ro.Site, ro.Stack = spec, stack
		inputs = append(inputs, spec.ClusterName)
		// The cobra default equals the host's value, so only the flag tells
		// a chosen architecture from a defaulted one.
		if !cmd.Flags().Changed("arch") {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: --arch not given; rendering for the host's %s\n", opts.arch)
		}
	}
	if opts.artifacts != "" {
		artifacts, err := stackartifacts.Load(opts.artifacts)
		if err != nil {
			return err
		}
		ro.Artifacts, ro.ArtifactsPath = artifacts, opts.artifacts
		ro.Registry = deploypath.Registries(opts.plainHTTP)
		inputs = append(inputs, "stack artifacts "+opts.artifacts)
	}
	result, err := deploypath.Render(cmd.Context(), ro)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "rendered %s for %s into %s\n", opts.deployPath, strings.Join(inputs, " and "), opts.out)
	if len(result.Images) > 0 {
		var refs []string
		for _, img := range result.Images {
			refs = append(refs, img.Reference)
		}
		fmt.Fprintf(out, "images: %s\n", strings.Join(refs, ", "))
	}
	if opts.ephemeral {
		fmt.Fprintf(out, "%s holds this site's credentials: %s; it is removed when the install ends.\n", opts.out, strings.Join(result.Credentials, ", "))
	} else {
		fmt.Fprintf(out, "%s holds this site's credentials: %s. Delete it after the install.\n", opts.out, strings.Join(result.Credentials, ", "))
	}
	return nil
}
