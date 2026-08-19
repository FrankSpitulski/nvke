// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"

	nkeproto "github.com/NVIDIA/nke/agent/pkg/agent/proto"

	"github.com/NVIDIA/nke/controller/internal/db"
)

type InstallCNIInput struct {
	ClusterID     uuid.UUID
	InitialNodeID uuid.UUID

	ClusterProvisioningConditionMessage string
}

func (a *Activities) InstallCNI(ctx context.Context, input InstallCNIInput) error {
	conditionUpdateWrapper := ActivityStatusConditionUpdateWrapper[struct{}]{
		Message:         input.ClusterProvisioningConditionMessage,
		UpdateCondition: a.clusterProvisioningConditionMessageUpdater(input.ClusterID),
		RenderError:     messageForTemporalError,
		ActivityBody: func() (struct{}, error) {
			return struct{}{}, a.installCNI(ctx, input)
		},
	}

	_, err := conditionUpdateWrapper.Run(ctx)
	return err
}

func (a *Activities) installCNI(ctx context.Context, input InstallCNIInput) error {
	logger := activity.GetLogger(ctx)

	cluster, err := a.db.GetCluster(ctx, input.ClusterID)
	if err != nil {
		return fmt.Errorf("failed to get cluster: %w", err)
	}
	node, err := a.db.GetNode(ctx, input.InitialNodeID)
	if err != nil {
		return fmt.Errorf("failed to get initial control plane node: %w", err)
	}

	variant, chartsPkg, err := cniInstallSpec(cluster.CniPlugin, cluster.CniVersion)
	if err != nil {
		return err
	}

	logger.Info("installing CNI charts package", "node", node.Name, "package", chartsPkg)
	if _, err := a.nodeAgentClient.InstallPackages(ctx, node, &nkeproto.InstallPackagesRequest{
		Packages: []string{chartsPkg},
		Update:   true,
	}); err != nil {
		return fmt.Errorf("failed to install CNI charts package %s on node %s: %w", chartsPkg, node.Name, err)
	}

	cniVersion := db.EffectiveCNIVersion(cluster.CniPlugin, strings.TrimSpace(cluster.CniVersion))
	req := &nkeproto.InstallCNIRequest{
		CniVariant:     variant,
		Version:        cniVersion,
		PodNetworkCidr: cluster.PodCIDR,
		ServiceCidr:    cluster.ServiceCIDR,
	}
	if a.cfg.CNIImageRegistry != "" {
		req.CniConfig = map[string]string{"global.registry": a.cfg.CNIImageRegistry}
	}

	logger.Info("calling InstallCNI", "node", node.Name, "variant", variant.String(), "version", cniVersion)
	resp, err := a.nodeAgentClient.InstallCNI(ctx, node, req)
	if err != nil {
		return fmt.Errorf("failed to install CNI on node %s: %w", node.Name, err)
	}
	if msg := strings.TrimSpace(resp.GetErrorMessage()); msg != "" {
		return fmt.Errorf("CNI install failed on node %s: %s", node.Name, msg)
	}
	status := strings.ToLower(strings.TrimSpace(resp.GetStatus()))
	if status != "" && status != cniStatusReady {
		return fmt.Errorf("CNI install on node %s returned status %q", node.Name, resp.GetStatus())
	}

	return nil
}

func cniInstallSpec(plugin db.CniPlugin, version string) (nkeproto.CniVariant, string, error) {
	stream := db.EffectiveCNIVersion(plugin, strings.TrimSpace(version))
	switch plugin {
	case db.CNIOvnKubernetes:
		return nkeproto.CniVariant_CNI_VARIANT_OVN, "nke-cni-ovn-charts-" + stream, nil
	case db.CNICalico:
		return nkeproto.CniVariant_CNI_VARIANT_CALICO, "nke-cni-calico-manifests-" + stream, nil
	default:
		return nkeproto.CniVariant_CNI_VARIANT_UNSPECIFIED, "", fmt.Errorf("unsupported CNI plugin: %s", plugin)
	}
}
