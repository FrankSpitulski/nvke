//go:build oidc_integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cases

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
	oidcenvtest "github.com/dsx-ai-factory/nke/controller/test/oidctest/envtest"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/issuer"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/probe"
)

const (
	testAudience          = "oidc-reference-api"
	protectedSPIFFEPrefix = "spiffe://platform.example.test/"
)

var protectedPrefixes = []string{
	"system:",
	"kubeadm:",
	"nke:",
	"nvidia:",
	protectedSPIFFEPrefix,
}

func TestReferenceOIDCAuthenticationMatrix(t *testing.T) {
	sharedUsername := protectedSPIFFEPrefix + "controller"
	sharedGroups := make([]string, 0, len(protectedPrefixes))
	for _, prefix := range protectedPrefixes {
		sharedGroups = append(sharedGroups, prefix+"reference-platform")
	}
	shared := startIssuer(t, sharedUsername, sharedGroups)
	direct := startIssuer(t, "external-operator", []string{"operators", "auditors", "viewers"})

	splitIssuers := make([]*issuer.Server, 0, len(protectedPrefixes))
	externals := []issuer.ReferenceProvider{referenceProvider(direct)}
	for _, prefix := range protectedPrefixes {
		mappingPrefix, _ := splitPrefix(prefix)
		split := startIssuer(t, "external-control", []string{"operators", "auditors", "viewers"})
		splitIssuers = append(splitIssuers, split)
		externals = append(externals, issuer.ReferenceProvider{
			Metadata:       split.Metadata(),
			UsernameClaim:  "username",
			UsernamePrefix: mappingPrefix,
			GroupsClaim:    "groups",
			GroupsPrefix:   mappingPrefix,
		})
	}

	configuration, err := issuer.CombinedReferenceConfig(
		referenceProvider(shared),
		externals,
		issuer.ReferencePolicy{ProtectedPrefixes: protectedPrefixes},
	)
	require.NoError(t, err)
	runner := startRunner(t, configuration)

	t.Run("REF-OIDC-003", func(t *testing.T) {
		token := signToken(t, shared, explicitTokenSpec(shared, nil))
		assertAcceptedThenForbidden(t, runner, token, sharedUsername, append(sharedGroups, "system:authenticated"))
	})

	t.Run("REF-OIDC-030", func(t *testing.T) {
		base := explicitTokenSpec(direct, map[string]any{
			"username": "external-operator",
			"groups":   []string{"operators", "auditors", "viewers"},
		})
		for index, prefix := range protectedPrefixes {
			t.Run(caseName(prefix), func(t *testing.T) {
				negative := mutateTokenSpec(base, func(spec *issuer.TokenSpec) {
					spec.Claims["username"] = prefix + "blocked-user"
				})
				if index == 0 {
					assertRBACControlAndRejection(t, runner, direct, base, negative)
					return
				}
				assertControlAndRejection(t, runner, direct, base, negative,
					"external-operator", []string{"operators", "auditors", "viewers", "system:authenticated"})
			})
		}
	})

	t.Run("REF-OIDC-031", func(t *testing.T) {
		base := explicitTokenSpec(direct, map[string]any{
			"username": "external-operator",
			"groups":   []string{"operators", "auditors", "viewers"},
		})
		for index, prefix := range protectedPrefixes {
			t.Run(caseName(prefix), func(t *testing.T) {
				for position := range 3 {
					t.Run(fmt.Sprintf("group-position-%d", position), func(t *testing.T) {
						negative := mutateTokenSpec(base, func(spec *issuer.TokenSpec) {
							groups := []string{"operators", "auditors", "viewers"}
							groups[position] = prefix + "blocked-group"
							spec.Claims["groups"] = groups
						})
						assertControlAndRejection(t, runner, direct, base, negative,
							"external-operator", []string{"operators", "auditors", "viewers", "system:authenticated"})
					})
				}
				testSplitProtectedIdentity(t, runner, splitIssuers[index], prefix)
			})
		}
	})
}

func testSplitProtectedIdentity(t *testing.T, runner *oidcenvtest.Runner, server *issuer.Server, prefix string) {
	t.Helper()
	mappingPrefix, claimPrefix := splitPrefix(prefix)
	base := explicitTokenSpec(server, map[string]any{
		"username": "external-control",
		"groups":   []string{"operators", "auditors", "viewers"},
	})
	controlUsername := mappingPrefix + "external-control"
	controlGroups := []string{
		mappingPrefix + "operators",
		mappingPrefix + "auditors",
		mappingPrefix + "viewers",
		"system:authenticated",
	}

	t.Run("split-username-mapping-and-claim", func(t *testing.T) {
		negative := mutateTokenSpec(base, func(spec *issuer.TokenSpec) {
			spec.Claims["username"] = claimPrefix + "blocked-user"
		})
		assertControlAndRejection(t, runner, server, base, negative, controlUsername, controlGroups)
	})
	t.Run("split-group-mapping-and-claim", func(t *testing.T) {
		negative := mutateTokenSpec(base, func(spec *issuer.TokenSpec) {
			spec.Claims["groups"] = []string{"operators", claimPrefix + "blocked-group", "viewers"}
		})
		assertControlAndRejection(t, runner, server, base, negative, controlUsername, controlGroups)
	})
}

func startIssuer(t *testing.T, username string, groups []string) *issuer.Server {
	t.Helper()
	server, err := issuer.Start(issuer.Config{
		Audience: testAudience,
		ExpectedClaims: contracts.ExpectedClaims{
			Username: username,
			Groups:   groups,
			Required: map[string]string{},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func referenceProvider(server *issuer.Server) issuer.ReferenceProvider {
	return issuer.ReferenceProvider{
		Metadata:      server.Metadata(),
		UsernameClaim: "username",
		GroupsClaim:   "groups",
	}
}

func startRunner(t *testing.T, authenticationConfiguration []byte) *oidcenvtest.Runner {
	t.Helper()
	runner, err := oidcenvtest.Start(t, oidcenvtest.Config{AuthenticationConfig: authenticationConfiguration})
	require.NoError(t, err, "KUBEBUILDER_ASSETS must contain setup-envtest 1.34.1 binaries")
	return runner
}

func explicitTokenSpec(server *issuer.Server, claims map[string]any) issuer.TokenSpec {
	metadata := server.Metadata()
	issuedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	return issuer.TokenSpec{
		Issuer:    metadata.IssuerURL,
		Audience:  []string{metadata.AcceptedAudience},
		Claims:    cloneClaims(claims),
		IssuedAt:  issuedAt,
		NotBefore: issuedAt.Add(-time.Minute),
		ExpiresAt: issuedAt.Add(4 * time.Hour),
	}
}

func mutateTokenSpec(base issuer.TokenSpec, mutate func(*issuer.TokenSpec)) issuer.TokenSpec {
	cloned := base
	cloned.Audience = append([]string{}, base.Audience...)
	cloned.Claims = cloneClaims(base.Claims)
	mutate(&cloned)
	return cloned
}

func cloneClaims(claims map[string]any) map[string]any {
	cloned := make(map[string]any, len(claims))
	for name, value := range claims {
		if stringsValue, ok := value.([]string); ok {
			cloned[name] = append([]string{}, stringsValue...)
			continue
		}
		cloned[name] = value
	}
	return cloned
}

func signToken(t *testing.T, server *issuer.Server, spec issuer.TokenSpec) string {
	t.Helper()
	token, err := server.SignToken(spec)
	require.NoError(t, err)
	return token
}

func assertRBACControlAndRejection(
	t *testing.T,
	runner *oidcenvtest.Runner,
	server *issuer.Server,
	controlSpec issuer.TokenSpec,
	negativeSpec issuer.TokenSpec,
) {
	t.Helper()
	fixtureContext, fixtureCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer fixtureCancel()
	fixture, err := probe.PrepareAdminFixture(fixtureContext, runner.AdminClient(), "oidc-ref-control", "external-operator")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		require.NoError(t, fixture.Cleanup(cleanupContext, runner.AdminClient()))
	})

	controlResult := runProbe(t, runner, signToken(t, server, controlSpec), fixture.AllowedRequest, fixture.DeniedRequest)
	assertAcceptedIdentity(t, controlResult, "external-operator", []string{"operators", "auditors", "viewers", "system:authenticated"}, runner.Diagnostics())
	require.NotNil(t, controlResult.AllowedRequest)
	require.True(t, controlResult.AllowedRequest.Allowed)
	require.Equal(t, 200, controlResult.AllowedRequest.HTTPStatus)
	require.NotNil(t, controlResult.DeniedRequest)
	require.False(t, controlResult.DeniedRequest.Allowed)
	require.Equal(t, 403, controlResult.DeniedRequest.HTTPStatus)

	assertRejected(t, runProbe(t, runner, signToken(t, server, negativeSpec), fixture.AllowedRequest, fixture.DeniedRequest), runner.Diagnostics())
}

func assertControlAndRejection(
	t *testing.T,
	runner *oidcenvtest.Runner,
	server *issuer.Server,
	controlSpec issuer.TokenSpec,
	negativeSpec issuer.TokenSpec,
	controlUsername string,
	controlGroups []string,
) {
	t.Helper()
	controlResult := runUnprivilegedProbe(t, runner, signToken(t, server, controlSpec))
	assertAcceptedIdentity(t, controlResult, controlUsername, controlGroups, runner.Diagnostics())
	require.NotNil(t, controlResult.AllowedRequest)
	require.False(t, controlResult.AllowedRequest.Allowed)
	require.Equal(t, 403, controlResult.AllowedRequest.HTTPStatus)

	assertRejected(t, runUnprivilegedProbe(t, runner, signToken(t, server, negativeSpec)), runner.Diagnostics())
}

func assertRejected(t *testing.T, result contracts.ProbeResult, diagnostics string) {
	t.Helper()
	require.Equal(t, contracts.AuthenticationRejected, result.Authentication, diagnostics)
	require.Equal(t, 401, result.HTTPStatus, diagnostics)
	require.Empty(t, result.FailureKind, diagnostics)
}

func assertAcceptedThenForbidden(t *testing.T, runner *oidcenvtest.Runner, token, username string, groups []string) {
	t.Helper()
	result := runUnprivilegedProbe(t, runner, token)
	assertAcceptedIdentity(t, result, username, groups, runner.Diagnostics())
	require.NotNil(t, result.AllowedRequest)
	require.False(t, result.AllowedRequest.Allowed)
	require.Equal(t, 403, result.AllowedRequest.HTTPStatus)
	require.NotNil(t, result.DeniedRequest)
	require.False(t, result.DeniedRequest.Allowed)
	require.Equal(t, 403, result.DeniedRequest.HTTPStatus)
}

func assertAcceptedIdentity(t *testing.T, result contracts.ProbeResult, username string, groups []string, diagnostics string) {
	t.Helper()
	require.Equal(t, contracts.AuthenticationAccepted, result.Authentication, diagnostics)
	require.Empty(t, result.FailureKind, diagnostics)
	require.Equal(t, username, result.Username)
	require.Equal(t, groups, result.Groups)
}

func runUnprivilegedProbe(t *testing.T, runner *oidcenvtest.Runner, token string) contracts.ProbeResult {
	t.Helper()
	return runProbe(t, runner, token,
		contracts.ResourceRequest{Verb: "list", Resource: "configmaps", Namespace: "oidc-reference-unprivileged"},
		contracts.ResourceRequest{Verb: "list", Resource: "secrets", Namespace: "oidc-reference-unprivileged"},
	)
}

func runProbe(
	t *testing.T,
	runner *oidcenvtest.Runner,
	token string,
	allowed contracts.ResourceRequest,
	denied contracts.ResourceRequest,
) contracts.ProbeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := probe.Probe(ctx, runner.ProbeRequest(allowed, denied), contracts.TokenInput{Reader: strings.NewReader(token)})
	require.NotEqual(t, contracts.ProbeFailureSetup, result.FailureKind, result.FailureDetail)
	require.NotEqual(t, contracts.ProbeFailureTransport, result.FailureKind, result.FailureDetail)
	return result
}

func splitPrefix(prefix string) (string, string) {
	index := len(prefix) / 2
	return prefix[:index], prefix[index:]
}

func caseName(prefix string) string {
	name := strings.TrimSuffix(prefix, ":")
	name = strings.TrimSuffix(name, "/")
	name = strings.ReplaceAll(name, "://", "-")
	name = strings.ReplaceAll(name, ".", "-")
	return name
}
