// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"fmt"

	"github.com/distribution/reference"
)

// ValidateRepository preserves legacy repository names when no digest is set.
// Digest inputs require a canonical, fully qualified repository without a tag
// or digest, so CRI normalization cannot change the name imported by containerd.
func ValidateRepository(repository, digest string) error {
	if digest == "" {
		return nil
	}
	named, err := reference.ParseNormalizedNamed(repository)
	if err != nil {
		return fmt.Errorf("invalid digest image repository %q: %w", repository, err)
	}
	if !reference.IsNameOnly(named) || named.String() != repository {
		return fmt.Errorf("digest image repository %q must be a canonical fully qualified name without a tag or digest", repository)
	}
	return nil
}
