// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cluster implements the tenant Cluster resource operations.
package cluster

import (
	"context"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster/mapping"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/limits"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

const (
	// MaxPageSize bounds datastore work for one public list request.
	MaxPageSize     = 100
	fieldManager    = "nke-public-api"
	defaultPageSize = 50
	specField       = "spec"
)

var clusterResource = internalv1.SchemeGroupVersion.WithResource("nvkeclusters")

// Config fixes the evaluation scope and datastore identity independently of requests.
type Config struct {
	Namespace    string
	DatastoreID  string
	PageTokenKey []byte
	Logger       *slog.Logger
}

// Service provides the same validation and persistence path to REST and gRPC.
type Service struct {
	clusterv1.UnimplementedClusterServiceServer
	resource   dynamic.ResourceInterface
	config     Config
	tokens     cipher.AEAD
	tokenScope []byte
}

// New binds a client to one configured namespace; it never loads credentials.
func New(client dynamic.Interface, config Config) (*Service, error) {
	if client == nil || len(validation.IsDNS1123Label(config.Namespace)) != 0 || config.DatastoreID == "" {
		return nil, fmt.Errorf("cluster API requires a client, namespace and datastore identity")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	tokens, err := newTokenCipher(config.PageTokenKey)
	if err != nil {
		return nil, err
	}
	scope, err := json.Marshal([]string{"cluster-list-v1", config.DatastoreID, config.Namespace})
	if err != nil {
		return nil, fmt.Errorf("encode pagination scope: %w", err)
	}
	config.PageTokenKey = nil
	return &Service{resource: client.Resource(clusterResource).Namespace(config.Namespace), config: config, tokens: tokens, tokenScope: scope}, nil
}

// Ready checks initialized datastore access without changing resource state.
func (s *Service) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, limits.ProbeTimeout)
	defer cancel()
	_, err := s.resource.List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return s.storageError(ctx, "ready", "", err)
	}
	return nil
}

// CreateCluster atomically creates a supported desired resource.
func (s *Service) CreateCluster(ctx context.Context, request *clusterv1.CreateClusterRequest) (*clusterv1.Cluster, error) {
	if err := validateRequest(request, request.GetName(), true); err != nil {
		return nil, err
	}
	spec, err := mapping.CreateSpecToInternal(request.GetSpec())
	if err != nil {
		return nil, invalidSpec(err)
	}
	if err := validateNetworks(request.GetSpec()); err != nil {
		return nil, err
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": internalv1.SchemeGroupVersion.String(), "kind": "NVKECluster",
		"metadata": map[string]any{"name": request.GetName(), "namespace": s.config.Namespace}, specField: spec,
	}}
	ctx, cancel := context.WithTimeout(ctx, limits.StorageTimeout)
	defer cancel()
	created, err := s.resource.Create(ctx, object, metav1.CreateOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict})
	return s.result(ctx, "create", request.GetName(), created, err)
}

// GetCluster reads directly from storage and returns only the public projection.
func (s *Service) GetCluster(ctx context.Context, request *clusterv1.GetClusterRequest) (*clusterv1.Cluster, error) {
	if err := validateRequest(request, request.GetName(), true); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.StorageTimeout)
	defer cancel()
	object, err := s.resource.Get(ctx, request.GetName(), metav1.GetOptions{})
	return s.result(ctx, "get", request.GetName(), object, err)
}

// ListClusters returns one bounded page with an opaque scope-bound continuation.
func (s *Service) ListClusters(ctx context.Context, request *clusterv1.ListClustersRequest) (*clusterv1.ListClustersResponse, error) {
	if err := validateRequest(request, "", false); err != nil {
		return nil, err
	}
	pageSize, continuation, err := s.page(request.GetPageSize(), request.GetPageToken())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, limits.StorageTimeout)
	defer cancel()
	objects, err := s.resource.List(ctx, metav1.ListOptions{Limit: int64(pageSize), Continue: continuation})
	if err != nil {
		return nil, s.storageError(ctx, "list", "", err)
	}
	if len(objects.Items) > int(pageSize) {
		return nil, s.internalError(ctx, "list", "", "storage exceeded the requested page size", nil)
	}
	response := &clusterv1.ListClustersResponse{Clusters: make([]*clusterv1.Cluster, 0, len(objects.Items))}
	for i := range objects.Items {
		object, err := s.result(ctx, "list", objects.Items[i].GetName(), &objects.Items[i], nil)
		if err != nil {
			return nil, err
		}
		response.Clusters = append(response.Clusters, object)
	}
	if objects.GetContinue() != "" {
		response.NextPageToken, err = s.encodePage(objects.GetContinue(), pageSize)
		if err != nil {
			return nil, s.internalError(ctx, "list", "", "could not issue page token", err)
		}
	}
	return response, nil
}

// ReplaceCluster corrects the complete writable projection before provisioning.
func (s *Service) ReplaceCluster(ctx context.Context, request *clusterv1.ReplaceClusterRequest) (*clusterv1.Cluster, error) {
	if err := validateRequest(request, request.GetName(), true); err != nil {
		return nil, err
	}
	version := request.GetResourceVersion()
	if version == "" || strings.TrimSpace(version) != version || strings.Trim(version, "0") == "" || len(version) > 1024 {
		return nil, fieldViolation("resource_version", "must be a previously returned version")
	}
	replacement, err := mapping.PrepareReplacement(request.GetSpec())
	if err != nil {
		return nil, invalidSpec(err)
	}
	ctx, cancel := context.WithTimeout(ctx, limits.StorageTimeout)
	defer cancel()
	stored, err := s.resource.Get(ctx, request.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, s.storageError(ctx, "replace", request.GetName(), err)
	}
	if stored.GetResourceVersion() != version {
		return nil, status.Error(codes.Aborted, "cluster changed; read its current resource_version")
	}
	if err := editable(stored); err != nil {
		return nil, err
	}
	spec, ok := stored.Object[specField].(map[string]any)
	if !ok || spec == nil {
		return nil, s.internalError(ctx, "replace", request.GetName(), "stored cluster spec is invalid", nil)
	}
	updated := runtime.DeepCopyJSON(spec)
	if err := replacement.Apply(updated); err != nil {
		return nil, s.internalError(ctx, "replace", request.GetName(), "cluster replacement mapping failed", err)
	}
	patch, err := guardedSpecPatch(version, spec, updated)
	if err != nil {
		return nil, s.internalError(ctx, "replace", request.GetName(), "cluster replacement mapping failed", err)
	}
	out, err := s.resource.Patch(ctx, stored.GetName(), types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict})
	return s.result(ctx, "replace", request.GetName(), out, err)
}

func validateRequest(request proto.Message, name string, named bool) error {
	if request == nil || !request.ProtoReflect().IsValid() || proto.Size(request) > limits.RequestBytes || unknownFields(request.ProtoReflect()) {
		return status.Error(codes.InvalidArgument, "invalid cluster request")
	}
	if named && len(validation.IsDNS1123Subdomain(name)) != 0 {
		return fieldViolation("name", "must be a DNS subdomain")
	}
	return nil
}

func editable(object *unstructured.Unstructured) error {
	if object.GetDeletionTimestamp() != nil {
		return status.Error(codes.FailedPrecondition, "a deleting cluster cannot be changed")
	}
	checkpoint, found, err := unstructured.NestedFieldNoCopy(object.Object, "status", "provisioning")
	if err != nil || (found && checkpoint != nil) {
		return status.Error(codes.FailedPrecondition, "cluster provisioning has started; spec changes are unavailable")
	}
	return nil
}

func guardedSpecPatch(version string, before, after map[string]any) ([]byte, error) {
	previous, err := json.Marshal(map[string]any{specField: before})
	if err != nil {
		return nil, err
	}
	// Include the read version even when the writable spec has not changed.
	next, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": version}, specField: after,
	})
	if err != nil {
		return nil, err
	}
	return jsonpatch.CreateMergePatch(previous, next)
}
