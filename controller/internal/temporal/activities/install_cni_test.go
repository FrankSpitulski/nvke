// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/agent/agenttest"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestCniInstallSpec(t *testing.T) {
	t.Parallel()

	variant, pkg, err := cniInstallSpec(db.CNIOvnKubernetes, "1.4.0")
	require.NoError(t, err)
	require.Equal(t, nkeproto.CniVariant_CNI_VARIANT_OVN, variant)
	require.Equal(t, "nke-cni-ovn-charts-1.4.0", pkg)

	variant, pkg, err = cniInstallSpec(db.CNICalico, "")
	require.NoError(t, err)
	require.Equal(t, nkeproto.CniVariant_CNI_VARIANT_CALICO, variant)
	require.Equal(t, "nke-cni-calico-manifests-"+db.DefaultCalicoCNIVersion, pkg)

	_, _, err = cniInstallSpec(db.CniPlugin("unknown"), "")
	require.Error(t, err)
}

func TestInstallCNICallsAgent(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	nodeID := uuid.New()
	cluster := db.Cluster{
		ID:          clusterID,
		CniPlugin:   db.CNIOvnKubernetes,
		CniVersion:  "1.4.0",
		PodCIDR:     "10.244.0.0/16",
		ServiceCIDR: "10.96.0.0/12",
	}
	node := db.Node{ID: nodeID, ClusterID: clusterID, Name: "cp-0", ControlPlane: true}

	mockDB := &testutil.MockDB{}
	mockDB.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
	mockDB.On("GetNode", mock.Anything, nodeID).Return(node, nil)

	fakeAgent := &agenttest.FakeClient{}
	a := &Activities{
		cfg:             config.Config{CNIImageRegistry: "nvcr.io/example"},
		db:              mockDB,
		nodeAgentClient: fakeAgent,
	}

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(a.installCNI)
	_, err := env.ExecuteActivity(a.installCNI, InstallCNIInput{
		ClusterID:     clusterID,
		InitialNodeID: nodeID,
	})
	require.NoError(t, err)

	require.Equal(t, []uuid.UUID{nodeID}, fakeAgent.InstallPackagesCalls)
	require.NotNil(t, fakeAgent.LastInstallPackages)
	require.Equal(t, []string{"nke-cni-ovn-charts-1.4.0"}, fakeAgent.LastInstallPackages.GetPackages())
	require.True(t, fakeAgent.LastInstallPackages.GetUpdate())

	require.Equal(t, []uuid.UUID{nodeID}, fakeAgent.InstallCNICalls)
	require.NotNil(t, fakeAgent.LastInstallCNI)
	require.Equal(t, nkeproto.CniVariant_CNI_VARIANT_OVN, fakeAgent.LastInstallCNI.GetCniVariant())
	require.Equal(t, "1.4.0", fakeAgent.LastInstallCNI.GetVersion())
	require.Equal(t, "10.244.0.0/16", fakeAgent.LastInstallCNI.GetPodNetworkCidr())
	require.Equal(t, map[string]string{"global.registry": "nvcr.io/example"}, fakeAgent.LastInstallCNI.GetCniConfig())

	mockDB.AssertExpectations(t)
}
