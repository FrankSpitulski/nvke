// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package identity acquires the node agent's own SPIFFE identity from the local
// SPIFFE Workload API.
//
// The agent cannot serve gRPC until the host's SPIRE agent has finished node
// attestation and been issued an SVID for it. How long that takes is a property
// of the attestation method rather than of the agent: a TPM quote is local and
// effectively immediate, while an IMDS-backed method depends on a network path
// to the host's paired DPU that can be down for the length of a bring-up.
//
// Acquisition therefore retries indefinitely instead of failing the process,
// and reports every attempt so that a host waiting on attestation says so
// rather than looking hung.
package identity

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Defaults for Acquirer. The first attempt is bounded short so an unattested
// host reports the fact within seconds, and the backoff is capped so a long
// outage does not keep logging at that rate for hours.
const (
	DefaultAttemptTimeout = 30 * time.Second
	DefaultMinBackoff     = 2 * time.Second
	DefaultMaxBackoff     = 2 * time.Minute
)

// Source supplies the X.509 material a SPIFFE mTLS listener needs: this agent's
// own SVID, and the bundles it verifies peers against. *workloadapi.X509Source
// satisfies it.
type Source interface {
	x509svid.Source
	x509bundle.Source
}

// NewSourceFunc opens a Source. It blocks until the Workload API delivers a
// first SVID or ctx is done, which is why Acquirer only ever calls it with a
// bounded context.
type NewSourceFunc func(ctx context.Context) (Source, error)

// WorkloadAPISource returns a NewSourceFunc backed by the Workload API socket at
// socketPath.
func WorkloadAPISource(socketPath string) NewSourceFunc {
	return func(ctx context.Context) (Source, error) {
		return workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	}
}

// Attempt describes one finished acquisition attempt.
type Attempt struct {
	// N counts attempts from 1.
	N int
	// Err is nil once the Workload API has returned an SVID.
	Err error
	// RetryIn is how long Acquirer waits before attempt N+1. Zero on success.
	RetryIn time.Duration
}

// Acquirer obtains a Source, retrying until it succeeds or its context is
// cancelled. NewSource must be set; the remaining fields fall back to the
// package defaults.
type Acquirer struct {
	NewSource NewSourceFunc

	// AttemptTimeout bounds a single call to NewSource.
	AttemptTimeout time.Duration
	// MinBackoff and MaxBackoff bound the doubling delay between attempts.
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// Observe, when set, is called once per finished attempt, successful or not.
	Observe func(Attempt)

	// sleep is a seam so tests do not wait out the backoff. Nil means a real,
	// context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// Acquire returns a Source and the SPIFFE ID of the SVID it carries. It returns
// an error only when ctx is cancelled: every other failure is retried, because
// a host that cannot attest yet may still attest later, and exiting would throw
// away the accumulated diagnostics along with the process.
func (a *Acquirer) Acquire(ctx context.Context) (Source, spiffeid.ID, error) {
	attemptTimeout := orDefault(a.AttemptTimeout, DefaultAttemptTimeout)
	minBackoff := orDefault(a.MinBackoff, DefaultMinBackoff)
	maxBackoff := orDefault(a.MaxBackoff, DefaultMaxBackoff)
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}

	backoff := minBackoff
	for n := 1; ; n++ {
		source, id, err := a.attempt(ctx, attemptTimeout)
		if err == nil {
			a.observe(Attempt{N: n})
			return source, id, nil
		}

		// A cancelled parent is the caller shutting us down, not an attestation
		// failure, and has to be distinguished from the per-attempt timeout.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, spiffeid.ID{}, fmt.Errorf("acquiring a SPIFFE identity: %w", ctxErr)
		}

		a.observe(Attempt{N: n, Err: err, RetryIn: backoff})

		if err := a.doSleep(ctx, backoff); err != nil {
			return nil, spiffeid.ID{}, fmt.Errorf("acquiring a SPIFFE identity: %w", err)
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// attempt makes one bounded attempt to open a source and read an SVID from it.
//
// Cancelling the attempt context after a successful return is safe: the
// Workload API watcher that keeps the source rotated runs on its own background
// context, and the constructor context only bounds the wait for the first
// update.
func (a *Acquirer) attempt(ctx context.Context, timeout time.Duration) (Source, spiffeid.ID, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	source, err := a.NewSource(attemptCtx)
	if err != nil {
		return nil, spiffeid.ID{}, fmt.Errorf("opening the workload API source: %w", err)
	}

	svid, err := source.GetX509SVID()
	if err != nil {
		closeSource(source)
		return nil, spiffeid.ID{}, fmt.Errorf("reading the agent SVID: %w", err)
	}

	return source, svid.ID, nil
}

func (a *Acquirer) observe(attempt Attempt) {
	if a.Observe != nil {
		a.Observe(attempt)
	}
}

func (a *Acquirer) doSleep(ctx context.Context, d time.Duration) error {
	if a.sleep != nil {
		return a.sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeSource releases a source that was opened but turned out to be unusable,
// so a retry loop does not leak a Workload API watcher per attempt.
func closeSource(source Source) {
	if closer, ok := source.(io.Closer); ok {
		// The source is being discarded, so a close failure has nowhere to go.
		_ = closer.Close()
	}
}

func orDefault(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
