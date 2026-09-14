// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSeedNetworkInterface(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preserve bool
	}{
		{"Darwin keeps the configured NIC and requests a MAC lease", true},
		{"Linux retains the eth0 rename", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := (Builder{MockMode: true}).Build(context.Background(), BuildInput{
				WorkingDir: dir, InstanceID: "test", Hostname: "test", NICMAC: "52:54:00:0a:0b:0c", PreserveInterfaceName: tc.preserve,
			})
			require.NoError(t, err)
			data, err := os.ReadFile(filepath.Join(dir, "network-config"))
			require.NoError(t, err)
			require.Contains(t, string(data), "dhcp4: true")
			require.Contains(t, string(data), `macaddress: "52:54:00:0a:0b:0c"`)
			if tc.preserve {
				require.NotContains(t, string(data), "set-name:")
				require.Contains(t, string(data), "dhcp-identifier: mac")
			} else {
				require.Contains(t, string(data), "set-name: eth0")
				require.NotContains(t, string(data), "dhcp-identifier:")
			}
		})
	}
}
