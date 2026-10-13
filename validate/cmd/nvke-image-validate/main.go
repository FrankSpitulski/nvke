// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// nvke-image-validate checks a built node image against its release evidence
// and the versioned check set, entirely offline.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/bundle"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/imagefs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
	"github.com/dsx-ai-factory/nvke/validate/pkg/inspect/guestfs"
	"github.com/dsx-ai-factory/nvke/validate/pkg/report"
)

// version is set at build time via -X main.version=.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("nvke-image-validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	versionFlag := fs.Bool("version", false, "Print the version and exit")
	image := fs.String("image", "", "Path to the qcow2 under validation")
	bundleDir := fs.String("bundle", "", "Path to the release evidence bundle directory (checksums.txt + checksums.txt.sigstore.json + trusted-root.json); verifies the evidence before any check runs")
	certIssuer := fs.String("cert-issuer", "https://token.actions.githubusercontent.com", "OIDC issuer the evidence signer's certificate must carry (bundle mode)")
	certSANRegex := fs.String("cert-san-regex", `^https://github\.com/dsx-ai-factory/nvke/\.github/workflows/os-image-artifact\.yml@.+$`, "Regex the evidence signer's SubjectAlternativeName must match (bundle mode)")
	checksums := fs.String("checksums", "", "Path to a bare checksums.txt: digest-only mode, no evidence verification — INT-002 reports a skip")
	registryPath := fs.String("checks", "", "Path to checks.yaml")
	name := fs.String("name", "", "Artifact release name; defaults to the image file's basename")
	variant := fs.String("target-variant", "", "Target variant: fde | non-fde")
	arch := fs.String("target-arch", "", "Target architecture: amd64 | arm64")
	ubuntu := fs.String("target-ubuntu", "", "Target Ubuntu release, e.g. 24.04.5")
	inspect := fs.String("inspect", "auto", "guestfs inspection tier: auto (on when guestfish is on PATH) | on | off")
	luksKeyFile := fs.String("luks-passphrase-file", "", "File holding the LUKS passphrase for FDE images (never pass the passphrase itself on the command line)")
	format := fs.String("format", "text", "Output format: text | json")
	if err := fs.Parse(args); err != nil {
		return report.ExitError
	}
	if *versionFlag {
		fmt.Fprintf(stdout, "nvke-image-validate version: %s\n", version)
		return report.ExitPass
	}
	for flagName, v := range map[string]string{
		"image": *image, "checks": *registryPath,
		"target-variant": *variant, "target-arch": *arch, "target-ubuntu": *ubuntu,
	} {
		if v == "" {
			fmt.Fprintf(stderr, "nvke-image-validate: --%s is required\n", flagName)
			fs.Usage()
			return report.ExitError
		}
	}
	if (*bundleDir == "") == (*checksums == "") {
		fmt.Fprintln(stderr, "nvke-image-validate: exactly one of --bundle or --checksums is required")
		fs.Usage()
		return report.ExitError
	}
	if *name == "" {
		*name = filepath.Base(*image)
	}

	reg, err := checks.LoadRegistry(*registryPath)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return report.ExitError
	}
	var sums map[string]string
	var evidence checks.EvidenceStatus
	var evidenceBundle *bundle.Bundle
	if *bundleDir != "" {
		b, err := bundle.Load(*bundleDir)
		if err != nil {
			fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
			return report.ExitError
		}
		sums = b.Checksums
		evidenceBundle = b
		evidence.Checked = true
		ev, err := b.VerifyEvidence(bundle.Identity{Issuer: *certIssuer, SANRegex: *certSANRegex})
		if err != nil {
			evidence.Err = err
		} else {
			evidence.SignerSAN = ev.SignerSAN
		}
	} else {
		sums, err = static.ParseChecksums(*checksums)
		if err != nil {
			fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
			return report.ExitError
		}
	}
	digest, err := static.HashFile(*image)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return report.ExitError
	}

	var imgFS checks.ImageFS
	var sbom map[string]string
	runInspection := false
	switch *inspect {
	case "on":
		runInspection = true
	case "auto":
		runInspection = guestfs.Available()
	case "off":
	default:
		fmt.Fprintf(stderr, "nvke-image-validate: unknown --inspect %q\n", *inspect)
		return report.ExitError
	}
	if runInspection {
		var passphrase string
		if *luksKeyFile != "" {
			raw, err := os.ReadFile(*luksKeyFile)
			if err != nil {
				fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
				return report.ExitError
			}
			passphrase = strings.TrimRight(string(raw), "\n")
		}
		session, err := guestfs.Open(context.Background(), *image, guestfs.Options{LUKSPassphrase: passphrase})
		if err != nil {
			fmt.Fprintf(stderr, "nvke-image-validate: opening image for inspection: %v\n", err)
			return report.ExitError
		}
		defer session.Close()
		imgFS = session
		if evidenceBundle != nil {
			pkgs, err := evidenceBundle.LoadSBOM(*name)
			if err != nil {
				fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
				return report.ExitError
			}
			sbom = pkgs
		}
	}

	target := checks.Target{
		ImagePath: *image,
		ImageName: *name,
		Variant:   checks.Variant(*variant),
		Arch:      *arch,
		Ubuntu:    *ubuntu,
	}
	impls := imagefs.Merge(static.Impls(), imagefs.Impls())
	results := checks.NewEngine(impls).Run(context.Background(), reg, target, checks.Inputs{
		ImageSHA256:  digest,
		Checksums:    sums,
		Evidence:     evidence,
		FS:           imgFS,
		SBOMPackages: sbom,
	})
	rep := report.New(reg, target, digest, results, time.Now())

	switch *format {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
			return report.ExitError
		}
		_, _ = stdout.Write(out)
	case "text":
		fmt.Fprint(stdout, rep.Text())
	default:
		fmt.Fprintf(stderr, "nvke-image-validate: unknown --format %q\n", *format)
		return report.ExitError
	}
	return rep.ExitCode()
}
