// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifact validates optional staged artifact inputs. Syntax validation
// does not authenticate artifact content or establish release membership.
package artifact

import (
	_ "crypto/sha256" // Register the OCI digest algorithms independently of callers.
	_ "crypto/sha512" // Registers SHA384 and SHA512 as well.
	"fmt"

	"github.com/opencontainers/go-digest"
)

// ValidateDigest accepts an omitted digest or a valid digest supported by OCI's
// digest library. It rejects malformed or unsupported nonempty values.
func ValidateDigest(value string) error {
	if value == "" {
		return nil
	}
	if _, err := digest.Parse(value); err != nil {
		return fmt.Errorf("invalid image digest %q: %w", value, err)
	}
	return nil
}
