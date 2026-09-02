// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/NVIDIA/nke/agent/internal/attest"
)

// The authorizer is the whole of this agent's access control, so these pin what it
// refuses as much as what it admits.
func TestAuthorizeClient(t *testing.T) {
	t.Parallel()

	const agentTrustDomain = "example.org"

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "legacy service path", id: "spiffe://example.org/nke-service/cloud-workflow-engine"},
		{name: "tenant-grammar path", id: "spiffe://example.org/tenant/nke/service/cloud-workflow-engine/bm/cwe"},
		{name: "tenant-grammar path, other service", id: "spiffe://example.org/tenant/nke/service/capi-manager/bm/cwe"},

		// Different paths that merely share leading characters.
		{name: "legacy prefix is not a substring match", id: "spiffe://example.org/nke-service-evil/cwe", wantErr: true},
		{name: "tenant prefix is not a substring match", id: "spiffe://example.org/tenant/nke/service-evil/cwe", wantErr: true},

		// The tenant segment is inside the prefix, so other tenants are refused.
		{name: "other tenant", id: "spiffe://example.org/tenant/acme/service/cloud-workflow-engine/bm/x", wantErr: true},

		{name: "unrelated path", id: "spiffe://example.org/nke-e2e/probe", wantErr: true},
		{name: "foreign trust domain", id: "spiffe://attacker.test/nke-service/cloud-workflow-engine", wantErr: true},
		{name: "trust domain is not a prefix match", id: "spiffe://example.org.attacker.test/nke-service/cwe", wantErr: true},
	}

	authorize := authorizeClient(agentTrustDomain)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := spiffeid.FromString(tc.id)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.id, err)
			}

			err = authorize(id)
			if tc.wantErr && err == nil {
				t.Fatalf("authorizeClient(%q) = nil, want an error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizeClient(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

func TestResolveIdentityMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mode        string
		modeSet     bool
		disableMTLS bool
		want        string
		wantErr     string
	}{
		{name: "default spiffe", want: identityModeSPIFFE},
		{name: "explicit spiffe", mode: "spiffe", modeSet: true, want: identityModeSPIFFE},
		{name: "controller-tls", mode: "controller-tls", modeSet: true, want: identityModeControllerTLS},
		{name: "plaintext", mode: "plaintext", modeSet: true, want: identityModePlaintext},
		{name: "disable-mtls alias", disableMTLS: true, want: identityModePlaintext},
		{name: "disable-mtls with explicit plaintext", mode: "plaintext", modeSet: true, disableMTLS: true, want: identityModePlaintext},
		{name: "unknown", mode: "tpm", modeSet: true, wantErr: "unknown identity-mode"},
		{name: "disable-mtls conflicts with spiffe", mode: "spiffe", modeSet: true, disableMTLS: true, wantErr: "conflicts"},
		{name: "disable-mtls conflicts with controller-tls", mode: "controller-tls", modeSet: true, disableMTLS: true, wantErr: "conflicts"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveIdentityMode(tc.mode, tc.modeSet, tc.disableMTLS)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveIdentityMode() = %q, nil; want error %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveIdentityMode() error %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveIdentityMode() = %v, want %q", err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("resolveIdentityMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServerTLSConfigNoClientAuth(t *testing.T) {
	t.Parallel()

	cfg := serverTLSConfig(tls.Certificate{})
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want TLS 1.2", cfg.MinVersion)
	}
}

func TestRedactAttestBody(t *testing.T) {
	t.Parallel()

	if got := redactAttestBody("ok"); got != "ok" {
		t.Fatalf("redactAttestBody(ok) = %q", got)
	}
	if got := redactAttestBody("-----BEGIN PRIVATE KEY-----\nabc\n"); got == "-----BEGIN PRIVATE KEY-----\nabc\n" {
		t.Fatal("redactAttestBody did not redact private key material")
	}
}

func TestMaybeControllerAttestSkippedWhenNotControllerTLS(t *testing.T) {
	t.Parallel()

	tlsCfg, err := maybeControllerAttest(t.Context(), AgentConfig{
		IdentityMode:       identityModeSPIFFE,
		ControllerEndpoint: "https://nke.example",
	})
	if err != nil {
		t.Fatalf("maybeControllerAttest() = %v, want nil when identity-mode is spiffe", err)
	}
	if tlsCfg != nil {
		t.Fatal("maybeControllerAttest() returned tls config for spiffe mode")
	}

	tlsCfg, err = maybeControllerAttest(t.Context(), AgentConfig{})
	if err != nil {
		t.Fatalf("maybeControllerAttest() = %v, want nil when identity-mode is unset", err)
	}
	if tlsCfg != nil {
		t.Fatal("maybeControllerAttest() returned tls config when skipped")
	}
}

func TestMaybeControllerAttestRequiresEndpoint(t *testing.T) {
	t.Parallel()

	_, err := maybeControllerAttest(t.Context(), AgentConfig{IdentityMode: identityModeControllerTLS})
	if err == nil {
		t.Fatal("maybeControllerAttest() = nil, want error when controller-endpoint is empty")
	}
	if !strings.Contains(err.Error(), "controller-endpoint") {
		t.Fatalf("maybeControllerAttest() error %q, want controller-endpoint", err.Error())
	}
}

func TestMaybeControllerAttestParsesServerTLS(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := testAgentTLSPEMs(t)
	payload, err := json.Marshal(attest.TLSMaterial{
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	tlsCfg, err := maybeControllerAttest(t.Context(), AgentConfig{
		IdentityMode:       identityModeControllerTLS,
		ControllerEndpoint: srv.URL,
		StubAttestJWT:      "test-jwt-svid",
	})
	if err != nil {
		t.Fatalf("maybeControllerAttest() = %v", err)
	}
	if tlsCfg == nil {
		t.Fatal("maybeControllerAttest() returned nil tls config")
	}
	if tlsCfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert", tlsCfg.ClientAuth)
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(tlsCfg.Certificates))
	}
	if gotAuth != "Bearer test-jwt-svid" {
		t.Fatalf("Authorization = %q, want Bearer test-jwt-svid", gotAuth)
	}
}

func TestControllerTLSAttestor(t *testing.T) {
	t.Parallel()

	stub := controllerTLSAttestor(AgentConfig{StubAttestJWT: "  stub-token  "})
	got, err := stub.JWT(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != "stub-token" {
		t.Fatalf("stub JWT = %q, want stub-token", got)
	}

	attestor := controllerTLSAttestor(AgentConfig{
		SPIFFESocketPath:   "unix:///tmp/spire-agent/public/api.sock",
		ControllerEndpoint: "https://nke.example",
		JWTAudience:        "nke-controller",
	})
	wl, ok := attestor.(attest.WorkloadAPIAttestor)
	if !ok {
		t.Fatalf("controllerTLSAttestor() = %T, want WorkloadAPIAttestor", attestor)
	}
	if wl.Audience != "nke-controller" {
		t.Fatalf("audience = %q, want nke-controller", wl.Audience)
	}
	if wl.SocketPath != "unix:///tmp/spire-agent/public/api.sock" {
		t.Fatalf("socket = %q", wl.SocketPath)
	}

	defaulted, ok := controllerTLSAttestor(AgentConfig{
		ControllerEndpoint: "https://nke.example",
	}).(attest.WorkloadAPIAttestor)
	if !ok {
		t.Fatal("expected WorkloadAPIAttestor when stub jwt is empty")
	}
	if defaulted.Audience != "https://nke.example" {
		t.Fatalf("default audience = %q, want controller endpoint", defaulted.Audience)
	}
}

func testAgentTLSPEMs(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nke-agent-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM
}
