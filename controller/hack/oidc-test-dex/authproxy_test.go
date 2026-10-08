//go:build oidc_dex_integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiserver "k8s.io/apiserver/pkg/apis/apiserver"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	kubeoidc "k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nke/controller/internal/kubeadmconfig"
)

const authProxyRedirectURI = "https://localhost/callback"

type authProxyCase struct {
	name           string
	username       string
	groups         []string
	usernamePrefix string
	groupsPrefix   string
	reject         bool
}

func TestNKEAuthenticationWithDexTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	requireCommand(t, "docker")
	dir := t.TempDir()
	port := reserveLoopbackPort(t)
	issuer := "https://127.0.0.1:" + strconv.Itoa(port)
	runtime := newAuthProxyRuntime(t, issuer, dir)
	startDex(t, ctx, fmt.Sprintf("nke-oidc-test-authproxy-%d", port), port, dir)
	client := tlsClient(t, runtime.CAPEM)
	t.Cleanup(client.CloseIdleConnections)
	waitForDex(t, ctx, client, issuer)
	policy := kubeadmconfig.OperatorJWTPolicy{
		ManagementJWTTrustDomain: "jwt.example.org",
		ReservedSubjects:         []string{"spiffe://exchange.example/nke-service/capi-manager"},
	}

	for _, testCase := range authProxyCases(policy) {
		t.Run(testCase.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("X-Remote-User", testCase.username)
			headers.Set("X-Remote-Group", strings.Join(testCase.groups, ","))
			token := acquireAuthProxyToken(t, ctx, client, runtime, headers)

			for _, usernameClaim := range []string{"name", "preferred_username", "email"} {
				t.Run(usernameClaim, func(t *testing.T) {
					authenticator := newKubernetesAuthenticator(t, ctx, client, runtime, usernameClaim, testCase, policy)
					response, ok, err := authenticator.AuthenticateToken(ctx, token)
					if testCase.reject {
						require.ErrorContains(t, err, "reserved")
						assert.False(t, ok)
						assert.Nil(t, response)
						return
					}
					require.NoError(t, err)
					require.True(t, ok)
					require.NotNil(t, response)
					assert.Equal(t, testCase.usernamePrefix+testCase.username, response.User.GetName())
					mappedGroups := make([]string, len(testCase.groups))
					for i, group := range testCase.groups {
						mappedGroups[i] = testCase.groupsPrefix + group
					}
					assert.ElementsMatch(t, mappedGroups, response.User.GetGroups())
				})
			}
		})
	}
}

func authProxyCases(policy kubeadmconfig.OperatorJWTPolicy) []authProxyCase {
	reserved := append(controllerconfig.ReservedIdentityPrefixes(), "spiffe://"+policy.ManagementJWTTrustDomain+"/")
	cases := make([]authProxyCase, 0, 6+2*len(reserved)+2*len(policy.ReservedSubjects))
	cases = append(cases,
		authProxyCase{name: "ordinary identity", username: "alice", groups: []string{"developers", "reviewers"}},
		authProxyCase{name: "no groups", username: "alice"},
		authProxyCase{name: "namespace lookalikes", username: "systematic:alice", groups: []string{"nvidia-team"}},
		authProxyCase{name: "prefix creates reserved username", username: "tem:admin", usernamePrefix: "sys", reject: true},
		authProxyCase{name: "prefix creates reserved group", username: "alice", groups: []string{"tem:masters"}, groupsPrefix: "sys", reject: true},
		authProxyCase{
			name: "safe prefixes isolate reserved raw claims", username: "system:admin", groups: []string{"system:masters"},
			usernamePrefix: "workforce:", groupsPrefix: "workforce:",
		},
	)
	for _, prefix := range reserved {
		cases = append(cases,
			authProxyCase{name: prefix + " username", username: prefix + "admin", groups: []string{"developers"}, reject: true},
			authProxyCase{name: prefix + " group", username: "alice", groups: []string{"developers", prefix + "admins"}, reject: true},
		)
	}
	for _, subject := range policy.ReservedSubjects {
		cases = append(cases,
			authProxyCase{name: "reserved subject username", username: subject, reject: true},
			authProxyCase{name: "reserved subject group", username: "alice", groups: []string{subject}, reject: true},
		)
	}
	return cases
}

func newAuthProxyRuntime(t *testing.T, issuer, directory string) dexRuntime {
	t.Helper()
	secret, err := randomText()
	require.NoError(t, err)
	cert, key, ca, err := certificate(issuer)
	require.NoError(t, err)
	runtime := dexRuntime{Issuer: issuer, ClientID: "oidc-test", ClientSecret: secret, CAPEM: string(ca)}
	config, err := json.Marshal(map[string]any{
		"issuer":  issuer,
		"storage": map[string]any{"type": "memory"},
		"web": map[string]any{
			"https": "0.0.0.0:5556", "tlsCert": "/runtime/tls.crt", "tlsKey": "/runtime/tls.key",
		},
		"oauth2": map[string]any{"skipApprovalScreen": true},
		"staticClients": []map[string]any{{
			"id": runtime.ClientID, "secret": secret, "name": "oidc-test", "redirectURIs": []string{authProxyRedirectURI},
		}},
		"connectors": []map[string]any{{"type": "authproxy", "id": "authproxy", "name": "Test auth proxy", "config": map[string]any{}}},
	})
	require.NoError(t, err)
	writeRuntimeFile(t, directory, "config.yaml", config)
	writeRuntimeFile(t, directory, "tls.crt", cert)
	writeRuntimeFile(t, directory, "tls.key", key)
	return runtime
}

func acquireAuthProxyToken(t *testing.T, ctx context.Context, client *http.Client, runtime dexRuntime, identity http.Header) string {
	t.Helper()
	state, err := randomText()
	require.NoError(t, err)
	query := url.Values{
		"response_type": {"code"}, "client_id": {runtime.ClientID}, "redirect_uri": {authProxyRedirectURI},
		"scope": {"openid profile email groups"}, "state": {state},
	}
	flowClient := *client
	flowClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	callback := authProxyRedirect(t, ctx, &flowClient, runtime.Issuer+"/auth/authproxy?"+query.Encode(), nil)
	issuerURL, err := url.Parse(runtime.Issuer)
	require.NoError(t, err)
	require.Equal(t, issuerURL.Scheme, callback.Scheme)
	require.Equal(t, issuerURL.Host, callback.Host)
	require.Equal(t, "/callback/authproxy", callback.Path)
	// The loopback-only test client supplies the headers an authenticating proxy would set.
	redirect := authProxyRedirect(t, ctx, &flowClient, callback.String(), identity)
	require.Equal(t, authProxyRedirectURI, redirect.Scheme+"://"+redirect.Host+redirect.Path)
	require.Empty(t, redirect.Query().Get("error"), "Dex rejected the proxy identity")
	require.Equal(t, state, redirect.Query().Get("state"))
	code := redirect.Query().Get("code")
	require.NotEmpty(t, code, "Dex returned no authorization code")
	values := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {authProxyRedirectURI},
		"client_id": {runtime.ClientID}, "client_secret": {runtime.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, runtime.Issuer+"/token", strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := flowClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "Dex authorization-code exchange failed")
	var body struct {
		IDToken string `json:"id_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.IDToken, "Dex returned no ID token")
	return body.IDToken
}

func authProxyRedirect(t *testing.T, ctx context.Context, client *http.Client, endpoint string, headers http.Header) *url.URL {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	require.NoError(t, err)
	if headers != nil {
		req.Header = headers.Clone()
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, resp.StatusCode, "Dex did not redirect")
	location, err := resp.Location()
	require.NoError(t, err)
	return location
}

func newKubernetesAuthenticator(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	runtime dexRuntime,
	usernameClaim string,
	testCase authProxyCase,
	policy kubeadmconfig.OperatorJWTPolicy,
) kubeoidc.AuthenticatorTokenWithHealthCheck {
	t.Helper()
	provider := controllerconfig.OIDCProvider{
		Name: "dex-spike", IssuerURL: runtime.Issuer, Audiences: []string{runtime.ClientID}, CABundle: runtime.CAPEM,
		UsernameClaim: usernameClaim, UsernamePrefix: testCase.usernamePrefix, GroupsClaim: "groups", GroupsPrefix: testCase.groupsPrefix,
	}
	config, err := kubeadmconfig.BuildOperatorJWTAuthenticator(provider, policy)
	require.NoError(t, err)
	var internal apiserver.JWTAuthenticator
	require.NoError(t, apiserverv1.Convert_v1_JWTAuthenticator_To_apiserver_JWTAuthenticator(&config, &internal, nil))
	lifecycleCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	authenticator, err := kubeoidc.New(lifecycleCtx, kubeoidc.Options{JWTAuthenticator: internal, Client: client})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return authenticator.HealthCheck() == nil }, 10*time.Second, 10*time.Millisecond,
		"Kubernetes OIDC authenticator did not initialize")
	return authenticator
}
