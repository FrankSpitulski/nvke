// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

func TestLoadConfigRejectsUnknownAndTrailingInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "unknown field", content: `{"tokens":[{"name":"valid","lifetime":"5m"}],"secret":"must not be accepted"}`, want: "unknown field"},
		{name: "trailing JSON", content: `{"tokens":[{"name":"valid","lifetime":"5m"}]} {}`, want: "trailing JSON"},
		{name: "zero lifetime", content: `{"tokens":[{"name":"valid","lifetime":"0s"}]}`, want: "non-zero Go duration"},
		{name: "unsafe name", content: `{"tokens":[{"name":"../valid","lifetime":"5m"}]}`, want: "must match"},
		{name: "duplicate name", content: `{"tokens":[{"name":"valid","lifetime":"5m"},{"name":"valid","lifetime":"-5m"}]}`, want: "duplicate token name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(configPath, []byte(test.content), 0o600))
			_, err := loadConfig(configPath)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestCheckedInConfigDefinesRepresentativeTokens(t *testing.T) {
	t.Parallel()
	config, err := loadConfig("config.json")
	require.NoError(t, err)
	names := make([]string, 0, len(config.Tokens))
	for _, token := range config.Tokens {
		names = append(names, token.Name)
	}
	assert.ElementsMatch(t, []string{"valid", "invalid-signature", "expired", "wrong-audience", "wrong-issuer"}, names)
}

func TestRunCleansRuntimeArtifactsOnShutdown(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	config := processConfig{
		IssuerURL:     "https://127.0.0.1:8443",
		Audience:      "cluster-a",
		UsernameClaim: "username",
		GroupsClaim:   "groups",
		Username:      "alice@example.com",
		Tokens: []processToken{
			{Name: "valid", Lifetime: "5m"},
			{Name: "invalid-signature", Lifetime: "5m", UntrustedSigningKey: true},
		},
	}
	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, encoded, 0o600))
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	getenv := func(name string) string {
		switch name {
		case configPathEnvironment:
			return configPath
		case runtimeEnvironment:
			return runtimeDir
		case listenEnvironment:
			return "127.0.0.1:0"
		default:
			return ""
		}
	}
	runResult := make(chan error, 1)
	go func() { runResult <- run(ctx, getenv) }()
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(runtimeDir, "tokens", "valid"))
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-runResult)
	entries, err := os.ReadDir(runtimeDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRuntimeArtifactsArePrivateAndRemoved(t *testing.T) {
	t.Parallel()
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	metadata := contracts.FixtureMetadata{
		SchemaVersion:    contracts.SchemaVersion,
		IssuerURL:        "https://issuer.example.test",
		JWKSURL:          "https://issuer.example.test/jwks",
		TLSCAPEM:         "runtime-ca",
		AcceptedAudience: "cluster-a",
		ExpectedClaims: contracts.ExpectedClaims{
			Username: "alice@example.com",
		},
		CleanupOwner: contracts.CleanupOwnerIssuer,
	}
	artifacts, err := writeRuntimeArtifacts(runtimeDir, metadata, map[string]string{
		"valid":             "runtime-token",
		"invalid-signature": "other-runtime-token",
	}, []byte("reference-config"))
	require.NoError(t, err)
	require.Len(t, artifacts.files, 5)
	for _, artifactPath := range artifacts.files {
		info, statErr := os.Stat(artifactPath)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	metadataBytes, err := os.ReadFile(filepath.Join(runtimeDir, "metadata.json"))
	require.NoError(t, err)
	var writtenMetadata contracts.FixtureMetadata
	require.NoError(t, json.Unmarshal(metadataBytes, &writtenMetadata))
	assert.Equal(t, metadata.IssuerURL, writtenMetadata.IssuerURL)
	require.NoError(t, artifacts.cleanup())
	for _, artifactPath := range artifacts.files {
		_, statErr := os.Stat(artifactPath)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
	assert.DirExists(t, runtimeDir, "cleanup removes only files owned by the issuer")
}

func TestEnvironmentOrDefault(t *testing.T) {
	t.Parallel()
	values := map[string]string{"SET": "selected", "SPACE": "  "}
	getenv := func(name string) string { return values[name] }
	assert.Equal(t, "selected", environmentOrDefault(getenv, "SET", "fallback"))
	assert.Equal(t, "fallback", environmentOrDefault(getenv, "SPACE", "fallback"))
}
