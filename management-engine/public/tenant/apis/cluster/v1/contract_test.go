// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusterv1_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

func TestJSONContractUsesOnlySnakeCase(t *testing.T) {
	files := []protoreflect.FileDescriptor{
		clusterv1.File_cluster_v1_cluster_proto, clusterv1.File_cluster_v1_spec_proto,
	}
	for _, file := range files {
		messages := file.Messages()
		for i := 0; i < messages.Len(); i++ {
			fields := messages.Get(i).Fields()
			for j := 0; j < fields.Len(); j++ {
				field := fields.Get(j)
				require.Equal(t, string(field.Name()), field.JSONName(), "%s", field.FullName())
			}
		}
	}
	for _, input := range []string{
		`{"controlPlaneClass":"standard"}`,
		`{"authentication":{}}`,
		`{"release":{"channel":{"name":"one","name":"two"}}}`,
		`{"cluster_network":{"pods":{"cidrBlocks":["10.0.0.0/8"]}}}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.CreateClusterSpec{}))
	}
	for _, input := range []string{
		`{"cluster_network":{}}`,
		`{"api_server_access":{}}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.ReplaceClusterSpec{}))
	}
}

func TestOpenAPIContractMatchesPublicJSON(t *testing.T) {
	data, err := os.ReadFile("../../../gen/cluster/v1/cluster.swagger.json")
	require.NoError(t, err)
	type parameter struct{ Name, In string }
	var document struct {
		Swagger string
		Info    struct {
			Title, Version, Description string
			License                     struct{ Name, URL string }
		}
		Paths       map[string]map[string]struct{ Parameters []parameter }
		Definitions map[string]struct {
			Type       string
			Enum       []string
			Properties map[string]struct {
				Type, Format string
				Ref          string `json:"$ref"`
			}
		}
	}
	require.NoError(t, json.Unmarshal(data, &document))
	require.Equal(t, "2.0", document.Swagger)
	require.Equal(t, "Tenant Cluster API", document.Info.Title)
	require.Equal(t, "v1", document.Info.Version)
	require.Equal(t, "Apache-2.0", document.Info.License.Name)
	require.Equal(t, "https://www.apache.org/licenses/LICENSE-2.0", document.Info.License.URL)
	require.Contains(t, document.Info.Description, "exactly one canonical IPv4")
	require.Contains(t, document.Info.Description, "must not overlap")
	routes := map[string][]parameter{}
	for path, methods := range document.Paths {
		for method, operation := range methods {
			routes[method+" "+path] = operation.Parameters
		}
	}
	wantRoutes := map[string][]parameter{
		"get /v1/clusters":        {{Name: "page_size", In: "query"}, {Name: "page_token", In: "query"}},
		"post /v1/clusters":       {{Name: "body", In: "body"}},
		"get /v1/clusters/{name}": {{Name: "name", In: "path"}},
		"put /v1/clusters/{name}": {{Name: "name", In: "path"}, {Name: "body", In: "body"}},
	}
	require.Len(t, routes, len(wantRoutes))
	for route, parameters := range wantRoutes {
		require.Contains(t, routes, route)
		require.ElementsMatch(t, parameters, routes[route], "%s", route)
	}
	for _, file := range []protoreflect.FileDescriptor{
		clusterv1.File_cluster_v1_cluster_proto, clusterv1.File_cluster_v1_spec_proto,
	} {
		messages := file.Messages()
		for i := range messages.Len() {
			message := messages.Get(i)
			definitionName, pathField := "v1"+string(message.Name()), protoreflect.Name("")
			switch message.Name() {
			case "GetClusterRequest", "ListClustersRequest":
				continue // These requests contain only path and query parameters.
			case "ReplaceClusterRequest":
				definitionName, pathField = "ClusterServiceReplaceClusterBody", "name"
			}
			definition, found := document.Definitions[definitionName]
			require.True(t, found, "%s", definitionName)
			fields, count := message.Fields(), 0
			for j := range fields.Len() {
				field := fields.Get(j)
				if field.Name() != pathField {
					require.Contains(t, definition.Properties, field.JSONName(), "%s", definitionName)
					count++
				}
			}
			require.Len(t, definition.Properties, count, "%s must expose exactly its protobuf fields", definitionName)
		}
	}
	for name, definition := range document.Definitions {
		if strings.HasPrefix(name, "v1") || name == "ClusterServiceReplaceClusterBody" {
			for field := range definition.Properties {
				require.Regexp(t, `^[a-z][a-z0-9]*(_[a-z0-9]+)*$`, field, "%s.%s", name, field)
			}
		}
	}
	int64Fields := map[string]string{"v1Cluster": "generation", "v1ClusterStatus": "observed_generation"}
	for definition, field := range int64Fields {
		property := document.Definitions[definition].Properties[field]
		require.Equal(t, "string", property.Type, "%s.%s", definition, field)
		require.Equal(t, "int64", property.Format, "%s.%s", definition, field)
	}
	statusFields := (&clusterv1.ClusterStatus{}).ProtoReflect().Descriptor().Fields()
	for _, field := range []protoreflect.Name{"state", "reason"} {
		property := document.Definitions["v1ClusterStatus"].Properties[string(field)]
		definition := document.Definitions[strings.TrimPrefix(property.Ref, "#/definitions/")]
		require.Equal(t, "string", definition.Type, "%s", field)
		values := statusFields.ByName(field).Enum().Values()
		var names []string
		for i := 0; i < values.Len(); i++ {
			if value := values.Get(i); value.Number() != 0 {
				names = append(names, string(value.Name()))
			}
		}
		require.Equal(t, names, definition.Enum, "%s", field)
	}
}

func TestStatusEnumJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *clusterv1.ClusterStatus
		json   string
	}{
		{"pending", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_PENDING, Reason: clusterv1.ClusterStatus_REASON_AWAITING_OBSERVATION},
			`{"state":"STATE_PENDING","reason":"REASON_AWAITING_OBSERVATION"}`},
		{"operational unknown", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_STATE_UNKNOWN, Reason: clusterv1.ClusterStatus_REASON_OBSERVATION_UNAVAILABLE},
			`{"state":"STATE_UNKNOWN","reason":"REASON_OBSERVATION_UNAVAILABLE"}`},
		{"unset is not operational unknown", &clusterv1.ClusterStatus{}, `{}`},
		{"unknown numeric value", &clusterv1.ClusterStatus{
			State: clusterv1.ClusterStatus_State(999), Reason: clusterv1.ClusterStatus_Reason(999)},
			`{"state":999,"reason":999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := protojson.Marshal(tc.status)
			require.NoError(t, err)
			require.JSONEq(t, tc.json, string(encoded))
			if tc.status.State != clusterv1.ClusterStatus_State(999) {
				return
			}
			// Unknown numeric values must survive decoding by an older client.
			var decoded clusterv1.ClusterStatus
			require.NoError(t, protojson.Unmarshal(encoded, &decoded))
			require.True(t, proto.Equal(tc.status, &decoded))
			binary, err := proto.Marshal(tc.status)
			require.NoError(t, err)
			require.NoError(t, proto.Unmarshal(binary, &decoded))
			require.True(t, proto.Equal(tc.status, &decoded))
		})
	}
	for _, input := range []string{
		`{"state":"STATE_FUTURE"}`, `{"reason":"REASON_FUTURE"}`, `{"state":"pending"}`,
	} {
		require.Error(t, protojson.Unmarshal([]byte(input), &clusterv1.ClusterStatus{}), input)
	}
	// Status is structurally absent from mutation requests, including numeric enums.
	for _, request := range []proto.Message{&clusterv1.CreateClusterRequest{}, &clusterv1.ReplaceClusterRequest{}} {
		for _, input := range []string{
			`{"status":{"state":"STATE_READY"}}`, `{"status":{"state":999}}`,
		} {
			require.Error(t, protojson.Unmarshal([]byte(input), request))
		}
		require.Nil(t, request.ProtoReflect().Descriptor().Fields().ByName("status"))
	}
}

func TestEnvelopeFieldNumbersAreStable(t *testing.T) {
	for _, test := range []struct {
		message protoreflect.MessageDescriptor
		names   []protoreflect.Name
	}{
		{(&clusterv1.Cluster{}).ProtoReflect().Descriptor(),
			[]protoreflect.Name{"name", "resource_version", "spec", "status", "uid", "generation"}},
		{(&clusterv1.CreateClusterRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"name", "spec"}},
		{(&clusterv1.GetClusterRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"name"}},
		{(&clusterv1.ReplaceClusterRequest{}).ProtoReflect().Descriptor(),
			[]protoreflect.Name{"name", "resource_version", "spec"}},
		{(&clusterv1.ListClustersRequest{}).ProtoReflect().Descriptor(), []protoreflect.Name{"page_size", "page_token"}},
	} {
		for i, name := range test.names {
			field := test.message.Fields().ByName(name)
			require.NotNil(t, field)
			require.Equal(t, protoreflect.FieldNumber(i+1), field.Number())
		}
	}
}
