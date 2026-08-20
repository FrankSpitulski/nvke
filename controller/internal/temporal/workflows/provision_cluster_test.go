// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workflows

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/NVIDIA/nke/controller/internal/temporal/activities"
)

func TestInstallKubeLocalDNSSchedulesInProcessActivity(t *testing.T) {
	clusterID := uuid.New()
	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestWorkflowEnvironment()
	a := &activities.Activities{}

	env.OnActivity(a.InstallKubeLocalDNS, mock.Anything, activities.InstallKubeLocalDNSInput{
		ClusterID:                           clusterID,
		ClusterProvisioningConditionMessage: "Installing kube-local DNS",
	}).Return(nil).Once()

	env.ExecuteWorkflow(installKubeLocalDNSTestWorkflow, clusterID)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	env.AssertExpectations(t)
}

func installKubeLocalDNSTestWorkflow(ctx workflow.Context, clusterID uuid.UUID) error {
	return installKubeLocalDNS(ctx, clusterID)
}

func TestPreserveLegacyCAPIKubeconfigPublish(t *testing.T) {
	tests := map[string]struct {
		version      workflow.Version
		wantActivity bool
	}{
		"old history schedules recorded activity": {
			version:      workflow.DefaultVersion,
			wantActivity: true,
		},
		"new history skips legacy writer": {
			version:      tenantAccessSolePublisherVersion,
			wantActivity: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clusterID := uuid.New()
			nodeID := uuid.New()
			testSuite := &testsuite.WorkflowTestSuite{}
			env := testSuite.NewTestWorkflowEnvironment()
			a := &activities.Activities{}
			env.OnGetVersion(
				"tenant-access-sole-publisher",
				workflow.DefaultVersion,
				tenantAccessSolePublisherVersion,
			).Return(tt.version).Once()
			if tt.wantActivity {
				env.OnActivity(a.ApplyCAPIClusterKubeconfigSecret, mock.Anything, activities.ApplyCAPIClusterKubeconfigSecretInput{
					ClusterID:                 clusterID,
					InitialControlPlaneNodeID: nodeID,
				}).Return(nil).Once()
			}

			env.ExecuteWorkflow(preserveLegacyCAPIKubeconfigPublishTestWorkflow, clusterID, nodeID)

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())
			env.AssertExpectations(t)
		})
	}
}

func preserveLegacyCAPIKubeconfigPublishTestWorkflow(ctx workflow.Context, clusterID, nodeID uuid.UUID) error {
	return preserveLegacyCAPIKubeconfigPublish(ctx, activities.InfraModeCAPI, clusterID, nodeID)
}
