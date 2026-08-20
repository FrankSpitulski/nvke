// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/agent"
	"github.com/NVIDIA/nke/controller/internal/capi"
	"github.com/NVIDIA/nke/controller/internal/config"
	"github.com/NVIDIA/nke/controller/internal/db"
)

type CAPIClusterScaffoldController struct {
	cfg             config.Config
	logger          *slog.Logger
	db              db.DB
	k8sClient       ctrlclient.Client
	nodeAgentClient agent.Client
}

func NewCAPIClusterScaffoldController(
	cfg config.Config,
	logger *slog.Logger,
	dbInstance db.DB,
	k8sClient ctrlclient.Client,
	nodeAgentClient agent.Client,
) *CAPIClusterScaffoldController {
	return &CAPIClusterScaffoldController{
		cfg:             cfg,
		logger:          logger,
		db:              dbInstance,
		k8sClient:       k8sClient,
		nodeAgentClient: nodeAgentClient,
	}
}

func (c *CAPIClusterScaffoldController) Reconcile(ctx context.Context, site db.Site) error {
	clusters, err := c.db.ListClusters(ctx, db.ClusterFilters{SiteID: &site.ID})
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	var reconcileErrs []error
	for _, cluster := range clusters {
		if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
			c.logger.Debug("Cluster is deleting, skipping CAPI cluster scaffold reconciliation", "clusterID", cluster.ID, "clusterName", cluster.Name)
			continue
		}
		manifest, err := capi.ClusterScaffoldManifest(cluster)
		if err != nil {
			c.logger.Error("Failed to resolve CAPI cluster scaffold manifest", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
			continue
		}
		if capi.IsEmptyManifest(manifest) {
			continue
		}

		if err := c.reconcileCluster(ctx, cluster); err != nil {
			c.logger.Error("Failed to reconcile CAPI cluster scaffold", "error", err, "clusterID", cluster.ID, "clusterName", cluster.Name)
			reconcileErrs = append(reconcileErrs, err)
		}
	}

	return errors.Join(reconcileErrs...)
}

func (c *CAPIClusterScaffoldController) reconcileCluster(ctx context.Context, cluster db.Cluster) error {
	if err := capi.ApplyClusterScaffold(ctx, c.k8sClient, cluster); err != nil {
		return fmt.Errorf("failed to apply CAPI cluster scaffold: %w", err)
	}
	return nil
}
