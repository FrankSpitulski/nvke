// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package envtest starts an isolated Kubernetes API server configured with the
// OIDC controlled issuer. It is reference-test infrastructure, not NKE.
package envtest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	controllerruntimeenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

const (
	binaryAssetsEnv          = "KUBEBUILDER_ASSETS"
	authenticationConfigArg  = "authentication-config"
	authenticationConfigFile = "authentication-config.yaml"
	maximumDiagnosticsBytes  = 32 << 10
)

// Config configures one reference API server. AuthenticationConfig is a
// Kubernetes AuthenticationConfiguration document. It lets each caller test
// its own issuer and claim policy. BinaryAssetsDirectory defaults to
// KUBEBUILDER_ASSETS.
type Config struct {
	AuthenticationConfig  []byte
	BinaryAssetsDirectory string
}

// Runner owns an isolated envtest API server and its certificate-based admin
// client. Token assertions must use probe.Probe, never AdminClient.
type Runner struct {
	adminConfig *rest.Config
	adminClient kubernetes.Interface
	environment *controllerruntimeenvtest.Environment
	diagnostics diagnosticBuffer
	directory   string
	closeOnce   sync.Once
	closeErr    error
}

// Start writes a structured authentication configuration and starts a real
// kube-apiserver plus etcd. It registers cleanup before any process starts.
// Missing binary assets fail this explicit runner instead of skipping a test.
func Start(tb testing.TB, config Config) (*Runner, error) {
	tb.Helper()

	runner := &Runner{directory: tb.TempDir()}
	tb.Cleanup(func() {
		if err := runner.Close(); err != nil {
			tb.Errorf("stopping OIDC envtest runner: %v", err)
		}
		if tb.Failed() {
			if diagnostics := runner.Diagnostics(); diagnostics != "" {
				tb.Logf("OIDC envtest API server diagnostics:\n%s", diagnostics)
			}
		}
	})

	if len(config.AuthenticationConfig) == 0 {
		return nil, fmt.Errorf("authentication configuration is required")
	}
	assetsDirectory, err := binaryAssetsDirectory(config.BinaryAssetsDirectory)
	if err != nil {
		return nil, err
	}
	authenticationConfigPath := filepath.Join(runner.directory, authenticationConfigFile)
	if err := os.WriteFile(authenticationConfigPath, config.AuthenticationConfig, 0o600); err != nil {
		return nil, fmt.Errorf("write authentication configuration: %w", err)
	}

	runner.environment = &controllerruntimeenvtest.Environment{
		BinaryAssetsDirectory:    assetsDirectory,
		ControlPlaneStartTimeout: 45 * time.Second,
		ControlPlaneStopTimeout:  45 * time.Second,
		AttachControlPlaneOutput: false,
	}
	apiServer := runner.environment.ControlPlane.GetAPIServer()
	apiServer.Out = &runner.diagnostics
	apiServer.Err = &runner.diagnostics
	apiServer.Configure().Append(authenticationConfigArg, authenticationConfigPath)

	adminConfig, err := runner.environment.Start()
	if err != nil {
		return nil, fmt.Errorf("start OIDC envtest API server: %w; diagnostics: %s", err, runner.Diagnostics())
	}
	runner.adminConfig = rest.CopyConfig(adminConfig)
	runner.adminClient, err = kubernetes.NewForConfig(runner.adminConfig)
	if err != nil {
		return nil, fmt.Errorf("create envtest admin client: %w", err)
	}
	return runner, nil
}

type diagnosticBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *diagnosticBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(data)
}

func (b *diagnosticBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}

// AdminClient returns the certificate-authenticated client for creating only
// test namespaces and RBAC. It must not be passed to token probes.
func (r *Runner) AdminClient() kubernetes.Interface {
	return r.adminClient
}

// ProbeRequest returns a token-only probe request for this runner. The caller
// supplies the test namespace resource requests from probe.PrepareAdminFixture.
func (r *Runner) ProbeRequest(allowed, denied contracts.ResourceRequest) contracts.ProbeRequest {
	return contracts.ProbeRequest{
		EndpointURL:    r.adminConfig.Host,
		ServerCAPEM:    string(r.adminConfig.TLSClientConfig.CAData),
		AllowedRequest: allowed,
		DeniedRequest:  denied,
	}
}

// Diagnostics returns a bounded, sanitized copy of API server output. The
// runner diagnostics never receive bearer tokens from the probe.
func (r *Runner) Diagnostics() string {
	diagnostics := r.diagnostics.String()
	if len(diagnostics) > maximumDiagnosticsBytes {
		diagnostics = diagnostics[len(diagnostics)-maximumDiagnosticsBytes:]
	}
	return sanitizeDiagnostics(diagnostics)
}

// Close stops the API server. The testing package removes the runtime files
// created with t.TempDir. Close is safe to call more than once.
func (r *Runner) Close() error {
	r.closeOnce.Do(func() {
		if r.environment != nil {
			r.closeErr = r.environment.Stop()
		}
	})
	return r.closeErr
}

func sanitizeDiagnostics(diagnostics string) string {
	const authorizationBearer = "authorization: bearer "

	lines := strings.Split(diagnostics, "\n")
	for index, line := range lines {
		prefixIndex := strings.Index(strings.ToLower(line), authorizationBearer)
		if prefixIndex >= 0 {
			lines[index] = line[:prefixIndex] + "Authorization: Bearer REDACTED"
		}
	}
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || character >= ' ' {
			return character
		}
		return -1
	}, strings.Join(lines, "\n"))
}

func binaryAssetsDirectory(configured string) (string, error) {
	assetsDirectory := configured
	if assetsDirectory == "" {
		assetsDirectory = os.Getenv(binaryAssetsEnv)
	}
	if assetsDirectory == "" {
		return "", fmt.Errorf("%s is required; resolve Kubernetes 1.34.1 assets with setup-envtest release-0.24", binaryAssetsEnv)
	}
	// #nosec G703 -- caller controls this read-only test binary directory.
	info, err := os.Stat(assetsDirectory)
	if err != nil {
		return "", fmt.Errorf("inspect %s directory %q: %w", binaryAssetsEnv, assetsDirectory, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s path %q is not a directory", binaryAssetsEnv, assetsDirectory)
	}
	return assetsDirectory, nil
}
