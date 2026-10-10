// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// fakeOperationProgress captures what the operation would have persisted.
type fakeOperationProgress struct {
	steps  []string
	result string
}

func (f *fakeOperationProgress) SetStep(step string)     { f.steps = append(f.steps, step) }
func (f *fakeOperationProgress) SetResult(result string) { f.result = result }

// a manifest whose bootstrap-defrag threshold is 100 MiB, small enough that
// tests can sit either side of it with round numbers.
const etcdManifestThreshold100MiB = `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --quota-backend-bytes=17179869184
        - --bootstrap-defrag-threshold-megabytes=100
`

func TestEtcdDefragPreflight(t *testing.T) {
	const mib = int64(1024 * 1024)

	tests := []struct {
		name        string
		manifest    string
		client      fakeEtcdMemberClient
		minVoters   int32
		wantRefused bool
		wantReason  string
	}{
		{
			name:     "passes when reclaimable space clears the threshold",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			minVoters: 3,
		},
		{
			name:     "refuses when reclaimable space is below the threshold",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				dbSize:      150 * mib,
				dbSizeInUse: 100 * mib,
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "below the configured bootstrap-defrag threshold",
		},
		{
			name: "refuses when no bootstrap-defrag flag is configured",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembersThree(),
				localID:     2,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "would not defragment at any size",
		},
		{
			name:     "refuses below the voting member floor",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:     testEtcdMembers(), // two voters
				localID:     2,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "minimum is 3",
		},
		{
			name:     "refuses when learners pad the member count",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members: []*etcdserverpb.Member{
					{ID: 1, Name: "cp-1"},
					{ID: 2, Name: "cp-2"},
					{ID: 3, Name: "learner", IsLearner: true},
				},
				localID:     2,
				dbSize:      500 * mib,
				dbSizeInUse: 100 * mib,
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "2 voting members",
		},
		{
			name:     "refuses an unhealthy local member",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:      testEtcdMembersThree(),
				localID:      2,
				dbSize:       500 * mib,
				dbSizeInUse:  100 * mib,
				statusErrors: []string{"NOSPACE"},
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "unhealthy",
		},
		{
			name:     "refuses when the member cannot be reached",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members:   testEtcdMembersThree(),
				localID:   2,
				statusErr: errors.New("connection refused"),
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "local etcd member status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			client := tt.client

			_, err := s.etcdDefragPreflight(context.Background(), &client, tt.minVoters)

			if !tt.wantRefused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantReason)
		})
	}
}

func TestPickEtcdLeadershipTransferee(t *testing.T) {
	tests := []struct {
		name    string
		members []*etcdserverpb.Member
		localID uint64
		wantID  uint64
		wantOK  bool
	}{
		{
			name:    "picks another voting member",
			members: testEtcdMembersThree(),
			localID: 2,
			wantID:  1,
			wantOK:  true,
		},
		{
			name: "skips learners, which cannot lead",
			members: []*etcdserverpb.Member{
				{ID: 1, Name: "learner", IsLearner: true},
				{ID: 3, Name: "cp-3"},
			},
			localID: 2,
			wantID:  3,
			wantOK:  true,
		},
		{
			name: "skips members that have not started",
			members: []*etcdserverpb.Member{
				{ID: 1, Name: ""},
				{ID: 3, Name: "cp-3"},
			},
			localID: 2,
			wantID:  3,
			wantOK:  true,
		},
		{
			name:    "reports none available on a single-member cluster",
			members: []*etcdserverpb.Member{{ID: 2, Name: "cp-2"}},
			localID: 2,
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickEtcdLeadershipTransferee(&clientv3.MemberListResponse{Members: tt.members}, tt.localID)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantID, got)
			}
		})
	}
}

func TestTransferEtcdLeadershipAway(t *testing.T) {
	t.Run("is a no-op when the local member is a follower", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1}

		require.NoError(t, s.transferEtcdLeadershipAway(context.Background(), client))
		assert.Zero(t, client.movedTo, "a follower must not trigger a leadership transfer")
	})

	t.Run("moves leadership off the local member", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 2}

		require.NoError(t, s.transferEtcdLeadershipAway(context.Background(), client))
		assert.Equal(t, uint64(1), client.movedTo)
		assert.Equal(t, []string{"https://cp-2:2379"}, client.movedFrom,
			"MoveLeader must be issued against the current leader, which is the local member")
	})

	t.Run("fails when there is nobody to hand leadership to", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
		client := &fakeEtcdMemberClient{
			members:  []*etcdserverpb.Member{{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}}},
			localID:  2,
			leaderID: 2,
		}

		err := s.transferEtcdLeadershipAway(context.Background(), client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no other voting etcd member")
	})
}

func TestRecordEtcdDefragOutcomes(t *testing.T) {
	t.Run("a refusal completes the operation successfully", func(t *testing.T) {
		s := &NodeAgentServer{}
		progress := &fakeOperationProgress{}

		// A refusal means nothing was touched, so the operation itself has not
		// failed; the REFUSED outcome carries the reason.
		err := s.recordEtcdDefragRefusal(progress, "below threshold", 123)
		require.NoError(t, err)

		var result persistedEtcdDefragResult
		require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_REFUSED), result.Outcome)
		assert.Equal(t, "below threshold", result.ErrorMessage)
		assert.Equal(t, int64(123), result.SizeBefore)
	})

	t.Run("an unrecovered member fails the operation and records the outcome", func(t *testing.T) {
		s := &NodeAgentServer{}
		progress := &fakeOperationProgress{}

		// Both signals matter: a caller watching only the operation phase must
		// see a failure, and one reading the outcome must see that etcd is down.
		err := s.recordEtcdDefragFailure(progress, "etcd never came back", 456)
		require.Error(t, err)

		var result persistedEtcdDefragResult
		require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
		assert.Equal(t, int64(456), result.SizeBefore)
	})
}

func testEtcdMembersThree() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
		{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
		{ID: 3, Name: "cp-3", ClientURLs: []string{"https://cp-3:2379"}},
	}
}
