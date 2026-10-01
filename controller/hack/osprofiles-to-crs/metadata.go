// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// profileMetadata is one entry of an OS profile metadata.yaml. CWE no longer
// reads these files; the converter is the only code that does.
type profileMetadata struct {
	KubernetesVersion         string `yaml:"kubernetes_version"`
	KubernetesVersionInjected bool   `yaml:"kubernetes_version_injected"`
	Architecture              string `yaml:"architecture"`
	BuildDate                 string `yaml:"build_date"`
	UbuntuVersion             string `yaml:"ubuntu_version,omitempty"`
	Variant                   string `yaml:"variant,omitempty"`
}

// metadataIndex is the index of all OS profiles: the active ones from
// osProfiles/metadata.yaml and the retired ones from osProfiles/deprecated.yaml.
type metadataIndex struct {
	Profiles           map[string]profileMetadata `yaml:"profiles"`
	DeprecatedProfiles map[string]profileMetadata `yaml:"-"`
}

// loadMetadata reads osProfiles/metadata.yaml, and osProfiles/deprecated.yaml
// when it exists, under profilesDir.
func loadMetadata(profilesDir string) (*metadataIndex, error) {
	metadataPath := filepath.Join(profilesDir, "osProfiles", "metadata.yaml")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}

	var index metadataIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse metadata file: %w", err)
	}

	deprecatedPath := filepath.Join(profilesDir, "osProfiles", "deprecated.yaml")
	deprecatedData, err := os.ReadFile(deprecatedPath)
	if err != nil {
		if os.IsNotExist(err) {
			index.DeprecatedProfiles = make(map[string]profileMetadata)
			return &index, nil
		}
		return nil, fmt.Errorf("failed to read deprecated metadata file: %w", err)
	}

	var deprecatedIndex metadataIndex
	if err := yaml.Unmarshal(deprecatedData, &deprecatedIndex); err != nil {
		return nil, fmt.Errorf("failed to parse deprecated metadata file: %w", err)
	}
	index.DeprecatedProfiles = deprecatedIndex.Profiles

	return &index, nil
}
