// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// debmanifest prints, or checks, the package-contract manifest of an
// nke-kubeadm-distro .deb.
//
//	debmanifest <deb>
//	    Print the normalized manifest to stdout.
//
//	debmanifest -golden <file> -deviations <file> <deb>
//	    Compare one .deb against a committed golden manifest, allowing the
//	    reviewed deviations in <file>. Exits non-zero on mismatch, including
//	    when a deviation had no effect.
//
//	debmanifest check -out-dir <dir> -golden-dir <dir> -deviations <file>
//	    Compare every "nke-kubeadm-distro-<version>_*_*.deb" in -out-dir
//	    against "<golden-dir>/s3-<version>.manifest", allowing the reviewed
//	    deviations in -deviations. A version with no matching golden is
//	    skipped and logged, not compared. Exits non-zero only once the
//	    deviations file has at least one directive (strict mode); with an
//	    empty deviations file, drift is logged as a warning and the exit
//	    code stays 0 (advisory mode). This is what `make contract-check`
//	    runs.
//
// Diagnostics go to stderr via log/slog; a printed manifest (the first form
// above) is the only thing debmanifest writes to stdout.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/NVIDIA/nke/os-image/packages/kubeadm-distro/internal/debmanifest"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "check" {
		return runCheck(args[1:])
	}
	return runCompare(args)
}

// runCompare implements the two single-.deb forms: printing a manifest, and
// (with -golden/-deviations) comparing one against a golden file.
func runCompare(args []string) int {
	fs := flag.NewFlagSet("debmanifest", flag.ContinueOnError)
	golden := fs.String("golden", "", "compare against this golden manifest file")
	deviations := fs.String("deviations", "", "reviewed deviations allowlist (required with -golden)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: debmanifest [-golden <file> -deviations <file>] <deb>")
		fmt.Fprintln(os.Stderr, "       debmanifest check -out-dir <dir> -golden-dir <dir> -deviations <file>")
		return 2
	}
	debPath := fs.Arg(0)

	actual, err := debmanifest.Generate(debPath)
	if err != nil {
		slog.Error("debmanifest", "err", err)
		return 1
	}

	if *golden == "" {
		fmt.Print(actual.String())
		return 0
	}
	if *deviations == "" {
		slog.Error("debmanifest: -deviations is required with -golden")
		return 2
	}

	goldenManifest, err := readManifest(*golden)
	if err != nil {
		slog.Error("debmanifest", "err", err)
		return 1
	}
	devs, err := readDeviations(*deviations)
	if err != nil {
		slog.Error("debmanifest", "err", err)
		return 1
	}

	diff, err := debmanifest.Compare(goldenManifest, actual, devs)
	if err != nil {
		slog.Error("manifest mismatch", "deb", debPath, "golden", *golden, "err", err)
		if diff != "" {
			fmt.Fprintln(os.Stderr, "--- golden (after deviations)\n+++ actual")
			fmt.Fprint(os.Stderr, diff)
		}
		return 1
	}
	slog.Info("matches golden within the reviewed deviations", "deb", debPath, "golden", *golden)
	return 0
}

// runCheck implements the "check" subcommand, the one `make contract-check`
// runs: debmanifest.Check over a whole OUT_DIR.
func runCheck(args []string) int {
	fs := flag.NewFlagSet("debmanifest check", flag.ContinueOnError)
	outDir := fs.String("out-dir", "", "directory of built .deb files (required)")
	goldenDir := fs.String("golden-dir", "", "directory of s3-<version>.manifest golden files (required)")
	deviations := fs.String("deviations", "", "reviewed deviations allowlist (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outDir == "" || *goldenDir == "" || *deviations == "" {
		fmt.Fprintln(os.Stderr, "usage: debmanifest check -out-dir <dir> -golden-dir <dir> -deviations <file>")
		return 2
	}

	report, err := debmanifest.Check(*outDir, *goldenDir, *deviations, slog.Default())
	if err != nil {
		slog.Error("debmanifest check", "err", err)
		return 1
	}
	// report.Skipped is already logged by Check itself, as it happens.

	if len(report.Checked) == 0 && len(report.Skipped) == 0 {
		slog.Warn("no nke-kubeadm-distro debs found", "outDir", *outDir)
		return 0
	}

	for name, diff := range report.Diffs {
		if report.Strict {
			slog.Error("package contract drift, and deviations.txt has reviewed entries so this is a hard failure", "deb", name)
		} else {
			slog.Warn("package contract drift, but deviations.txt is still empty so this is advisory only", "deb", name)
		}
		fmt.Fprintf(os.Stderr, "--- golden (after deviations): %s\n+++ actual: %s\n%s", name, name, diff)
	}

	if report.Failed() {
		slog.Error("contract-check failed: either the drift is a bug, or it belongs in deviations.txt with a reason")
		return 1
	}
	slog.Info("contract-check passed", "checked", len(report.Checked), "skipped", len(report.Skipped), "strict", report.Strict)
	return 0
}

func readManifest(path string) (debmanifest.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return debmanifest.Manifest{}, fmt.Errorf("reading golden manifest %s: %w", path, err)
	}
	m, err := debmanifest.ParseManifest(string(data))
	if err != nil {
		return debmanifest.Manifest{}, fmt.Errorf("parsing golden manifest %s: %w", path, err)
	}
	return m, nil
}

func readDeviations(path string) ([]*debmanifest.Deviation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading deviations %s: %w", path, err)
	}
	devs, err := debmanifest.ParseDeviations(string(data))
	if err != nil {
		return nil, fmt.Errorf("parsing deviations %s: %w", path, err)
	}
	return devs, nil
}
