// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issuer

import (
	"fmt"
	"sort"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

// ReferenceProvider describes claim mapping for a directly configured test API
// server. It is test input and is not an NKE configuration model.
type ReferenceProvider struct {
	Metadata       contracts.FixtureMetadata
	UsernameClaim  string
	UsernamePrefix string
	GroupsClaim    string
	GroupsPrefix   string
}

// ReferencePolicy lists protected identity prefixes for reference tests. These
// rules express test policy. Their presence does not prove NKE installs them.
type ReferencePolicy struct {
	ProtectedPrefixes []string
}

// ExternalOnlyReferenceConfig renders a test-only AuthenticationConfiguration
// with one external provider and the supplied reference protection policy.
func ExternalOnlyReferenceConfig(external ReferenceProvider, policy ReferencePolicy) ([]byte, error) {
	provider, err := referenceJWTProvider(external, &policy)
	if err != nil {
		return nil, err
	}
	return renderReferenceConfig([]apiserverv1.JWTAuthenticator{provider})
}

// CombinedReferenceConfig renders a test-only AuthenticationConfiguration with
// an established shared provider followed by one or more external providers.
// Reference protection rules apply only to the external providers.
func CombinedReferenceConfig(shared ReferenceProvider, externals []ReferenceProvider, policy ReferencePolicy) ([]byte, error) {
	if len(externals) == 0 {
		return nil, fmt.Errorf("at least one external provider is required")
	}
	sharedProvider, err := referenceJWTProvider(shared, nil)
	if err != nil {
		return nil, fmt.Errorf("shared provider: %w", err)
	}
	providers := make([]apiserverv1.JWTAuthenticator, 1, 1+len(externals))
	providers[0] = sharedProvider
	for index, external := range externals {
		externalProvider, externalErr := referenceJWTProvider(external, &policy)
		if externalErr != nil {
			return nil, fmt.Errorf("external provider %d: %w", index, externalErr)
		}
		providers = append(providers, externalProvider)
	}
	return renderReferenceConfig(providers)
}

func referenceJWTProvider(provider ReferenceProvider, policy *ReferencePolicy) (apiserverv1.JWTAuthenticator, error) {
	if err := provider.Metadata.Validate(); err != nil {
		return apiserverv1.JWTAuthenticator{}, fmt.Errorf("fixture metadata: %w", err)
	}
	if provider.UsernameClaim == "" {
		return apiserverv1.JWTAuthenticator{}, fmt.Errorf("username claim is required")
	}
	usernamePrefix := provider.UsernamePrefix
	jwtProvider := apiserverv1.JWTAuthenticator{
		Issuer: apiserverv1.Issuer{
			URL:                  provider.Metadata.IssuerURL,
			CertificateAuthority: provider.Metadata.TLSCAPEM,
			Audiences:            []string{provider.Metadata.AcceptedAudience},
		},
		ClaimMappings: apiserverv1.ClaimMappings{
			Username: apiserverv1.PrefixedClaimOrExpression{
				Claim:  provider.UsernameClaim,
				Prefix: &usernamePrefix,
			},
		},
	}
	if provider.GroupsClaim != "" {
		groupsPrefix := provider.GroupsPrefix
		jwtProvider.ClaimMappings.Groups = apiserverv1.PrefixedClaimOrExpression{
			Claim:  provider.GroupsClaim,
			Prefix: &groupsPrefix,
		}
	}

	requiredNames := make([]string, 0, len(provider.Metadata.ExpectedClaims.Required))
	for name := range provider.Metadata.ExpectedClaims.Required {
		requiredNames = append(requiredNames, name)
	}
	sort.Strings(requiredNames)
	for _, name := range requiredNames {
		jwtProvider.ClaimValidationRules = append(jwtProvider.ClaimValidationRules, apiserverv1.ClaimValidationRule{
			Claim:         name,
			RequiredValue: provider.Metadata.ExpectedClaims.Required[name],
		})
	}

	if policy != nil {
		for _, prefix := range policy.ProtectedPrefixes {
			if prefix == "" {
				return apiserverv1.JWTAuthenticator{}, fmt.Errorf("protected prefix must not be empty")
			}
			quoted := strconv.Quote(prefix)
			jwtProvider.UserValidationRules = append(jwtProvider.UserValidationRules,
				apiserverv1.UserValidationRule{
					Expression: "!user.username.startsWith(" + quoted + ")",
					Message:    "username uses a test-policy protected prefix",
				},
				apiserverv1.UserValidationRule{
					Expression: "user.groups.all(group, !group.startsWith(" + quoted + "))",
					Message:    "group uses a test-policy protected prefix",
				},
			)
		}
	}
	return jwtProvider, nil
}

func renderReferenceConfig(providers []apiserverv1.JWTAuthenticator) ([]byte, error) {
	config := apiserverv1.AuthenticationConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiserverv1.SchemeGroupVersion.String(),
			Kind:       "AuthenticationConfiguration",
		},
		JWT: providers,
	}
	rendered, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal test-only reference AuthenticationConfiguration: %w", err)
	}
	return rendered, nil
}
