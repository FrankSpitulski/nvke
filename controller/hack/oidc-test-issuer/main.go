// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// oidc-test-issuer runs the controlled OIDC fixture for CAPD reference tests.
// It has no token-signing HTTP endpoint. A bounded startup token set is written
// to a private runtime volume instead.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
	"github.com/dsx-ai-factory/nke/controller/test/oidctest/issuer"
)

const (
	defaultConfigPath = "/etc/nke-oidc-test-issuer/config.json"
	defaultRuntimeDir = "/run/nke-oidc-test-issuer"
	defaultListen     = ":8443"

	configPathEnvironment = "NKE_OIDC_TEST_CONFIG"
	runtimeEnvironment    = "NKE_OIDC_TEST_RUNTIME_DIR"
	listenEnvironment     = "NKE_OIDC_TEST_LISTEN"
)

type processConfig struct {
	IssuerURL         string            `json:"issuer_url"`
	Audience          string            `json:"audience"`
	UsernameClaim     string            `json:"username_claim"`
	UsernamePrefix    string            `json:"username_prefix"`
	GroupsClaim       string            `json:"groups_claim"`
	GroupsPrefix      string            `json:"groups_prefix"`
	Username          string            `json:"username"`
	Groups            []string          `json:"groups"`
	RequiredClaims    map[string]string `json:"required_claims"`
	Tokens            []processToken    `json:"tokens"`
	ProtectedPrefixes []string          `json:"protected_prefixes"`
}

type processToken struct {
	Name                string         `json:"name"`
	Issuer              string         `json:"issuer,omitempty"`
	Audiences           []string       `json:"audiences,omitempty"`
	Claims              map[string]any `json:"claims,omitempty"`
	Lifetime            string         `json:"lifetime"`
	NotBeforeOffset     string         `json:"not_before_offset,omitempty"`
	UntrustedSigningKey bool           `json:"untrusted_signing_key,omitempty"`
}

var tokenNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		slog.Error("controlled issuer stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	configPath := environmentOrDefault(getenv, configPathEnvironment, defaultConfigPath)
	runtimeDir := environmentOrDefault(getenv, runtimeEnvironment, defaultRuntimeDir)
	listenAddress := environmentOrDefault(getenv, listenEnvironment, defaultListen)

	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fixture, err := issuer.New(issuer.Config{
		IssuerURL:     config.IssuerURL,
		Audience:      config.Audience,
		UsernameClaim: config.UsernameClaim,
		GroupsClaim:   config.GroupsClaim,
		ExpectedClaims: contracts.ExpectedClaims{
			Username: config.Username,
			Groups:   config.Groups,
			Required: config.RequiredClaims,
		},
	})
	if err != nil {
		return fmt.Errorf("create controlled issuer: %w", err)
	}
	tokens := make(map[string]string, len(config.Tokens))
	for _, configuredToken := range config.Tokens {
		lifetime, parseErr := time.ParseDuration(configuredToken.Lifetime)
		if parseErr != nil || lifetime == 0 {
			return fmt.Errorf("token %q lifetime must be a non-zero Go duration", configuredToken.Name)
		}
		var notBefore time.Time
		if configuredToken.NotBeforeOffset != "" {
			offset, offsetErr := time.ParseDuration(configuredToken.NotBeforeOffset)
			if offsetErr != nil {
				return fmt.Errorf("token %q not_before_offset must be a Go duration", configuredToken.Name)
			}
			notBefore = time.Now().Add(offset)
		}
		token, signErr := fixture.SignToken(issuer.TokenSpec{
			Issuer:              configuredToken.Issuer,
			Audience:            configuredToken.Audiences,
			Claims:              configuredToken.Claims,
			ExpiresAt:           time.Now().Add(lifetime),
			NotBefore:           notBefore,
			UntrustedSigningKey: configuredToken.UntrustedSigningKey,
		})
		if signErr != nil {
			return fmt.Errorf("sign token %q: %w", configuredToken.Name, signErr)
		}
		tokens[configuredToken.Name] = token
	}
	provider := issuer.ReferenceProvider{
		Metadata:       fixture.Metadata(),
		UsernameClaim:  config.UsernameClaim,
		UsernamePrefix: config.UsernamePrefix,
		GroupsClaim:    config.GroupsClaim,
		GroupsPrefix:   config.GroupsPrefix,
	}
	referenceConfig, err := issuer.ExternalOnlyReferenceConfig(provider, issuer.ReferencePolicy{ProtectedPrefixes: config.ProtectedPrefixes})
	if err != nil {
		return fmt.Errorf("render external-only reference config: %w", err)
	}
	artifacts, err := writeRuntimeArtifacts(runtimeDir, fixture.Metadata(), tokens, referenceConfig)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := artifacts.cleanup(); cleanupErr != nil {
			slog.Error("clean controlled issuer runtime artifacts", "err", cleanupErr)
		}
	}()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen for controlled issuer: %w", err)
	}
	server := &http.Server{
		Handler:           fixture.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         fixture.TLSConfig(),
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(tls.NewListener(listener, fixture.TLSConfig()))
	}()
	slog.Info("controlled issuer ready", "issuer", fixture.Metadata().IssuerURL, "listen", listenAddress)

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down controlled issuer: %w", err)
		}
		serveErr := <-serveErrors
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve controlled issuer: %w", serveErr)
		}
		return nil
	case serveErr := <-serveErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve controlled issuer: %w", serveErr)
	}
}

func loadConfig(configPath string) (processConfig, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return processConfig{}, fmt.Errorf("open controlled issuer config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var config processConfig
	if err := decoder.Decode(&config); err != nil {
		return processConfig{}, fmt.Errorf("decode controlled issuer config: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return processConfig{}, err
	}
	if len(config.Tokens) == 0 {
		return processConfig{}, fmt.Errorf("at least one token spec is required")
	}
	seenNames := make(map[string]struct{}, len(config.Tokens))
	for _, token := range config.Tokens {
		if !tokenNamePattern.MatchString(token.Name) {
			return processConfig{}, fmt.Errorf("token name %q must match %s", token.Name, tokenNamePattern)
		}
		if _, found := seenNames[token.Name]; found {
			return processConfig{}, fmt.Errorf("duplicate token name %q", token.Name)
		}
		seenNames[token.Name] = struct{}{}
		if lifetime, parseErr := time.ParseDuration(token.Lifetime); parseErr != nil || lifetime == 0 {
			return processConfig{}, fmt.Errorf("token %q lifetime must be a non-zero Go duration", token.Name)
		}
	}
	return config, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("controlled issuer config contains trailing JSON")
		}
		return fmt.Errorf("decode trailing controlled issuer config: %w", err)
	}
	return nil
}

type runtimeArtifacts struct {
	files []string
	dirs  []string
}

func writeRuntimeArtifacts(
	runtimeDir string,
	metadata contracts.FixtureMetadata,
	tokens map[string]string,
	referenceConfig []byte,
) (runtimeArtifacts, error) {
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return runtimeArtifacts{}, fmt.Errorf("create controlled issuer runtime directory: %w", err)
	}
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return runtimeArtifacts{}, fmt.Errorf("marshal controlled issuer metadata: %w", err)
	}
	metadataJSON = append(metadataJSON, '\n')

	artifacts := runtimeArtifacts{}
	files := []struct {
		name    string
		content []byte
	}{
		{name: "metadata.json", content: metadataJSON},
		{name: "ca.crt", content: []byte(metadata.TLSCAPEM)},
		{name: "external-only-authentication-config.yaml", content: referenceConfig},
	}
	for _, file := range files {
		filePath := filepath.Join(runtimeDir, file.name)
		if err := writePrivateFile(filePath, file.content); err != nil {
			_ = artifacts.cleanup()
			return runtimeArtifacts{}, err
		}
		artifacts.files = append(artifacts.files, filePath)
	}
	tokenDir := filepath.Join(runtimeDir, "tokens")
	if err := os.RemoveAll(tokenDir); err != nil {
		_ = artifacts.cleanup()
		return runtimeArtifacts{}, fmt.Errorf("remove stale private token directory: %w", err)
	}
	if err := os.Mkdir(tokenDir, 0o700); err != nil {
		_ = artifacts.cleanup()
		return runtimeArtifacts{}, fmt.Errorf("create private token directory: %w", err)
	}
	artifacts.dirs = append(artifacts.dirs, tokenDir)
	for name, token := range tokens {
		tokenPath := filepath.Join(tokenDir, name)
		if err := writePrivateFile(tokenPath, []byte(token)); err != nil {
			_ = artifacts.cleanup()
			return runtimeArtifacts{}, err
		}
		artifacts.files = append(artifacts.files, tokenPath)
	}
	return artifacts, nil
}

func writePrivateFile(filePath string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".oidc-test-issuer-*")
	if err != nil {
		return fmt.Errorf("create private runtime artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect private runtime artifact: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write private runtime artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close private runtime artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return fmt.Errorf("publish private runtime artifact: %w", err)
	}
	return nil
}

func (a runtimeArtifacts) cleanup() error {
	var failureCount int
	for _, artifactPath := range a.files {
		if err := os.Remove(artifactPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failureCount++
		}
	}
	for index := len(a.dirs) - 1; index >= 0; index-- {
		if err := os.Remove(a.dirs[index]); err != nil && !errors.Is(err, os.ErrNotExist) {
			failureCount++
		}
	}
	if failureCount != 0 {
		return fmt.Errorf("remove %d controlled issuer runtime artifacts", failureCount)
	}
	return nil
}

func environmentOrDefault(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}
