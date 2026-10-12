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
	"time"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
	"github.com/dsx-ai-factory/nvke/validate/pkg/checks/static"
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
	checksums := fs.String("checksums", "", "Path to the release's checksums.txt (its signature is PR2's job; see AGENTS.md)")
	registryPath := fs.String("checks", "", "Path to checks.yaml")
	name := fs.String("name", "", "Artifact release name; defaults to the image file's basename")
	variant := fs.String("target-variant", "", "Target variant: fde | non-fde")
	arch := fs.String("target-arch", "", "Target architecture: amd64 | arm64")
	ubuntu := fs.String("target-ubuntu", "", "Target Ubuntu release, e.g. 24.04.5")
	format := fs.String("format", "text", "Output format: text | json")
	if err := fs.Parse(args); err != nil {
		return report.ExitError
	}
	if *versionFlag {
		fmt.Fprintf(stdout, "nvke-image-validate version: %s\n", version)
		return report.ExitPass
	}
	for flagName, v := range map[string]string{
		"image": *image, "checksums": *checksums, "checks": *registryPath,
		"target-variant": *variant, "target-arch": *arch, "target-ubuntu": *ubuntu,
	} {
		if v == "" {
			fmt.Fprintf(stderr, "nvke-image-validate: --%s is required\n", flagName)
			fs.Usage()
			return report.ExitError
		}
	}
	if *name == "" {
		*name = filepath.Base(*image)
	}

	reg, err := checks.LoadRegistry(*registryPath)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return report.ExitError
	}
	sums, err := static.ParseChecksums(*checksums)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return report.ExitError
	}
	digest, err := static.HashFile(*image)
	if err != nil {
		fmt.Fprintf(stderr, "nvke-image-validate: %v\n", err)
		return report.ExitError
	}

	target := checks.Target{
		ImagePath: *image,
		ImageName: *name,
		Variant:   checks.Variant(*variant),
		Arch:      *arch,
		Ubuntu:    *ubuntu,
	}
	results := checks.NewEngine(static.Impls()).Run(context.Background(), reg, target, checks.Inputs{
		ImageSHA256: digest,
		Checksums:   sums,
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
