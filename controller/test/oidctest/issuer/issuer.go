// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package issuer provides a controlled, test-only OIDC issuer. It creates all
// signing keys and TLS material at runtime and exposes only discovery, JWKS, and
// readiness endpoints.
package issuer

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	jwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

const (
	defaultListenAddress = "127.0.0.1:0"
	defaultTokenLifetime = 10 * time.Minute
	keyUse               = "sig"
	readinessPath        = "/readyz"
)

// Config defines one controlled issuer. IssuerURL may be empty only when Start
// creates an in-process server and assigns its loopback URL.
type Config struct {
	IssuerURL       string
	Audience        string
	ExpectedClaims  contracts.ExpectedClaims
	UsernameClaim   string
	GroupsClaim     string
	AdditionalNames []string
	ListenAddress   string
}

// TokenSpec selects the registered and private claims for one signed token.
// Empty registered values use the fixture defaults. Claims must not replace
// iss, aud, exp, iat, or nbf.
type TokenSpec struct {
	Issuer              string
	Audience            []string
	Claims              map[string]any
	ExpiresAt           time.Time
	IssuedAt            time.Time
	NotBefore           time.Time
	UntrustedSigningKey bool
}

// Fixture owns one ephemeral signing key pair and TLS certificate chain.
type Fixture struct {
	metadata        contracts.FixtureMetadata
	defaultClaims   map[string]any
	trustedSigner   jose.Signer
	untrustedSigner jose.Signer
	publicKey       jose.JSONWebKey
	tlsCertificate  tls.Certificate
	handler         http.Handler
}

// New generates a controlled issuer with runtime-only signing and TLS keys.
func New(config Config) (*Fixture, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}

	issuerURL, err := url.Parse(config.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("parse issuer URL: %w", err)
	}
	trustedSigner, publicKey, err := newSigner("")
	if err != nil {
		return nil, fmt.Errorf("create trusted signer: %w", err)
	}
	untrustedSigner, _, err := newSigner(publicKey.KeyID)
	if err != nil {
		return nil, fmt.Errorf("create untrusted signer: %w", err)
	}
	tlsCertificate, caPEM, err := newTLSCertificate(issuerURL.Hostname(), config.AdditionalNames)
	if err != nil {
		return nil, err
	}

	usernameClaim := config.UsernameClaim
	if usernameClaim == "" {
		usernameClaim = "username"
	}
	groupsClaim := config.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	defaultClaims := make(map[string]any, len(config.ExpectedClaims.Required)+2)
	defaultClaims[usernameClaim] = config.ExpectedClaims.Username
	defaultClaims[groupsClaim] = slices.Clone(config.ExpectedClaims.Groups)
	for name, value := range config.ExpectedClaims.Required {
		defaultClaims[name] = value
	}

	fixture := &Fixture{
		metadata: contracts.FixtureMetadata{
			SchemaVersion:    contracts.SchemaVersion,
			IssuerURL:        config.IssuerURL,
			JWKSURL:          strings.TrimSuffix(config.IssuerURL, "/") + "/jwks",
			TLSCAPEM:         string(caPEM),
			AcceptedAudience: config.Audience,
			ExpectedClaims: contracts.ExpectedClaims{
				Username: config.ExpectedClaims.Username,
				Groups:   slices.Clone(config.ExpectedClaims.Groups),
				Required: cloneStringMap(config.ExpectedClaims.Required),
			},
			CleanupOwner: contracts.CleanupOwnerIssuer,
		},
		defaultClaims:   defaultClaims,
		trustedSigner:   trustedSigner,
		untrustedSigner: untrustedSigner,
		publicKey:       publicKey,
		tlsCertificate:  tlsCertificate,
	}
	fixture.handler = fixture.newHandler(issuerURL.Path)
	return fixture, nil
}

// Metadata returns public fixture facts. The result contains no token or key.
func (f *Fixture) Metadata() contracts.FixtureMetadata {
	metadata := f.metadata
	metadata.ExpectedClaims.Groups = slices.Clone(f.metadata.ExpectedClaims.Groups)
	metadata.ExpectedClaims.Required = cloneStringMap(f.metadata.ExpectedClaims.Required)
	return metadata
}

// Handler returns the discovery, JWKS, and readiness HTTP handler.
func (f *Fixture) Handler() http.Handler {
	return f.handler
}

// TLSConfig returns a TLS configuration containing the ephemeral server
// certificate. Callers may use it with tls.NewListener.
func (f *Fixture) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{f.tlsCertificate},
	}
}

// SignToken signs a token with the trusted key, or with a second key omitted
// from JWKS when UntrustedSigningKey is true.
func (f *Fixture) SignToken(spec TokenSpec) (string, error) {
	for name := range spec.Claims {
		if isRegisteredClaim(name) {
			return "", fmt.Errorf("claim %q must use its TokenSpec field", name)
		}
	}

	now := time.Now().UTC()
	issuerValue := spec.Issuer
	if issuerValue == "" {
		issuerValue = f.metadata.IssuerURL
	}
	audience := slices.Clone(spec.Audience)
	if len(audience) == 0 {
		audience = []string{f.metadata.AcceptedAudience}
	}
	expiresAt := spec.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(defaultTokenLifetime)
	}
	issuedAt := spec.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = now
	}

	claims := cloneAnyMap(f.defaultClaims)
	for name, value := range spec.Claims {
		claims[name] = value
	}
	claims["iss"] = issuerValue
	claims["aud"] = audience
	claims["exp"] = expiresAt.Unix()
	claims["iat"] = issuedAt.Unix()
	if !spec.NotBefore.IsZero() {
		claims["nbf"] = spec.NotBefore.Unix()
	}

	signer := f.trustedSigner
	if spec.UntrustedSigningKey {
		signer = f.untrustedSigner
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return token, nil
}

// Server is an in-process controlled issuer listening on a loopback address.
type Server struct {
	*Fixture
	server    *http.Server
	listener  net.Listener
	closeOnce sync.Once
	closeErr  error
}

// Start creates an in-process HTTPS issuer. When IssuerURL is empty, Start uses
// the selected loopback port as the exact token issuer.
func Start(config Config) (*Server, error) {
	listenAddress := config.ListenAddress
	if listenAddress == "" {
		listenAddress = defaultListenAddress
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for controlled issuer: %w", err)
	}
	if config.IssuerURL == "" {
		config.IssuerURL = "https://" + listener.Addr().String()
	}
	fixture, err := New(config)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	httpServer := &http.Server{
		Handler:           fixture.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         fixture.TLSConfig(),
	}
	tlsListener := tls.NewListener(listener, fixture.TLSConfig())
	server := &Server{Fixture: fixture, server: httpServer, listener: listener}
	go func() {
		_ = httpServer.Serve(tlsListener)
	}()
	return server, nil
}

// Client returns an HTTP client that trusts only this fixture's runtime CA.
func (s *Server) Client() (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(s.metadata.TLSCAPEM)) {
		return nil, errors.New("append controlled issuer CA")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}},
	}, nil
}

// Close stops the in-process server. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.closeErr = s.server.Shutdown(ctx)
		if errors.Is(s.closeErr, http.ErrServerClosed) {
			s.closeErr = nil
		}
	})
	return s.closeErr
}

func (f *Fixture) newHandler(issuerPath string) http.Handler {
	discoveryPath := path.Join(issuerPath, "/.well-known/openid-configuration")
	jwksPath := path.Join(issuerPath, "/jwks")
	mux := http.NewServeMux()
	mux.HandleFunc(discoveryPath, getOnly(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, map[string]any{
			"issuer":                                f.metadata.IssuerURL,
			"jwks_uri":                              f.metadata.JWKSURL,
			"response_types_supported":              []string{"id_token"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{string(jose.ES256)},
		})
	}))
	mux.HandleFunc(jwksPath, getOnly(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{f.publicKey}})
	}))
	mux.HandleFunc(readinessPath, getOnly(func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, map[string]string{"status": "ready"})
	}))
	return mux
}

func getOnly(handler http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handler(response, request)
	}
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		http.Error(response, "encode response", http.StatusInternalServerError)
	}
}

func validateConfig(config Config) error {
	parsed, err := url.ParseRequestURI(config.IssuerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("issuer URL must be an absolute HTTPS URL without user info, query, or fragment")
	}
	if strings.HasSuffix(parsed.Path, "/") {
		return fmt.Errorf("issuer URL path must not end with a slash")
	}
	if config.Audience == "" {
		return fmt.Errorf("accepted audience is required")
	}
	if config.ExpectedClaims.Username == "" {
		return fmt.Errorf("expected username is required")
	}
	for name := range config.ExpectedClaims.Required {
		if isRegisteredClaim(name) {
			return fmt.Errorf("required claim %q conflicts with a registered claim", name)
		}
	}
	return nil
}

func isRegisteredClaim(name string) bool {
	switch name {
	case "iss", "aud", "exp", "iat", "nbf":
		return true
	default:
		return false
	}
}

func newSigner(keyID string) (jose.Signer, jose.JSONWebKey, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, jose.JSONWebKey{}, fmt.Errorf("generate ES256 key: %w", err)
	}
	publicKey := jose.JSONWebKey{Key: &privateKey.PublicKey, Algorithm: string(jose.ES256), Use: keyUse}
	if keyID == "" {
		thumbprint, err := publicKey.Thumbprint(crypto.SHA256)
		if err != nil {
			return nil, jose.JSONWebKey{}, fmt.Errorf("calculate signing key thumbprint: %w", err)
		}
		keyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	}
	publicKey.KeyID = keyID
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), publicKey.KeyID),
	)
	if err != nil {
		return nil, jose.JSONWebKey{}, fmt.Errorf("create ES256 signer: %w", err)
	}
	return signer, publicKey, nil
}

func newTLSCertificate(host string, additionalNames []string) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS CA key: %w", err)
	}
	now := time.Now().UTC()
	caSerial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "NKE controlled issuer test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create TLS CA certificate: %w", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse TLS CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate TLS server key: %w", err)
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range append([]string{host}, additionalNames...) {
		if ip := net.ParseIP(name); ip != nil {
			serverTemplate.IPAddresses = append(serverTemplate.IPAddresses, ip)
		} else if name != "" {
			serverTemplate.DNSNames = append(serverTemplate.DNSNames, name)
		}
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create TLS server certificate: %w", err)
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal TLS server key: %w", err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}),
	)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load generated TLS key pair: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return certificate, caPEM, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneAnyMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
