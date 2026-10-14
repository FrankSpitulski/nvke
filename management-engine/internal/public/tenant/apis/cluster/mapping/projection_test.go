// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mapping

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

func createSpec() *clusterv1.CreateClusterSpec {
	return &clusterv1.CreateClusterSpec{
		Release: &clusterv1.CreateRelease{
			Channel: &clusterv1.CreateReleaseChannel{Name: new("stable")},
		},
		ControlPlaneClass: new("standard"),
		ClusterNetwork: &clusterv1.CreateClusterNetwork{
			Pods:     &clusterv1.CreateClusterNetworkPods{CidrBlocks: []string{"10.244.0.0/16"}},
			Services: &clusterv1.CreateClusterNetworkServices{CidrBlocks: []string{"10.96.0.0/12"}},
		},
	}
}

func TestProjectionMasksPrivateDataAndUsesSnakeCase(t *testing.T) {
	stored, err := CreateSpecToInternal(createSpec())
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(stored, "private-value", "release", "channel", "privateField"))
	stored["authentication"] = map[string]any{"private": "not public"}
	require.NoError(t, unstructured.SetNestedField(stored, "cluster.local", "clusterNetwork", "serviceDomain"))
	public, err := ProjectSpec(stored)
	require.NoError(t, err)
	encoded, err := protojson.Marshal(public)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"release":{"channel":{"name":"stable"}},
		"control_plane_class":"standard",
		"cluster_network":{"pods":{"cidr_blocks":["10.244.0.0/16"]},"services":{"cidr_blocks":["10.96.0.0/12"]}}
	}`, string(encoded))
	public.ClusterNetwork.Pods.CidrBlocks[0] = "changed"
	storedCIDRs, _, err := unstructured.NestedStringSlice(stored, "clusterNetwork", "pods", "cidrBlocks")
	require.NoError(t, err)
	require.Equal(t, []string{"10.244.0.0/16"}, storedCIDRs)
}

func TestReplacementPreservesPrivateAndCreateOnlyFields(t *testing.T) {
	stored, err := CreateSpecToInternal(createSpec())
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(stored, "old-version", "release", "channel", "version"))
	require.NoError(t, unstructured.SetNestedField(stored, "private", "release", "channel", "futureField"))
	beforeNetwork := runtime.DeepCopyJSONValue(stored["clusterNetwork"])
	replacement := &clusterv1.ReplaceClusterSpec{
		Release: &clusterv1.ReplaceRelease{
			Channel: &clusterv1.ReplaceReleaseChannel{Name: new("preview")},
		},
		ControlPlaneClass: new("large"),
	}
	prepared, err := PrepareReplacement(replacement)
	require.NoError(t, err)
	replacement.ControlPlaneClass = new("changed-after-validation")
	require.NoError(t, prepared.Apply(stored))
	channel, _, err := unstructured.NestedMap(stored, "release", "channel")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"name": "preview", "futureField": "private"}, channel)
	require.Equal(t, beforeNetwork, stored["clusterNetwork"])
	require.Equal(t, "large", stored["controlPlaneClass"])
}

func TestDerivedConstraintsAndPresence(t *testing.T) {
	for _, tc := range []struct {
		name, field, description string
		mutate                   func(*clusterv1.CreateClusterSpec)
		replacement              *clusterv1.ReplaceClusterSpec
	}{
		{name: "missing spec", field: "spec", description: "is required"},
		{name: "required release", field: "spec.release", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release = nil }},
		{name: "required name", field: "spec.release.channel.name", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = nil }},
		{name: "empty name", field: "spec.release.channel.name", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = new("") }},
		{name: "invalid name", field: "spec.release.channel.name", description: "has invalid format", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Name = new("UPPER") }},
		{name: "empty optional version", field: "spec.release.channel.version", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) { s.Release.Channel.Version = new("") }},
		{name: "long optional version", field: "spec.release.channel.version", description: "has invalid length", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.Release.Channel.Version = new(strings.Repeat("v", 129))
		}},
		{name: "required class", field: "spec.control_plane_class", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ControlPlaneClass = nil }},
		{name: "invalid class", field: "spec.control_plane_class", description: "has invalid format", mutate: func(s *clusterv1.CreateClusterSpec) {
			s.ControlPlaneClass = new("PRIVATE-INVALID-VALUE")
		}},
		{name: "missing networks", field: "spec.cluster_network", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork = nil }},
		{name: "missing pod networks", field: "spec.cluster_network.pods", description: "is required", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork.Pods = nil }},
		{name: "empty cidr list", field: "spec.cluster_network.pods.cidr_blocks", description: "has invalid item count", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork.Pods.CidrBlocks = nil }},
		{name: "too many cidrs", field: "spec.cluster_network.pods.cidr_blocks", description: "has invalid item count", mutate: func(s *clusterv1.CreateClusterSpec) { s.ClusterNetwork.Pods.CidrBlocks = []string{"a", "b", "c"} }},
		{name: "replacement requires class", field: "spec.control_plane_class", description: "is required", replacement: &clusterv1.ReplaceClusterSpec{
			Release: &clusterv1.ReplaceRelease{Channel: &clusterv1.ReplaceReleaseChannel{Name: new("preview")}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.replacement != nil {
				_, err = PrepareReplacement(tc.replacement)
			} else {
				var input *clusterv1.CreateClusterSpec
				if tc.mutate != nil {
					input = createSpec()
					tc.mutate(input)
				}
				_, err = CreateSpecToInternal(input)
			}
			var fieldError *FieldError
			require.ErrorAs(t, err, &fieldError)
			require.Equal(t, tc.field, fieldError.Field)
			require.Equal(t, tc.description, fieldError.Description)
			require.NotContains(t, err.Error(), "PRIVATE-INVALID-VALUE")
		})
	}
}

func TestProjectionRejectsMalformedStorageWithoutEchoingValues(t *testing.T) {
	for _, stored := range []map[string]any{
		{"release": "private-secret"},
		{"controlPlaneClass": map[string]any{"secret": "private-secret"}},
		{"clusterNetwork": map[string]any{"pods": map[string]any{"cidrBlocks": []any{"private-secret", int64(4)}}}},
	} {
		_, err := ProjectSpec(stored)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private-secret")
	}
}
