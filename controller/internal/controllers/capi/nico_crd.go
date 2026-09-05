// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	nicov1 "github.com/NVIDIA/cluster-api-provider-nico/api/v1alpha1"
)

// NicoMachineCRDInstalled reports whether the management cluster serves the
// CAPNICo NicoMachine CRD. A watch on a kind with no CRD never syncs, and
// controller-runtime then stops the whole manager.
//
// A missing CRD is false, not an error. Any other discovery failure is an
// error: it says nothing about whether the provider is installed.
func NicoMachineCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	gk := schema.GroupKind{Group: nicov1.GroupVersion.Group, Kind: "NicoMachine"}

	switch _, err := mapper.RESTMapping(gk, nicov1.GroupVersion.Version); {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("looking up %s: %w", gk, err)
	}
}
