// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"errors"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

// TestValidateDigest checks optional input compatibility and independently
// registered algorithms, preserving upstream errors for invalid content.
func TestValidateDigest(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        error
	}{
		{"omitted", "", nil},
		{"sha256", "sha256:" + strings.Repeat("a", 64), nil},
		{"sha384", "sha384:" + strings.Repeat("b", 96), nil},
		{"sha512", "sha512:" + strings.Repeat("c", 128), nil},
		{"wrong length", "sha256:xyz", digest.ErrDigestInvalidLength},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64), digest.ErrDigestInvalidFormat},
		{"unsupported", "unknown:" + strings.Repeat("a", 64), digest.ErrDigestUnsupported},
		{"absent separator", "sha256", digest.ErrDigestInvalidFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDigest(tc.value)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateDigest = %v, want %v", err, tc.want)
			}
		})
	}
}
