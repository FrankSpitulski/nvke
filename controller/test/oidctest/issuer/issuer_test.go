// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issuer

import (
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

func TestControlledIssuerPublishesTLSVerifiedDiscoveryAndJWKS(t *testing.T) {
	t.Parallel()
	server := startTestServer(t, Config{
		Audience: "cluster-a",
		ExpectedClaims: contracts.ExpectedClaims{
			Username: "alice@example.com",
			Groups:   []string{"operators", "readers"},
			Required: map[string]string{"tenant": "green"},
		},
	})
	metadata := server.Metadata()
	require.NoError(t, metadata.Validate())

	untrustedClient := &http.Client{Timeout: 5 * time.Second}
	untrustedRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, metadata.IssuerURL+"/.well-known/openid-configuration", http.NoBody)
	require.NoError(t, err)
	untrustedResponse, err := untrustedClient.Do(untrustedRequest)
	if untrustedResponse != nil {
		untrustedResponse.Body.Close()
	}
	require.Error(t, err)

	client, err := server.Client()
	require.NoError(t, err)
	discoveryResponse := get(t, client, metadata.IssuerURL+"/.well-known/openid-configuration")
	defer discoveryResponse.Body.Close()
	require.Equal(t, http.StatusOK, discoveryResponse.StatusCode)
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwks_uri"`
	}
	require.NoError(t, json.NewDecoder(discoveryResponse.Body).Decode(&discovery))
	assert.Equal(t, metadata.IssuerURL, discovery.Issuer)
	assert.Equal(t, metadata.JWKSURL, discovery.JWKSURL)

	jwksResponse := get(t, client, metadata.JWKSURL)
	defer jwksResponse.Body.Close()
	require.Equal(t, http.StatusOK, jwksResponse.StatusCode)
	var keySet jose.JSONWebKeySet
	require.NoError(t, json.NewDecoder(jwksResponse.Body).Decode(&keySet))
	require.Len(t, keySet.Keys, 1)
	assert.IsType(t, &ecdsa.PublicKey{}, keySet.Keys[0].Key)
	assert.True(t, keySet.Keys[0].IsPublic())

	readinessResponse := get(t, client, metadata.IssuerURL+readinessPath)
	defer readinessResponse.Body.Close()
	assert.Equal(t, http.StatusOK, readinessResponse.StatusCode)

	tokenRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, metadata.IssuerURL+"/token", http.NoBody)
	require.NoError(t, err)
	tokenRequest.Header.Set("Content-Type", "application/json")
	tokenResponse, err := client.Do(tokenRequest)
	require.NoError(t, err)
	defer tokenResponse.Body.Close()
	assert.Equal(t, http.StatusNotFound, tokenResponse.StatusCode, "the fixture must not expose a signing endpoint")
}

func TestSignedTokensSupportClaimsExpiryAndUntrustedKey(t *testing.T) {
	t.Parallel()
	server := startTestServer(t, Config{
		Audience:      "cluster-a",
		UsernameClaim: "preferred_username",
		GroupsClaim:   "roles",
		ExpectedClaims: contracts.ExpectedClaims{
			Username: "alice@example.com",
			Groups:   []string{"operators"},
			Required: map[string]string{"tenant": "green"},
		},
	})
	metadata := server.Metadata()
	client, err := server.Client()
	require.NoError(t, err)
	keySet := fetchKeySet(t, client, metadata.JWKSURL)

	expiresAt := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	validToken, err := server.SignToken(TokenSpec{
		ExpiresAt: expiresAt,
		Claims: map[string]any{
			"preferred_username": "selected@example.com",
			"roles":              []string{"selected-a", "selected-b"},
			"custom":             "selected-value",
		},
	})
	require.NoError(t, err)
	claims := verifyToken(t, validToken, keySet, metadata.IssuerURL, metadata.AcceptedAudience)
	assert.Equal(t, metadata.IssuerURL, claims["iss"])
	assert.Equal(t, "selected@example.com", claims["preferred_username"])
	assert.ElementsMatch(t, []any{"selected-a", "selected-b"}, claims["roles"])
	assert.Equal(t, "green", claims["tenant"])
	assert.Equal(t, "selected-value", claims["custom"])
	assert.EqualValues(t, expiresAt.Unix(), claims["exp"])

	invalidToken, err := server.SignToken(TokenSpec{UntrustedSigningKey: true})
	require.NoError(t, err)
	invalidParsed, err := jwt.Parse(invalidToken, func(token *jwt.Token) (any, error) {
		keyID, _ := token.Header["kid"].(string)
		keys := keySet.Key(keyID)
		require.Len(t, keys, 1, "the invalid token must select the advertised key and fail cryptographic verification")
		return keys[0].Key, nil
	}, jwt.WithValidMethods([]string{"ES256"}))
	require.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)
	require.NotNil(t, invalidParsed)
	assert.False(t, invalidParsed.Valid)
	_, err = parseToken(invalidToken, keySet, metadata.IssuerURL, metadata.AcceptedAudience)
	require.ErrorIs(t, err, jwt.ErrTokenSignatureInvalid)

	expiredToken, err := server.SignToken(TokenSpec{ExpiresAt: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	_, err = parseToken(expiredToken, keySet, metadata.IssuerURL, metadata.AcceptedAudience)
	require.ErrorIs(t, err, jwt.ErrTokenExpired)

	overriddenToken, err := server.SignToken(TokenSpec{
		Issuer:   "https://wrong-issuer.example.test",
		Audience: []string{"wrong-audience"},
	})
	require.NoError(t, err)
	_, err = parseToken(overriddenToken, keySet, metadata.IssuerURL, metadata.AcceptedAudience)
	require.Error(t, err)
}

func TestDistinctServersUseDistinctIssuersAndKeys(t *testing.T) {
	t.Parallel()
	first := startTestServer(t, defaultTestConfig())
	second := startTestServer(t, defaultTestConfig())
	require.NotEqual(t, first.Metadata().IssuerURL, second.Metadata().IssuerURL)

	firstToken, err := first.SignToken(TokenSpec{})
	require.NoError(t, err)
	firstClient, err := first.Client()
	require.NoError(t, err)
	firstKeys := fetchKeySet(t, firstClient, first.Metadata().JWKSURL)
	firstClaims := verifyToken(t, firstToken, firstKeys, first.Metadata().IssuerURL, first.Metadata().AcceptedAudience)
	assert.Equal(t, first.Metadata().IssuerURL, firstClaims["iss"])

	secondToken, err := second.SignToken(TokenSpec{})
	require.NoError(t, err)
	secondClient, err := second.Client()
	require.NoError(t, err)
	secondKeys := fetchKeySet(t, secondClient, second.Metadata().JWKSURL)
	secondClaims := verifyToken(t, secondToken, secondKeys, second.Metadata().IssuerURL, second.Metadata().AcceptedAudience)
	assert.Equal(t, second.Metadata().IssuerURL, secondClaims["iss"])
	_, err = parseToken(firstToken, secondKeys, first.Metadata().IssuerURL, first.Metadata().AcceptedAudience)
	require.Error(t, err)
}

func TestServerCloseReleasesListener(t *testing.T) {
	server, err := Start(defaultTestConfig())
	require.NoError(t, err)
	client, err := server.Client()
	require.NoError(t, err)
	issuerURL := server.Metadata().IssuerURL
	require.NoError(t, server.Close())
	require.NoError(t, server.Close())

	request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, issuerURL+readinessPath, http.NoBody)
	require.NoError(t, requestErr)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
}

func TestSignTokenRejectsRegisteredClaimOverride(t *testing.T) {
	t.Parallel()
	server := startTestServer(t, defaultTestConfig())
	_, err := server.SignToken(TokenSpec{Claims: map[string]any{"iss": "hidden override"}})
	require.ErrorContains(t, err, "must use its TokenSpec field")
}

func startTestServer(t *testing.T, config Config) *Server {
	t.Helper()
	server, err := Start(config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	return server
}

func defaultTestConfig() Config {
	return Config{
		Audience: "cluster-a",
		ExpectedClaims: contracts.ExpectedClaims{
			Username: "alice@example.com",
			Groups:   []string{"operators"},
			Required: map[string]string{"tenant": "green"},
		},
	}
}

func get(t *testing.T, client *http.Client, requestURL string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, requestURL, http.NoBody)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	return response
}

func fetchKeySet(t *testing.T, client *http.Client, jwksURL string) jose.JSONWebKeySet {
	t.Helper()
	response := get(t, client, jwksURL)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var keySet jose.JSONWebKeySet
	require.NoError(t, json.Unmarshal(data, &keySet))
	return keySet
}

func verifyToken(t *testing.T, raw string, keySet jose.JSONWebKeySet, expectedIssuer, expectedAudience string) jwt.MapClaims {
	t.Helper()
	claims, err := parseToken(raw, keySet, expectedIssuer, expectedAudience)
	require.NoError(t, err)
	return claims
}

func parseToken(raw string, keySet jose.JSONWebKeySet, expectedIssuer, expectedAudience string) (jwt.MapClaims, error) {
	parsed, err := jwt.Parse(raw, func(token *jwt.Token) (any, error) {
		keyID, _ := token.Header["kid"].(string)
		keys := keySet.Key(keyID)
		if len(keys) != 1 {
			return nil, assert.AnError
		}
		return keys[0].Key, nil
	},
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithIssuer(expectedIssuer),
		jwt.WithAudience(expectedAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, assert.AnError
	}
	return claims, nil
}
