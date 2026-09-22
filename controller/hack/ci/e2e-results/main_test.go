// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyRequiredResults(t *testing.T) {
	multiPodCIDR := spec("multi pod CIDR connectivity", []string{multiPodCIDRsLabel, podCIDRLabel})
	multiQualification := spec("multi pod CIDR connectivity", []string{multiPodCIDRsLabel, podCIDRLabel, ovnQualificationLabel})
	qualification := spec("OVN lifecycle", []string{ovnQualificationLabel})
	ordinaryPodCIDR := spec("ordinary pod CIDR validation", []string{ovnQualificationLabel, podCIDRLabel})
	unrelated := spec("unrelated skipped spec", []string{"happy-path"})

	tests := []struct {
		name     string
		selected []specReport
		actual   []specReport
		wantErr  string
	}{
		{
			name:     "required multi-CIDR qualification case appears once and passes",
			selected: []specReport{multiQualification, qualification},
			actual:   []specReport{multiQualification, qualification},
		},
		{
			name:     "runtime skipped required multi-CIDR assertion fails",
			selected: []specReport{multiPodCIDR},
			actual:   []specReport{withState(multiPodCIDR, "skipped")},
			wantErr:  "finished skipped",
		},
		{name: "missing required live result fails", selected: []specReport{multiPodCIDR}, wantErr: "missing from live report"},
		{
			name:     "failed qualification case fails",
			selected: []specReport{qualification},
			actual:   []specReport{withState(qualification, "failed")},
			wantErr:  "finished failed",
		},
		{
			name:     "pending required qualification case fails",
			selected: []specReport{qualification},
			actual:   []specReport{withState(qualification, "pending")},
			wantErr:  "finished pending",
		},
		{
			name:     "unrelated legitimate skip does not fail",
			selected: []specReport{unrelated},
			actual:   []specReport{withState(unrelated, "skipped")},
		},
		{
			name:     "manual partial selection does not require excluded multi-CIDR assertion",
			selected: []specReport{spec("happy path", []string{"happy-path"})},
			actual:   []specReport{spec("happy path", []string{"happy-path"})},
		},
		{
			name:     "non-multi pod-CIDR runtime skip in qualification is allowed",
			selected: []specReport{ordinaryPodCIDR, qualification},
			actual:   []specReport{withState(ordinaryPodCIDR, "skipped"), qualification},
		},
		{
			name:     "manual multi-CIDR partial selection can exclude the assertion",
			selected: []specReport{spec("multi create", []string{multiPodCIDRsLabel})},
			actual:   []specReport{spec("multi create", []string{multiPodCIDRsLabel})},
		},
		{
			name:     "duplicate required live result fails",
			selected: []specReport{multiPodCIDR},
			actual:   []specReport{multiPodCIDR, multiPodCIDR},
			wantErr:  "duplicate result",
		},
		{
			name:     "dry-run label-filter exclusions are not selected",
			selected: []specReport{withState(multiPodCIDR, "skipped")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyRequiredResults(tt.selected, tt.actual)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyRequiredResults() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyRequiredResults() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportRejectsMalformedAndEmptyReports(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("[] trailing JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReport(malformed); err == nil {
		t.Fatal("loadReport(malformed) succeeded, want error")
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReport(empty); err == nil || !strings.Contains(err.Error(), "no suites") {
		t.Fatalf("loadReport(empty) error = %v, want no suites", err)
	}
}

func TestGinkgoDryRunFixtureSelectsOnlyPassedItSpecs(t *testing.T) {
	report, err := loadReport(filepath.Join("testdata", "ginkgo-dry-run.json"))
	if err != nil {
		t.Fatalf("loadReport() error = %v", err)
	}
	selected := selectedItSpecs(report)
	if len(selected) != 2 {
		t.Fatalf("selectedItSpecs() returned %d specs, want 2", len(selected))
	}
	if err := verifyRequiredResults(report, report); err != nil {
		t.Fatalf("verifyRequiredResults() error = %v", err)
	}
}

func spec(leaf string, labels []string) specReport {
	return specReport{
		ContainerHierarchyTexts:  []string{"NKE E2E", "tenant cluster"},
		ContainerHierarchyLabels: [][]string{labels},
		LeafNodeText:             leaf,
		LeafNodeType:             "It",
		State:                    passedState,
	}
}

func withState(report specReport, state string) specReport {
	report.State = state
	return report
}
