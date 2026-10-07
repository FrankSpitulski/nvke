// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package probe builds credential-isolated Kubernetes clients and records
// token authentication and authorization evidence for OIDC tests.
package probe

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/dsx-ai-factory/nke/controller/test/oidctest/contracts"
)

const (
	maxTokenBytes      = 1 << 20
	verbList           = "list"
	resourceConfigMaps = "configmaps"
	resourceSecrets    = "secrets"
)

// Probe authenticates token against request.EndpointURL, records the identity
// returned by SelfSubjectReview, then performs the two requested resource
// reads. It never accepts an admin client or a kubeconfig.
func Probe(ctx context.Context, request contracts.ProbeRequest, token contracts.TokenInput) contracts.ProbeResult {
	if err := validateResourceRequest(request.AllowedRequest); err != nil {
		return setupFailure(err, "")
	}
	if err := validateResourceRequest(request.DeniedRequest); err != nil {
		return setupFailure(err, "")
	}
	tokenValue, err := readToken(token.Reader)
	if err != nil {
		return setupFailure(err, "")
	}
	config, err := newTokenOnlyConfig(request, tokenValue)
	if err != nil {
		return setupFailure(err, tokenValue)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return setupFailure(err, tokenValue)
	}

	identity, err := clientset.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return authenticationFailure(err, tokenValue)
	}

	result := contracts.ProbeResult{
		Authentication: contracts.AuthenticationAccepted,
		Username:       identity.Status.UserInfo.Username,
		Groups:         append([]string{}, identity.Status.UserInfo.Groups...),
	}
	if result.Groups == nil {
		result.Groups = []string{}
	}
	if result.Username == "" {
		result.Authentication = contracts.AuthenticationNotAttempted
		result.FailureKind = contracts.ProbeFailureSetup
		result.FailureDetail = "identity unavailable from SelfSubjectReview"
		return result
	}

	allowed, err := resourceRead(ctx, clientset, request.AllowedRequest, tokenValue)
	if err != nil {
		result.FailureKind = contracts.ProbeFailureTransport
		result.FailureDetail = sanitize(err.Error(), tokenValue)
		return result
	}
	result.AllowedRequest = &allowed

	denied, err := resourceRead(ctx, clientset, request.DeniedRequest, tokenValue)
	if err != nil {
		result.FailureKind = contracts.ProbeFailureTransport
		result.FailureDetail = sanitize(err.Error(), tokenValue)
		return result
	}
	result.DeniedRequest = &denied
	return result
}

// NewTokenOnlyConfig creates a Kubernetes client configuration from the
// supplied endpoint, CA bundle, and bearer token. It has no credential fallbacks
// or inherited transport configuration.
func NewTokenOnlyConfig(request contracts.ProbeRequest, token contracts.TokenInput) (*rest.Config, error) {
	tokenValue, err := readToken(token.Reader)
	if err != nil {
		return nil, err
	}
	return newTokenOnlyConfig(request, tokenValue)
}

func newTokenOnlyConfig(request contracts.ProbeRequest, tokenValue string) (*rest.Config, error) {
	endpoint, err := url.ParseRequestURI(request.EndpointURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" {
		return nil, errors.New("endpoint URL must be an absolute HTTPS URL")
	}
	if block, _ := pem.Decode([]byte(request.ServerCAPEM)); block == nil {
		return nil, errors.New("server CA PEM is required")
	}
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM([]byte(request.ServerCAPEM)) {
		return nil, errors.New("server CA PEM does not contain a certificate")
	}

	return &rest.Config{
		Host:        request.EndpointURL,
		BearerToken: tokenValue,
		Proxy:       http.ProxyURL(nil),
		TLSClientConfig: rest.TLSClientConfig{
			CAData: []byte(request.ServerCAPEM),
		},
	}, nil
}

func readToken(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("token reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxTokenBytes+1))
	if err != nil {
		return "", errors.New("read bearer token")
	}
	if len(data) > maxTokenBytes {
		return "", errors.New("bearer token exceeds maximum size")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("bearer token is required")
	}
	if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", errors.New("bearer token contains whitespace")
	}
	return token, nil
}

func resourceRead(ctx context.Context, clientset kubernetes.Interface, request contracts.ResourceRequest, token string) (contracts.RequestOutcome, error) {
	response := clientset.CoreV1().RESTClient().Get().Namespace(request.Namespace).Resource(request.Resource).Do(ctx)
	var code int
	response.StatusCode(&code)
	err := response.Error()
	if err == nil {
		return contracts.RequestOutcome{HTTPStatus: code, Allowed: true}, nil
	}
	if status, ok := statusFacts(err, token); ok {
		return contracts.RequestOutcome{HTTPStatus: status.code, Allowed: false, Reason: status.reason, Message: status.message}, nil
	}
	return contracts.RequestOutcome{}, err
}

func validateResourceRequest(request contracts.ResourceRequest) error {
	if request.Verb != verbList {
		return fmt.Errorf("unsupported probe request verb %q", request.Verb)
	}
	if request.Namespace == "" {
		return errors.New("probe request namespace is required")
	}
	if request.Resource == "" {
		return errors.New("probe request resource is required")
	}
	return nil
}

func authenticationFailure(err error, token string) contracts.ProbeResult {
	result := contracts.ProbeResult{
		Authentication: contracts.AuthenticationNotAttempted,
	}
	if status, ok := statusFacts(err, token); ok {
		result.HTTPStatus = status.code
		result.Reason = status.reason
		result.Message = status.message
		if status.code == 401 {
			result.Authentication = contracts.AuthenticationRejected
			return result
		}
		if status.code == 403 {
			// A SelfSubjectReview 403 does not expose an authenticated identity.
			// The contract has no "identity unknown" state, so retain the status
			// evidence and classify the incomplete setup separately from a 401.
			result.FailureKind = contracts.ProbeFailureSetup
			result.FailureDetail = "identity unavailable from SelfSubjectReview"
			return result
		}
	}
	result.FailureKind = contracts.ProbeFailureTransport
	result.FailureDetail = sanitize(err.Error(), token)
	return result
}

func setupFailure(err error, token string) contracts.ProbeResult {
	return contracts.ProbeResult{
		FailureKind:    contracts.ProbeFailureSetup,
		FailureDetail:  sanitize(err.Error(), token),
		Authentication: contracts.AuthenticationNotAttempted,
	}
}

type statusFact struct {
	code    int
	reason  string
	message string
}

func statusFacts(err error, token string) (statusFact, bool) {
	var statusErr apierrors.APIStatus
	if !errors.As(err, &statusErr) {
		return statusFact{}, false
	}
	status := statusErr.Status()
	return statusFact{code: int(status.Code), reason: sanitize(string(status.Reason), token), message: sanitize(status.Message, token)}, true
}

func sanitize(value, token string) string {
	if token != "" {
		value = strings.ReplaceAll(value, token, "REDACTED")
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	const maxDetailBytes = 4096
	if len(value) > maxDetailBytes {
		return value[:maxDetailBytes] + "..."
	}
	return value
}
