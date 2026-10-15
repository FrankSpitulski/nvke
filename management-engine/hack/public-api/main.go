// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command public-api generates the bounded tenant projection from the stored CRD.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

const (
	contractDir  = "public/tenant/apis/cluster/v1"
	generatedDir = "public/tenant/gen/cluster/v1"
)

// main dispatches generation or verification from CLI flags and reports failures to calling scripts.
func main() {
	root := flag.String("root", ".", "management-engine module directory")
	check := flag.Bool("check", false, "compare generated artifacts without writing")
	updateLock := flag.Bool("update-lock", false, "register reviewed additive exposure fields")
	descriptor := flag.String("http-descriptor", "", "generate HTTP metadata from a compiled FileDescriptorSet")
	flag.Parse()
	var err error
	if *descriptor != "" {
		err = generateRoutes(*root, *descriptor, *check)
	} else {
		err = run(*root, *check, *updateLock)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run compiles policy, verifies compatibility and renders artifacts so generation uses one checked model.
func run(root string, check, updateLock bool) error {
	if check && updateLock {
		return fmt.Errorf("check and update-lock are mutually exclusive")
	}
	policyData, err := os.ReadFile(filepath.Join(root, contractDir, "exposure.json")) // #nosec G304 -- Local build input.
	if err != nil {
		return fmt.Errorf("read exposure policy: %w", err)
	}
	crdPath := filepath.Join(root, "config/crd/bases/internal.nvke.nvidia.com_nvkeclusters.yaml")
	crdData, err := os.ReadFile(crdPath) // #nosec G304 -- Trusted local build input.
	if err != nil {
		return fmt.Errorf("read internal CRD: %w", err)
	}
	model, err := compile(crdData, policyData)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(root, contractDir, "wire-lock.json")
	lockData, err := os.ReadFile(lockPath) // #nosec G304 -- Trusted local build input.
	if err != nil && (!os.IsNotExist(err) || !updateLock) {
		return fmt.Errorf("read compatibility registry: %w", err)
	}
	newLock, err := verifyLock(model, lockData, updateLock)
	if err != nil {
		return err
	}
	outputs, err := render(model)
	if err != nil {
		return err
	}
	if updateLock {
		outputs[filepath.Join(contractDir, "wire-lock.json")] = newLock
	}
	for path, data := range outputs {
		if err := writeOutput(filepath.Join(root, path), data, check); err != nil {
			return err
		}
	}
	return nil
}

// writeOutput writes an artifact or checks its existing bytes so CI can detect generated drift without edits.
func writeOutput(path string, data []byte, check bool) error {
	if check {
		current, err := os.ReadFile(path) // #nosec G304 -- Compare the generator's own output path.
		if err != nil || !bytes.Equal(current, data) {
			return fmt.Errorf("generated artifact differs: %s; run make generate-public-api", path)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- Public generated source directory.
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G306 -- Public generated source, never credentials.
		return fmt.Errorf("write generated artifact: %w", err)
	}
	return nil
}

// verifyLock compares current records with the reviewed baseline to reject incompatible contract changes.
func verifyLock(model *model, data []byte, update bool) ([]byte, error) {
	previous := map[string]record{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &previous); err != nil {
			return nil, fmt.Errorf("read compatibility registry: %w", err)
		}
	}
	// Ancestor admission rules can constrain public fields without changing their leaf schemas.
	current := map[string]record{
		"$resource": {Name: "resource_admission", Schema: model.resourceAdmission},
		"$spec":     {Name: "spec_admission", Schema: model.root.Schema},
	}
	for _, node := range model.nodes {
		current[node.Path] = node.record
	}
	maps.Copy(current, model.createAdmission)
	for path, old := range previous {
		next, found := current[path]
		if !found || !equalJSON(old, next) {
			return nil, fmt.Errorf("frozen v1 contract changed at %s; use a new public version", path)
		}
	}
	if len(previous) > 0 {
		for path, next := range current {
			if _, exists := previous[path]; exists {
				continue
			}
			// An old replacement client omits new fields and would silently clear them.
			if slices.Contains(next.Access, modeReplace) {
				return nil, fmt.Errorf("new replacement field %s changes the frozen v1 replacement set; "+
					"use a new public version", path)
			}
			if next.Required && slices.Contains(next.Access, modeCreate) {
				return nil, fmt.Errorf("new required create field %s breaks existing v1 requests; use a new public version", path)
			}
		}
	}
	if !update && !equalJSON(previous, current) {
		return nil, fmt.Errorf("unregistered public fields; review and run generator with -update-lock")
	}
	encoded, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode compatibility registry: %w", err)
	}
	return append(encoded, '\n'), nil
}

// equalJSON compares canonical JSON encodings so registry records can be checked across decoded Go types.
func equalJSON(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
