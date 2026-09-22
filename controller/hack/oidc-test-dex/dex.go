// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dex creates runtime-only Dex inputs for OIDC smoke tests.
package dex

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// Image pins Dex v2.45.1 by manifest digest.
	Image = "ghcr.io/dexidp/dex@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"

	passwordGrant = "password"
)

// Runtime holds generated test credentials. Never serialize or log it.
type Runtime struct {
	Username     string
	Password     string
	ClientID     string
	ClientSecret string
	Issuer       string
	CAPEM        string
}

// NewRuntime generates a local account, OAuth client, and TLS material for issuer.
func NewRuntime(issuer string) (Runtime, string, []byte, []byte, error) {
	parsedIssuer, err := url.ParseRequestURI(issuer)
	if err != nil {
		return Runtime{}, "", nil, nil, fmt.Errorf("parse issuer: %w", err)
	}
	if parsedIssuer.Scheme != "https" || parsedIssuer.Host == "" {
		return Runtime{}, "", nil, nil, fmt.Errorf("issuer must use HTTPS and include a host")
	}
	secret, err := randomText(32)
	if err != nil {
		return Runtime{}, "", nil, nil, err
	}
	password, err := randomText(32)
	if err != nil {
		return Runtime{}, "", nil, nil, err
	}
	passwordHash, err := bcryptHash(password)
	if err != nil {
		return Runtime{}, "", nil, nil, fmt.Errorf("hash password: %w", err)
	}
	cert, key, ca, err := certificate(issuer)
	if err != nil {
		return Runtime{}, "", nil, nil, err
	}
	runtime := Runtime{
		Username:     "dex-test-" + secret[:8] + "@example.test",
		Password:     password,
		ClientID:     "oidc-test",
		ClientSecret: secret,
		Issuer:       issuer,
		CAPEM:        string(ca),
	}
	// Dex v2.45.1 stores static client secrets as plaintext. Only the local
	// password database takes bcrypt hashes. The smoke test writes this config
	// to a temporary directory and removes it when the test ends.
	config := fmt.Sprintf(`issuer: %s
storage:
  type: memory
web:
  https: 0.0.0.0:5556
  tlsCert: /runtime/tls.crt
  tlsKey: /runtime/tls.key
oauth2:
  passwordConnector: local
enablePasswordDB: true
staticClients:
- id: %s
  secret: %s
  name: oidc-test
  redirectURIs: [https://localhost/callback]
staticPasswords:
- email: %s
  hash: %s
  username: dex-test-user
  userID: dex-test-user
`, issuer, runtime.ClientID, runtime.ClientSecret, runtime.Username, passwordHash)
	return runtime, config, cert, key, nil
}

// AcquireIDToken uses Dex's password grant without putting credentials in argv.
func AcquireIDToken(ctx context.Context, client *http.Client, runtime Runtime) (string, error) {
	values := url.Values{
		"grant_type":    {passwordGrant},
		"username":      {runtime.Username},
		"password":      {runtime.Password},
		"scope":         {"openid profile email groups"},
		"client_id":     {runtime.ClientID},
		"client_secret": {runtime.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runtime.Issuer+"/token", strings.NewReader(values.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var body struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.IDToken == "" {
		return "", fmt.Errorf("dex returned no ID token")
	}
	return body.IDToken, nil
}

// VerifyIDToken checks discovery, JWKS, signature, issuer, and audience.
func VerifyIDToken(ctx context.Context, client *http.Client, raw, issuer, audience string) error {
	discovery, err := getDiscovery(ctx, client, issuer)
	if err != nil {
		return err
	}
	if discovery.Issuer != issuer {
		return fmt.Errorf("discovery issuer %q does not match %q", discovery.Issuer, issuer)
	}
	keys, err := getKeys(ctx, client, discovery.JWKS)
	if err != nil {
		return err
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return fmt.Errorf("parse ID token: %w", err)
	}
	var claims jwt.Claims
	verified := false
	for _, key := range keys.Keys {
		if err := parsed.Claims(key.Key, &claims); err == nil {
			verified = true
			break
		}
	}
	if !verified {
		return fmt.Errorf("verify ID token signature")
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{audience}, Time: time.Now()}, time.Minute); err != nil {
		return fmt.Errorf("validate ID token claims: %w", err)
	}
	return nil
}

// VerifyRuntimeIDToken verifies a token and its generated account identity.
func VerifyRuntimeIDToken(ctx context.Context, client *http.Client, raw string, runtime Runtime) error {
	if err := VerifyIDToken(ctx, client, raw, runtime.Issuer, runtime.ClientID); err != nil {
		return err
	}
	keys, err := getKeys(ctx, client, runtime.Issuer+"/keys")
	if err != nil {
		return err
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return fmt.Errorf("parse ID token: %w", err)
	}
	var identity struct {
		Email string `json:"email"`
	}
	valid := false
	for _, key := range keys.Keys {
		if err := parsed.Claims(key.Key, &identity); err == nil {
			valid = true
			break
		}
	}
	if !valid || identity.Email != runtime.Username {
		return fmt.Errorf("ID token identity does not match the generated account")
	}
	return nil
}

type discoveryDocument struct {
	Issuer string `json:"issuer"`
	JWKS   string `json:"jwks_uri"`
}

func getDiscovery(ctx context.Context, client *http.Client, issuer string) (discoveryDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", http.NoBody)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("create discovery request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return discoveryDocument{}, fmt.Errorf("get discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return discoveryDocument{}, fmt.Errorf("discovery endpoint returned HTTP %d", resp.StatusCode)
	}
	var discovery discoveryDocument
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return discoveryDocument{}, fmt.Errorf("decode discovery response: %w", err)
	}
	if discovery.JWKS == "" {
		return discoveryDocument{}, fmt.Errorf("discovery response has no JWKS URI")
	}
	return discovery, nil
}

func getKeys(ctx context.Context, client *http.Client, jwksURL string) (jose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, http.NoBody)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("create JWKS request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("get JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jose.JSONWebKeySet{}, fmt.Errorf("JWKS endpoint returned HTTP %d", resp.StatusCode)
	}
	var keys jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("decode JWKS response: %w", err)
	}
	return keys, nil
}

func randomText(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return fmt.Sprintf("%x", bytes)[:length], nil
}

func bcryptHash(value string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", "import bcrypt,sys; print(bcrypt.hashpw(sys.stdin.buffer.read(), bcrypt.gensalt()).decode())")
	cmd.Stdin = strings.NewReader(value)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("generate bcrypt hash with python3 bcrypt: %w", err)
	}
	hash := strings.TrimSpace(string(output))
	return strings.Replace(hash, "$2b$", "$2a$", 1), nil
}

func certificate(rawIssuer string) ([]byte, []byte, []byte, error) {
	issuer, err := url.Parse(rawIssuer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse issuer: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: issuer.Hostname()},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{issuer.Hostname()},
	}
	if ip := net.ParseIP(issuer.Hostname()); ip != nil {
		template.IPAddresses = []net.IP{ip}
		template.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal TLS key: %w", err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}), cert, nil
}
