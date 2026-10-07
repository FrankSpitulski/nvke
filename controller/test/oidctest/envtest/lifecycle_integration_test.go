// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build oidc_integration

package envtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/issuer"
)

const (
	assertionFailureEnvironment = "OIDC_ENVTEST_ASSERTION_FAILURE"
	assertionFailureMarkerEnv   = "OIDC_ENVTEST_ASSERTION_FAILURE_MARKER"
)

func TestEnvtestStartupFailureCleansUp(t *testing.T) {
	_, err := Start(t, Config{AuthenticationConfig: []byte("not: [valid")})
	require.Error(t, err)
	require.ErrorContains(t, err, "start OIDC envtest API server")
}

func TestEnvtestAssertionFailureCleansUp(t *testing.T) {
	if os.Getenv(assertionFailureEnvironment) == "1" {
		t.Cleanup(func() {
			markerPath := os.Getenv(assertionFailureMarkerEnv)
			require.NoError(t, os.WriteFile(markerPath, []byte("runner cleanup completed"), 0o600))
		})
		issuerServer := startLifecycleIssuer(t)
		_, err := Start(t, Config{AuthenticationConfig: lifecycleAuthenticationConfig(t, issuerServer)})
		require.NoError(t, err)
		t.Errorf("intentional assertion failure after runner startup")
		return
	}

	markerPath := filepath.Join(t.TempDir(), "cleanup-marker")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEnvtestAssertionFailureCleansUp$", "-test.v")
	command.Env = append(os.Environ(), assertionFailureEnvironment+"=1", assertionFailureMarkerEnv+"="+markerPath)
	output, err := command.CombinedOutput()
	require.Error(t, err)
	require.NoError(t, ctx.Err())
	require.Contains(t, string(output), "intentional assertion failure after runner startup")
	require.NotContains(t, string(output), "stopping OIDC envtest runner")
	marker, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	require.Equal(t, "runner cleanup completed", string(marker))
}

func startLifecycleIssuer(t *testing.T) *issuer.Server {
	t.Helper()
	server, err := issuer.Start(issuer.Config{
		Audience: "oidc-envtest-lifecycle",
		ExpectedClaims: contracts.ExpectedClaims{
			Username: "lifecycle@example.test",
			Groups:   []string{"operators"},
			Required: map[string]string{},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func lifecycleAuthenticationConfig(t *testing.T, server *issuer.Server) []byte {
	t.Helper()
	metadata := server.Metadata()
	configuration, err := kubeadmconfig.BuildCombinedAuthenticationConfig(
		metadata.AcceptedAudience,
		controllerconfig.ManagementOIDCAuthConfig{},
		nil,
		[]controllerconfig.OIDCProvider{{
			Name:          "lifecycle",
			IssuerURL:     metadata.IssuerURL,
			Audiences:     []string{metadata.AcceptedAudience},
			CABundle:      metadata.TLSCAPEM,
			UsernameClaim: "username",
			GroupsClaim:   "groups",
		}},
		kubeadmconfig.OperatorJWTPolicy{},
	)
	require.NoError(t, err)
	file, err := kubeadmconfig.GenerateAPIServerAuthenticationConfig(configuration, "v1.34.1")
	require.NoError(t, err)
	return file.Content
}
