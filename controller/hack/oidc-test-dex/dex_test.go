//go:build oidc_dex_integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dex

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dexImage pins Dex v2.45.1 by manifest digest.
const dexImage = "ghcr.io/dexidp/dex@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"

type dexRuntime struct {
	ClientID     string
	ClientSecret string
	Issuer       string
	CAPEM        string
}

func requireCommand(t *testing.T, command string) {
	t.Helper()
	if _, err := exec.LookPath(command); err != nil {
		t.Fatal(err)
	}
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp4", "127.0.0.1:0")
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
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", string(containerID)).Run(); err != nil {
			t.Errorf("remove Dex container: %v", err)
		}
	})
	cmd := exec.CommandContext(ctx, "docker", "run", "--detach", "--cidfile", cidFile, "--name", name, "--user", user, "-p", fmt.Sprintf("127.0.0.1:%d:5556", port), "-v", directory+":/runtime:ro", "--entrypoint", "dex", dexImage, "serve", "/runtime/config.yaml")
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

func randomText() (string, error) {
	const length = 32
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return fmt.Sprintf("%x", bytes)[:length], nil
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
