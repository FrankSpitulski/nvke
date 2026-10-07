// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

const testToken = "test-only-bearer-token"

func TestNewTokenOnlyConfigExcludesFallbackCredentials(t *testing.T) {
	t.Parallel()

	server := newTestAPIServer(t, http.StatusOK)
	request := server.request()
	config, err := NewTokenOnlyConfig(request, contracts.TokenInput{Reader: strings.NewReader(testToken)})
	require.NoError(t, err)
	require.Equal(t, request.EndpointURL, config.Host)
	require.Equal(t, testToken, config.BearerToken)
	require.Empty(t, config.BearerTokenFile)
	require.Empty(t, config.Username)
	require.Empty(t, config.Password)
	require.Nil(t, config.AuthProvider)
	require.Nil(t, config.ExecProvider)
	require.Nil(t, config.Transport)
	require.Nil(t, config.WrapTransport)
	require.Nil(t, config.Dial)
	require.NotNil(t, config.Proxy)
	proxyURL, err := config.Proxy(&http.Request{})
	require.NoError(t, err)
	require.Nil(t, proxyURL)
	require.Empty(t, config.Impersonate.UserName)
	require.Empty(t, config.TLSClientConfig.CertData)
	require.Empty(t, config.TLSClientConfig.KeyData)
	require.Empty(t, config.TLSClientConfig.CertFile)
	require.Empty(t, config.TLSClientConfig.KeyFile)
	require.False(t, config.TLSClientConfig.Insecure)
	require.Equal(t, []byte(request.ServerCAPEM), config.TLSClientConfig.CAData)
}

func TestProbeRecordsIdentityAndAuthorization(t *testing.T) {
	t.Parallel()

	server := newTestAPIServer(t, http.StatusOK)
	result := Probe(context.Background(), server.request(), contracts.TokenInput{Reader: strings.NewReader(testToken)})

	require.Equal(t, contracts.AuthenticationAccepted, result.Authentication)
	require.Equal(t, "alice@example.test", result.Username)
	require.Equal(t, []string{"developers", "viewers"}, result.Groups)
	require.NotNil(t, result.AllowedRequest)
	require.True(t, result.AllowedRequest.Allowed)
	require.Equal(t, http.StatusOK, result.AllowedRequest.HTTPStatus)
	require.NotNil(t, result.DeniedRequest)
	require.False(t, result.DeniedRequest.Allowed)
	require.Equal(t, http.StatusForbidden, result.DeniedRequest.HTTPStatus)
	require.Equal(t, "Forbidden", result.DeniedRequest.Reason)
	require.Empty(t, result.FailureKind)
	require.NotContains(t, result.Message, testToken)
	require.NotContains(t, result.FailureDetail, testToken)
}

func TestProbeClassifiesAuthenticationAndTransportFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		authenticationCode int
		wantAuthentication contracts.AuthenticationState
		wantFailureKind    contracts.ProbeFailureKind
		wantStatus         int
		wantDetail         string
	}{
		{
			name:               "unauthorized bearer token is authentication rejection",
			authenticationCode: http.StatusUnauthorized,
			wantAuthentication: contracts.AuthenticationRejected,
			wantStatus:         http.StatusUnauthorized,
		},
		{
			name:               "forbidden self subject review leaves identity unavailable",
			authenticationCode: http.StatusForbidden,
			wantAuthentication: contracts.AuthenticationNotAttempted,
			wantFailureKind:    contracts.ProbeFailureSetup,
			wantStatus:         http.StatusForbidden,
			wantDetail:         "identity unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newTestAPIServer(t, test.authenticationCode)
			result := Probe(context.Background(), server.request(), contracts.TokenInput{Reader: strings.NewReader(testToken)})
			require.Equal(t, test.wantAuthentication, result.Authentication)
			require.Equal(t, test.wantFailureKind, result.FailureKind)
			require.Equal(t, test.wantStatus, result.HTTPStatus)
			require.NotContains(t, result.Message, testToken)
			require.NotContains(t, result.FailureDetail, testToken)
			if test.wantDetail != "" {
				require.Contains(t, result.FailureDetail, test.wantDetail)
			}
		})
	}

	t.Run("TLS failure is transport failure", func(t *testing.T) {
		t.Parallel()
		server := newTestAPIServer(t, http.StatusOK)
		request := server.request()
		request.ServerCAPEM = unrelatedCAPEM(t)
		result := Probe(context.Background(), request, contracts.TokenInput{Reader: strings.NewReader(testToken)})
		require.Equal(t, contracts.AuthenticationNotAttempted, result.Authentication)
		require.Equal(t, contracts.ProbeFailureTransport, result.FailureKind)
		require.NotContains(t, result.FailureDetail, testToken)
	})
}

func TestProbeRejectsInvalidResourceRequestBeforeAuthentication(t *testing.T) {
	t.Parallel()

	server := newTestAPIServer(t, http.StatusOK)
	request := server.request()
	request.AllowedRequest.Verb = "create"
	result := Probe(context.Background(), request, contracts.TokenInput{Reader: strings.NewReader(testToken)})
	require.Equal(t, contracts.ProbeFailureSetup, result.FailureKind)
	require.Equal(t, contracts.AuthenticationNotAttempted, result.Authentication)
	require.Contains(t, result.FailureDetail, "unsupported probe request verb")
}

func TestProbePreservesIdentityAfterResourceTransportFailure(t *testing.T) {
	t.Parallel()

	server := newTestAPIServer(t, http.StatusOK)
	server.closeAfterIdentity = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := Probe(ctx, server.request(), contracts.TokenInput{Reader: strings.NewReader(testToken)})
	require.Equal(t, contracts.AuthenticationAccepted, result.Authentication)
	require.Equal(t, "alice@example.test", result.Username)
	require.Equal(t, contracts.ProbeFailureTransport, result.FailureKind)
	require.Nil(t, result.AllowedRequest)
}

func TestPrepareAdminFixtureCreatesNarrowResourcesAndCleansUp(t *testing.T) {
	t.Parallel()

	admin := fake.NewSimpleClientset()
	fixture, err := PrepareAdminFixture(context.Background(), admin, "oidc-probe-test", "alice@example.test")
	require.NoError(t, err)
	require.Equal(t, contracts.ResourceRequest{Verb: verbList, Resource: resourceConfigMaps, Namespace: fixture.Namespace}, fixture.AllowedRequest)
	require.Equal(t, contracts.ResourceRequest{Verb: verbList, Resource: resourceSecrets, Namespace: fixture.Namespace}, fixture.DeniedRequest)

	role, err := admin.RbacV1().Roles(fixture.Namespace).Get(context.Background(), fixtureRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{resourceConfigMaps}, role.Rules[0].Resources)
	require.Equal(t, []string{verbList}, role.Rules[0].Verbs)
	binding, err := admin.RbacV1().RoleBindings(fixture.Namespace).Get(context.Background(), fixtureBindingName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "alice@example.test", binding.Subjects[0].Name)

	require.NoError(t, fixture.Cleanup(context.Background(), admin))
	_, err = admin.CoreV1().Namespaces().Get(context.Background(), fixture.Namespace, metav1.GetOptions{})
	require.Error(t, err)
}

type testAPIServer struct {
	server             *httptest.Server
	caPEM              string
	authenticationCode int
	closeAfterIdentity bool
}

func newTestAPIServer(t *testing.T, authenticationCode int) *testAPIServer {
	t.Helper()
	fixture := &testAPIServer{authenticationCode: authenticationCode}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.serveHTTP))
	fixture.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(fixture.server.Close)
	fixture.caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.server.Certificate().Raw}))
	return fixture
}

func (s *testAPIServer) request() contracts.ProbeRequest {
	return contracts.ProbeRequest{
		EndpointURL:    s.server.URL,
		ServerCAPEM:    s.caPEM,
		AllowedRequest: contracts.ResourceRequest{Verb: verbList, Resource: resourceConfigMaps, Namespace: "oidc-probe-test"},
		DeniedRequest:  contracts.ResourceRequest{Verb: verbList, Resource: resourceSecrets, Namespace: "oidc-probe-test"},
	}
}

func (s *testAPIServer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+testToken {
		writeStatus(writer, http.StatusUnauthorized, "Unauthorized", "invalid bearer token")
		return
	}
	switch request.URL.Path {
	case "/apis/authentication.k8s.io/v1/selfsubjectreviews":
		if s.authenticationCode != http.StatusOK {
			writeStatus(writer, s.authenticationCode, http.StatusText(s.authenticationCode), "self subject review denied")
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{
			"apiVersion": "authentication.k8s.io/v1",
			"kind":       "SelfSubjectReview",
			"status": map[string]any{"userInfo": map[string]any{
				"username": "alice@example.test",
				"groups":   []string{"developers", "viewers"},
			}},
		})
	case "/api/v1/namespaces/oidc-probe-test/configmaps":
		if s.closeAfterIdentity {
			panic(http.ErrAbortHandler)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "items": []any{}})
	case "/api/v1/namespaces/oidc-probe-test/secrets":
		writeStatus(writer, http.StatusForbidden, "Forbidden", "secrets is forbidden")
	default:
		writeStatus(writer, http.StatusNotFound, "NotFound", "unknown test endpoint")
	}
}

func writeStatus(writer http.ResponseWriter, code int, reason, message string) {
	writeJSON(writer, code, map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     "Failure",
		"reason":     reason,
		"message":    message,
		"code":       code,
	})
}

func writeJSON(writer http.ResponseWriter, code int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		panic(err)
	}
}

func unrelatedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificate, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}))
}
