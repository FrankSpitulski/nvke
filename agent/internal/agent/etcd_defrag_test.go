// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
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
			wantReason:  "local etcd member is unhealthy: connection refused",
		},
		{
			// A status with no header is as unusable as no status at all: the
			// member ID is what the leadership check reads. The pre-patch gate
			// used to accept it and only the post-restart wait would notice.
			name:     "refuses a status with no header",
			manifest: etcdManifestThreshold100MiB,
			client: fakeEtcdMemberClient{
				members: testEtcdMembersThree(),
				localID: 2,
				statusFn: func(int) (*clientv3.StatusResponse, error) {
					return &clientv3.StatusResponse{}, nil
				},
			},
			minVoters:   3,
			wantRefused: true,
			wantReason:  "etcd status has no header",
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

// etcdCrictlRunner answers the `crictl ps` probes the static-pod waits make, by
// modelling what actually decides the answer: kubelet replaces the etcd
// container whenever the manifest is removed or its content changes, and the
// replacement carries a new container ID. Both etcd RPCs depend on that - the
// defrag flow moves the manifest aside and back, the patch flow rewrites it in
// place - and the ID changing is the only signal that distinguishes the new pod
// from the old one still serving during kubelet's scan window.
//
// Driving this off the file on disk rather than a fixed script is what lets the
// tests exercise the real stop and restore waits, including a manifest that is
// parked and never comes back.
type etcdCrictlRunner struct {
	t            *testing.T
	manifestPath string

	// stuck models a container that will not exit while its manifest is away,
	// which is the condition that times the stop phase out and would otherwise
	// leave etcd parked.
	stuck bool
	// startDelay is how many probes report nothing running after a swap,
	// forcing the restart wait to actually sleep. Without it the replacement
	// appears on the first probe and no cancellation could ever be observed.
	startDelay int

	generation   int
	lastContent  string
	observed     bool
	absent       bool
	delayedSoFar int
	calls        int
}

func (r *etcdCrictlRunner) Run(name string, _ []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.t.Helper()
	require.Equal(r.t, "crictl", name, "the static-pod waits should only shell out to crictl")
	r.calls++

	content, err := os.ReadFile(r.manifestPath)
	if err != nil {
		r.absent = true
		if r.stuck {
			return etcdCrictlPS(r.containerID()), nil
		}
		return etcdCrictlPS(), nil
	}
	if !r.observed {
		r.observed = true
		r.lastContent = string(content)
		return etcdCrictlPS(r.containerID()), nil
	}
	if r.absent || string(content) != r.lastContent {
		r.absent = false
		r.lastContent = string(content)
		r.generation++
		r.delayedSoFar = 0
	}
	if r.delayedSoFar < r.startDelay {
		r.delayedSoFar++
		return etcdCrictlPS(), nil
	}
	return etcdCrictlPS(r.containerID()), nil
}

func (r *etcdCrictlRunner) containerID() string {
	return fmt.Sprintf("etcd-%d", r.generation)
}

func (r *etcdCrictlRunner) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	out, err := r.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: out}, err
}

func (*etcdCrictlRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

func etcdCrictlPS(ids ...string) []byte {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`{"id":%q}`, id))
	}
	return []byte(`{"containers":[` + strings.Join(entries, ",") + `]}`)
}

// etcdRestartTestServer builds a control-plane server whose manifest sits in a
// temp root, with every wait compressed so the timeout paths run in
// milliseconds instead of minutes.
func etcdRestartTestServer(t *testing.T, client *fakeEtcdMemberClient) (*NodeAgentServer, *etcdCrictlRunner) {
	t.Helper()
	s := writeEtcdManifest(t, etcdManifestThreshold100MiB)
	runner := &etcdCrictlRunner{t: t, manifestPath: s.etcdManifestPath()}
	s.etcdClient = client
	s.Runner = runner
	s.etcdHealthTimeout = 100 * time.Millisecond
	s.etcdHealthPollInterval = time.Millisecond
	s.etcdExitTimeout = 100 * time.Millisecond
	s.drainKubeVipPollInterval = time.Millisecond
	return s, runner
}

// etcdSizingStatus reports a shrinking backend: the probes before the restart
// see sizeBefore and everything after sees sizeAfter, which is how the RPC
// measures what the restart reclaimed.
func etcdSizingStatus(sizeBefore, sizeInUse, sizeAfter int64, firstPostRestartCall int) func(int) (*clientv3.StatusResponse, error) {
	return func(call int) (*clientv3.StatusResponse, error) {
		dbSize := sizeBefore
		if call >= firstPostRestartCall {
			dbSize = sizeAfter
		}
		return &clientv3.StatusResponse{
			Header:      &etcdserverpb.ResponseHeader{MemberId: 2},
			Leader:      1,
			DbSize:      dbSize,
			DbSizeInUse: sizeInUse,
		}, nil
	}
}

func decodeDefragResult(t *testing.T, progress *fakeOperationProgress) persistedEtcdDefragResult {
	t.Helper()
	var result persistedEtcdDefragResult
	require.NoError(t, json.Unmarshal([]byte(progress.result), &result))
	return result
}

func etcdManifestIsActive(t *testing.T, s *NodeAgentServer) bool {
	t.Helper()
	_, err := os.Stat(s.etcdManifestPath())
	return err == nil
}

func TestRunEtcdDefragRestartSequence(t *testing.T) {
	const mib = int64(1024 * 1024)

	t.Run("a member that comes back smaller reports what it reclaimed", func(t *testing.T) {
		// Status call 3 is the first probe after the restart: calls 1 and 2 are
		// the preflight and the leadership check.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: etcdSizingStatus(500*mib, 100*mib, 120*mib, 3),
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		require.NoError(t, s.runEtcdDefrag(context.Background(), progress, 3))

		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED), result.Outcome)
		assert.Equal(t, 500*mib, result.SizeBefore)
		assert.Equal(t, 120*mib, result.SizeAfter)
		assert.Equal(t, 380*mib, result.Reclaimed)
		assert.Contains(t, progress.steps, stepEtcdDefragWaitingHealthy)
		assert.True(t, etcdManifestIsActive(t, s), "the manifest must be back in place")
	})

	t.Run("a member that comes back the same size is NOT_RECLAIMED, not a failure", func(t *testing.T) {
		// Live data cannot be defragmented away. The caller has to be able to
		// tell that apart from a broken restart, or it will retry forever.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: etcdSizingStatus(500*mib, 100*mib, 500*mib, 3),
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		require.NoError(t, s.runEtcdDefrag(context.Background(), progress, 3))

		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED), result.Outcome)
		assert.Zero(t, result.Reclaimed)
	})

	// The regression this exists for: stopStaticPod moves the manifest aside
	// before it waits for the container to exit, so a timeout in that wait used
	// to return straight out and leave etcd.yaml parked with etcd down and
	// nothing scheduled to bring it back.
	t.Run("a stop that times out still puts the manifest back", func(t *testing.T) {
		client := &fakeEtcdMemberClient{
			members:     testEtcdMembersThree(),
			localID:     2,
			leaderID:    1,
			dbSize:      500 * mib,
			dbSizeInUse: 100 * mib,
		}
		s, runner := etcdRestartTestServer(t, client)
		runner.stuck = true
		progress := &fakeOperationProgress{}

		err := s.runEtcdDefrag(context.Background(), progress, 3)

		// Nil, because the member is verifiably back: this is a clean stop for
		// a rollout, not a node that needs an operator.
		require.NoError(t, err)
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_RESTORED), result.Outcome)
		assert.Contains(t, result.ErrorMessage, "stop etcd static pod")
		assert.True(t, etcdManifestIsActive(t, s), "a failed stop must not leave the manifest parked")
	})

	t.Run("a member that never returns healthy is FAILED_NOT_RESTORED and errors", func(t *testing.T) {
		// The manifest is already back by the time the health wait runs, so
		// there is nothing left for the agent to restore here - only an
		// operator can help an etcd that will not start from its own manifest.
		client := &fakeEtcdMemberClient{
			members:  testEtcdMembersThree(),
			localID:  2,
			leaderID: 1,
			statusFn: func(call int) (*clientv3.StatusResponse, error) {
				if call >= 3 {
					return nil, errors.New("connection refused")
				}
				return &clientv3.StatusResponse{
					Header:      &etcdserverpb.ResponseHeader{MemberId: 2},
					Leader:      1,
					DbSize:      500 * mib,
					DbSizeInUse: 100 * mib,
				}, nil
			},
		}
		s, _ := etcdRestartTestServer(t, client)
		progress := &fakeOperationProgress{}

		err := s.runEtcdDefrag(context.Background(), progress, 3)

		require.Error(t, err, "a node needing intervention must fail the operation, not just the outcome")
		result := decodeDefragResult(t, progress)
		assert.Equal(t, int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED), result.Outcome)
		assert.Contains(t, result.ErrorMessage, "did not return healthy")
		assert.True(t, etcdManifestIsActive(t, s))
	})
}

func testEtcdMembersThree() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		{ID: 1, Name: "cp-1", ClientURLs: []string{"https://cp-1:2379"}},
		{ID: 2, Name: "cp-2", ClientURLs: []string{"https://cp-2:2379"}},
		{ID: 3, Name: "cp-3", ClientURLs: []string{"https://cp-3:2379"}},
	}
}
