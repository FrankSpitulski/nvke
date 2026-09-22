// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package envtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/issuer"
)

func TestStartFailsWhenAssetsAreUnset(t *testing.T) {
	t.Setenv(binaryAssetsEnv, "")
	_, err := Start(t, Config{AuthenticationConfig: testAuthenticationConfig(t)})
	require.Error(t, err)
	require.ErrorContains(t, err, binaryAssetsEnv+" is required")
}

func TestStartRequiresAuthenticationConfiguration(t *testing.T) {
	_, err := Start(t, Config{})
	require.Error(t, err)
	require.ErrorContains(t, err, "authentication configuration is required")
}

func TestDiagnosticsRedactsBearerToken(t *testing.T) {
	runner := &Runner{}
	_, err := runner.diagnostics.Write([]byte("Authorization: Bearer test-token\n"))
	require.NoError(t, err)
	require.Equal(t, "Authorization: Bearer REDACTED\n", runner.Diagnostics())
}

func TestProbeRequestUsesOnlyRunnerEndpointAndCA(t *testing.T) {
	runner := &Runner{adminConfig: &rest.Config{
		Host:            "https://127.0.0.1:6443",
		TLSClientConfig: rest.TLSClientConfig{CAData: []byte("runtime CA")},
	}}
	request := runner.ProbeRequest(contracts.ResourceRequest{Verb: "list", Resource: "configmaps", Namespace: "test"}, contracts.ResourceRequest{Verb: "list", Resource: "secrets", Namespace: "test"})
	require.Equal(t, "https://127.0.0.1:6443", request.EndpointURL)
	require.Equal(t, "runtime CA", request.ServerCAPEM)
}

func testProvider() issuer.ReferenceProvider {
	return issuer.ReferenceProvider{
		Metadata: contracts.FixtureMetadata{
			SchemaVersion:    contracts.SchemaVersion,
			IssuerURL:        "https://issuer.example.test",
			JWKSURL:          "https://issuer.example.test/jwks",
			TLSCAPEM:         "runtime-only CA",
			AcceptedAudience: "kubernetes",
			ExpectedClaims:   contracts.ExpectedClaims{Username: "alice@example.test"},
			CleanupOwner:     contracts.CleanupOwnerIssuer,
		},
		UsernameClaim: "username",
		GroupsClaim:   "groups",
	}
}

func testAuthenticationConfig(t *testing.T) []byte {
	t.Helper()
	configuration, err := issuer.ExternalOnlyReferenceConfig(testProvider(), issuer.ReferencePolicy{})
	require.NoError(t, err)
	return configuration
}
