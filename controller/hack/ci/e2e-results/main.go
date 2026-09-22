// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// e2e-results verifies that required E2E specs selected by Ginkgo passed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

const (
	passedState           = "passed"
	multiPodCIDRsLabel    = "multi-pod-cidrs"
	podCIDRLabel          = "pod-cidr"
	ovnQualificationLabel = "ovn-qualification"
)

type suiteReport struct {
	SpecReports []specReport `json:"SpecReports"`
}

type specReport struct {
	ContainerHierarchyTexts  []string   `json:"ContainerHierarchyTexts"`
	ContainerHierarchyLabels [][]string `json:"ContainerHierarchyLabels"`
	LeafNodeLabels           []string   `json:"LeafNodeLabels"`
	LeafNodeText             string     `json:"LeafNodeText"`
	LeafNodeType             string     `json:"LeafNodeType"`
	State                    string     `json:"State"`
}

func main() {
	selectedPath := flag.String("selected-report", "", "Ginkgo JSON report produced by the dry run")
	reportPath := flag.String("report", "", "Ginkgo JSON report produced by the live run")
	flag.Parse()

	if *selectedPath == "" || *reportPath == "" {
		fmt.Fprintln(os.Stderr, "both --selected-report and --report are required")
		os.Exit(2)
	}

	selected, err := loadReport(*selectedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read selected E2E report: %v\n", err)
		os.Exit(1)
	}
	actual, err := loadReport(*reportPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read live E2E report: %v\n", err)
		os.Exit(1)
	}
	if err := verifyRequiredResults(selected, actual); err != nil {
		fmt.Fprintf(os.Stderr, "required E2E result check failed: %v\n", err)
		os.Exit(1)
	}
}

func loadReport(path string) ([]specReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var suites []suiteReport
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&suites); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("report contains a trailing JSON value")
		}
		return nil, fmt.Errorf("read trailing report data: %w", err)
	}
	if len(suites) == 0 {
		return nil, errors.New("report contains no suites")
	}

	var specs []specReport
	for _, suite := range suites {
		specs = append(specs, suite.SpecReports...)
	}
	if len(specs) == 0 {
		return nil, errors.New("report contains no specs")
	}
	return specs, nil
}

func verifyRequiredResults(selected, actual []specReport) error {
	selected = selectedItSpecs(selected)
	actual = itSpecs(actual)

	return requirePassed(filter(selected, isRequired), actual)
}

func itSpecs(specs []specReport) []specReport {
	return filter(specs, func(spec specReport) bool { return spec.LeafNodeType == "It" })
}

func selectedItSpecs(specs []specReport) []specReport {
	return filter(specs, func(spec specReport) bool {
		return spec.LeafNodeType == "It" && spec.State == passedState
	})
}

func isRequired(spec specReport) bool {
	multiPodCIDR := hasLabel(spec, multiPodCIDRsLabel) && hasLabel(spec, podCIDRLabel)
	qualification := hasLabel(spec, ovnQualificationLabel)
	// The ordinary OVN smoke does not request multiple pod CIDRs. Its shared
	// pod-CIDR validation therefore skips by design. The multi-CIDR entry has
	// both labels, so its required assertion is never covered by this exception.
	ordinaryPodCIDRSkip := hasLabel(spec, podCIDRLabel) && !hasLabel(spec, multiPodCIDRsLabel)
	return multiPodCIDR || (qualification && !ordinaryPodCIDRSkip)
}

func filter(specs []specReport, keep func(specReport) bool) []specReport {
	filtered := make([]specReport, 0, len(specs))
	for _, spec := range specs {
		if keep(spec) {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

func requirePassed(required, actual []specReport) error {
	actualByName := make(map[string]specReport, len(actual))
	for _, spec := range actual {
		name := fullText(spec)
		if _, exists := actualByName[name]; exists {
			return fmt.Errorf("live report has duplicate result for %q", name)
		}
		actualByName[name] = spec
	}

	seen := make(map[string]struct{}, len(required))
	for _, requiredSpec := range required {
		name := fullText(requiredSpec)
		if _, exists := seen[name]; exists {
			return fmt.Errorf("selected report has duplicate required result for %q", name)
		}
		seen[name] = struct{}{}

		actualSpec, exists := actualByName[name]
		if !exists {
			return fmt.Errorf("required selected spec is missing from live report: %q", name)
		}
		if actualSpec.State != passedState {
			return fmt.Errorf("required selected spec %q finished %s", name, actualSpec.State)
		}
	}
	return nil
}

func fullText(spec specReport) string {
	parts := append(append([]string{}, spec.ContainerHierarchyTexts...), spec.LeafNodeText)
	return strings.Join(parts, " ")
}

func hasLabel(spec specReport, wanted string) bool {
	if slices.Contains(spec.LeafNodeLabels, wanted) {
		return true
	}
	for _, labels := range spec.ContainerHierarchyLabels {
		if slices.Contains(labels, wanted) {
			return true
		}
	}
	return false
}
