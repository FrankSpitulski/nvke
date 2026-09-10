// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAgentID = "spiffe://example.org/host/6f1b2c3d"

// fakeSource stands in for *workloadapi.X509Source. It counts Close calls so
// tests can assert the retry loop does not leak a watcher per attempt.
type fakeSource struct {
	id      string
	svidErr error
	closes  int
}

func (f *fakeSource) GetX509SVID() (*x509svid.SVID, error) {
	if f.svidErr != nil {
		return nil, f.svidErr
	}
	return &x509svid.SVID{ID: spiffeid.RequireFromString(f.id)}, nil
}

func (f *fakeSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, nil
}

func (f *fakeSource) Close() error {
	f.closes++
	return nil
}

// recordSleeps replaces the backoff wait so tests run instantly, and returns the
// slice the durations land in.
func recordSleeps(a *Acquirer) *[]time.Duration {
	slept := make([]time.Duration, 0)
	a.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return &slept
}

func TestAcquireSucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	var attempts []Attempt
	acquirer := &Acquirer{
		NewSource: func(context.Context) (Source, error) {
			return &fakeSource{id: testAgentID}, nil
		},
		Observe: func(a Attempt) { attempts = append(attempts, a) },
	}
	recordSleeps(acquirer)

	source, id, err := acquirer.Acquire(context.Background())

	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, testAgentID, id.String())
	assert.Equal(t, "example.org", id.TrustDomain().Name())

	require.Len(t, attempts, 1, "a successful acquisition should be observed once")
	assert.Equal(t, 1, attempts[0].N)
	assert.NoError(t, attempts[0].Err)
	assert.Zero(t, attempts[0].RetryIn, "there is nothing to retry after success")
}

// The point of the retry loop: a host that is not attested yet is a wait, not a
// failure, and the wait has to be observable while it is happening.
func TestAcquireRetriesUntilTheWorkloadAPIAnswers(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failures int
		newErr   error
		svidErr  error
	}{
		"socket absent until the SPIRE agent starts": {
			failures: 3,
			newErr:   errors.New("no such file or directory"),
		},
		"socket present but the node is not attested": {
			failures: 2,
			newErr:   context.DeadlineExceeded,
		},
		"source opens but has no SVID yet": {
			failures: 2,
			svidErr:  errors.New("missing X509-SVID"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls int
			var opened []*fakeSource
			var attempts []Attempt

			acquirer := &Acquirer{
				NewSource: func(context.Context) (Source, error) {
					calls++
					if calls <= tc.failures {
						if tc.newErr != nil {
							return nil, tc.newErr
						}
						source := &fakeSource{id: testAgentID, svidErr: tc.svidErr}
						opened = append(opened, source)
						return source, nil
					}
					return &fakeSource{id: testAgentID}, nil
				},
				Observe: func(a Attempt) { attempts = append(attempts, a) },
			}
			slept := recordSleeps(acquirer)

			source, id, err := acquirer.Acquire(context.Background())

			require.NoError(t, err)
			require.NotNil(t, source)
			assert.Equal(t, testAgentID, id.String())
			assert.Equal(t, tc.failures+1, calls)

			// Every failed attempt is reported, and only the last one is a success.
			require.Len(t, attempts, tc.failures+1)
			for i := range tc.failures {
				assert.Errorf(t, attempts[i].Err, "attempt %d should carry its error", i+1)
				assert.Equal(t, i+1, attempts[i].N)
				assert.NotZero(t, attempts[i].RetryIn)
			}
			assert.NoError(t, attempts[tc.failures].Err)
			assert.Len(t, *slept, tc.failures, "one backoff wait per failed attempt")

			// A source that opened but could not produce an SVID is closed rather
			// than left watching the Workload API.
			for i, s := range opened {
				assert.Equalf(t, 1, s.closes, "discarded source %d should be closed once", i)
			}
		})
	}
}

func TestAcquireBackoffDoublesAndIsCapped(t *testing.T) {
	t.Parallel()

	acquirer := &Acquirer{
		NewSource: func(context.Context) (Source, error) {
			return nil, errors.New("not attested")
		},
		MinBackoff: time.Second,
		MaxBackoff: 4 * time.Second,
	}

	// Stop after enough attempts to reach and hold the cap.
	var slept []time.Duration
	acquirer.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 5 {
			return errors.New("stop")
		}
		return nil
	}

	_, _, err := acquirer.Acquire(context.Background())

	require.Error(t, err)
	assert.Equal(t, []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		4 * time.Second,
		4 * time.Second,
	}, slept)
}

func TestAcquireDefaultsApply(t *testing.T) {
	t.Parallel()

	acquirer := &Acquirer{
		NewSource: func(ctx context.Context) (Source, error) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "each attempt must be bounded")
			assert.WithinDuration(t, time.Now().Add(DefaultAttemptTimeout), deadline, time.Second)
			return &fakeSource{id: testAgentID}, nil
		},
	}

	_, _, err := acquirer.Acquire(context.Background())
	require.NoError(t, err)
}

// MaxBackoff below MinBackoff is a misconfiguration, not a reason to busy-loop.
func TestAcquireClampsInvertedBackoffBounds(t *testing.T) {
	t.Parallel()

	acquirer := &Acquirer{
		NewSource: func(context.Context) (Source, error) {
			return nil, errors.New("not attested")
		},
		MinBackoff: 10 * time.Second,
		MaxBackoff: time.Second,
	}

	var slept []time.Duration
	acquirer.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 2 {
			return errors.New("stop")
		}
		return nil
	}

	_, _, err := acquirer.Acquire(context.Background())

	require.Error(t, err)
	assert.Equal(t, []time.Duration{10 * time.Second, 10 * time.Second}, slept)
}

// Shutdown has to win against the retry loop, and has to be distinguishable
// from an attestation failure.
func TestAcquireStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cancelDuringSleep bool
	}{
		"cancelled while opening a source": {},
		"cancelled while backing off":      {cancelDuringSleep: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			acquirer := &Acquirer{
				NewSource: func(context.Context) (Source, error) {
					if !tc.cancelDuringSleep {
						cancel()
					}
					return nil, errors.New("not attested")
				},
			}
			acquirer.sleep = func(ctx context.Context, _ time.Duration) error {
				if tc.cancelDuringSleep {
					cancel()
				}
				return ctx.Err()
			}

			source, id, err := acquirer.Acquire(ctx)

			require.ErrorIs(t, err, context.Canceled)
			assert.Nil(t, source)
			assert.True(t, id.IsZero())
		})
	}
}

func TestWorkloadAPISourceFailsClosedOnAMissingSocket(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	source, err := WorkloadAPISource("unix:///nonexistent/nke-agent-test/api.sock")(ctx)

	require.Error(t, err)
	assert.Nil(t, source)
}
