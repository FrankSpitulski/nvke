// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package auth provides a gRPC server interceptor that authenticates callers
// against a shared bearer token read from a file.
//
// THE EMPTY TOKEN IS THE THING TO KNOW ABOUT HERE.
//
// crypto/subtle.ConstantTimeCompare returns 1 for two empty slices, because it
// compares lengths and then finds no bytes that differ. So an empty expected
// token matches an empty presented token, and the interceptor authenticates the
// caller. Nothing about that is obvious from reading the comparison.
//
// Two states reached it, and both are guarded now:
//
//   - A token file that exists but is empty or only whitespace. os.ReadFile
//     succeeds, TrimSpace leaves "", and the authenticator served every request
//     that sent an empty Authorization header.
//   - A zero-value TokenAuthenticator. The type is exported with unexported
//     fields, so &auth.TokenAuthenticator{} compiles anywhere and yields an
//     interceptor with no token at all. This package lives under pkg/, so that
//     is reachable by any importer once this repository is public.
//
// The rule the code now follows: an empty token never authenticates anyone,
// whichever side it is on.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// reloadInterval is how often the token file is re-read, so a rotated token is
// picked up without restarting the process.
const reloadInterval = 10 * time.Second

// ErrEmptyToken is returned when the token file holds nothing but whitespace.
// It is a distinct error because an empty token is not a missing file: reading
// it succeeds, and without this the authenticator would start and accept
// callers that present nothing.
var ErrEmptyToken = errors.New("auth: token file is empty")

type TokenAuthenticator struct {
	mu        sync.RWMutex
	token     string
	tokenFile string

	stop     chan struct{}
	stopOnce sync.Once
}

// NewTokenAuthenticator reads tokenFile and starts watching it for changes.
//
// It fails when the file is missing, unreadable, or empty. Failing to start is
// the point: a server that cannot load a token must not run without one.
func NewTokenAuthenticator(tokenFile string) (*TokenAuthenticator, error) {
	a := &TokenAuthenticator{
		tokenFile: tokenFile,
		stop:      make(chan struct{}),
	}
	if err := a.loadToken(); err != nil {
		return nil, err
	}
	go a.watchToken()
	return a, nil
}

// Close stops the reload goroutine. It is safe to call more than once.
//
// Without it the goroutine ran until the process exited, with no way to stop
// it, so every authenticator ever constructed leaked one — including in tests.
func (a *TokenAuthenticator) Close() {
	a.stopOnce.Do(func() { close(a.stop) })
}

func (a *TokenAuthenticator) loadToken() error {
	b, err := os.ReadFile(a.tokenFile)
	if err != nil {
		return fmt.Errorf("auth: reading token file %q: %w", a.tokenFile, err)
	}

	t := strings.TrimSpace(string(b))
	if t == "" {
		return fmt.Errorf("%w: %q", ErrEmptyToken, a.tokenFile)
	}

	a.mu.Lock()
	a.token = t
	a.mu.Unlock()
	return nil
}

func (a *TokenAuthenticator) watchToken() {
	ticker := time.NewTicker(reloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			// A failed reload keeps the token already in memory rather than
			// clearing it. Truncating the file for an instant during a rotation
			// is a normal thing to observe, and treating that as "no token" is
			// worse than serving the previous one for another interval.
			if err := a.loadToken(); err != nil {
				slog.Error("auth: failed to reload token, keeping the current one",
					"file", a.tokenFile, "err", err)
			}
		}
	}
}

func (a *TokenAuthenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing request metadata")
		}

		ah := md.Get("authorization")
		if len(ah) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization header")
		}

		token := strings.TrimPrefix(ah[0], "Bearer ")

		a.mu.RLock()
		expected := a.token
		a.mu.RUnlock()

		// Both guards are load-bearing, and neither is redundant.
		//
		// `expected == ""` catches a zero-value TokenAuthenticator, which
		// NewTokenAuthenticator cannot produce but any importer can.
		// `token == ""` refuses a caller presenting nothing, which would
		// otherwise match an empty expected token through ConstantTimeCompare.
		//
		// These run before the comparison, so the timing of a rejected empty
		// token is not constant. That is deliberate and leaks nothing: it
		// reveals only that one side was empty, which is not a secret.
		if expected == "" || token == "" {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}

		return handler(ctx, req)
	}
}
