// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package attest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testTLSPEMs(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
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
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM
}

func TestStubAttestorJWT(t *testing.T) {
	t.Parallel()

	got, err := (StubAttestor{JWTValue: "  token  "}).JWT(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "token", got)

	_, err = (StubAttestor{}).JWT(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestWorkloadAPIAttestorEmptyAudience(t *testing.T) {
	t.Parallel()

	_, err := (WorkloadAPIAttestor{}).JWT(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audience")
}

func TestParseTLSMaterial(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := testTLSPEMs(t)
	raw, err := json.Marshal(TLSMaterial{
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
	})
	require.NoError(t, err)

	cert, err := ParseTLSMaterial(raw)
	require.NoError(t, err)
	require.NotNil(t, cert)
	require.NotEmpty(t, cert.Certificate)

	_, err = ParseTLSMaterial([]byte(`{"status":"stub-ok"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing certificate")

	_, err = ParseTLSMaterial([]byte("not-json"))
	require.Error(t, err)
}

func TestClientAttestPostsBearerAndParsesTLS(t *testing.T) {
	t.Parallel()

	certPEM, keyPEM := testTLSPEMs(t)
	payload, err := json.Marshal(TLSMaterial{
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
	})
	require.NoError(t, err)

	var (
		gotAuth string
		gotBody Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, DefaultPath, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	client := &Client{
		Endpoint: srv.URL,
		HTTP:     srv.Client(),
		Attestor: StubAttestor{JWTValue: "illustration-jwt"},
	}
	result, err := client.Attest(context.Background(), Request{NodeName: "node-a"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, result.StatusCode)
	assert.True(t, strings.HasSuffix(result.URL, DefaultPath))
	assert.Equal(t, "Bearer illustration-jwt", gotAuth)
	assert.Equal(t, "node-a", gotBody.NodeName)
	require.NotNil(t, result.Certificate)
	require.NotEmpty(t, result.Certificate.Certificate)
}

func TestClientAttestUnexpectedStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such route"))
	}))
	t.Cleanup(srv.Close)

	client := &Client{Endpoint: srv.URL, HTTP: srv.Client()}
	result, err := client.Attest(context.Background(), Request{})
	require.Error(t, err)
	assert.Equal(t, http.StatusNotFound, result.StatusCode)
	assert.Contains(t, result.Body, "no such route")
	assert.Contains(t, err.Error(), "404")
	assert.Nil(t, result.Certificate)
}

func TestClientAttestURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		path     string
		want     string
		wantErr  string
	}{
		{
			name:     "default path",
			endpoint: "https://nke.example",
			want:     "https://nke.example/stub/v1/attest",
		},
		{
			name:     "custom path",
			endpoint: "https://nke.example/nke",
			path:     "/v1/nodes/attest",
			want:     "https://nke.example/v1/nodes/attest",
		},
		{
			name:    "empty endpoint",
			wantErr: "empty",
		},
		{
			name:     "not absolute",
			endpoint: "/relative",
			wantErr:  "absolute",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&Client{Endpoint: tc.endpoint, Path: tc.path}).attestURL()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
