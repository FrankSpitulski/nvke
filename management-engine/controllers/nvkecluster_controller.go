// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Controller setup and reconcile dispatch coordinate lifecycle work and status persistence.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/dsx-ai-factory/nvke/controller/api"
	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/nvkecontroller"
)

const (
	nvkeClusterFinalizer = "internal.nvke.nvidia.com/cluster-cleanup"
	defaultPollInterval  = 10 * time.Second
	clusterNameSuffix    = "-mgmt-eng"
)

// ControllerClient is the existing REST API surface used by cluster reconciliation.
type ControllerClient interface {
	Get(ctx context.Context, id string) (*api.Cluster, error)
	Create(ctx context.Context, request api.CreateClusterRequest) (*api.Cluster, error)
	Delete(ctx context.Context, id string) error
	GetNodePool(ctx context.Context, id string) (*api.NodePool, error)
	CreateNodePool(ctx context.Context, request api.CreateNodePoolRequest) (*api.NodePool, error)
}

// NVKEClusterReconciler coordinates cluster provisioning from declarative intent.
type NVKEClusterReconciler struct {
	client.Client
	NVKEClient   ControllerClient
	PollInterval time.Duration
}

// +kubebuilder:rbac:groups=internal.nvke.nvidia.com,resources=nvkeclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=internal.nvke.nvidia.com,resources=nvkeclusters/status,verbs=update;patch

func (r *NVKEClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling NVKECluster")

	var cluster internalv1.NVKECluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("create nvkecluster patch helper: %w", err)
	}
	defer func() {
		if conditionErr := setNVKEClusterConditions(&cluster); conditionErr != nil {
			retErr = errors.Join(retErr, conditionErr)
		}
		if patchErr := patchHelper.Patch(ctx, &cluster, patch.WithOwnedConditions{Conditions: nvkeClusterOwnedConditions}); client.IgnoreNotFound(patchErr) != nil {
			retErr = errors.Join(retErr, fmt.Errorf("patch nvkecluster: %w", patchErr))
		}
	}()

	if r.NVKEClient == nil {
		propagateNVKEControllerConditions(&cluster, nil)
		return ctrl.Result{}, nil
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cluster)
	}
	if !controllerutil.ContainsFinalizer(&cluster, nvkeClusterFinalizer) {
		// Persist cleanup responsibility before making any provisioning request.
		controllerutil.AddFinalizer(&cluster, nvkeClusterFinalizer)
		return ctrl.Result{}, nil
	}
	return r.reconcileCluster(ctx, &cluster)
}

func (r *NVKEClusterReconciler) reconcileCluster(ctx context.Context, cluster *internalv1.NVKECluster) (ctrl.Result, error) {
	observedCluster, err := r.ensureCluster(ctx, cluster)
	propagateNVKEControllerConditions(cluster, observedCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure nvke controller cluster: %w", err)
	}
	conditions.Set(cluster, metav1.Condition{
		Type:   internalv1.ClusterReconciledCondition,
		Status: metav1.ConditionTrue,
		Reason: internalv1.ReconcileSucceededReason,
	})
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

func (r *NVKEClusterReconciler) reconcileNodePool(ctx context.Context, cluster *internalv1.NVKECluster, pool *internalv1.NodePool) error {
	var status *internalv1.NodePoolStatus
	for i := range cluster.Status.NodePools {
		if cluster.Status.NodePools[i].Name == pool.Name {
			status = &cluster.Status.NodePools[i]
			break
		}
	}
	if status == nil {
		cluster.Status.NodePools = append(cluster.Status.NodePools, internalv1.NodePoolStatus{Name: pool.Name})
		status = &cluster.Status.NodePools[len(cluster.Status.NodePools)-1]
	}
	if status.ID == "" {
		request, err := resolveNodePoolRequest(cluster, pool)
		if err != nil {
			return fmt.Errorf("resolve node pool %q request: %w", pool.Name, err)
		}
		observedPool, err := r.NVKEClient.CreateNodePool(ctx, request)
		if err != nil {
			return fmt.Errorf("create nvke controller node pool %q: %w", pool.Name, err)
		}
		status.ID = observedPool.ID.String()
		return nil
	}
	observedPool, err := r.NVKEClient.GetNodePool(ctx, status.ID)
	if err != nil {
		return fmt.Errorf("get nvke controller node pool %q: %w", status.ID, err)
	}
	if observedPool.ClusterID.String() != cluster.Status.ID {
		return fmt.Errorf("node pool %q belongs to cluster %q, expected %q", status.ID, observedPool.ClusterID, cluster.Status.ID)
	}
	return nil
}

func (r *NVKEClusterReconciler) ensureCluster(ctx context.Context, cluster *internalv1.NVKECluster) (*api.Cluster, error) {
	if cluster.Status.ID != "" {
		observedCluster, err := r.NVKEClient.Get(ctx, cluster.Status.ID)
		if err != nil {
			return nil, fmt.Errorf("get nvke controller cluster %q: %w", cluster.Status.ID, err)
		}
		return observedCluster, nil
	}

	request, err := resolveControllerRequest(cluster)
	if err != nil {
		conditions.Set(cluster, metav1.Condition{
			Type:    internalv1.ClusterReconciledCondition,
			Status:  metav1.ConditionFalse,
			Reason:  internalv1.ReconcileFailedReason,
			Message: err.Error(),
		})
		return nil, fmt.Errorf("resolve controller request: %w", err)
	}
	observedCluster, err := r.NVKEClient.Create(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("create cluster: %w", err)
	}
	cluster.Status.ID = observedCluster.ID.String()
	return observedCluster, nil
}

// ClusterRemoteName returns the backing cluster name used by the management engine.
func ClusterRemoteName(cluster *internalv1.NVKECluster) string {
	return cluster.Name + "-" + cluster.Namespace + clusterNameSuffix
}

func (r *NVKEClusterReconciler) reconcileDelete(ctx context.Context, cluster *internalv1.NVKECluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, nvkeClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	if cluster.Status.ID == "" {
		controllerutil.RemoveFinalizer(cluster, nvkeClusterFinalizer)
		return ctrl.Result{}, nil
	}

	observedCluster, err := r.NVKEClient.Get(ctx, cluster.Status.ID)
	propagateNVKEControllerConditions(cluster, observedCluster)
	if errors.Is(err, nvkecontroller.ErrNotFound) {
		controllerutil.RemoveFinalizer(cluster, nvkeClusterFinalizer)
		return ctrl.Result{}, nil
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get nvke controller cluster %q for deletion: %w", cluster.Status.ID, err)
	}

	if observedCluster.Conditions.Deleting.Status != api.ConditionStatus("True") {
		if err := r.NVKEClient.Delete(ctx, cluster.Status.ID); err != nil && !errors.Is(err, nvkecontroller.ErrNotFound) {
			return ctrl.Result{}, fmt.Errorf("delete nvke controller cluster %q: %w", cluster.Status.ID, err)
		}
	}
	return ctrl.Result{RequeueAfter: r.PollInterval}, nil
}

// Cluster intent resolution combines the spec with selected configuration for reconciliation.
// NVKE controller REST API specific calls will be replaced with the CAPI objects, but this is a temporary placeholder.

// InvalidSpecError identifies a request that needs corrected configuration.
type InvalidSpecError struct{ Message string }

func (e *InvalidSpecError) Error() string { return e.Message }

// resolveControllerRequest translates supported cluster intent using the temporary hardcoded class configuration.
func resolveControllerRequest(cluster *internalv1.NVKECluster) (api.CreateClusterRequest, error) {
	var empty api.CreateClusterRequest
	if err := validateControllerSpec(cluster); err != nil {
		return empty, fmt.Errorf("validate cluster spec: %w", err)
	}
	var request api.CreateClusterRequest
	if err := resolveControllerClass(cluster.Spec.ControlPlaneClass, &request); err != nil {
		return empty, fmt.Errorf("resolve control-plane class: %w", err)
	}
	request.Name = ClusterRemoteName(cluster)
	if cluster.UID == "" {
		return empty, invalidControllerRequest("A persisted cluster UID is required before provisioning")
	}
	request.CniPlugin = api.CreateClusterRequestCniPluginOvnKubernetes
	request.EtcdEncrypted = true
	pod, err := controllerNetworkPrefix(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks)
	if err != nil {
		return empty, fmt.Errorf("resolve pod network: %w", err)
	}
	service, err := controllerNetworkPrefix(cluster.Spec.ClusterNetwork.Services.CIDRBlocks)
	if err != nil {
		return empty, fmt.Errorf("resolve service network: %w", err)
	}
	request.Network = &api.ClusterNetworkCreateConfig{PodCIDR: pod, ServiceCIDR: service}
	return request, nil
}

// resolveNodePoolRequest uses a paused DockerMachineTemplate until worker configuration resolution is implemented.
func resolveNodePoolRequest(cluster *internalv1.NVKECluster, pool *internalv1.NodePool) (api.CreateNodePoolRequest, error) {
	var request api.CreateNodePoolRequest
	clusterID, err := uuid.Parse(cluster.Status.ID)
	if err != nil || clusterID == uuid.Nil {
		return request, invalidControllerRequest("A persisted cluster ID is required before creating node pools")
	}
	if pool.Replicas == nil || *pool.Replicas < 0 {
		return request, invalidControllerRequest("Node pool replicas must be a nonnegative count")
	}
	request = api.CreateNodePoolRequest{
		ClusterID:                   clusterID,
		Name:                        string(pool.Name),
		DesiredNodeCount:            int(*pool.Replicas),
		Infra:                       &api.InfraNodePoolProvider{MachineTemplate: pausedDockerMachineTemplate()},
		NodeNamePrefix:              string(pool.Name),
		ProvisioningMonitorDisabled: new(true),
	}
	if params := pool.UserParams; params != nil {
		request.NodeLabels = make(map[string]string, len(params.Labels))
		for key, value := range params.Labels {
			request.NodeLabels[key] = string(value)
		}
		request.NodeTaints = make([]api.NodeTaint, 0, len(params.Taints))
		for _, taint := range params.Taints {
			request.NodeTaints = append(request.NodeTaints, api.NodeTaint{
				Key:    string(taint.Key),
				Effect: api.NodeTaintEffect(taint.Effect),
				Value:  string(taint.Value),
			})
		}
	}
	return request, nil
}

func invalidControllerRequest(message string) error { return &InvalidSpecError{Message: message} }

func validateControllerSpec(cluster *internalv1.NVKECluster) error {
	if len(cluster.Spec.NodePools) != 0 {
		return invalidControllerRequest("Worker node pools are not supported")
	}
	if cluster.Spec.Network != nil {
		return invalidControllerRequest("Worker network selection is not supported")
	}
	if cluster.Spec.Authentication != nil {
		return invalidControllerRequest("Custom authentication is not supported")
	}
	if cluster.Spec.APIServerAccess.PublicAccessEnabled || len(cluster.Spec.APIServerAccess.PublicAccessCIDRs) != 0 {
		return invalidControllerRequest("Public API access configuration is not supported")
	}
	if runtime := cluster.Spec.AcceleratedRuntime; runtime != nil && (runtime.Name != "none" || runtime.Version != "") {
		return invalidControllerRequest("Accelerated runtime configuration is not supported")
	}
	if cluster.Spec.ClusterNetwork.ServiceDomain != "cluster.local" {
		return invalidControllerRequest("Only the cluster.local service domain is supported")
	}
	return nil
}

func controllerNetworkPrefix(blocks []internalv1.NetworkCIDR) (string, error) {
	if len(blocks) != 1 {
		return "", invalidControllerRequest("Exactly one IPv4 pod range and one IPv4 service range are required")
	}
	prefix, err := netip.ParsePrefix(string(blocks[0]))
	if err != nil || !prefix.Addr().Is4() {
		return "", invalidControllerRequest("Pod and service ranges must be valid IPv4 CIDRs")
	}
	// The NVKE controller remains authoritative for private ranges, overlap, size, and capacity validation.
	return prefix.Masked().String(), nil
}

// Status mapping presents provisioning observations and failures as public cluster conditions.

var nvkeClusterOwnedConditions = []string{
	clusterv1.AvailableCondition,
	internalv1.SyncedCondition,
	internalv1.ReadyCondition,
	internalv1.ControlPlaneReadyCondition,
	clusterv1.DeletingCondition,
	internalv1.NVKEControllerProvisionedCondition,
	internalv1.NVKEControllerProvisioningCondition,
	internalv1.NVKEControllerDeletingCondition,
	internalv1.NVKEControllerControlPlaneUpgradingCondition,
	internalv1.NVKEControllerCPNodesRecyclingCondition,
	internalv1.NVKEControllerUpdateFailedCondition,
	internalv1.ClusterReconciledCondition,
}

var nvkeControllerNegativeConditionTypes = []string{
	internalv1.NVKEControllerProvisioningCondition,
	internalv1.NVKEControllerDeletingCondition,
}

func propagateNVKEControllerConditions(cluster *internalv1.NVKECluster, observedCluster *api.Cluster) {
	if observedCluster == nil {
		observedCluster = &api.Cluster{}
	}
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerProvisionedCondition, &observedCluster.Conditions.Provisioned)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerProvisioningCondition, &observedCluster.Conditions.Provisioning)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerDeletingCondition, &observedCluster.Conditions.Deleting)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerControlPlaneUpgradingCondition, observedCluster.Conditions.ControlPlaneUpgrading)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerCPNodesRecyclingCondition, observedCluster.Conditions.CpNodesRecycling)
	setNVKEControllerCondition(cluster, internalv1.NVKEControllerUpdateFailedCondition, observedCluster.Conditions.UpdateFailed)
}

func setNVKEControllerCondition(cluster *internalv1.NVKECluster, conditionType string, child *api.Condition) {
	if child == nil {
		conditions.Set(cluster, metav1.Condition{
			Type:   conditionType,
			Status: metav1.ConditionUnknown,
			Reason: internalv1.NotYetReportedReason,
		})
		return
	}
	status := metav1.ConditionStatus(child.Status)
	reason := internalv1.ReportedReason
	switch status {
	case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
	default:
		status = metav1.ConditionUnknown
		reason = internalv1.NotYetReportedReason
	}
	conditions.Set(cluster, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.NewTime(child.LastTransitionTime),
		Reason:             reason,
		Message:            child.Message,
	})
}

func setNodePoolConditions(pool *internalv1.NodePoolStatus, generation int64) error {
	ready, err := conditions.NewSummaryCondition(
		pool,
		internalv1.ReadyCondition,
		conditions.ForConditionTypes{
			internalv1.NVKEControllerProvisionedCondition,
			internalv1.NVKEControllerProvisioningCondition,
			internalv1.NVKEControllerDeletingCondition,
		},
		conditions.NegativePolarityConditionTypes(nvkeControllerNegativeConditionTypes),
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(nvkeControllerNegativeConditionTypes...)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	)
	if err != nil {
		return fmt.Errorf("summarize node pool %q Ready condition: %w", pool.Name, err)
	}
	ready.ObservedGeneration = generation
	conditions.Set(pool, *ready)
	return nil
}

func setNVKEClusterConditions(cluster *internalv1.NVKECluster) error {
	for i := range cluster.Status.NodePools {
		if err := setNodePoolConditions(&cluster.Status.NodePools[i], cluster.Generation); err != nil {
			return err
		}
	}
	if cluster.DeletionTimestamp.IsZero() {
		conditions.Set(cluster, metav1.Condition{
			Type:   clusterv1.DeletingCondition,
			Status: metav1.ConditionFalse,
			Reason: clusterv1.NotDeletingReason,
		})
	} else {
		conditions.Set(cluster, metav1.Condition{
			Type:    clusterv1.DeletingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  clusterv1.DeletingReason,
			Message: "Cluster deletion is in progress",
		})
	}

	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.ControlPlaneReadyCondition,
		conditions.ForConditionTypes{
			internalv1.NVKEControllerProvisionedCondition,
			internalv1.NVKEControllerProvisioningCondition,
			internalv1.NVKEControllerDeletingCondition,
		},
		conditions.NegativePolarityConditionTypes(nvkeControllerNegativeConditionTypes),
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(nvkeControllerNegativeConditionTypes...)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster ControlPlaneReady condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.SyncedCondition,
		conditions.ForConditionTypes{internalv1.ClusterReconciledCondition, internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					internalv1.NotSyncedReason,
					internalv1.SyncUnknownReason,
					internalv1.SyncedReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Synced condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		clusterv1.AvailableCondition,
		conditions.ForConditionTypes{internalv1.ControlPlaneReadyCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotAvailableReason,
					clusterv1.AvailableUnknownReason,
					clusterv1.AvailableReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Available condition: %w", err)
	}
	if err := conditions.SetSummaryCondition(
		cluster,
		cluster,
		internalv1.ReadyCondition,
		conditions.ForConditionTypes{
			clusterv1.AvailableCondition,
			internalv1.SyncedCondition,
			clusterv1.DeletingCondition,
		},
		conditions.NegativePolarityConditionTypes{clusterv1.DeletingCondition},
		conditions.CustomMergeStrategy{
			MergeStrategy: conditions.DefaultMergeStrategy(
				conditions.GetPriorityFunc(conditions.GetDefaultMergePriorityFunc(clusterv1.DeletingCondition)),
				conditions.ComputeReasonFunc(conditions.GetDefaultComputeMergeReasonFunc(
					clusterv1.NotReadyReason,
					clusterv1.ReadyUnknownReason,
					clusterv1.ReadyReason,
				)),
			),
		},
	); err != nil {
		return fmt.Errorf("summarize NVKECluster Ready condition: %w", err)
	}
	return nil
}

func pausedDockerMachineTemplate() map[string]any {
	return map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "DockerMachineTemplate",
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{clusterv1.PausedAnnotation: "true"}},
				"spec":     map[string]any{},
			},
		},
	}
}

func resolveControllerClass(class internalv1.ResourceName, request *api.CreateClusterRequest) error {
	_ = class
	request.ControlPlaneKubernetesVersion = "1.36.1"
	request.ControlPlaneConfig = &api.CreateClusterControlPlaneConfig{
		DesiredCount:                1,
		ProvisioningMonitorDisabled: new(true),
		Infra:                       &api.InfraControlPlaneProvider{MachineTemplate: pausedDockerMachineTemplate()},
	}
	request.Infra = &api.InfraClusterProvider{Cluster: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "DockerCluster",
		"spec":       map[string]any{},
	}}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NVKEClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&internalv1.NVKECluster{}).
		Named("nvkecluster").
		Complete(r)
}
