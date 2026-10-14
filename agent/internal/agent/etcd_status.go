// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const (
	// etcdContainerName is the name kubeadm gives the etcd container in its
	// static-pod manifest.
	etcdContainerName = "etcd"

	// etcdManifestName is the filename of the kubeadm-rendered etcd static-pod
	// manifest inside defaultStaticPodManifestDir.
	etcdManifestName = "etcd.yaml"

	// etcdQuotaBackendBytesFlag caps the backend size; etcd raises a NOSPACE
	// alarm and goes read-only when the file reaches it.
	etcdQuotaBackendBytesFlag = "quota-backend-bytes"

	// The bootstrap-defrag threshold flag was renamed upstream when it
	// graduated. etcd 3.5 (Kubernetes <= 1.33) only accepts the experimental
	// spelling; etcd 3.6+ (Kubernetes >= 1.34) accepts the stable one. Readers
	// must check both, since a node can be running either.
	etcdBootstrapDefragFlag             = "bootstrap-defrag-threshold-megabytes"
	etcdExperimentalBootstrapDefragFlag = "experimental-bootstrap-defrag-threshold-megabytes"

	// mebibyte converts the threshold flag's megabyte unit to the bytes the
	// sizing fields are expressed in. etcd treats the flag as mebibytes.
	mebibyte = int64(1024 * 1024)
)

// etcdManifestPath is the absolute path of the local etcd static-pod manifest.
func (s *NodeAgentServer) etcdManifestPath() string {
	return s.rootedPath(filepath.Join(defaultStaticPodManifestDir, etcdManifestName))
}

// readEtcdManifest reads the etcd static-pod manifest and returns, in order,
// the raw YAML (for a byte-for-byte restore) and its JSON form (for the flag
// helpers).
func (s *NodeAgentServer) readEtcdManifest() ([]byte, []byte, error) {
	path := s.etcdManifestPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read etcd manifest %s: %w", path, err)
	}
	asJSON, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse etcd manifest %s: %w", path, err)
	}
	return raw, asJSON, nil
}

// etcdBootstrapDefragThresholdBytes returns the configured bootstrap-defrag
// threshold in bytes, accepting either spelling of the flag.
//
// found=false means no restart would defragment at any size, which covers two
// cases that have to behave identically: the flag being absent, and the flag
// being present but set to zero or less. etcd treats a threshold of zero as
// disabled, so reporting it as configured would make the preflight comparison
// "reclaimable < 0" - never true - and the member would be restarted for a
// defragmentation that etcd was never going to perform.
func etcdBootstrapDefragThresholdBytes(manifestJSON []byte) (int64, bool, error) {
	for _, flag := range []string{etcdBootstrapDefragFlag, etcdExperimentalBootstrapDefragFlag} {
		raw, found, err := readFlagValue(manifestJSON, etcdContainerName, flag)
		if err != nil {
			return 0, false, err
		}
		if !found {
			continue
		}
		megabytes, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse --%s value %q: %w", flag, raw, err)
		}
		if megabytes <= 0 {
			return 0, false, nil
		}
		return megabytes * mebibyte, true, nil
	}
	return 0, false, nil
}

// etcdQuotaBackendBytes resolves the backend quota, preferring what the server
// reports and falling back to the manifest flag.
//
// The fallback is load-bearing rather than defensive: StatusResponse.DbSizeQuota
// was added in etcd 3.6, so a 3.5 server — which is what Kubernetes <= 1.33
// ships, and what most of the fleet runs today — leaves it zero no matter how
// the quota is configured. Zero from both sources means the flag is unset and
// etcd's built-in default applies; it is reported as zero so the caller can
// treat it as unknown rather than divide by it.
func etcdQuotaBackendBytes(st *clientv3.StatusResponse, manifestJSON []byte) (int64, error) {
	if st != nil && st.DbSizeQuota > 0 {
		return st.DbSizeQuota, nil
	}
	raw, found, err := readFlagValue(manifestJSON, etcdContainerName, etcdQuotaBackendBytesFlag)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	quota, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse --%s value %q: %w", etcdQuotaBackendBytesFlag, raw, err)
	}
	return quota, nil
}

// etcdStatusHealthError reports why a Status response does not describe a
// healthy local member, or nil when it does.
//
// Every RPC that gates on member health shares it so the gates cannot drift.
// They did: the pre-patch check once accepted a status carrying an alarm that
// the post-restart wait rejected, so a member already reporting NOSPACE was
// taken down, waited on for the full health timeout, rolled back and waited on
// again, before failing for a condition that predated the call. A restart does
// not clear an alarm, so such a member has to read as unhealthy up front.
func etcdStatusHealthError(st *clientv3.StatusResponse, err error) error {
	switch {
	case err != nil:
		return err
	case st == nil:
		return errors.New("etcd returned no status")
	case st.Header == nil:
		return errors.New("etcd status has no header")
	case len(st.Errors) > 0:
		return fmt.Errorf("etcd reports errors: %v", st.Errors)
	default:
		return nil
	}
}

// etcdStatusDbSize is the backend size from a possibly-absent status, so a
// caller can report the size it managed to observe alongside a health failure.
func etcdStatusDbSize(st *clientv3.StatusResponse) int64 {
	if st == nil {
		return 0
	}
	return st.DbSize
}

// reclaimableBytes is the space a defragmentation could return to the
// filesystem: the gap between the backend file and the live data inside it.
// Negative values are impossible in practice but clamped, since a caller
// comparing against a threshold must never see a negative.
func reclaimableBytes(dbSize, dbSizeInUse int64) int64 {
	if dbSize <= dbSizeInUse {
		return 0
	}
	return dbSize - dbSizeInUse
}

// votingMemberCount counts non-learner members. Learners do not vote, so they
// do not count toward the quorum a restart has to preserve.
//
// This is a count of *registered* voters. MemberList reports membership, not
// liveness, so an offline voter is still counted here and this must not be used
// on its own to decide that a restart is safe - see availableVotingMembers.
func votingMemberCount(members *clientv3.MemberListResponse) int32 {
	if members == nil {
		return 0
	}
	var count int32
	for _, m := range members.Members {
		if !m.GetIsLearner() {
			count++
		}
	}
	return count
}

// availableVotingMembers counts the voting members that answer a Status probe,
// including the local one, which the caller has already verified.
//
// Registered membership is not a safety signal. On a three-voter cluster with
// one peer already offline, a count of registered voters sees three and allows
// the restart, which leaves one voter against a quorum of two: writes fail for
// the whole restart, and for however long a bootstrap defragmentation then
// takes. Probing is what tells those two situations apart.
//
// A peer that fails to answer is counted as unavailable rather than treated as
// an error, because that is the condition being measured. Each probe is bounded
// separately so one unreachable peer cannot stall the preflight.
func (s *NodeAgentServer) availableVotingMembers(
	ctx context.Context,
	client etcdMemberClient,
	members *clientv3.MemberListResponse,
	localID uint64,
) int32 {
	if members == nil {
		return 0
	}
	available := int32(1) // the local member, whose health the caller checked
	for _, m := range members.Members {
		if m.GetID() == localID || m.GetIsLearner() {
			continue
		}
		endpoints := m.GetClientURLs()
		if len(endpoints) == 0 {
			// A member that has not advertised a client URL cannot be probed and
			// cannot serve clients either, so it is not available.
			slog.WarnContext(ctx, "etcd voting member advertises no client URL; treating it as unavailable",
				"member", fmt.Sprintf("%x", m.GetID()))
			continue
		}
		probeCtx, cancel := s.etcdCallContext(ctx)
		st, err := client.Status(probeCtx, endpoints[0])
		cancel()
		if healthErr := etcdStatusHealthError(st, err); healthErr != nil {
			slog.WarnContext(ctx, "etcd voting member did not answer a status probe; treating it as unavailable",
				"member", fmt.Sprintf("%x", m.GetID()), "err", healthErr)
			continue
		}
		available++
	}
	return available
}

// etcdRestartSafetyFloor is how many voting members must be available before
// the local one may be restarted.
//
// It is the larger of the caller's floor and what the registered membership
// needs in order to keep a quorum once this member goes down. Those differ: a
// five-member cluster with two already offline has three available, which
// clears a floor of three but would leave two against a quorum of three.
//
// A minVoters of 1 is the documented opt-in for topologies where no value can
// preserve quorum, so it disables the quorum term rather than overriding the
// caller's explicit decision to accept a brief outage.
func etcdRestartSafetyFloor(registeredVoters, minVoters int32) int32 {
	if minVoters <= 1 {
		return 1
	}
	// Quorum is registeredVoters/2+1; one more than that has to be available so
	// the quorum survives the member that is about to go down.
	return max(minVoters, registeredVoters/2+2)
}

// GetEtcdStatus reports the local etcd member's backend sizing and role so a
// caller can decide whether a defragmentation is warranted. Read-only: it uses
// etcd's Status and MemberList maintenance APIs and reads the static-pod
// manifest, and is safe to poll.
func (s *NodeAgentServer) GetEtcdStatus(
	ctx context.Context,
	_ *pb.GetEtcdStatusRequest,
) (*pb.GetEtcdStatusResponse, error) {
	defer logRPCSpan(ctx, "GetEtcdStatus")()

	if !s.isControlPlaneRole() {
		role := ""
		if s.NodeStatus != nil {
			role = s.NodeStatus.Role
		}
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"get etcd status: control-plane node required, current role %q",
			role,
		)
	}

	_, manifestJSON, err := s.readEtcdManifest()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "get etcd status: %v", err)
	}

	client, err := s.newEtcdMemberClient()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "get etcd status: connect to local kubeadm etcd: %v", err)
	}
	defer client.Close()

	statusCtx, cancelStatus := s.etcdCallContext(ctx)
	localStatus, err := client.Status(statusCtx, etcdLocalEndpoint)
	cancelStatus()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get etcd status: local member status: %v", err)
	}
	if localStatus == nil || localStatus.Header == nil {
		return nil, status.Error(codes.Internal, "get etcd status: local etcd status has no member ID")
	}

	memberListCtx, cancelMemberList := s.etcdCallContext(ctx)
	members, err := client.MemberList(memberListCtx)
	cancelMemberList()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get etcd status: list etcd members: %v", err)
	}

	quota, err := etcdQuotaBackendBytes(localStatus, manifestJSON)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "get etcd status: %v", err)
	}

	return &pb.GetEtcdStatusResponse{
		DbTotalSizeBytes:      localStatus.DbSize,
		DbTotalSizeInUseBytes: localStatus.DbSizeInUse,
		ReclaimableBytes:      reclaimableBytes(localStatus.DbSize, localStatus.DbSizeInUse),
		QuotaBackendBytes:     quota,
		IsLeader:              localStatus.Leader != 0 && localStatus.Leader == localStatus.Header.MemberId,
		VotingMemberCount:     votingMemberCount(members),
		MemberId:              fmt.Sprintf("%x", localStatus.Header.MemberId),
		Healthy:               len(localStatus.Errors) == 0,
	}, nil
}
