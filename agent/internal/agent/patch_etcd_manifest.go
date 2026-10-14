// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	nvkeutils "github.com/dsx-ai-factory/nvke/agent/pkg/utils"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	// etcdManifestBackupPrefix names the timestamped snapshots PatchEtcdManifest
	// writes. The trailing dot keeps a glob on it from matching the fixed-name
	// etcd.yaml the stop/restore flow parks in the same directory.
	etcdManifestBackupPrefix = etcdManifestName + "."

	// maxEtcdManifestBackups bounds how many snapshots are retained.
	maxEtcdManifestBackups = 5

	// defaultEtcdHealthTimeout bounds the wait for the local member to report
	// healthy after a restart. It is generous because a restart that crosses the
	// bootstrap-defrag threshold rewrites the whole backend file before etcd
	// starts serving, and that scales with database size rather than being a
	// fixed startup cost.
	defaultEtcdHealthTimeout = 10 * time.Minute

	// defaultEtcdHealthPollInterval is the gap between health probes.
	defaultEtcdHealthPollInterval = 2 * time.Second

	// defaultEtcdCallTimeout bounds a single etcd API call.
	//
	// clientv3 waits for the connection to become ready rather than failing
	// fast, so a call made on a context with no deadline blocks for as long as
	// the member is unreachable. The defragmentation path runs on the operation
	// framework's background context, which has no deadline, so one such call
	// would hang the operation in RUNNING forever while it holds
	// staticPodMutationMu - blocking certificate renewal, kube-vip restore and
	// every other static-pod transaction until the agent process restarted. The
	// outer health-wait deadline does not help, because it is only consulted
	// after a call returns.
	defaultEtcdCallTimeout = 15 * time.Second

	// defaultEtcdExitTimeout bounds the wait for the etcd container to exit
	// after its manifest is moved aside.
	//
	// The shutdown flow's 60s is not enough here. That budget has to cover
	// kubelet noticing the manifest is gone, which is bounded by its
	// file-check frequency rather than by anything the agent controls, and then
	// the pod's full termination grace period. Overrunning it is not a benign
	// timeout for this flow: the manifest is already parked, so the caller has
	// to put it back, and every second of the overrun is etcd downtime.
	defaultEtcdExitTimeout = 3 * time.Minute
)

func (s *NodeAgentServer) etcdHealthTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdHealthTimeout > 0 {
		return s.etcdHealthTimeout
	}
	return defaultEtcdHealthTimeout
}

func (s *NodeAgentServer) etcdHealthPollIntervalOrDefault() time.Duration {
	if s != nil && s.etcdHealthPollInterval > 0 {
		return s.etcdHealthPollInterval
	}
	return defaultEtcdHealthPollInterval
}

// etcdCallContext derives a context for one etcd API call. Every etcd call in
// these RPCs goes through it, so none can block past its own deadline.
func (s *NodeAgentServer) etcdCallContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.etcdCallTimeoutOrDefault())
}

func (s *NodeAgentServer) etcdCallTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdCallTimeout > 0 {
		return s.etcdCallTimeout
	}
	return defaultEtcdCallTimeout
}

func (s *NodeAgentServer) etcdExitTimeoutOrDefault() time.Duration {
	if s != nil && s.etcdExitTimeout > 0 {
		return s.etcdExitTimeout
	}
	return defaultEtcdExitTimeout
}

// etcdMutationBudgetOrDefault bounds the whole post-write phase as a backstop.
//
// It has to accommodate four sequential waits, each already capped at the health
// timeout: the pod swap and health wait for the apply, and the same pair again
// for a rollback. Those inner waits are bounded by wall-clock deadlines rather
// than by this context, so nothing here can hang indefinitely; this exists so
// the detached context cannot outlive the work, and it is deliberately not
// tighter, because truncating a legitimate rollback is worse than a long wait.
func (s *NodeAgentServer) etcdMutationBudgetOrDefault() time.Duration {
	return 4 * s.etcdHealthTimeoutOrDefault()
}

// waitForLocalEtcdHealthy polls the local member until Status succeeds and
// reports no errors, or the deadline passes. A freshly restarted etcd refuses
// connections for a while and, when it defragments on the way up, stays
// unreachable for the length of the rewrite, so connection failures are
// expected and retried rather than treated as terminal.
//
// It returns the last successful status so callers can read the post-restart
// backend size without a second round trip.
func (s *NodeAgentServer) waitForLocalEtcdHealthy(
	ctx context.Context,
	client etcdMemberClient,
) (*clientv3.StatusResponse, error) {
	deadline := time.Now().Add(s.etcdHealthTimeoutOrDefault())
	for {
		probeCtx, cancel := s.etcdCallContext(ctx)
		st, err := client.Status(probeCtx, etcdLocalEndpoint)
		cancel()
		lastErr := etcdStatusHealthError(st, err)
		if lastErr == nil {
			return st, nil
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("etcd did not become healthy within %s: %w", s.etcdHealthTimeoutOrDefault(), lastErr)
		}
		if err := sleepOrDone(ctx, s.etcdHealthPollIntervalOrDefault()); err != nil {
			return nil, err
		}
	}
}

// PatchEtcdManifest applies convergent flag operations to the local etcd
// static-pod manifest.
//
// It follows the same two-lane error-boundary contract as
// PatchAPIServerManifest: the APPLIED / UNCHANGED / FAILED_RESTORED /
// FAILED_NOT_RESTORED business outcomes ride on the response with a NIL gRPC
// error, so a rollout can tell a clean halt from a node that needs
// intervention. Only role, request-validation, and manifest-read/parse failures
// use real gRPC error codes.
//
// The difference from the apiserver RPC is that here the pod restart is the
// point rather than a side effect: etcd evaluates its bootstrap-defrag
// threshold only at startup. Re-running against an already-configured manifest
// returns UNCHANGED and leaves etcd alone.
//
// Callers are responsible for serializing this across members. The agent
// verifies local health before and after, but it cannot see whether a peer is
// mid-restart, so concurrent calls across members risk quorum loss.
func (s *NodeAgentServer) PatchEtcdManifest(
	ctx context.Context,
	req *pb.PatchEtcdManifestRequest,
) (*pb.PatchEtcdManifestResponse, error) {
	defer logRPCSpan(ctx, "PatchEtcdManifest")()

	if !s.isControlPlaneRole() {
		role := ""
		if s.NodeStatus != nil {
			role = s.NodeStatus.Role
		}
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"patch etcd manifest: control-plane node required, current role %q",
			role,
		)
	}
	if err := validatePatchEtcdManifestRequest(req); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "patch etcd manifest: %v", err)
	}

	// Unlike StartEtcdDefrag this RPC is a configuration path, so a caller may
	// legitimately need it on a topology where no value preserves quorum. The
	// default still refuses those; passing 1 is the explicit opt-in.
	minVoters := req.GetMinVotingMembers()
	if minVoters <= 0 {
		minVoters = defaultMinVotingMembersForDefrag
	}

	// Serialize against the other agent-owned static-pod transactions
	// (graceful shutdown, certificate renewal) so two flows never have the etcd
	// manifest in a half-applied state at the same time.
	s.staticPodMutationMu.Lock()
	defer s.staticPodMutationMu.Unlock()

	// Phase 1: compute the target manifest in memory. Nothing is written here,
	// so any failure leaves the node untouched.
	manifestPath := s.etcdManifestPath()
	manifestRaw, origJSON, err := s.readEtcdManifest()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "patch etcd manifest: %v", err)
	}

	mergedJSON, err := applyFlagOps(origJSON, etcdContainerName, req.GetFlagOps())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "patch etcd manifest: apply flag ops: %v", err)
	}
	if jsonSemanticEqual(origJSON, mergedJSON) {
		slog.InfoContext(ctx, "PatchEtcdManifest: manifest already matches request - UNCHANGED", "manifest", manifestPath)
		return &pb.PatchEtcdManifestResponse{
			Outcome: pb.PatchEtcdManifestResponse_OUTCOME_UNCHANGED,
		}, nil
	}
	mergedYAML, err := yaml.JSONToYAML(mergedJSON)
	if err != nil {
		return s.etcdPatchCleanFailure("", fmt.Sprintf("marshal merged etcd manifest: %v", err)), nil
	}

	client, err := s.newEtcdMemberClient()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "patch etcd manifest: connect to local kubeadm etcd: %v", err)
	}
	defer client.Close()

	// Refuse to restart a member that is not healthy now: the post-restart wait
	// could not then distinguish damage this call caused from damage that
	// predated it.
	preStatusCtx, cancelPreStatus := s.etcdCallContext(ctx)
	preStatus, statusErr := client.Status(preStatusCtx, etcdLocalEndpoint)
	cancelPreStatus()
	if healthErr := etcdStatusHealthError(preStatus, statusErr); healthErr != nil {
		return s.etcdPatchCleanFailure("", fmt.Sprintf("local etcd member is not healthy before patch: %v", healthErr)), nil
	}
	sizeBefore := preStatus.DbSize

	// Refuse to restart when the cluster could not survive losing this member.
	// The count that matters is of members actually serving, not of members
	// registered: see etcdRestartSafetyFloor. This is still only a snapshot, so
	// it does not make concurrent calls across members safe - the caller
	// sequences those - but it stops a patch from finishing off an already
	// degraded cluster.
	memberListCtx, cancelMemberList := s.etcdCallContext(ctx)
	members, err := client.MemberList(memberListCtx)
	cancelMemberList()
	if err != nil {
		return s.etcdPatchCleanFailure("", fmt.Sprintf("list etcd members: %v", err)), nil
	}
	registeredVoters := votingMemberCount(members)
	availableVoters := s.availableVotingMembers(ctx, client, members, preStatus.Header.MemberId)
	if floor := etcdRestartSafetyFloor(registeredVoters, minVoters); availableVoters < floor {
		return s.etcdPatchCleanFailure("", fmt.Sprintf(
			"refusing to restart etcd: %d of %d voting members are available, need %d",
			availableVoters, registeredVoters, floor,
		)), nil
	}

	backupPath, err := s.snapshotStaticPodManifest(manifestPath, etcdManifestBackupPrefix, maxEtcdManifestBackups, manifestRaw)
	if err != nil {
		return s.etcdPatchCleanFailure("", fmt.Sprintf("snapshot etcd manifest: %v", err)), nil
	}

	// Phase 2: commit. Past the write the live manifest is mutated and kubelet
	// will swap the pod, so every failure below has to restore and re-verify.
	runner := s.Runner
	if runner == nil {
		runner = &nvkeutils.ExecRunner{}
	}
	previousContainerIDs, err := listStaticPodContainerIDs(runner, etcdContainerName)
	if err != nil {
		return s.etcdPatchCleanFailure(backupPath, fmt.Sprintf("list etcd containers: %v", err)), nil
	}

	spec := fileSpec{path: manifestPath, content: mergedYAML, mode: patchManifestFileMode, uid: 0, gid: 0}
	if err := atomicWriteFileWithOwnership(spec, s.kubeadmFileOps); err != nil {
		// The atomic write failed, so the original manifest is intact and no pod
		// swap was triggered.
		return s.etcdPatchCleanFailure(backupPath, fmt.Sprintf("write merged etcd manifest %s: %v", manifestPath, err)), nil
	}

	// Past the write the node is mutated, so the wait and any rollback must not
	// inherit the caller's cancellation. A client that disconnected during the
	// health wait used to be read as a failed restart: that rewrote the
	// manifest, swapping the pod a second time and possibly interrupting the
	// defragmentation it had just triggered, then reported
	// FAILED_NOT_RESTORED without having verified anything, because the rollback
	// inherited the same dead context. The wait is minutes long by design, which
	// is longer than a typical client deadline, so this was reachable in normal
	// operation rather than only on an explicit cancel.
	mutationCtx, cancelMutation := context.WithTimeout(context.WithoutCancel(ctx), s.etcdMutationBudgetOrDefault())
	defer cancelMutation()

	if err := s.waitForEtcdRestart(mutationCtx, runner, client, previousContainerIDs); err != nil {
		slog.ErrorContext(ctx, "etcd did not come back healthy after manifest patch; restoring", "err", err)
		return s.etcdPatchRestore(mutationCtx, runner, client, backupPath, manifestPath, manifestRaw, sizeBefore,
			fmt.Sprintf("etcd restart after patch failed: %v", err)), nil
	}

	postStatusCtx, cancelPostStatus := s.etcdCallContext(mutationCtx)
	postStatus, err := client.Status(postStatusCtx, etcdLocalEndpoint)
	cancelPostStatus()
	sizeAfter := int64(0)
	if err == nil && postStatus != nil {
		sizeAfter = postStatus.DbSize
	}

	slog.InfoContext(ctx, "PatchEtcdManifest: applied convergent change to local etcd",
		"manifest", manifestPath,
		"backup", backupPath,
		"dbSizeBefore", sizeBefore,
		"dbSizeAfter", sizeAfter,
	)
	return &pb.PatchEtcdManifestResponse{
		Outcome:                pb.PatchEtcdManifestResponse_OUTCOME_APPLIED,
		BackupManifestPath:     backupPath,
		DbTotalSizeBytesBefore: sizeBefore,
		DbTotalSizeBytesAfter:  sizeAfter,
	}, nil
}

// waitForEtcdRestart waits for kubelet to swap the etcd pod and for the new
// member to report healthy. Gating on a NEW container ID first matters: without
// it the health probe could be satisfied by the OLD etcd still serving during
// kubelet's manifest-scan window, and the call would report success without the
// flag ever taking effect.
func (s *NodeAgentServer) waitForEtcdRestart(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	client etcdMemberClient,
	previousContainerIDs []string,
) error {
	if err := s.waitForStaticPodRunning(ctx, runner, etcdContainerName, previousContainerIDs, s.etcdHealthTimeoutOrDefault()); err != nil {
		return fmt.Errorf("wait for etcd pod swap: %w", err)
	}
	if _, err := s.waitForLocalEtcdHealthy(ctx, client); err != nil {
		return fmt.Errorf("wait for etcd health: %w", err)
	}
	return nil
}

// etcdPatchCleanFailure reports a failure that did not disturb the running
// member: an in-memory computation failure, or a pre-write failure such as
// snapshotting. There is nothing to roll back, so the node is clean and the
// outcome is FAILED_RESTORED.
func (s *NodeAgentServer) etcdPatchCleanFailure(backupPath, failureMessage string) *pb.PatchEtcdManifestResponse {
	slog.Warn("PatchEtcdManifest: failed before any mutation; node left in original state", "reason", failureMessage)
	return &pb.PatchEtcdManifestResponse{
		Outcome:            pb.PatchEtcdManifestResponse_OUTCOME_FAILED_RESTORED,
		ErrorMessage:       failureMessage,
		BackupManifestPath: backupPath,
	}
}

// etcdPatchRestore rewrites the pre-call manifest bytes and waits for etcd to
// come back. Rewriting the file is not by itself proof of recovery, so the
// health wait is load-bearing: only a member that returns healthy justifies
// FAILED_RESTORED, which tells a rollout it may stop cleanly. Anything else is
// FAILED_NOT_RESTORED, meaning this member may be down and the rollout must not
// advance to the next one.
func (s *NodeAgentServer) etcdPatchRestore(
	ctx context.Context,
	runner nvkeutils.CommandRunner,
	client etcdMemberClient,
	backupPath, manifestPath string,
	manifestRaw []byte,
	sizeBefore int64,
	failureMessage string,
) *pb.PatchEtcdManifestResponse {
	notRestored := func(reason string) *pb.PatchEtcdManifestResponse {
		return &pb.PatchEtcdManifestResponse{
			Outcome:                pb.PatchEtcdManifestResponse_OUTCOME_FAILED_NOT_RESTORED,
			ErrorMessage:           fmt.Sprintf("%s; node not confirmed restored: %s", failureMessage, reason),
			BackupManifestPath:     backupPath,
			DbTotalSizeBytesBefore: sizeBefore,
		}
	}

	previousContainerIDs, err := listStaticPodContainerIDs(runner, etcdContainerName)
	if err != nil {
		// Carry on with the restore regardless; losing the container-ID baseline
		// only weakens the pod-swap gate, and leaving the patched manifest in
		// place would be worse.
		slog.WarnContext(ctx, "etcdPatchRestore: failed to list etcd containers before restore", "err", err)
	}

	spec := fileSpec{path: manifestPath, content: manifestRaw, mode: patchManifestFileMode, uid: 0, gid: 0}
	if werr := atomicWriteFileWithOwnership(spec, s.kubeadmFileOps); werr != nil {
		slog.ErrorContext(ctx, "etcdPatchRestore: failed to rewrite manifest from in-memory snapshot", "manifest", manifestPath, "err", werr)
		return notRestored(werr.Error())
	}
	slog.InfoContext(ctx, "etcdPatchRestore: rewrote etcd manifest from in-memory snapshot", "manifest", manifestPath)

	if rerr := s.waitForEtcdRestart(ctx, runner, client, previousContainerIDs); rerr != nil {
		slog.ErrorContext(ctx, "etcdPatchRestore: etcd did not return healthy after rollback", "manifest", manifestPath, "err", rerr)
		return notRestored(rerr.Error())
	}
	return &pb.PatchEtcdManifestResponse{
		Outcome:                pb.PatchEtcdManifestResponse_OUTCOME_FAILED_RESTORED,
		ErrorMessage:           failureMessage,
		BackupManifestPath:     backupPath,
		DbTotalSizeBytesBefore: sizeBefore,
	}
}

// validatePatchEtcdManifestRequest enforces the request contract: at least one
// flag op, each carrying a well-formed Set or Remove whose name is a single
// bare token.
func validatePatchEtcdManifestRequest(req *pb.PatchEtcdManifestRequest) error {
	if req == nil {
		return errors.New("request is required")
	}
	if len(req.GetFlagOps()) == 0 {
		return errors.New("request must set at least one flag op")
	}
	for i, op := range req.GetFlagOps() {
		if op == nil {
			return fmt.Errorf("flag op %d is nil", i)
		}
		switch {
		case op.GetSet() != nil:
			if err := validateFlagName(op.GetSet().GetName()); err != nil {
				return fmt.Errorf("flag op %d (set): %w", i, err)
			}
		case op.GetRemove() != nil:
			if err := validateFlagName(op.GetRemove().GetName()); err != nil {
				return fmt.Errorf("flag op %d (remove): %w", i, err)
			}
		default:
			return fmt.Errorf("flag op %d has neither set nor remove", i)
		}
	}
	return nil
}
