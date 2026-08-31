// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package attest is an illustration-only client for Security's proposed
// call-home bootstrap: the agent is given a controller URL (no trust-bundle
// PEM), obtains a JWT, and POSTs it to the controller.
//
// This is not a CWE API. The default path is deliberately under /stub/.
// TPM2-backed JWTs are not implemented; use StubAttestor until the JWT
// profile (alg, claims, nonce) is specified.
package attest

import (
	"bytes"
	"context"
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

	// DefaultStubJWT is an obviously fake token, not a compact JWT.
	DefaultStubJWT = "nke-agent-stub-attest-jwt"

	maxResponseBytes = 4096
)

// Attestor produces the bootstrap token that would be TPM-backed in production.
type Attestor interface {
	JWT(ctx context.Context) (string, error)
}

// StubAttestor returns a configured string. No TPM.
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

// TPMAttestor is a placeholder for tpm2-tools (quote / AK-signed JWT).
type TPMAttestor struct{}

// JWT always returns a not-implemented error.
func (TPMAttestor) JWT(_ context.Context) (string, error) {
	return "", errors.New("tpm jwt attestor is not implemented")
}

// Request is the JSON body posted with the JWT. The real schema is unspecified.
type Request struct {
	NodeName string `json:"nodeName,omitempty"`
}

// Result is the raw HTTP outcome. The agent does not interpret join tokens.
type Result struct {
	URL        string
	StatusCode int
	Body       string
}

// Client POSTs a JWT to a controller endpoint using the default TLS trust
// store (no extra PEM).
type Client struct {
	// Endpoint is an absolute URL (scheme + host), with no attest path required.
	Endpoint string
	// Path is appended to Endpoint. Empty means DefaultPath.
	Path string
	HTTP *http.Client
	// Attestor defaults to StubAttestor{JWTValue: DefaultStubJWT} when nil.
	Attestor Attestor
}

// Attest fetches a JWT and POSTs it as Authorization: Bearer to the stub path.
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
