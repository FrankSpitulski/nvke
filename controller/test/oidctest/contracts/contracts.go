// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package contracts defines the data shared by the local OIDC issuer,
// reference API server, and token-only probe. It is not an NKE API.
package contracts

import (
	"fmt"
	"io"
	"net/url"
)

const (
	// SchemaVersion identifies the fixture metadata format.
	SchemaVersion = "nke.oidc.test/v1"
)

// CleanupOwner identifies the test component responsible for deleting a fixture.
type CleanupOwner string

const (
	// CleanupOwnerIssuer removes issuer TLS material and signing keys.
	CleanupOwnerIssuer CleanupOwner = "issuer"
)

// FixtureMetadata describes a runtime-created OIDC issuer without carrying a
// bearer token or signing key. TLSCAPEM is the runtime-generated trust bundle.
type FixtureMetadata struct {
	SchemaVersion    string         `json:"schema_version"`
	IssuerURL        string         `json:"issuer_url"`
	JWKSURL          string         `json:"jwks_url"`
	TLSCAPEM         string         `json:"tls_ca_pem"`
	AcceptedAudience string         `json:"accepted_audience"`
	ExpectedClaims   ExpectedClaims `json:"expected_claims"`
	CleanupOwner     CleanupOwner   `json:"cleanup_owner"`
}

// ExpectedClaims records the identity facts a fixture token must produce.
// Required contains exact string claim matches.
type ExpectedClaims struct {
	Username string            `json:"username"`
	Groups   []string          `json:"groups"`
	Required map[string]string `json:"required"`
}

// Validate checks metadata before a fixture or reference config consumes it.
func (m FixtureMetadata) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", m.SchemaVersion)
	}
	if err := validateHTTPSURL("issuer URL", m.IssuerURL); err != nil {
		return err
	}
	if err := validateHTTPSURL("JWKS URL", m.JWKSURL); err != nil {
		return err
	}
	if m.TLSCAPEM == "" {
		return fmt.Errorf("TLS CA PEM is required")
	}
	if m.AcceptedAudience == "" {
		return fmt.Errorf("accepted audience is required")
	}
	if m.ExpectedClaims.Username == "" {
		return fmt.Errorf("expected username is required")
	}
	if m.CleanupOwner == "" {
		return fmt.Errorf("cleanup owner is required")
	}
	return nil
}

func validateHTTPSURL(name, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute HTTPS URL", name)
	}
	return nil
}

// TokenInput supplies a runtime bearer token to an in-process probe.
type TokenInput struct {
	Reader io.Reader
}

// ResourceRequest identifies a real Kubernetes request used for RBAC evidence.
type ResourceRequest struct {
	Verb      string
	Resource  string
	Namespace string
}

// ProbeRequest contains the endpoint and two resource requests exercised with
// one bearer token. It carries no fallback credentials.
type ProbeRequest struct {
	EndpointURL    string
	ServerCAPEM    string
	AllowedRequest ResourceRequest
	DeniedRequest  ResourceRequest
}

// ProbeFailureKind identifies an environment failure before a completed check.
type ProbeFailureKind string

const (
	// ProbeFailureSetup covers invalid inputs and unavailable test setup.
	ProbeFailureSetup ProbeFailureKind = "setup"
	// ProbeFailureTransport covers DNS, connection, and TLS failures.
	ProbeFailureTransport ProbeFailureKind = "transport"
)

// AuthenticationState records what the Kubernetes API did with the bearer token.
type AuthenticationState string

const (
	// AuthenticationNotAttempted means setup or transport stopped the probe.
	AuthenticationNotAttempted AuthenticationState = "not_attempted"
	// AuthenticationRejected means the API returned an authentication rejection.
	AuthenticationRejected AuthenticationState = "rejected"
	// AuthenticationAccepted means the API accepted the token.
	AuthenticationAccepted AuthenticationState = "accepted"
)

// RequestOutcome is the result of one real Kubernetes resource request.
type RequestOutcome struct {
	HTTPStatus int
	Allowed    bool
	Reason     string
	Message    string
}

// ProbeResult reports authentication, identity, and RBAC facts.
type ProbeResult struct {
	FailureKind    ProbeFailureKind
	FailureDetail  string
	Authentication AuthenticationState
	HTTPStatus     int
	Reason         string
	Message        string
	Username       string
	Groups         []string
	AllowedRequest *RequestOutcome
	DeniedRequest  *RequestOutcome
}
