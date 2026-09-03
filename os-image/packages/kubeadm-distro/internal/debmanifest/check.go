// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package debmanifest

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// debFilePattern matches "nke-kubeadm-distro-<version>_<revision>_<arch>.deb"
// and captures the Kubernetes patch version, e.g. "1.36.3" out of
// "nke-kubeadm-distro-1.36.3_1_amd64.deb".
var debFilePattern = regexp.MustCompile(`^nke-kubeadm-distro-(\d+\.\d+\.\d+)_[^_]+_[^_]+\.deb$`)

// CheckReport is the result of comparing every nke-kubeadm-distro .deb in an
// OUT_DIR against the committed testdata/golden manifests.
type CheckReport struct {
	// Checked is every deb basename that had a matching golden and was
	// compared, regardless of outcome, in the order compared.
	Checked []string
	// Skipped is every deb basename with no matching golden, in the order
	// encountered.
	Skipped []string
	// Diffs maps a checked deb's basename to its comparison error text
	// (Compare's error plus its diff, if any), for every mismatch —
	// including an unused deviation.
	Diffs map[string]string
	// Strict is true when the deviations file had at least one directive
	// line: a non-empty Diffs is then a hard failure rather than a warning.
	Strict bool
}

// Failed reports whether this report should fail a build: at least one diff
// while Strict. An advisory-mode report (Strict == false) is never Failed,
// no matter how much drift it found — see testdata/golden/README.md for why
// contract-check starts advisory.
func (r CheckReport) Failed() bool {
	return r.Strict && len(r.Diffs) > 0
}

// Check compares every "nke-kubeadm-distro-<version>_<revision>_<arch>.deb"
// in outDir against "<goldenDir>/s3-<version>.manifest", allowing the
// deviations in deviationsPath. A version with no matching golden is
// recorded in Skipped and logged via logger, not compared. logger may be
// nil, in which case Check is silent.
//
// The run is advisory — CheckReport.Failed reports false regardless of
// Diffs — while deviationsPath has no directive line (only comments and
// blank lines) yet, and strict once it has at least one, matching the rule
// `make contract-check` used to apply itself before this function replaced
// its shell loop.
func Check(outDir, goldenDir, deviationsPath string, logger *slog.Logger) (CheckReport, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	devText, err := os.ReadFile(deviationsPath)
	if err != nil {
		return CheckReport{}, fmt.Errorf("reading deviations %s: %w", deviationsPath, err)
	}
	devs, err := ParseDeviations(string(devText))
	if err != nil {
		return CheckReport{}, fmt.Errorf("parsing deviations %s: %w", deviationsPath, err)
	}
	report := CheckReport{Diffs: map[string]string{}, Strict: hasDirective(string(devText))}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return CheckReport{}, fmt.Errorf("reading %s: %w", outDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && debFilePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := debFilePattern.FindStringSubmatch(name)[1]
		goldenPath := filepath.Join(goldenDir, fmt.Sprintf("s3-%s.manifest", version))

		goldenText, err := os.ReadFile(goldenPath)
		if err != nil {
			if os.IsNotExist(err) {
				report.Skipped = append(report.Skipped, name)
				logger.Info("no golden for this version, skipping", "deb", name, "golden", goldenPath)
				continue
			}
			return CheckReport{}, fmt.Errorf("reading golden %s: %w", goldenPath, err)
		}
		golden, err := ParseManifest(string(goldenText))
		if err != nil {
			return CheckReport{}, fmt.Errorf("parsing golden %s: %w", goldenPath, err)
		}

		actual, err := Generate(filepath.Join(outDir, name))
		if err != nil {
			return CheckReport{}, fmt.Errorf("generating manifest for %s: %w", name, err)
		}

		logger.Info("comparing against golden", "deb", name, "golden", goldenPath)
		report.Checked = append(report.Checked, name)
		if diff, err := Compare(golden, actual, devs); err != nil {
			msg := err.Error()
			if diff != "" {
				msg += "\n" + diff
			}
			report.Diffs[name] = msg
		}
	}

	if len(report.Checked) == 0 && len(report.Skipped) == 0 {
		logger.Info("no nke-kubeadm-distro debs found", "outDir", outDir)
	}

	return report, nil
}

// hasDirective reports whether text has at least one non-blank,
// non-comment line, the same rule `make contract-check` used to test for
// with `grep -vE '^[[:space:]]*(#.*)?$'` before its loop moved into Check.
func hasDirective(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return true
		}
	}
	return false
}
