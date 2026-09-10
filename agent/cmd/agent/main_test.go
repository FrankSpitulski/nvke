// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	server "github.com/NVIDIA/nke/agent/internal/agent"
	"github.com/NVIDIA/nke/agent/internal/identity"
)

// The authorizer is the whole of this agent's access control, so these pin what it
// refuses as much as what it admits.
func TestAuthorizeClient(t *testing.T) {
	t.Parallel()

	const agentTrustDomain = "example.org"

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "legacy service path", id: "spiffe://example.org/nke-service/cloud-workflow-engine"},
		{name: "tenant-grammar path", id: "spiffe://example.org/tenant/nke/service/cloud-workflow-engine/bm/cwe"},
		{name: "tenant-grammar path, other service", id: "spiffe://example.org/tenant/nke/service/capi-manager/bm/cwe"},

		// Different paths that merely share leading characters.
		{name: "legacy prefix is not a substring match", id: "spiffe://example.org/nke-service-evil/cwe", wantErr: true},
		{name: "tenant prefix is not a substring match", id: "spiffe://example.org/tenant/nke/service-evil/cwe", wantErr: true},

		// The tenant segment is inside the prefix, so other tenants are refused.
		{name: "other tenant", id: "spiffe://example.org/tenant/acme/service/cloud-workflow-engine/bm/x", wantErr: true},

		{name: "unrelated path", id: "spiffe://example.org/nke-e2e/probe", wantErr: true},
		{name: "foreign trust domain", id: "spiffe://attacker.test/nke-service/cloud-workflow-engine", wantErr: true},
		{name: "trust domain is not a prefix match", id: "spiffe://example.org.attacker.test/nke-service/cwe", wantErr: true},
	}

	authorize := authorizeClient(agentTrustDomain)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := spiffeid.FromString(tc.id)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.id, err)
			}

			err = authorize(id)
			if tc.wantErr && err == nil {
				t.Fatalf("authorizeClient(%q) = nil, want an error", tc.id)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("authorizeClient(%q) = %v, want nil", tc.id, err)
			}
		})
	}
}

// recordingPersister collects every snapshot, so a test can assert on what a
// host reports while it is still waiting rather than only on the final state.
type recordingPersister struct {
	mu       sync.Mutex
	statuses []server.SPIFFEStatus
}

func (p *recordingPersister) Persist(status *server.NodeStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = append(p.statuses, status.SPIFFE)
	return nil
}

func (p *recordingPersister) LoadFromStateFile() (*server.NodeStatus, error) {
	return &server.NodeStatus{}, nil
}

func (p *recordingPersister) recorded() []server.SPIFFEStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]server.SPIFFEStatus(nil), p.statuses...)
}

// A host that cannot attest has to say so. These pin what a failed attempt
// reports, and that a successful one is left for the caller that knows the ID.
func TestSpiffeStatusObserver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		attempt identity.Attempt
		want    []server.SPIFFEStatus
	}{
		{
			name:    "failed attempt reports the wait",
			attempt: identity.Attempt{N: 2, Err: errors.New("dial unix: no such file"), RetryIn: 4 * time.Second},
			want: []server.SPIFFEStatus{{
				State:        server.SPIFFEStateWaiting,
				Attempts:     2,
				LastError:    "dial unix: no such file",
				RetrySeconds: 4,
			}},
		},
		{
			// Success carries the SPIFFE ID, which only Acquire returns, so the
			// observer must not overwrite the status with an ID-less one.
			name:    "successful attempt records nothing",
			attempt: identity.Attempt{N: 1},
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			persister := &recordingPersister{}
			agentServer := &server.NodeAgentServer{
				Persister:  persister,
				NodeStatus: &server.NodeStatus{},
			}

			spiffeStatusObserver(agentServer)(tc.attempt)

			got := persister.recorded()
			if len(got) != len(tc.want) {
				t.Fatalf("recorded %d statuses, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("status %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Acquisition retries forever, so cancellation is the only way out. Before this
// wiring the agent called log.Fatalf here and took the process down with it.
func TestBuildSpiffeMtlsConfigReturnsOnContextCancel(t *testing.T) {
	t.Parallel()

	// A path that cannot become a Workload API socket, so every attempt fails.
	socketPath := "unix://" + filepath.Join(t.TempDir(), "absent.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := buildSpiffeMtlsConfig(ctx, socketPath, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("buildSpiffeMtlsConfig() = nil, want a context error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("buildSpiffeMtlsConfig() = %v, want a context deadline error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("buildSpiffeMtlsConfig did not return after its context was cancelled")
	}
}
