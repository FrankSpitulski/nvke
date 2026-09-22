// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package contracts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFixtureMetadataValidate(t *testing.T) {
	t.Parallel()
	metadata := FixtureMetadata{
		SchemaVersion:    SchemaVersion,
		IssuerURL:        "https://issuer.example.test",
		JWKSURL:          "https://issuer.example.test/jwks",
		TLSCAPEM:         "test CA",
		AcceptedAudience: "kubernetes",
		ExpectedClaims:   ExpectedClaims{Username: "alice@example.test"},
		CleanupOwner:     CleanupOwnerIssuer,
	}
	require.NoError(t, metadata.Validate())

	metadata.AcceptedAudience = ""
	require.ErrorContains(t, metadata.Validate(), "accepted audience is required")
}
