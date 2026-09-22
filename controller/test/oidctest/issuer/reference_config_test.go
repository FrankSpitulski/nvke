// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issuer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

func TestReferenceAuthenticationConfigurations(t *testing.T) {
	t.Parallel()
	externals := []ReferenceProvider{
		referenceProvider("https://external-one.example.test", "external-one-audience"),
		referenceProvider("https://external-two.example.test", "external-two-audience"),
	}
	shared := referenceProvider("https://shared.example.test", "shared-audience")
	policy := ReferencePolicy{ProtectedPrefixes: []string{"system:", "nke:"}}

	tests := []struct {
		name          string
		render        func() ([]byte, error)
		wantProviders int
	}{
		{
			name: "external only",
			render: func() ([]byte, error) {
				return ExternalOnlyReferenceConfig(externals[0], policy)
			},
			wantProviders: 1,
		},
		{
			name: "combined shared and external providers",
			render: func() ([]byte, error) {
				return CombinedReferenceConfig(shared, externals, policy)
			},
			wantProviders: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rendered, err := test.render()
			require.NoError(t, err)
			var config apiserverv1.AuthenticationConfiguration
			require.NoError(t, yaml.Unmarshal(rendered, &config))
			assert.Equal(t, "apiserver.config.k8s.io/v1", config.APIVersion)
			assert.Equal(t, "AuthenticationConfiguration", config.Kind)
			require.Len(t, config.JWT, test.wantProviders)
			if test.wantProviders == 1 {
				assertReferenceProvider(t, config.JWT[0], externals[0], 4)
			} else {
				assertReferenceProvider(t, config.JWT[0], shared, 0)
				assert.Empty(t, config.JWT[0].UserValidationRules, "reference policy must not rewrite the established shared provider")
				for index, external := range externals {
					assertReferenceProvider(t, config.JWT[index+1], external, 4)
				}
			}
		})
	}
}

func TestCombinedReferenceConfigRequiresExternalProvider(t *testing.T) {
	t.Parallel()
	_, err := CombinedReferenceConfig(referenceProvider("https://shared.example.test", "shared-audience"), nil, ReferencePolicy{})
	require.ErrorContains(t, err, "at least one external provider is required")
}

func TestReferenceConfigCanOmitGroupsMapping(t *testing.T) {
	t.Parallel()
	provider := referenceProvider("https://external.example.test", "audience")
	provider.GroupsClaim = ""
	rendered, err := ExternalOnlyReferenceConfig(provider, ReferencePolicy{})
	require.NoError(t, err)
	var config apiserverv1.AuthenticationConfiguration
	require.NoError(t, yaml.Unmarshal(rendered, &config))
	assert.Empty(t, config.JWT[0].ClaimMappings.Groups.Claim)
}

func assertReferenceProvider(t *testing.T, rendered apiserverv1.JWTAuthenticator, expected ReferenceProvider, userValidationRules int) {
	t.Helper()
	assert.Equal(t, expected.Metadata.IssuerURL, rendered.Issuer.URL)
	assert.Equal(t, expected.Metadata.TLSCAPEM, rendered.Issuer.CertificateAuthority)
	assert.Equal(t, []string{expected.Metadata.AcceptedAudience}, rendered.Issuer.Audiences)
	assert.Equal(t, expected.UsernameClaim, rendered.ClaimMappings.Username.Claim)
	require.NotNil(t, rendered.ClaimMappings.Username.Prefix)
	assert.Equal(t, expected.UsernamePrefix, *rendered.ClaimMappings.Username.Prefix)
	assert.Equal(t, expected.GroupsClaim, rendered.ClaimMappings.Groups.Claim)
	require.NotNil(t, rendered.ClaimMappings.Groups.Prefix)
	assert.Equal(t, expected.GroupsPrefix, *rendered.ClaimMappings.Groups.Prefix)
	assert.Len(t, rendered.UserValidationRules, userValidationRules)
	require.Len(t, rendered.ClaimValidationRules, 1)
	assert.Equal(t, "tenant", rendered.ClaimValidationRules[0].Claim)
	assert.Equal(t, "green", rendered.ClaimValidationRules[0].RequiredValue)
}

func referenceProvider(issuerURL, audience string) ReferenceProvider {
	return ReferenceProvider{
		Metadata: contracts.FixtureMetadata{
			SchemaVersion:    contracts.SchemaVersion,
			IssuerURL:        issuerURL,
			JWKSURL:          issuerURL + "/jwks",
			TLSCAPEM:         "test-only-ca",
			AcceptedAudience: audience,
			ExpectedClaims: contracts.ExpectedClaims{
				Username: "alice@example.com",
				Groups:   []string{"operators"},
				Required: map[string]string{"tenant": "green"},
			},
			CleanupOwner: contracts.CleanupOwnerIssuer,
		},
		UsernameClaim: "username",
		GroupsClaim:   "groups",
	}
}
