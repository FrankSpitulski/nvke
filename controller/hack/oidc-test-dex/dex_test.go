//go:build oidc_dex_integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dex

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDexPasswordGrantAndVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	requireCommand(t, "docker")
	dir := t.TempDir()
	port := reserveLoopbackPort(t)
	issuer := "https://127.0.0.1:" + strconv.Itoa(port)
	runtime, config, cert, key, err := NewRuntime(issuer)
	if err != nil {
		t.Fatal(err)
	}
	writeRuntimeFile(t, dir, "config.yaml", []byte(config))
	writeRuntimeFile(t, dir, "tls.crt", cert)
	writeRuntimeFile(t, dir, "tls.key", key)
	name := fmt.Sprintf("nke-oidc-test-dex-%d", port)
	startDex(t, ctx, name, port, dir)
	client := tlsClient(t, runtime.CAPEM)
	waitForDex(t, ctx, client, issuer)
	token, err := AcquireIDToken(ctx, client, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRuntimeIDToken(ctx, client, token, runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Password = "wrong-password"
	if _, err := AcquireIDToken(ctx, client, runtime); err == nil {
		t.Fatal("wrong password acquired a token")
	}
}

func TestNewRuntimeCreatesIndependentRuntimeMaterial(t *testing.T) {
	runtime, config, cert, key, err := NewRuntime("https://127.0.0.1:5556")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Username == "" || runtime.Password == "" || runtime.ClientSecret == "" || runtime.CAPEM == "" {
		t.Fatal("NewRuntime returned incomplete runtime material")
	}
	if len(cert) == 0 || len(key) == 0 {
		t.Fatal("NewRuntime returned incomplete TLS material")
	}
	for _, value := range []string{runtime.Issuer, runtime.Username, runtime.ClientID, runtime.ClientSecret} {
		if !strings.Contains(config, value) {
			t.Fatal("Dex config omitted generated runtime material")
		}
	}
}

func requireCommand(t *testing.T, command string) {
	t.Helper()
	if _, err := exec.LookPath(command); err != nil {
		t.Fatal(err)
	}
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func writeRuntimeFile(t *testing.T, directory, name string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startDex(t *testing.T, ctx context.Context, name string, port int, directory string) {
	t.Helper()
	user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	cidFile := filepath.Join(directory, "container-id")
	t.Cleanup(func() {
		containerID, err := os.ReadFile(cidFile)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Errorf("read Dex container ID: %v", err)
			return
		}
		containerID = []byte(strings.TrimSpace(string(containerID)))
		if len(containerID) == 0 {
			t.Error("Dex container ID file is empty")
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", string(containerID)).Run(); err != nil {
			t.Errorf("remove Dex container: %v", err)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--detach", "--cidfile", cidFile, "--name", name, "--user", user, "-p", fmt.Sprintf("127.0.0.1:%d:5556", port), "-v", directory+":/runtime:ro", "--entrypoint", "dex", Image, "serve", "/runtime/config.yaml")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

func tlsClient(t *testing.T, caPEM string) *http.Client {
	t.Helper()
	pools := x509.NewCertPool()
	if !pools.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("add generated Dex CA to trust pool")
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pools}},
		Timeout:   3 * time.Second,
	}
}

func waitForDex(t *testing.T, ctx context.Context, client *http.Client, issuer string) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatal("Dex did not become ready")
		case <-ticker.C:
		}
	}
}
