// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"errors"
	"net/netip"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster/mapping"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

func unknownFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap() && field.MapValue().Message() != nil:
			value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
				unknown = unknownFields(item.Message())
				return !unknown
			})
		case field.IsList() && field.Message() != nil:
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				if unknownFields(list.Get(i).Message()) {
					unknown = true
					break
				}
			}
		case !field.IsMap() && field.Message() != nil:
			unknown = unknownFields(value.Message())
		}
		return !unknown
	})
	return unknown
}

func validateNetworks(spec *clusterv1.CreateClusterSpec) error {
	pods, err := singleIPv4Prefix(spec.GetClusterNetwork().GetPods().GetCidrBlocks())
	if err != nil {
		return fieldViolation("spec.cluster_network.pods.cidr_blocks", "must contain one canonical IPv4 CIDR")
	}
	services, err := singleIPv4Prefix(spec.GetClusterNetwork().GetServices().GetCidrBlocks())
	if err != nil {
		return fieldViolation("spec.cluster_network.services.cidr_blocks", "must contain one canonical IPv4 CIDR")
	}
	if pods.Overlaps(services) {
		return fieldViolation("spec.cluster_network", "pod and service networks must not overlap")
	}
	return nil
}

func singleIPv4Prefix(values []string) (netip.Prefix, error) {
	if len(values) != 1 {
		return netip.Prefix{}, errors.New("one network range is required")
	}
	prefix, err := netip.ParsePrefix(values[0])
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
		return netip.Prefix{}, errors.New("a canonical IPv4 prefix is required")
	}
	return prefix, nil
}

func invalidSpec(err error) error {
	if invalid, ok := errors.AsType[*mapping.FieldError](err); ok {
		return fieldViolation(invalid.Field, invalid.Description)
	}
	return status.Error(codes.InvalidArgument, "cluster spec is invalid")
}

func fieldViolation(field, description string) error {
	base := status.New(codes.InvalidArgument, field+": "+description)
	detailed, err := base.WithDetails(&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{
		Field: field, Description: description,
	}}})
	if err != nil {
		return base.Err()
	}
	return detailed.Err()
}
