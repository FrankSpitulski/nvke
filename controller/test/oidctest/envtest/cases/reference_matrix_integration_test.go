// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build oidc_integration

package cases

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
	oidcenvtest "github.com/dsx-ai-factory/nke/controller/test/oidctest/envtest"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/issuer"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/probe"
)

const (
	testAudience             = "oidc-snapshot-api"
	testKubernetesVersion    = "v1.34.1"
	managementTrustDomain    = "management.example.test"
	sharedSubject            = "spiffe://management.example.test/nke/controller"
	reservedExchangeSubject  = "spiffe://exchange.example.test/nke-service/capi-manager"
	unprivilegedNamespace    = "oidc-snapshot-unprivileged"
	operatorFixtureNamespace = "oidc-snapshot-operator"
)

func TestSnapshotOIDCAuthenticationMatrix(t *testing.T) {
	shared := startIssuer(t, "shared", sharedSubject, []string{"ignored-shared-group"}, "sub", "groups")
	direct := startIssuer(t, "direct", "external-operator", []string{"operators", "auditors"}, "username", "groups")
	mapped := startIssuer(t, "mapped", "external-control", []string{"operators"}, "username", "groups")

	providers := []controllerconfig.OIDCProvider{
		operatorProvider("direct", direct, "", ""),
		operatorProvider("mapped", mapped, "sys", "spi"),
	}
	configuration, err := kubeadmconfig.BuildCombinedAuthenticationConfig(
		testAudience,
		controllerconfig.ManagementOIDCAuthConfig{Enabled: true, IssuerURL: shared.Metadata().IssuerURL},
		[]byte(shared.Metadata().TLSCAPEM),
		providers,
		kubeadmconfig.OperatorJWTPolicy{
			ManagementJWTTrustDomain: managementTrustDomain,
			ReservedSubjects:         []string{reservedExchangeSubject},
		},
	)
	require.NoError(t, err)
	file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(configuration, testKubernetesVersion)
	require.NoError(t, err)
	runner := startRunner(t, file.Content)

	t.Run("SNAP-OIDC-001", func(t *testing.T) {
		result := runUnprivilegedProbe(t, runner, signToken(t, shared, explicitTokenSpec(shared, nil)))
		assertAcceptedIdentity(t, result, sharedSubject, []string{"system:authenticated"}, runner.Diagnostics())
		assertForbiddenPair(t, result, runner.Diagnostics())
	})

	t.Run("SNAP-OIDC-002", func(t *testing.T) {
		fixtureContext, fixtureCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer fixtureCancel()
		fixture, err := probe.PrepareAdminFixture(fixtureContext, runner.AdminClient(), operatorFixtureNamespace, "external-operator")
		require.NoError(t, err)
		t.Cleanup(func() {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			require.NoError(t, fixture.Cleanup(cleanupContext, runner.AdminClient()))
		})

		control := explicitTokenSpec(direct, map[string]any{
			"username": "external-operator",
			"groups":   []string{"operators", "auditors"},
		})
		controlResult := runProbe(t, runner, signToken(t, direct, control), fixture.AllowedRequest, fixture.DeniedRequest)
		assertAcceptedIdentity(t, controlResult, "external-operator", []string{"operators", "auditors", "system:authenticated"}, runner.Diagnostics())
		require.NotNil(t, controlResult.AllowedRequest, runner.Diagnostics())
		require.True(t, controlResult.AllowedRequest.Allowed, runner.Diagnostics())
		require.Equal(t, 200, controlResult.AllowedRequest.HTTPStatus, runner.Diagnostics())
		require.NotNil(t, controlResult.DeniedRequest, runner.Diagnostics())
		require.False(t, controlResult.DeniedRequest.Allowed, runner.Diagnostics())
		require.Equal(t, 403, controlResult.DeniedRequest.HTTPStatus, runner.Diagnostics())

		directProtected := []struct {
			name   string
			claims map[string]any
		}{
			{name: "reserved username prefix", claims: map[string]any{"username": "system:blocked", "groups": []string{"operators"}}},
			{name: "management trust-domain group", claims: map[string]any{"username": "external-operator", "groups": []string{"spiffe://management.example.test/nke/operators"}}},
			{name: "exact reserved subject", claims: map[string]any{"username": reservedExchangeSubject, "groups": []string{"operators"}}},
		}
		for _, test := range directProtected {
			t.Run(test.name, func(t *testing.T) {
				result := runProbe(t, runner, signToken(t, direct, explicitTokenSpec(direct, test.claims)), fixture.AllowedRequest, fixture.DeniedRequest)
				assertRejected(t, result, runner.Diagnostics())
			})
		}
	})

	t.Run("SNAP-OIDC-003", func(t *testing.T) {
		control := explicitTokenSpec(mapped, map[string]any{"username": "external-control", "groups": []string{"operators"}})
		controlResult := runUnprivilegedProbe(t, runner, signToken(t, mapped, control))
		assertAcceptedIdentity(t, controlResult, "sysexternal-control", []string{"spioperators", "system:authenticated"}, runner.Diagnostics())
		assertForbiddenPair(t, controlResult, runner.Diagnostics())

		mappedProtected := []struct {
			name   string
			claims map[string]any
		}{
			{name: "username split across mapping and claim", claims: map[string]any{"username": "tem:blocked", "groups": []string{"operators"}}},
			{name: "group split across mapping and claim", claims: map[string]any{"username": "external-control", "groups": []string{"ffe://management.example.test/nke/operators"}}},
		}
		for _, test := range mappedProtected {
			t.Run(test.name, func(t *testing.T) {
				result := runUnprivilegedProbe(t, runner, signToken(t, mapped, explicitTokenSpec(mapped, test.claims)))
				assertRejected(t, result, runner.Diagnostics())
			})
		}
	})
}

func startIssuer(t *testing.T, name, username string, groups []string, usernameClaim, groupsClaim string) *issuer.Server {
	t.Helper()
	server, err := issuer.Start(issuer.Config{
		Audience:      testAudience,
		UsernameClaim: usernameClaim,
		GroupsClaim:   groupsClaim,
		ExpectedClaims: contracts.ExpectedClaims{
			Username: username,
			Groups:   groups,
			Required: map[string]string{"fixture": name},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func operatorProvider(name string, server *issuer.Server, usernamePrefix, groupsPrefix string) controllerconfig.OIDCProvider {
	metadata := server.Metadata()
	return controllerconfig.OIDCProvider{
		Name:           name,
		IssuerURL:      metadata.IssuerURL,
		Audiences:      []string{testAudience},
		UsernameClaim:  "username",
		UsernamePrefix: usernamePrefix,
		GroupsClaim:    "groups",
		GroupsPrefix:   groupsPrefix,
		RequiredClaims: map[string]string{"fixture": name},
		CABundle:       metadata.TLSCAPEM,
	}
}

func startRunner(t *testing.T, authenticationConfiguration []byte) *oidcenvtest.Runner {
	t.Helper()
	runner, err := oidcenvtest.Start(t, oidcenvtest.Config{AuthenticationConfig: authenticationConfiguration})
	require.NoError(t, err, "KUBEBUILDER_ASSETS must contain setup-envtest Kubernetes 1.34.1 binaries")
	return runner
}

func explicitTokenSpec(server *issuer.Server, claims map[string]any) issuer.TokenSpec {
	metadata := server.Metadata()
	issuedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	return issuer.TokenSpec{
		Issuer:    metadata.IssuerURL,
		Audience:  []string{metadata.AcceptedAudience},
		Claims:    claims,
		IssuedAt:  issuedAt,
		NotBefore: issuedAt.Add(-time.Minute),
		ExpiresAt: issuedAt.Add(4 * time.Hour),
	}
}

func signToken(t *testing.T, server *issuer.Server, spec issuer.TokenSpec) string {
	t.Helper()
	token, err := server.SignToken(spec)
	require.NoError(t, err)
	return token
}

func assertRejected(t *testing.T, result contracts.ProbeResult, diagnostics string) {
	t.Helper()
	require.Equal(t, contracts.AuthenticationRejected, result.Authentication, diagnostics)
	require.Equal(t, 401, result.HTTPStatus, diagnostics)
	require.Empty(t, result.FailureKind, diagnostics)
}

func assertForbiddenPair(t *testing.T, result contracts.ProbeResult, diagnostics string) {
	t.Helper()
	require.NotNil(t, result.AllowedRequest, diagnostics)
	require.False(t, result.AllowedRequest.Allowed, diagnostics)
	require.Equal(t, 403, result.AllowedRequest.HTTPStatus, diagnostics)
	require.NotNil(t, result.DeniedRequest, diagnostics)
	require.False(t, result.DeniedRequest.Allowed, diagnostics)
	require.Equal(t, 403, result.DeniedRequest.HTTPStatus, diagnostics)
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
		contracts.ResourceRequest{Verb: "list", Resource: "configmaps", Namespace: unprivilegedNamespace},
		contracts.ResourceRequest{Verb: "list", Resource: "secrets", Namespace: unprivilegedNamespace},
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
