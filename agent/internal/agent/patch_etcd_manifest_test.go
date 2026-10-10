// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

func setBootstrapDefragOp(value string) []*pb.FlagOp {
	return []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{
		Name:  etcdBootstrapDefragFlag,
		Value: value,
	}}}}
}

func TestValidatePatchEtcdManifestRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     *pb.PatchEtcdManifestRequest
		wantErr string
	}{
		{
			name: "a well-formed set op is accepted",
			req:  &pb.PatchEtcdManifestRequest{FlagOps: setBootstrapDefragOp("2048")},
		},
		{
			name:    "nil request is rejected",
			wantErr: "request is required",
		},
		{
			name:    "no flag ops is rejected",
			req:     &pb.PatchEtcdManifestRequest{},
			wantErr: "at least one flag op",
		},
		{
			name: "a flag op with neither set nor remove is rejected",
			req: &pb.PatchEtcdManifestRequest{
				FlagOps: []*pb.FlagOp{{}},
			},
			wantErr: "neither set nor remove",
		},
		{
			name: "leading dashes in a flag name are rejected",
			req: &pb.PatchEtcdManifestRequest{
				FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Set{Set: &pb.SetFlag{Name: "--foo", Value: "1"}}}},
			},
			wantErr: "leading dashes",
		},
		{
			name: "an embedded equals sign in a flag name is rejected",
			req: &pb.PatchEtcdManifestRequest{
				FlagOps: []*pb.FlagOp{{Op: &pb.FlagOp_Remove{Remove: &pb.RemoveFlag{Name: "foo=1"}}}},
			},
			wantErr: "must not contain '='",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePatchEtcdManifestRequest(tt.req)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPatchEtcdManifest(t *testing.T) {
	t.Run("an already-configured manifest is UNCHANGED and does not restart etcd", func(t *testing.T) {
		// The restart is the expensive part of this RPC, so convergence matters:
		// re-running a completed rollout must not bounce every member again.
		s := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=2048"]
`)
		client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2}
		s.etcdClient = client

		resp, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})
		require.NoError(t, err)
		assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_UNCHANGED, resp.GetOutcome())
		assert.Zero(t, client.statusCalls, "UNCHANGED must short-circuit before etcd is contacted")
	})

	t.Run("a differing value is not treated as unchanged", func(t *testing.T) {
		s := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=200"]
`)
		// No etcd client is injected, so the call gets as far as connecting to
		// the local member and fails there. Reaching the connection attempt is
		// the proof that the differing value was not swallowed as UNCHANGED.
		_, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "connect to local kubeadm etcd")
	})

	t.Run("worker nodes are rejected", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.NodeStatus = &NodeStatus{Role: "worker"}

		_, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("an invalid request is rejected before anything is read", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)

		_, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{})
		require.Error(t, err)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("a missing manifest is a precondition failure", func(t *testing.T) {
		s := &NodeAgentServer{
			RootPath:   t.TempDir(),
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		}

		_, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}

func TestApplyFlagOpsOnEtcdManifest(t *testing.T) {
	// The etcd manifest is kubeadm's command-carries-flags shape, so a new flag
	// must land in command rather than opening an args list.
	s := writeEtcdManifest(t, etcdManifestWithFlags)
	_, manifestJSON, err := s.readEtcdManifest()
	require.NoError(t, err)

	out, err := applyFlagOps(manifestJSON, etcdContainerName, setBootstrapDefragOp("4096"))
	require.NoError(t, err)

	value, found, err := readFlagValue(out, etcdContainerName, etcdBootstrapDefragFlag)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "4096", value)

	// Setting the stable spelling must not disturb the experimental one that is
	// already present; migrating between them is an explicit remove, not a
	// side effect of a set.
	experimental, found, err := readFlagValue(out, etcdContainerName, etcdExperimentalBootstrapDefragFlag)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "2048", experimental)
}
