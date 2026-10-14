// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func configuration(t *testing.T) (Options, *clientcmdapi.Config) {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("example-token"), 0600))
	key := filepath.Join(dir, "page-key")
	require.NoError(t, os.WriteFile(key, bytes.Repeat([]byte{1}, 32), 0600))
	options := Options{
		Kubeconfig: filepath.Join(dir, "kubeconfig"), PageTokenKeyFile: key,
		Namespace: "evaluation", EvaluationNoAuth: true,
		HTTPAddress: "127.0.0.1:0", GRPCAddress: "127.0.0.1:0", HealthAddress: "127.0.0.1:0",
	}
	raw := clientcmdapi.NewConfig()
	raw.CurrentContext = "storage"
	raw.Contexts["storage"] = &clientcmdapi.Context{Cluster: "storage", AuthInfo: "api"}
	raw.Clusters["storage"] = &clientcmdapi.Cluster{Server: "https://storage.example:6443", CertificateAuthorityData: []byte("REDACTED")}
	raw.AuthInfos["api"] = &clientcmdapi.AuthInfo{TokenFile: token}
	return options, raw
}

func TestExplicitStorageIgnoresAmbientRouting(t *testing.T) {
	for name, test := range map[string]struct {
		relative    bool
		certificate bool
	}{
		"absolute token":       {},
		"relative token":       {relative: true},
		"absolute certificate": {certificate: true},
		"relative certificate": {relative: true, certificate: true},
	} {
		t.Run(name, func(t *testing.T) {
			options, raw := configuration(t)
			dir := filepath.Dir(options.Kubeconfig)
			cluster, identity := raw.Clusters["storage"], raw.AuthInfos["api"]
			cluster.CertificateAuthorityData = nil
			cluster.CertificateAuthority = filepath.Join(dir, "ca")
			require.NoError(t, os.WriteFile(cluster.CertificateAuthority, []byte("REDACTED"), 0600))
			if test.certificate {
				identity.TokenFile = ""
				identity.ClientCertificate = filepath.Join(dir, "certificate")
				identity.ClientKey = filepath.Join(dir, "key")
				require.NoError(t, os.WriteFile(identity.ClientCertificate, []byte("REDACTED"), 0600))
				require.NoError(t, os.WriteFile(identity.ClientKey, []byte("REDACTED"), 0600))
			}
			if test.relative {
				for _, path := range []*string{&cluster.CertificateAuthority, &identity.TokenFile, &identity.ClientCertificate, &identity.ClientKey} {
					if *path != "" {
						*path = filepath.Base(*path)
					}
				}
			}
			require.NoError(t, clientcmd.WriteToFile(*raw, options.Kubeconfig))
			t.Chdir(t.TempDir())
			t.Setenv("KUBECONFIG", "/must-not-use-ambient-config")
			t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
			loaded, err := Load(options)
			require.NoError(t, err)
			require.Equal(t, cluster.Server, loaded.Storage.Host)
			require.NotNil(t, loaded.Storage.Proxy)
			proxy, err := loaded.Storage.Proxy(&http.Request{})
			require.NoError(t, err)
			require.Nil(t, proxy)
			require.Equal(t, filepath.Join(dir, "ca"), loaded.Storage.CAFile)
			if test.certificate {
				require.Equal(t, filepath.Join(dir, "certificate"), loaded.Storage.CertFile)
				require.Equal(t, filepath.Join(dir, "key"), loaded.Storage.KeyFile)
				require.Empty(t, loaded.Storage.BearerTokenFile)
			} else {
				require.Equal(t, filepath.Join(dir, "token"), loaded.Storage.BearerTokenFile)
				require.Equal(t, "example-token", loaded.Storage.BearerToken)
			}
			require.Len(t, loaded.PageTokenKey, 32)
			require.NotEmpty(t, loaded.DatastoreID)
			other, err := Load(options)
			require.NoError(t, err)
			require.Equal(t, loaded.DatastoreID, other.DatastoreID)
			require.Equal(t, loaded.PageTokenKey, other.PageTokenKey)
		})
	}
}

func TestRejectUnsafeDatastoreConfiguration(t *testing.T) {
	for name, test := range map[string]struct {
		mutate  func(*clientcmdapi.Config)
		wantErr string
	}{
		"http": {func(c *clientcmdapi.Config) { c.Clusters["storage"].Server = "http://storage.example" }, "HTTPS origin"},
		"URL credentials": {func(c *clientcmdapi.Config) {
			c.Clusters["storage"].Server = "https://user:example-token@storage.example"
		}, "HTTPS origin"},
		"URL query":  {func(c *clientcmdapi.Config) { c.Clusters["storage"].Server += "?target=elsewhere" }, "HTTPS origin"},
		"URL path":   {func(c *clientcmdapi.Config) { c.Clusters["storage"].Server += "/proxy" }, "HTTPS origin"},
		"insecure":   {func(c *clientcmdapi.Config) { c.Clusters["storage"].InsecureSkipTLSVerify = true }, "explicit CA, verified TLS"},
		"missing CA": {func(c *clientcmdapi.Config) { c.Clusters["storage"].CertificateAuthorityData = nil }, "explicit CA, verified TLS"},
		"proxy":      {func(c *clientcmdapi.Config) { c.Clusters["storage"].ProxyURL = "http://proxy.invalid" }, "direct access without a proxy"},
		"exec": {func(c *clientcmdapi.Config) {
			c.AuthInfos["api"].Exec = &clientcmdapi.ExecConfig{
				Command: "must-not-run", APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			}
		}, "exec, auth-provider"},
		"auth provider": {func(c *clientcmdapi.Config) {
			c.AuthInfos["api"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "unsupported"}
		}, "exec, auth-provider"},
		"impersonation":      {func(c *clientcmdapi.Config) { c.AuthInfos["api"].Impersonate = "other" }, "impersonation are unsupported"},
		"embedded token":     {func(c *clientcmdapi.Config) { c.AuthInfos["api"].Token = "example-token" }, "mounted tokenFile"},
		"missing identity":   {func(c *clientcmdapi.Config) { delete(c.AuthInfos, "api") }, "explicit cluster and identity"},
		"missing context":    {func(c *clientcmdapi.Config) { c.CurrentContext = "absent" }, "existing current-context"},
		"missing credential": {func(c *clientcmdapi.Config) { c.AuthInfos["api"].TokenFile = "" }, "exactly one tokenFile or client certificate/key pair"},
	} {
		t.Run(name, func(t *testing.T) {
			options, raw := configuration(t)
			test.mutate(raw)
			require.NoError(t, clientcmd.WriteToFile(*raw, options.Kubeconfig))
			_, err := Load(options)
			require.ErrorContains(t, err, test.wantErr)
			require.NotContains(t, err.Error(), "example-token")
		})
	}
}

func TestExplicitEvaluationAndScope(t *testing.T) {
	for name, test := range map[string]struct {
		mutate  func(*Options)
		wantErr string
	}{
		"auth mode":       {func(o *Options) { o.EvaluationNoAuth = false }, "--evaluation-no-auth"},
		"ambient config":  {func(o *Options) { o.Kubeconfig = "" }, "explicit absolute paths"},
		"relative config": {func(o *Options) { o.Kubeconfig = "config" }, "explicit absolute paths"},
		"namespace":       {func(o *Options) { o.Namespace = "" }, "namespace must be a DNS label"},
		"listener":        {func(o *Options) { o.HTTPAddress = "not-an-address" }, "invalid listener address"},
		"relative key":    {func(o *Options) { o.PageTokenKeyFile = "key" }, "explicit absolute paths"},
	} {
		t.Run(name, func(t *testing.T) {
			options, raw := configuration(t)
			require.NoError(t, clientcmd.WriteToFile(*raw, options.Kubeconfig))
			test.mutate(&options)
			_, err := Load(options)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestRejectShortPaginationKey(t *testing.T) {
	options, raw := configuration(t)
	require.NoError(t, clientcmd.WriteToFile(*raw, options.Kubeconfig))
	require.NoError(t, os.WriteFile(options.PageTokenKeyFile, []byte("example-key"), 0600))
	_, err := Load(options)
	require.ErrorContains(t, err, "32 to 64")
}
