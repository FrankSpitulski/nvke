// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

// writeEtcdManifest lays down an etcd static-pod manifest under a temporary
// root and returns a server rooted there, so the manifest-reading paths run
// against a real file without touching the host.
func writeEtcdManifest(t *testing.T, manifest string) *NodeAgentServer {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, defaultStaticPodManifestDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, etcdManifestName), []byte(manifest), 0o600))
	return &NodeAgentServer{
		RootPath:   root,
		NodeStatus: &NodeStatus{Role: ControlPlaneRole},
		// The manifest-writing paths resolve root's uid and gid, which a test
		// environment's user database may not carry.
		kubeadmFileOps: fileOps{
			lookupUser: func(name string) (*user.User, error) {
				if name == "root" {
					return &user.User{Username: "root", Uid: "0"}, nil
				}
				return nil, user.UnknownUserError(name)
			},
			lookupGroup: func(name string) (*user.Group, error) {
				if name == "root" {
					return &user.Group{Name: "root", Gid: "0"}, nil
				}
				return nil, user.UnknownGroupError(name)
			},
			chown: func(string, int, int) error { return nil },
		},
	}
}

const etcdManifestWithFlags = `apiVersion: v1
kind: Pod
metadata:
  name: etcd
spec:
  containers:
    - name: etcd
      command:
        - etcd
        - --quota-backend-bytes=17179869184
        - --experimental-bootstrap-defrag-threshold-megabytes=2048
`

func TestReadFlagValue(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		flag      string
		wantValue string
		wantFound bool
	}{
		{
			name:      "token form in command",
			manifest:  etcdManifestWithFlags,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "17179869184",
			wantFound: true,
		},
		{
			name: "split form is read as a value",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--quota-backend-bytes", "123"]
`,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "123",
			wantFound: true,
		},
		{
			name: "flag living in args is found",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
      args: ["--quota-backend-bytes=42"]
`,
			flag:      etcdQuotaBackendBytesFlag,
			wantValue: "42",
			wantFound: true,
		},
		{
			name: "bare boolean flag is found with an empty value",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--some-toggle", "--other=1"]
`,
			flag:      "some-toggle",
			wantValue: "",
			wantFound: true,
		},
		{
			name:      "absent flag is reported as not found",
			manifest:  etcdManifestWithFlags,
			flag:      "nonexistent-flag",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			_, manifestJSON, err := s.readEtcdManifest()
			require.NoError(t, err)

			value, found, err := readFlagValue(manifestJSON, etcdContainerName, tt.flag)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}

func TestEtcdBootstrapDefragThresholdBytes(t *testing.T) {
	tests := []struct {
		name      string
		manifest  string
		wantBytes int64
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "experimental spelling is accepted",
			manifest:  etcdManifestWithFlags,
			wantBytes: 2048 * 1024 * 1024,
			wantFound: true,
		},
		{
			name: "stable spelling is accepted",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=512"]
`,
			wantBytes: 512 * 1024 * 1024,
			wantFound: true,
		},
		{
			name: "neither spelling present",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`,
			wantFound: false,
		},
		{
			name: "non-numeric value is an error rather than a silent zero",
			manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd", "--bootstrap-defrag-threshold-megabytes=lots"]
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := writeEtcdManifest(t, tt.manifest)
			_, manifestJSON, err := s.readEtcdManifest()
			require.NoError(t, err)

			got, found, err := etcdBootstrapDefragThresholdBytes(manifestJSON)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantBytes, got)
		})
	}
}

func TestEtcdQuotaBackendBytes(t *testing.T) {
	s := writeEtcdManifest(t, etcdManifestWithFlags)
	_, manifestJSON, err := s.readEtcdManifest()
	require.NoError(t, err)

	t.Run("server-reported quota wins when populated", func(t *testing.T) {
		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{DbSizeQuota: 999}, manifestJSON)
		require.NoError(t, err)
		assert.Equal(t, int64(999), got)
	})

	t.Run("falls back to the manifest flag on etcd 3.5", func(t *testing.T) {
		// etcd 3.5 does not populate DbSizeQuota, which is the fleet today, so
		// the manifest fallback is the live path rather than a safety net.
		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{}, manifestJSON)
		require.NoError(t, err)
		assert.Equal(t, int64(17179869184), got)
	})

	t.Run("absent from both sources reports zero", func(t *testing.T) {
		bare := writeEtcdManifest(t, `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: etcd
      command: ["etcd"]
`)
		_, bareJSON, err := bare.readEtcdManifest()
		require.NoError(t, err)

		got, err := etcdQuotaBackendBytes(&clientv3.StatusResponse{}, bareJSON)
		require.NoError(t, err)
		assert.Zero(t, got, "zero signals unknown so callers decline rather than dividing by it")
	})
}

func TestReclaimableBytes(t *testing.T) {
	assert.Equal(t, int64(400), reclaimableBytes(1000, 600))
	assert.Equal(t, int64(0), reclaimableBytes(600, 600))
	assert.Equal(t, int64(0), reclaimableBytes(500, 600), "must never report negative reclaimable space")
}

func TestVotingMemberCount(t *testing.T) {
	assert.Equal(t, int32(0), votingMemberCount(nil))
	assert.Equal(t, int32(2), votingMemberCount(&clientv3.MemberListResponse{
		Members: []*etcdserverpb.Member{
			{ID: 1, Name: "cp-1"},
			{ID: 2, Name: "cp-2"},
			{ID: 3, Name: "learner", IsLearner: true},
		},
	}), "learners do not vote so they do not count toward quorum")
}

func TestGetEtcdStatus(t *testing.T) {
	t.Run("reports sizing, leadership and voters", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{
			members:     testEtcdMembers(),
			localID:     2,
			leaderID:    2,
			dbSize:      1_000_000,
			dbSizeInUse: 600_000,
		}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.Equal(t, int64(1_000_000), resp.GetDbTotalSizeBytes())
		assert.Equal(t, int64(600_000), resp.GetDbTotalSizeInUseBytes())
		assert.Equal(t, int64(400_000), resp.GetReclaimableBytes())
		assert.Equal(t, int64(17179869184), resp.GetQuotaBackendBytes())
		assert.True(t, resp.GetIsLeader())
		assert.Equal(t, int32(2), resp.GetVotingMemberCount())
		assert.Equal(t, "2", resp.GetMemberId())
		assert.True(t, resp.GetHealthy())
	})

	t.Run("a follower is not reported as leader", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2, leaderID: 1}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetIsLeader())
	})

	t.Run("a cluster with no leader is not reported as leader", func(t *testing.T) {
		// Leader 0 means no leader is elected. Comparing it against a member ID
		// of 0 must not read as "this member leads".
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 0, leaderID: 0}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetIsLeader())
	})

	t.Run("status errors mark the member unhealthy", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.etcdClient = &fakeEtcdMemberClient{
			members:      testEtcdMembers(),
			localID:      2,
			statusErrors: []string{"NOSPACE"},
		}

		resp, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.NoError(t, err)
		assert.False(t, resp.GetHealthy())
	})

	t.Run("worker nodes are rejected", func(t *testing.T) {
		s := writeEtcdManifest(t, etcdManifestWithFlags)
		s.NodeStatus = &NodeStatus{Role: "worker"}
		s.etcdClient = &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2}

		_, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("a missing manifest is a precondition failure", func(t *testing.T) {
		s := &NodeAgentServer{
			RootPath:   t.TempDir(),
			NodeStatus: &NodeStatus{Role: ControlPlaneRole},
			etcdClient: &fakeEtcdMemberClient{members: testEtcdMembers(), localID: 2},
		}

		_, err := s.GetEtcdStatus(context.Background(), &pb.GetEtcdStatusRequest{})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
}
