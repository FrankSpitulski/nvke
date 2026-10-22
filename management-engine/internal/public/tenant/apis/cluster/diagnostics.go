// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Datastore failures become stable public errors and diagnostics that omit private values.
package cluster

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

// storageError distinguishes caller outcomes from storage failures that operators must diagnose.
func (s *Service) storageError(ctx context.Context, operation, name string, err error) error {
	if apierrors.IsNotFound(err) && operation != "get" && operation != "replace" {
		s.logFailure(ctx, operation, name, "cluster datastore configuration is unavailable", err)
		return status.Error(codes.Internal, "cluster storage is not configured; contact the service administrator")
	}
	public := publicStorageError(err)
	switch status.Code(public) {
	case codes.Internal, codes.Unavailable, codes.DeadlineExceeded:
		s.logFailure(ctx, operation, name, "cluster datastore request failed", err)
	case codes.InvalidArgument:
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			s.logFailure(ctx, operation, name, "cluster datastore rejected configuration", err)
		}
	}
	return public
}

// publicStorageError translates datastore failures without exposing upstream error contents.
func publicStorageError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return status.Error(codes.DeadlineExceeded, "storage request timed out; a write may have completed")
	case apierrors.IsNotFound(err):
		return status.Error(codes.NotFound, "cluster not found")
	case apierrors.IsAlreadyExists(err):
		return status.Error(codes.AlreadyExists, "cluster already exists")
	case apierrors.IsConflict(err):
		return status.Error(codes.Aborted, "cluster changed; read its current resource_version")
	case apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		return status.Error(codes.InvalidArgument, "page token expired; restart listing")
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		return status.Error(codes.Internal, "cluster storage identity is not authorized; contact the service administrator")
	case apierrors.IsInternalError(err):
		return status.Error(codes.Internal, "cluster storage failed")
	case apierrors.IsTooManyRequests(err):
		return status.Error(codes.ResourceExhausted, "cluster storage is busy")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return status.Error(codes.InvalidArgument, "cluster configuration is invalid")
	default:
		return status.Error(codes.Unavailable, "cluster storage is unavailable")
	}
}

// internalError records an internal failure while returning only its reviewed public message.
func (s *Service) internalError(ctx context.Context, operation, name, message string, err error) error {
	s.logFailure(ctx, operation, name, message, err)
	return status.Error(codes.Internal, message)
}

// logFailure records classifications, never error strings: Kubernetes errors and
// wrapped network errors can contain request bodies, credentials and private URLs.
func (s *Service) logFailure(ctx context.Context, operation, name, message string, err error) {
	if name != "" && len(validation.IsDNS1123Subdomain(name)) != 0 {
		name = "invalid"
	}
	attrs := []any{"operation", operation, "resource", clusterResource.Resource,
		"namespace", s.config.Namespace, "name", name, "error_class", errorClass(err), "error_type", fmt.Sprintf("%T", err)}
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		attrs = append(attrs, "storage_status", apiStatus.Status().Code)
	}
	s.config.Logger.ErrorContext(ctx, message, attrs...)
}

// errorClass supplies actionable failure categories without logging sensitive error strings.
func errorClass(err error) string {
	_, unknownCA := errors.AsType[x509.UnknownAuthorityError](err)
	_, hostname := errors.AsType[x509.HostnameError](err)
	_, certificate := errors.AsType[x509.CertificateInvalidError](err)
	_, dns := errors.AsType[*net.DNSError](err)
	switch {
	case err == nil:
		return "invalid_storage_response"
	case apierrors.IsNotFound(err):
		return "storage_not_found"
	case apierrors.IsUnauthorized(err):
		return "storage_unauthorized"
	case apierrors.IsForbidden(err):
		return "storage_forbidden"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "storage_timeout"
	case apierrors.IsInternalError(err):
		return "storage_internal_error"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "storage_invalid_configuration"
	case unknownCA:
		return "tls_unknown_authority"
	case hostname:
		return "tls_hostname_mismatch"
	case certificate:
		return "tls_invalid_certificate"
	case dns:
		return "dns_lookup_failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	default:
		return "unexpected_failure"
	}
}
