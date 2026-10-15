// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// TestDeployedPublicAPI crosses both public transports, the datastore and controller reconciliation.
// Run through make test-public-api-dev so scope comes from the selected Kind environment.
func TestDeployedPublicAPI(t *testing.T) {
	if os.Getenv("PUBLIC_API_SMOKE") != "1" {
		t.Skip("requires the running Tilt environment; use make test-public-api-dev")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: requiredEnv(t, "KUBECONFIG")},
		&clientcmd.ConfigOverrides{CurrentContext: requiredEnv(t, "STATE_CONTEXT")},
	).ClientConfig()
	require.NoError(t, err)
	config.Timeout = 5 * time.Second
	storage, err := dynamic.NewForConfig(config)
	require.NoError(t, err)
	resource := storage.Resource(resourceGVR()).Namespace(requiredEnv(t, "PUBLIC_API_NAMESPACE"))
	connection, err := grpc.NewClient(requiredEnv(t, "PUBLIC_API_GRPC"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	rpc := clusterv1.NewClusterServiceClient(connection)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	httpURL := requiredEnv(t, "PUBLIC_API_HTTP") + "/v1/clusters"

	conditionState, conditionReason := metav1.ConditionUnknown, "ProvisioningNotConfigured"
	publicState := clusterv1.ClusterStatus_STATE_UNKNOWN
	publicReason := clusterv1.ClusterStatus_REASON_STATUS_UNAVAILABLE
	switch mode := requiredEnv(t, "PUBLIC_API_CONTROLLER_MODE"); mode {
	case "unconfigured":
	case "cwe":
		conditionState, conditionReason = metav1.ConditionFalse, "UnsupportedConfiguration"
		publicState = clusterv1.ClusterStatus_STATE_FAILED
		publicReason = clusterv1.ClusterStatus_REASON_UNSUPPORTED_CONFIGURATION
	default:
		t.Fatalf("unsupported controller mode %q", mode)
	}

	// An unbound selection also prevents provisioning when running with the optional CWE stack.
	name := "public-api-smoke-" + uuid.NewString()
	request := storageCreate(t, name)
	request.Spec.Release.Channel.Name = new(name)
	request.Spec.ControlPlaneClass = new(name)
	created := new(clusterv1.Cluster)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		stored, err := resource.Get(cleanup, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		require.NoError(t, err)
		uid := stored.GetUID()
		if created.Uid != "" {
			require.Equal(t, created.Uid, string(uid))
		}
		err = resource.Delete(cleanup, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		}
		require.Eventually(t, func() bool {
			_, err := resource.Get(cleanup, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 25*time.Second, 200*time.Millisecond, "smoke resource cleanup must finish without bypassing finalizers")
	})
	smokeHTTP(t, ctx, httpClient, http.MethodPost, httpURL, request, http.StatusOK, created)
	require.NotEmpty(t, created.Uid)
	assertStored := func(want *clusterv1.Cluster, version string) {
		t.Helper()
		stored, err := resource.Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, want.Uid, string(stored.GetUID()))
		require.Equal(t, want.Generation, stored.GetGeneration())
		channel := map[string]any{"name": name}
		if version != "" {
			channel["version"] = version
		}
		require.Equal(t, map[string]any{
			"release": map[string]any{"channel": channel}, "controlPlaneClass": name,
			"clusterNetwork": map[string]any{
				"pods":     map[string]any{"cidrBlocks": []any{"10.244.0.0/16"}},
				"services": map[string]any{"cidrBlocks": []any{"10.96.0.0/12"}}, "serviceDomain": "cluster.local",
			},
			"apiServerAccess": map[string]any{"publicAccessEnabled": false},
		}, stored.Object["spec"])
	}
	assertObserved := func(want *clusterv1.Cluster) {
		t.Helper()
		var observed internalv1.NVKECluster
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			stored, err := resource.Get(ctx, name, metav1.GetOptions{})
			if !assert.NoError(collect, err) {
				return
			}
			observed = internalv1.NVKECluster{}
			if !assert.NoError(collect, runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, &observed)) {
				return
			}
			assert.Equal(collect, want.Generation, observed.Status.ObservedGeneration)
			ready := meta.FindStatusCondition(observed.Status.Conditions, internalv1.ReadyCondition)
			if assert.NotNil(collect, ready) {
				assert.Equal(collect, want.Generation, ready.ObservedGeneration)
			}
		}, 30*time.Second, 200*time.Millisecond, "controller must observe generation %d", want.Generation)
		require.Nil(t, observed.Status.Provisioning, "smoke selection must not start provisioning")
		for _, kind := range []string{internalv1.ReadyCondition, internalv1.SyncedCondition} {
			condition := meta.FindStatusCondition(observed.Status.Conditions, kind)
			require.NotNil(t, condition)
			require.Equal(t, want.Generation, condition.ObservedGeneration, kind)
			require.Equal(t, conditionState, condition.Status, kind)
			if kind == internalv1.SyncedCondition {
				require.Equal(t, conditionReason, condition.Reason)
			}
		}
		httpRead := new(clusterv1.Cluster)
		smokeHTTP(t, ctx, httpClient, http.MethodGet, httpURL+"/"+name, nil, http.StatusOK, httpRead)
		grpcRead, err := rpc.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: name})
		require.NoError(t, err)
		for _, read := range []*clusterv1.Cluster{httpRead, grpcRead} {
			require.Equal(t, want.Uid, read.Uid)
			require.Equal(t, want.Generation, read.Generation)
			require.True(t, proto.Equal(want.Spec, read.Spec))
			require.NotNil(t, read.Status)
			require.Equal(t, publicState, read.Status.State)
			require.Equal(t, publicReason, read.Status.Reason)
			require.Equal(t, want.Generation, read.Status.ObservedGeneration)
		}
		t.Logf("controller observed generation %d; HTTP/gRPC GET returned %s/%s", want.Generation, publicState, publicReason)
	}
	assertStored(created, "")
	assertObserved(created)
	_, err = resource.Patch(ctx, name, types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"public-api-smoke/private":"PRIVATE-SMOKE"}}}`), metav1.PatchOptions{})
	require.NoError(t, err)
	current, err := rpc.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: name})
	require.NoError(t, err)
	require.True(t, proto.Equal(created.Spec, current.Spec))
	require.NotEqual(t, created.ResourceVersion, current.ResourceVersion)

	// The controller may update status concurrently; only retry the documented version conflict.
	var replaced *clusterv1.Cluster
	for attempt := 0; ; attempt++ {
		current, err = rpc.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: name})
		require.NoError(t, err)
		replacement := &clusterv1.ReplaceClusterRequest{Name: name, ResourceVersion: current.ResourceVersion,
			Spec: &clusterv1.ReplaceClusterSpec{ControlPlaneClass: new(name),
				Release: &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{
					Name: new(name), Version: new("smoke-version"),
				}}}}
		replaced, err = rpc.ReplaceCluster(ctx, replacement)
		if status.Code(err) == codes.Aborted && attempt < 20 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		require.NoError(t, err)
		break
	}
	require.Equal(t, created.Generation+1, replaced.Generation)
	assertStored(replaced, "smoke-version")
	assertObserved(replaced)
	stale := &clusterv1.ReplaceClusterRequest{Name: name, ResourceVersion: created.ResourceVersion,
		Spec: &clusterv1.ReplaceClusterSpec{ControlPlaneClass: new(name),
			Release: &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new(name)}}}}
	_, err = rpc.ReplaceCluster(ctx, stale)
	require.Equal(t, codes.Aborted, status.Code(err))
	smokeHTTP(t, ctx, httpClient, http.MethodPut, httpURL+"/"+name, stale, http.StatusConflict, nil)
	stored, err := resource.Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "PRIVATE-SMOKE", stored.GetAnnotations()["public-api-smoke/private"])
	assertStored(replaced, "smoke-version")

	for _, transport := range []string{"http", "grpc"} {
		found := false
		page := &clusterv1.ListClustersRequest{PageSize: 100}
		for {
			var listed *clusterv1.ListClustersResponse
			if transport == "http" {
				listed = new(clusterv1.ListClustersResponse)
				path := fmt.Sprintf("%s?page_size=100&page_token=%s", httpURL, page.PageToken)
				smokeHTTP(t, ctx, httpClient, http.MethodGet, path, nil, http.StatusOK, listed)
			} else {
				listed, err = rpc.ListClusters(ctx, page)
				require.NoError(t, err)
			}
			for _, cluster := range listed.Clusters {
				if cluster.Name == name {
					require.Equal(t, created.Uid, cluster.Uid)
					found = true
				}
			}
			if listed.NextPageToken == "" {
				break
			}
			page.PageToken = listed.NextPageToken
		}
		require.True(t, found, "%s list must contain the persisted cluster", transport)
	}
	t.Logf("REST/gRPC persistence, controller observations, defaults, masking and stale-write checks passed: %s uid=%s",
		name, created.Uid)
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	require.NotEmpty(t, value, "run make test-public-api-dev to set %s", name)
	return value
}

func smokeHTTP(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	method, url string,
	input proto.Message,
	code int,
	output proto.Message,
) {
	t.Helper()
	var body []byte
	var err error
	if input != nil {
		body, err = protojson.Marshal(input)
		require.NoError(t, err)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	require.NoError(t, err)
	require.Equal(t, code, response.StatusCode, "%s %s: %s", method, url, data)
	for _, private := range []string{
		"PRIVATE-SMOKE", "managedFields", "resourceVersion", "controlPlaneClass", "apiServerAccess", "serviceDomain",
	} {
		require.NotContains(t, string(data), private)
	}
	if output != nil {
		require.NoError(t, protojson.Unmarshal(data, output), strings.TrimSpace(string(data)))
	}
}
