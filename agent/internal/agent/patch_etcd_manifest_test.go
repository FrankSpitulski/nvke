// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
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

// etcdManifestBelowTarget differs from the 2048 the tests request, so the RPC
// gets past the UNCHANGED short-circuit and into the restart path.
const etcdManifestBelowTarget = `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=200"]
`

func patchEtcdTestServer(t *testing.T, client *fakeEtcdMemberClient) (*NodeAgentServer, *etcdCrictlRunner) {
	t.Helper()
	s := writeEtcdManifest(t, etcdManifestBelowTarget)
	runner := &etcdCrictlRunner{t: t, manifestPath: s.etcdManifestPath()}
	s.etcdClient = client
	s.Runner = runner
	s.etcdHealthTimeout = 100 * time.Millisecond
	s.etcdHealthPollInterval = time.Millisecond
	s.etcdExitTimeout = 100 * time.Millisecond
	s.drainKubeVipPollInterval = time.Millisecond
	return s, runner
}

func patchedThreshold(t *testing.T, s *NodeAgentServer) string {
	t.Helper()
	_, manifestJSON, err := s.readEtcdManifest()
	require.NoError(t, err)
	value, _, err := readFlagValue(manifestJSON, etcdContainerName, etcdBootstrapDefragFlag)
	require.NoError(t, err)
	return value
}

func TestPatchEtcdManifestPreconditions(t *testing.T) {
	t.Run("a cluster that cannot survive the restart is refused", func(t *testing.T) {
		// Two voters means restarting one costs quorum. StartEtcdDefrag already
		// refused this; the patch RPC does the same destructive thing and used
		// to go ahead.
		client := &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}
		s, _ := patchEtcdTestServer(t, client)

		resp, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})

		require.NoError(t, err)
		assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())
		assert.Contains(t, resp.GetErrorMessage(), "2 of 2 voting members are available, need 3")
		assert.Equal(t, "200", patchedThreshold(t, s), "a refusal must not write the manifest")
	})

	t.Run("min_voting_members lets a caller patch a topology that cannot preserve quorum", func(t *testing.T) {
		// The configuration path has to work on single- and two-member control
		// planes, where the restart is a deliberate brief outage.
		client := &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}
		s, _ := patchEtcdTestServer(t, client)

		resp, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps:          setBootstrapDefragOp("2048"),
			MinVotingMembers: 1,
		})

		require.NoError(t, err)
		assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
		assert.Equal(t, "2048", patchedThreshold(t, s))
	})

	t.Run("a member already reporting an alarm is refused before the write", func(t *testing.T) {
		// A restart does not clear an alarm, so proceeding would bounce the
		// member, wait out the health timeout, roll it back, wait again, and
		// then fail for a condition that predated the call.
		client := &fakeEtcdMemberClient{
			members:      testEtcdMembersThree(),
			localID:      2,
			leaderID:     1,
			statusErrors: []string{"NOSPACE"},
		}
		s, runner := patchEtcdTestServer(t, client)

		resp, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})

		require.NoError(t, err)
		assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())
		assert.Contains(t, resp.GetErrorMessage(), "NOSPACE")
		assert.Equal(t, "200", patchedThreshold(t, s), "a refusal must not write the manifest")
		assert.Zero(t, runner.calls, "etcd must not be restarted for a pre-existing alarm")
	})

	t.Run("an absent status header is refused rather than read as healthy", func(t *testing.T) {
		client := &fakeEtcdMemberClient{
			members: testEtcdMembersThree(),
			localID: 2,
			statusFn: func(context.Context, int) (*clientv3.StatusResponse, error) {
				return nil, nil //nolint:nilnil // models a client returning neither status nor error
			},
		}
		s, runner := patchEtcdTestServer(t, client)

		resp, err := s.PatchEtcdManifest(context.Background(), &pb.PatchEtcdManifestRequest{
			FlagOps: setBootstrapDefragOp("2048"),
		})

		require.NoError(t, err)
		assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_FAILED_RESTORED, resp.GetOutcome())
		assert.Contains(t, resp.GetErrorMessage(), "etcd returned no status")
		assert.Zero(t, runner.calls)
	})
}

// The regression this exists for: the post-write wait used to run on the
// caller's context. A client that disconnected during it was read as a failed
// restart, which rewrote the manifest - swapping the pod a second time, possibly
// interrupting the defragmentation it had just triggered - and then reported
// FAILED_NOT_RESTORED without verifying anything, because the rollback inherited
// the same dead context. The wait is minutes long by design, so an ordinary
// client deadline was enough to trigger it.
func TestPatchEtcdManifestSurvivesCallerCancellation(t *testing.T) {
	client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1}
	s, runner := patchEtcdTestServer(t, client)
	// Make the replacement pod take a few probes to appear, so the restart wait
	// genuinely sleeps. Without that it returns on the first probe and no
	// cancellation could ever be observed.
	runner.startDelay = 3

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := s.PatchEtcdManifest(ctx, &pb.PatchEtcdManifestRequest{
		FlagOps: setBootstrapDefragOp("2048"),
	})

	require.NoError(t, err)
	assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_APPLIED, resp.GetOutcome())
	assert.Equal(t, "2048", patchedThreshold(t, s), "the patch must survive the caller going away")
}

// failingListRunner fails its first crictl listing and then reports the same
// container forever: the patched etcd that is still serving because kubelet
// never swapped the pod.
type failingListRunner struct {
	t     *testing.T
	calls int
}

func (r *failingListRunner) Run(name string, _ []string, _ *nvkeutils.CommandOptions) ([]byte, error) {
	r.t.Helper()
	require.Equal(r.t, "crictl", name)
	r.calls++
	if r.calls == 1 {
		return nil, errors.New("crictl ps: connection to the container runtime failed")
	}
	return etcdCrictlPS("etcd-old"), nil
}

func (r *failingListRunner) RunSplitOutput(name string, args []string, opts *nvkeutils.CommandOptions) (nvkeutils.CommandOutput, error) {
	out, err := r.Run(name, args, opts)
	return nvkeutils.CommandOutput{Stdout: out}, err
}

func (*failingListRunner) RunDetached(string, []string, *nvkeutils.CommandOptions) error {
	return errors.New("unexpected detached command")
}

// The regression this exists for: the rollback tolerates a failed container
// listing, which is right, but it passed the resulting nil through as an empty
// baseline rather than an unknown one. An empty baseline makes every running
// container count as new, so the patched etcd that was still serving satisfied
// the pod-swap gate on the first poll, the health probe passed against that
// same process, and the call reported FAILED_RESTORED - "the node is back in
// its pre-call state" - without a swap ever happening.
func TestEtcdPatchRestoreDoesNotClaimRestoredWhenTheListingFails(t *testing.T) {
	client := &fakeEtcdMemberClient{members: testEtcdMembersThree(), localID: 2, leaderID: 1}
	s, _ := patchEtcdTestServer(t, client)
	manifestRaw, _, err := s.readEtcdManifest()
	require.NoError(t, err)
	runner := &failingListRunner{t: t}

	resp := s.etcdPatchRestore(context.Background(), runner, client,
		"/backup/etcd.yaml", s.etcdManifestPath(), manifestRaw, 123, "etcd restart after patch failed")

	assert.Equal(t, pb.PatchEtcdManifestResponse_OUTCOME_FAILED_NOT_RESTORED, resp.GetOutcome(),
		"an unverifiable rollback must not be reported as restored")
	assert.Contains(t, resp.GetErrorMessage(), "wait for etcd pod swap")
	assert.Greater(t, runner.calls, 1, "the restore must still run after the listing fails")
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
