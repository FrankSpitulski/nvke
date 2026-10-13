// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package guestfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseListenOutput(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{name: "the eval form guestfish prints", out: "GUESTFISH_PID=4321; export GUESTFISH_PID\n", want: "4321"},
		{name: "pid alone", out: "GUESTFISH_PID=7", want: "7"},
		{name: "no pid is an error", out: "something else", wantErr: true},
		{name: "empty pid is an error", out: "GUESTFISH_PID=; export GUESTFISH_PID", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseListenOutput(tt.out)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseStatNS(t *testing.T) {
	out := "st_dev: 2049\nst_ino: 12\nst_mode: 16832\nst_nlink: 2\nst_uid: 998\nst_gid: 997\n"
	st, err := parseStatNS(out)
	require.NoError(t, err)
	// 16832 = 0o40700: a directory with 0700 permissions.
	assert.Equal(t, uint32(0o700), st.Perm)
	assert.Equal(t, 998, st.UID)
	assert.Equal(t, 997, st.GID)

	_, err = parseStatNS("st_mode: 384\n")
	require.Error(t, err, "missing uid/gid fields must not default to root")
}
