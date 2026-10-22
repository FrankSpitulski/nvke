// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_ValidationLabelFilter(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"neither set leaves the filter alone": {nil, ""},
		"the site":                            {map[string]string{siteValidateEnv: "true", tenantValidateEnv: "false"}, "site-validate"},
		"the site, tenant unset":              {map[string]string{siteValidateEnv: "true"}, "site-validate"},
		"the site and a tenant":               {map[string]string{siteValidateEnv: "true", tenantValidateEnv: "true"}, "site-validate || tenant-validate"},
		"a tenant alone":                      {map[string]string{siteValidateEnv: "false", tenantValidateEnv: "true"}, "tenant-validate"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := validationLabelFilter(func(key string) string { return tc.env[key] })
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUnit_ValidationLabelFilterRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"both false":     {siteValidateEnv: "false", tenantValidateEnv: "false"},
		"site garbage":   {siteValidateEnv: "yes please"},
		"tenant garbage": {siteValidateEnv: "true", tenantValidateEnv: "sometimes"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validationLabelFilter(func(key string) string { return env[key] })
			require.Error(t, err)
		})
	}
}
