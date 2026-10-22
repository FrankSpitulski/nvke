// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Encrypted continuation tokens support stateless pagination without exposing datastore cursors.
package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/limits"
)

const maxPageTokenBytes = 16 << 10

type pageCursor struct {
	// Continue resumes the datastore snapshot without exposing its internal cursor.
	Continue string `json:"continue"`
	// Size prevents a continuation from changing the requested page boundary.
	Size int32 `json:"size"`
	// Expires bounds replay independently of the datastore cursor's lifetime.
	Expires int64 `json:"expires"`
}

// newTokenCipher derives an authenticated cipher shared by replicas using the same secret.
func newTokenCipher(key []byte) (cipher.AEAD, error) {
	if len(key) < limits.PageTokenKeyMinBytes || len(key) > limits.PageTokenKeyMaxBytes {
		return nil, fmt.Errorf("page-token key must contain between %d and %d bytes", limits.PageTokenKeyMinBytes, limits.PageTokenKeyMaxBytes)
	}
	digest := sha256.Sum256(key)
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, fmt.Errorf("create page-token cipher: %w", err)
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// page validates pagination inputs and restores the original page size when resuming a list.
func (s *Service) page(size int32, token string) (int32, string, error) {
	if size < 0 || size > MaxPageSize {
		return 0, "", fieldViolation("page_size", "must be between 1 and 100, or omitted")
	}
	if token == "" {
		if size == 0 {
			size = defaultPageSize
		}
		return size, "", nil
	}
	cursor, err := s.decodePage(token)
	if err != nil || (size != 0 && size != cursor.Size) {
		return 0, "", status.Error(codes.InvalidArgument, "invalid or expired page token, or changed page size; restart listing")
	}
	return cursor.Size, cursor.Continue, nil
}

// encodePage seals an expiring cursor to the configured datastore and namespace.
func (s *Service) encodePage(continuation string, size int32) (string, error) {
	data, err := json.Marshal(pageCursor{Continue: continuation, Size: size, Expires: time.Now().Add(15 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	sealed := s.tokens.Seal(nil, nil, data, s.tokenScope) // #nosec G407 -- NewGCMWithRandomNonce generates and prepends the nonce. Nil is required.
	token := base64.RawURLEncoding.EncodeToString(sealed)
	if len(token) > maxPageTokenBytes {
		return "", fmt.Errorf("storage continuation exceeds page token limit")
	}
	return token, nil
}

// decodePage rejects altered, expired or out-of-scope cursors before they reach storage.
func (s *Service) decodePage(token string) (pageCursor, error) {
	invalid := fmt.Errorf("invalid page token")
	if len(token) > maxPageTokenBytes {
		return pageCursor{}, invalid
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(sealed) < s.tokens.Overhead() {
		return pageCursor{}, invalid
	}
	data, err := s.tokens.Open(nil, nil, sealed, s.tokenScope)
	if err != nil {
		return pageCursor{}, invalid
	}
	var cursor pageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Continue == "" || cursor.Size <= 0 || cursor.Size > MaxPageSize || cursor.Expires <= time.Now().Unix() {
		return pageCursor{}, invalid
	}
	return cursor, nil
}
