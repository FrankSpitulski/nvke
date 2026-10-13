// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"strings"
	"testing"
)

func TestValidateRepositoryUsesDockerCanonicalNamesOnlyForDigestInputs(t *testing.T) {
	for _, tc := range []struct {
		name, repository string
		valid            bool
	}{
		{"qualified registry", "ghcr.io/example/controller", true},
		{"qualified Docker Hub", "docker.io/library/profiles", true},
		{"localhost", "localhost/profiles", true},
		{"registry port", "registry:5000/team/profiles", true},
		{"IPv6 registry", "[::1]:5000/team/profiles", true},
		{"legacy profiles", "nke-e2e-profiles", false},
		{"namespace only", "example/profiles", false},
		{"Docker Hub shorthand", "docker.io/profiles", false},
		{"legacy Docker Hub host", "index.docker.io/library/profiles", false},
		{"tag suffix", "ghcr.io/example/profiles:tag", false},
		{"digest suffix", "ghcr.io/example/profiles@sha256:" + strings.Repeat("a", 64), false},
		{"uppercase path", "ghcr.io/Example/profiles", false},
		{"whitespace", " ghcr.io/example/profiles", false},
		{"URL", "https://ghcr.io/example/profiles", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateRepository(tc.repository, "sha256:"+strings.Repeat("a", 64)); (err == nil) != tc.valid {
				t.Fatalf("ValidateRepository(%q) = %v, valid=%t", tc.repository, err, tc.valid)
			}
			if err := ValidateRepository(tc.repository, ""); err != nil {
				t.Fatalf("legacy input acquired a digest-only requirement: %v", err)
			}
		})
	}
}
