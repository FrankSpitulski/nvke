// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	operationTypeEtcdDefrag = "etcd_defrag"

	// defaultMinVotingMembersForDefrag is the floor below which the agent will
	// not restart the local member. Three voters means the cluster survives
	// losing this one; at two the restart costs quorum, and at one it is a full
	// API outage. Callers may raise it but the default refuses the unsafe
	// topologies rather than making the operator opt out of them.
	defaultMinVotingMembersForDefrag = 3

	stepEtcdDefragPreflight      = "etcd_defrag_preflight"
	stepEtcdDefragTransferLeader = "etcd_defrag_transferring_leadership"
	stepEtcdDefragStopping       = "etcd_defrag_stopping_etcd"
	stepEtcdDefragWaitingExit    = "etcd_defrag_waiting_for_etcd_exit"
	stepEtcdDefragRestarting     = "etcd_defrag_restarting_etcd"
	stepEtcdDefragWaitingStart   = "etcd_defrag_waiting_for_etcd_start"
	stepEtcdDefragWaitingHealthy = "etcd_defrag_waiting_for_etcd_health"
	stepEtcdDefragMeasuring      = "etcd_defrag_measuring"
)

// persistedEtcdDefragResult is the typed result carried through the generic
// operation framework, which stores only a string. Encoded on completion and
// decoded by GetEtcdDefragStatus.
type persistedEtcdDefragResult struct {
	Outcome      int32  `json:"outcome"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	SizeBefore   int64  `json:"sizeBefore"`
	SizeAfter    int64  `json:"sizeAfter"`
	Reclaimed    int64  `json:"reclaimed"`
}

// StartEtcdDefrag starts an asynchronous offline defragmentation of the local
// etcd member by restarting it, so etcd defragments during bootstrap. It never
// calls etcdctl defrag, which would block the member for the whole rewrite.
func (s *NodeAgentServer) StartEtcdDefrag(
	ctx context.Context,
	req *pb.StartEtcdDefragRequest,
) (*pb.StartEtcdDefragResponse, error) {
	defer logRPCSpan(ctx, "StartEtcdDefrag")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if !s.isControlPlaneRole() {
		role := ""
		if s.NodeStatus != nil {
			role = s.NodeStatus.Role
		}
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"start etcd defrag: control-plane node required, current role %q",
			role,
		)
	}
	if s.Persister == nil {
		return nil, status.Error(codes.FailedPrecondition, "status persister is not configured")
	}

	minVoters := req.GetMinVotingMembers()
	if minVoters <= 0 {
		minVoters = defaultMinVotingMembersForDefrag
	}

	op, err := s.startNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeEtcdDefrag,
	}, func(runCtx context.Context, progress nodeAgentOperationProgress) error {
		return s.runEtcdDefrag(runCtx, progress, minVoters)
	})
	if err != nil {
		return nil, err
	}
	return &pb.StartEtcdDefragResponse{Operation: nodeAgentOperationToProto(op)}, nil
}

// GetEtcdDefragStatus returns the persisted state of an asynchronous etcd
// defragmentation, including how many bytes the restart reclaimed.
func (s *NodeAgentServer) GetEtcdDefragStatus(
	ctx context.Context,
	req *pb.GetEtcdDefragStatusRequest,
) (*pb.GetEtcdDefragStatusResponse, error) {
	defer logRPCSpan(ctx, "GetEtcdDefragStatus")()

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := s.getNodeAgentOperation(nodeAgentOperationSpec{
		ID:   strings.TrimSpace(req.GetOperationId()),
		Type: operationTypeEtcdDefrag,
	})
	if err != nil {
		return nil, err
	}

	resp := &pb.GetEtcdDefragStatusResponse{Operation: nodeAgentOperationToProto(op)}
	if op.Result == "" {
		// Still running, or it failed before reaching a typed outcome. The
		// operation phase and error message carry what is known.
		resp.ErrorMessage = op.ErrorMessage
		return resp, nil
	}
	var result persistedEtcdDefragResult
	if err := json.Unmarshal([]byte(op.Result), &result); err != nil {
		return nil, status.Errorf(codes.Internal, "decode etcd defrag result: %v", err)
	}
	resp.Outcome = pb.GetEtcdDefragStatusResponse_Outcome(result.Outcome)
	resp.ErrorMessage = result.ErrorMessage
	resp.DbTotalSizeBytesBefore = result.SizeBefore
	resp.DbTotalSizeBytesAfter = result.SizeAfter
	resp.ReclaimedBytes = result.Reclaimed
	return resp, nil
}

// runEtcdDefrag performs the whole per-node sequence: preflight, leadership
// transfer, the manifest move that restarts etcd, and verification that the
// member came back.
//
// Note the asymmetry in how the two failure families are reported. A refusal
// returns nil so the operation succeeds with OUTCOME_REFUSED: the agent
// correctly declined and the node is untouched. A member that does not come
// back returns an error so the operation lands in phase FAILED *and* carries
// OUTCOME_FAILED_NOT_RESTORED, because a caller polling only the phase must not
// mistake that for a clean stop.
func (s *NodeAgentServer) runEtcdDefrag(
	ctx context.Context,
	progress nodeAgentOperationProgress,
	minVoters int32,
) error {
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}

	client, err := s.newEtcdMemberClient()
	if err != nil {
		return s.recordEtcdDefragRefusal(progress, fmt.Sprintf("connect to local kubeadm etcd: %v", err), 0)
	}
	defer client.Close()

	progress.SetStep(stepEtcdDefragPreflight)
	sizeBefore, err := s.etcdDefragPreflight(ctx, client, minVoters)
	if err != nil {
		return s.recordEtcdDefragRefusal(progress, err.Error(), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragTransferLeader)
	if err := s.transferEtcdLeadershipAway(ctx, client); err != nil {
		// Leadership could not be moved, so a restart here would make the
		// cluster briefly unavailable for writes. Nothing has been touched yet,
		// so refusing is free and strictly safer than proceeding.
		return s.recordEtcdDefragRefusal(progress, fmt.Sprintf("transfer etcd leadership away from local member: %v", err), sizeBefore)
	}

	// Past this point etcd is taken down. Any failure to bring it back is
	// FAILED_NOT_RESTORED, not a refusal.
	if err := s.stopStaticPod(ctx, runner, progress, staticPodStopSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		stoppingStep:  stepEtcdDefragStopping,
		waitingStep:   stepEtcdDefragWaitingExit,
		timeout:       time.Duration(defaultStaticPodExitTimeout) * time.Second,
	}); err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf("stop etcd static pod: %v", err), sizeBefore)
	}

	if err := s.restoreStaticPod(ctx, runner, progress, staticPodRestoreSpec{
		containerName: etcdContainerName,
		manifestName:  etcdManifestName,
		restoringStep: stepEtcdDefragRestarting,
		waitingStep:   stepEtcdDefragWaitingStart,
		timeout:       s.etcdHealthTimeoutOrDefault(),
	}); err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf("restore etcd static pod: %v", err), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragWaitingHealthy)
	postStatus, err := s.waitForLocalEtcdHealthy(ctx, client)
	if err != nil {
		return s.recordEtcdDefragFailure(progress, fmt.Sprintf("etcd did not return healthy after restart: %v", err), sizeBefore)
	}

	progress.SetStep(stepEtcdDefragMeasuring)
	sizeAfter := postStatus.DbSize
	reclaimed := int64(0)
	if sizeBefore > sizeAfter {
		reclaimed = sizeBefore - sizeAfter
	}

	// A restart that crossed the threshold but reclaimed nothing means the
	// backend was live data, not free pages. That is a real answer, not a
	// failure, and the caller should stop retrying rather than loop.
	outcome := pb.GetEtcdDefragStatusResponse_OUTCOME_RECLAIMED
	if reclaimed == 0 {
		outcome = pb.GetEtcdDefragStatusResponse_OUTCOME_NOT_RECLAIMED
	}
	slog.InfoContext(ctx, "etcd defragmentation complete",
		"outcome", outcome.String(),
		"dbSizeBefore", sizeBefore,
		"dbSizeAfter", sizeAfter,
		"reclaimedBytes", reclaimed,
	)
	return recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:    int32(outcome),
		SizeBefore: sizeBefore,
		SizeAfter:  sizeAfter,
		Reclaimed:  reclaimed,
	})
}

// etcdDefragPreflight verifies every condition that must hold before the member
// is taken down, and returns the pre-restart backend size. Every error it
// returns is a refusal: nothing here mutates anything, so the caller can report
// OUTCOME_REFUSED knowing the node is untouched.
func (s *NodeAgentServer) etcdDefragPreflight(
	ctx context.Context,
	client etcdMemberClient,
	minVoters int32,
) (int64, error) {
	_, manifestJSON, err := s.readEtcdManifest()
	if err != nil {
		return 0, err
	}
	threshold, found, err := etcdBootstrapDefragThresholdBytes(manifestJSON)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf(
			"etcd manifest sets neither --%s nor --%s, so a restart would not defragment at any size",
			etcdBootstrapDefragFlag, etcdExperimentalBootstrapDefragFlag,
		)
	}

	localStatus, err := client.Status(ctx, etcdLocalEndpoint)
	if err != nil {
		return 0, fmt.Errorf("local etcd member status: %w", err)
	}
	if localStatus == nil || localStatus.Header == nil {
		return 0, errors.New("local etcd status has no member ID")
	}
	sizeBefore := localStatus.DbSize
	if len(localStatus.Errors) > 0 {
		return sizeBefore, fmt.Errorf("local etcd member is unhealthy: %v", localStatus.Errors)
	}

	members, err := client.MemberList(ctx)
	if err != nil {
		return sizeBefore, fmt.Errorf("list etcd members: %w", err)
	}
	if voters := votingMemberCount(members); voters < minVoters {
		return sizeBefore, fmt.Errorf(
			"refusing to restart etcd with %d voting members, minimum is %d", voters, minVoters,
		)
	}

	// The gate that makes the restart worth its cost. Reclaimable space below
	// the configured bootstrap threshold means etcd would start without
	// defragmenting, so the restart would be downtime for nothing.
	reclaimable := reclaimableBytes(localStatus.DbSize, localStatus.DbSizeInUse)
	if reclaimable < threshold {
		return sizeBefore, fmt.Errorf(
			"reclaimable space %d bytes is below the configured bootstrap-defrag threshold %d bytes; a restart would not defragment",
			reclaimable, threshold,
		)
	}
	return sizeBefore, nil
}

// transferEtcdLeadershipAway moves leadership to another voting member when the
// local member is the leader, so the restart does not cost a leader election.
// It is a no-op when the local member is already a follower.
func (s *NodeAgentServer) transferEtcdLeadershipAway(ctx context.Context, client etcdMemberClient) error {
	localStatus, err := client.Status(ctx, etcdLocalEndpoint)
	if err != nil {
		return fmt.Errorf("local etcd member status: %w", err)
	}
	if localStatus == nil || localStatus.Header == nil {
		return fmt.Errorf("local etcd status has no member ID")
	}
	localID := localStatus.Header.MemberId
	if localStatus.Leader != localID {
		return nil
	}

	members, err := client.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("list etcd members: %w", err)
	}
	transfereeID, ok := pickEtcdLeadershipTransferee(members, localID)
	if !ok {
		return fmt.Errorf("no other voting etcd member is available to take leadership")
	}
	localEndpoints := etcdMemberClientURLs(members, localID)
	if len(localEndpoints) == 0 {
		localEndpoints = []string{etcdLocalEndpoint}
	}
	if err := client.TransferLeadership(ctx, localEndpoints, transfereeID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "transferred etcd leadership before defragmentation restart",
		"from", fmt.Sprintf("%x", localID),
		"to", fmt.Sprintf("%x", transfereeID),
	)
	return nil
}

// pickEtcdLeadershipTransferee chooses a started, voting member other than the
// local one. Learners cannot be promoted to leader, and a member that has not
// started (no name yet) cannot serve, so both are skipped.
func pickEtcdLeadershipTransferee(members *clientv3.MemberListResponse, localID uint64) (uint64, bool) {
	if members == nil {
		return 0, false
	}
	for _, m := range members.Members {
		if m.GetID() == localID || m.GetIsLearner() || m.GetName() == "" {
			continue
		}
		return m.GetID(), true
	}
	return 0, false
}

// etcdMemberClientURLs returns the advertised client URLs for a member ID.
func etcdMemberClientURLs(members *clientv3.MemberListResponse, id uint64) []string {
	if members == nil {
		return nil
	}
	for _, m := range members.Members {
		if m.GetID() == id {
			return m.GetClientURLs()
		}
	}
	return nil
}

// recordEtcdDefragRefusal persists a REFUSED outcome and completes the
// operation successfully: nothing was changed, which is the intended behaviour
// when a precondition does not hold.
func (s *NodeAgentServer) recordEtcdDefragRefusal(
	progress nodeAgentOperationProgress,
	reason string,
	sizeBefore int64,
) error {
	slog.Warn("refusing etcd defragmentation; etcd was not restarted", "reason", reason)
	return recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:      int32(pb.GetEtcdDefragStatusResponse_OUTCOME_REFUSED),
		ErrorMessage: reason,
		SizeBefore:   sizeBefore,
	})
}

// recordEtcdDefragFailure persists a FAILED_NOT_RESTORED outcome and returns an
// error so the operation itself is marked failed. etcd was taken down and did
// not verifiably come back, so this node needs intervention and a rollout must
// not advance past it.
func (s *NodeAgentServer) recordEtcdDefragFailure(
	progress nodeAgentOperationProgress,
	reason string,
	sizeBefore int64,
) error {
	slog.Error("etcd defragmentation failed and the member was not confirmed healthy", "reason", reason)
	if err := recordEtcdDefragResult(progress, persistedEtcdDefragResult{
		Outcome:      int32(pb.GetEtcdDefragStatusResponse_OUTCOME_FAILED_NOT_RESTORED),
		ErrorMessage: reason,
		SizeBefore:   sizeBefore,
	}); err != nil {
		return err
	}
	return fmt.Errorf("%s", reason)
}

func recordEtcdDefragResult(progress nodeAgentOperationProgress, result persistedEtcdDefragResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode etcd defrag result: %w", err)
	}
	progress.SetResult(string(encoded))
	return nil
}
