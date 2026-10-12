// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Registry is the parsed, validated checks.yaml: the versioned set of checks a
// release is judged by. The version travels into every report so a verdict
// names the check set that produced it.
type Registry struct {
	Version string     `yaml:"version"`
	Checks  []CheckDef `yaml:"checks"`
}

// LoadRegistry reads and validates a checks.yaml. Unknown fields are rejected
// so a typo in a selector cannot silently widen a check's scope.
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading check registry: %w", err)
	}
	var reg Registry
	dec := yaml.NewDecoder(newBytesReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("parsing check registry %s: %w", path, err)
	}
	if reg.Version == "" {
		return nil, fmt.Errorf("check registry %s: version is required", path)
	}
	if len(reg.Checks) == 0 {
		return nil, fmt.Errorf("check registry %s: no checks defined", path)
	}
	seen := make(map[string]bool, len(reg.Checks))
	for _, c := range reg.Checks {
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("check registry %s: %w", path, err)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("check registry %s: duplicate check id %s", path, c.ID)
		}
		seen[c.ID] = true
	}
	return &reg, nil
}
