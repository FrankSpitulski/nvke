// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRegistry(t *testing.T) {
	valid := `
version: 0.1.0
checks:
  - id: X-001
    family: test
    severity: blocking
    method: static
    verifies: a thing
    why: a reason
`
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "valid registry loads", yaml: valid},
		{name: "missing version", yaml: "checks:\n  - id: X\n    family: f\n    severity: warn\n    method: static\n    verifies: v\n    why: w\n", wantErr: "version is required"},
		{name: "no checks", yaml: "version: 1.0.0\nchecks: []\n", wantErr: "no checks defined"},
		{name: "duplicate id", yaml: valid + "  - id: X-001\n    family: test\n    severity: warn\n    method: static\n    verifies: v\n    why: w\n", wantErr: "duplicate check id"},
		{name: "unknown severity", yaml: "version: 1.0.0\nchecks:\n  - id: X\n    family: f\n    severity: fatal\n    method: static\n    verifies: v\n    why: w\n", wantErr: "unknown severity"},
		{name: "unknown method", yaml: "version: 1.0.0\nchecks:\n  - id: X\n    family: f\n    severity: warn\n    method: telepathy\n    verifies: v\n    why: w\n", wantErr: "unknown method"},
		{name: "missing why", yaml: "version: 1.0.0\nchecks:\n  - id: X\n    family: f\n    severity: warn\n    method: static\n    verifies: v\n", wantErr: "verifies and why"},
		{name: "unknown field is rejected", yaml: "version: 1.0.0\nbogus: true\nchecks:\n  - id: X\n    family: f\n    severity: warn\n    method: static\n    verifies: v\n    why: w\n", wantErr: "field bogus not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checks.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o644))
			reg, err := LoadRegistry(path)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "0.1.0", reg.Version)
			assert.Len(t, reg.Checks, 1)
		})
	}
}

// TestShippedRegistryLoads guards the checks.yaml this module ships: a registry
// that fails its own loader must never merge.
func TestShippedRegistryLoads(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join("..", "..", "checks.yaml"))
	require.NoError(t, err)
	assert.NotEmpty(t, reg.Version)
	assert.NotEmpty(t, reg.Checks)
}
