// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDarwinLease(t *testing.T) {
	data := "{\n ip_address=192.168.64.7\n hw_address=1,52:54:0:a:b:c\n lease=0x7fffffff\n}\n{\n ip_address=192.168.64.8\n hw_address=1,52:54:0:d:e:f\n lease=0x7fffffff\n}"
	ip, ok := darwinLease(data, "52:54:00:0a:0b:0c")
	require.True(t, ok)
	require.Equal(t, "192.168.64.7", ip)
	_, ok = darwinLease(data, "52:54:00:00:00:00")
	require.False(t, ok)
	_, ok = darwinLease("{\nip_address=invalid\nhw_address=1,52:54:0:a:b:c\n lease=0x7fffffff\n}", "52:54:00:0a:0b:0c")
	require.False(t, ok)
	_, ok = darwinLease("{\nip_address=192.168.64.3\nhw_address=1,52:54:0:a:b:c\nlease=1\n}", "52:54:00:0a:0b:0c")
	require.False(t, ok)
}
