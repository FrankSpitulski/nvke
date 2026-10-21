// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
	"github.com/dsx-ai-factory/nvke/validate/pkg/runner"
)

type validateImageOptions struct {
	image              string
	bundle             string
	trustedRoot        string
	checksums          string
	certIssuer         string
	certSANRegex       string
	checks             string
	targetBOM          string
	name               string
	variant            string
	arch               string
	ubuntu             string
	inspect            string
	boot               string
	bootTimeout        time.Duration
	luksPassphraseFile string
	emitVerdict        string
	signKey            string
	format             string
}

func newValidateImageCmd(ver string) *cobra.Command {
	opts := &validateImageOptions{}
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Check a node image against its release evidence",
		Long: `Check a built node image against its release evidence and the versioned
check set, entirely offline, before the image is registered.

The report prints to stdout (--format json for pipelines). The command exits 0
only when every blocking check passed; any failure — a failing check or a run
that could not evaluate — exits 1, like every other nvkectl failure. A
pipeline that needs the three-way exit contract (pass / blocking fail /
could-not-evaluate) runs the standalone nvke-image-validate binary instead;
both share the same engine and flags.

--emit-verdict with --sign-key additionally writes the result as a DSSE-signed
attestation over the image's digest, which downstream gates re-check with
nvke-verdict instead of ever re-reading the image.`,
		Example: `  nvkectl validate image --image nvke-node-fde-ubuntu-26.04.1-amd64.qcow2 \
    --bundle release-evidence/ --trusted-root /etc/nvke/trusted-root.json --checks checks.yaml \
    --target-variant fde --target-arch amd64 --target-ubuntu 26.04.1
  nvkectl validate image --image img.qcow2 --checksums checksums.txt --checks checks.yaml \
    --target-variant non-fde --target-arch arm64 --target-ubuntu 26.04.1 --format json`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidateImage(cmd, opts, ver)
		},
	}
	cmd.Flags().StringVar(&opts.image, "image", "", "Path to the qcow2 under validation (required)")
	cmd.Flags().StringVar(&opts.bundle, "bundle", "", "Release evidence bundle directory; verifies the evidence before any check runs (requires --trusted-root)")
	cmd.Flags().StringVar(&opts.trustedRoot, "trusted-root", "", "Operator-pinned trusted root that authenticates the evidence (bundle mode); pinned out-of-band, never read from the evidence directory")
	cmd.Flags().StringVar(&opts.checksums, "checksums", "", "Bare checksums.txt: digest-only mode, the unverified evidence reports as a skip")
	cmd.Flags().StringVar(&opts.certIssuer, "cert-issuer", runner.DefaultCertIssuer, "OIDC issuer the evidence signer's certificate must carry (bundle mode)")
	cmd.Flags().StringVar(&opts.certSANRegex, "cert-san-regex", runner.DefaultCertSANRegex, "Regex the evidence signer's SubjectAlternativeName must match (bundle mode)")
	cmd.Flags().StringVar(&opts.checks, "checks", "", "Path to checks.yaml (required)")
	cmd.Flags().StringVar(&opts.targetBOM, "target-bom", "", "Target BOM YAML (components: {name: version}); omitted, the BOM-driven COMPAT checks report a skip")
	cmd.Flags().StringVar(&opts.name, "name", "", "Artifact release name; defaults to the image file's basename")
	cmd.Flags().StringVar(&opts.variant, "target-variant", "", "Target variant: fde | non-fde (required)")
	cmd.Flags().StringVar(&opts.arch, "target-arch", "", "Target architecture: amd64 | arm64 (required)")
	cmd.Flags().StringVar(&opts.ubuntu, "target-ubuntu", "", "Target Ubuntu release, e.g. 26.04.1 (required)")
	cmd.Flags().StringVar(&opts.inspect, "inspect", "auto", "guestfs inspection tier: auto (on when guestfish is on PATH) | on | off")
	cmd.Flags().StringVar(&opts.boot, "boot", "off", "boot tier (boots a throwaway copy under QEMU, minutes): on | off")
	cmd.Flags().DurationVar(&opts.bootTimeout, "boot-timeout", 0, "Wait bound for the boot tier's probe channel (0 = default)")
	cmd.Flags().StringVar(&opts.luksPassphraseFile, "luks-passphrase-file", "", "File holding the LUKS passphrase for FDE images")
	cmd.Flags().StringVar(&opts.emitVerdict, "emit-verdict", "", "Write the result as a DSSE-signed attestation to this path")
	cmd.Flags().StringVar(&opts.signKey, "sign-key", "", "PEM private key (Ed25519 or ECDSA P-256) that signs the verdict")
	cmd.Flags().StringVar(&opts.format, "format", "text", "Report format: text | json")
	for _, required := range []string{"image", "checks", "target-variant", "target-arch", "target-ubuntu"} {
		_ = cmd.MarkFlagRequired(required)
	}
	cmd.MarkFlagsOneRequired("bundle", "checksums")
	cmd.MarkFlagsMutuallyExclusive("bundle", "checksums")
	cmd.MarkFlagsMutuallyExclusive("trusted-root", "checksums")
	cmd.MarkFlagsRequiredTogether("bundle", "trusted-root")
	cmd.MarkFlagsRequiredTogether("emit-verdict", "sign-key")
	return cmd
}

// runValidateImage runs the shared validation engine and folds its three-way
// exit contract into nvkectl's single error path: the distinction between "a
// blocking check failed" and "could not evaluate" survives in the message,
// while the code stays 1 as cli/docs/compatibility.md promises.
func runValidateImage(cmd *cobra.Command, opts *validateImageOptions, ver string) error {
	exit, err := runner.Run(cmd.Context(), runner.Options{
		ImagePath:          opts.image,
		Name:               opts.name,
		BundleDir:          opts.bundle,
		ChecksumsPath:      opts.checksums,
		CertIssuer:         opts.certIssuer,
		CertSANRegex:       opts.certSANRegex,
		RegistryPath:       opts.checks,
		Variant:            opts.variant,
		Arch:               opts.arch,
		Ubuntu:             opts.ubuntu,
		BOMPath:            opts.targetBOM,
		Inspect:            opts.inspect,
		Boot:               opts.boot,
		BootTimeout:        opts.bootTimeout,
		LUKSPassphraseFile: opts.luksPassphraseFile,
		EmitVerdictPath:    opts.emitVerdict,
		SignKeyPath:        opts.signKey,
		Format:             opts.format,
		ValidatorVersion:   "nvkectl " + ver,
		TrustedRootPath:    opts.trustedRoot,
	}, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	switch exit {
	case report.ExitPass:
		return nil
	case report.ExitFail:
		return errors.New("the image failed validation: a blocking check failed; the report above names it")
	default:
		return errors.New("validation could not evaluate the image; the report above names the check that errored")
	}
}
