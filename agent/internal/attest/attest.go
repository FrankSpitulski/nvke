// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package attest implements the agent side of optional controller-issued
// server TLS.
//
// Production default for nke-agent remains SPIFFE X.509 mTLS (TPM node
// attestation is configured on the SPIRE agent, not here). The controller-tls
// path is opt-in:
//
//  1. Cloud-init still plants the SPIRE server endpoint and trust bundle on
//     the node SPIRE agent (unchanged).
//  2. After the SPIRE agent attests the node (DPU attestor, not TPM), this
//     package fetches a JWT-SVID from the Workload API.
//  3. The JWT is POSTed to the NKE controller. On success the controller
//     returns a TLS certificate and private key used as gRPC server TLS
//     (no client certificates; not mTLS).
//
// The POST path is not a CWE API yet. StubAttestor exists so tests can skip
// the Workload API.
package attest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// DefaultPath is not a CWE route. It exists so the agent can POST without
	// pretending an attest API already shipped.
	DefaultPath = "/stub/v1/attest"

	// DefaultStubJWT is an obviously fake token for tests. Production
	// controller-tls uses WorkloadAPIAttestor, not this string.
	DefaultStubJWT = "nke-agent-stub-attest-jwt"

	// PEM certificate + key comfortably fit under this cap.
	maxResponseBytes = 64 * 1024
)

// Attestor produces the JWT offered to the controller.
type Attestor interface {
	JWT(ctx context.Context) (string, error)
}

// StubAttestor returns a configured string. No Workload API, no TPM.
type StubAttestor struct {
	JWTValue string
}

// JWT returns JWTValue, or an error if it is empty.
func (s StubAttestor) JWT(_ context.Context) (string, error) {
	v := strings.TrimSpace(s.JWTValue)
	if v == "" {
		return "", errors.New("stub attest jwt is empty")
	}
	return v, nil
}

// Request is the JSON body posted with the JWT. The real schema is unspecified.
type Request struct {
	NodeName string `json:"nodeName,omitempty"`
}

// TLSMaterial is the success body from the controller: PEM certificate and
// matching private key for the agent gRPC server.
type TLSMaterial struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

// Result is the HTTP outcome plus a parsed server certificate on success.
type Result struct {
	URL         string
	StatusCode  int
	Body        string
	Certificate *tls.Certificate
}

// Client POSTs a JWT to a controller endpoint using the default TLS trust
// store (no extra PEM). The SPIRE trust bundle stays with the SPIRE agent.
type Client struct {
	// Endpoint is an absolute URL (scheme + host), with no attest path required.
	Endpoint string
	// Path is appended to Endpoint. Empty means DefaultPath.
	Path string
	HTTP *http.Client
	// Attestor defaults to StubAttestor{JWTValue: DefaultStubJWT} when nil.
	Attestor Attestor
}

// ParseTLSMaterial unmarshals controller JSON and builds a tls.Certificate.
func ParseTLSMaterial(body []byte) (*tls.Certificate, error) {
	var m TLSMaterial
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse attest tls json: %w", err)
	}
	if strings.TrimSpace(m.Certificate) == "" || strings.TrimSpace(m.PrivateKey) == "" {
		return nil, errors.New("attest response missing certificate or privateKey")
	}
	cert, err := tls.X509KeyPair([]byte(m.Certificate), []byte(m.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse attest tls key pair: %w", err)
	}
	return &cert, nil
}

// Attest fetches a JWT and POSTs it as Authorization: Bearer to the stub path.
// A 2xx response must contain TLSMaterial JSON.
func (c *Client) Attest(ctx context.Context, req Request) (Result, error) {
	if c == nil {
		return Result{}, errors.New("attest client is nil")
	}

	target, err := c.attestURL()
	if err != nil {
		return Result{}, err
	}

	attestor := c.Attestor
	if attestor == nil {
		attestor = StubAttestor{JWTValue: DefaultStubJWT}
	}
	jwt, err := attestor.JWT(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("attest jwt: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("marshal attest request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("attest request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+jwt)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("attest post to %s: %w", target, err)
	}
	defer resp.Body.Close()

	limited, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{URL: target, StatusCode: resp.StatusCode}, fmt.Errorf("read attest response: %w", err)
	}
	if len(limited) > maxResponseBytes {
		limited = limited[:maxResponseBytes]
	}

	result := Result{
		URL:        target,
		StatusCode: resp.StatusCode,
		Body:       string(limited),
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("attest %s: unexpected status %d", target, resp.StatusCode)
	}

	cert, err := ParseTLSMaterial(limited)
	if err != nil {
		return result, fmt.Errorf("attest %s: %w", target, err)
	}
	result.Certificate = cert
	return result, nil
}

func (c *Client) attestURL() (string, error) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return "", errors.New("controller endpoint is empty")
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse controller endpoint: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("controller endpoint must be an absolute url")
	}

	path := strings.TrimSpace(c.Path)
	if path == "" {
		path = DefaultPath
	}
	rel, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse attest path: %w", err)
	}
	return base.ResolveReference(rel).String(), nil
}
