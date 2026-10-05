// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestRouteCmdManager(t *testing.T) {
	vip := &net.IPNet{IP: net.ParseIP("172.31.0.10").To4(), Mask: net.CIDRMask(32, 32)}
	multi := &netlink.Route{Dst: vip, MultiPath: []*netlink.NexthopInfo{{Gw: net.ParseIP("192.168.64.2")}, {Gw: net.ParseIP("192.168.64.3")}}}
	notInTable := []byte("route: writing to routing socket: not in table\n")

	tests := []struct {
		name string
		call func(routeCmdManager) error
		// errs are the errors of successive commands; output is shared by all.
		errs    []error
		output  []byte
		want    [][]string
		wantErr bool
	}{
		{
			name: "replace changes an existing route via the first next hop",
			call: func(m routeCmdManager) error { return m.RouteReplace(multi) },
			want: [][]string{{"change", "-host", "172.31.0.10", "192.168.64.2"}},
		},
		{
			name:   "replace adds when the route is not in the table",
			call:   func(m routeCmdManager) error { return m.RouteReplace(multi) },
			errs:   []error{errors.New("exit 1")},
			output: notInTable,
			want:   [][]string{{"change", "-host", "172.31.0.10", "192.168.64.2"}, {"add", "-host", "172.31.0.10", "192.168.64.2"}},
		},
		{
			name:    "replace surfaces other failures",
			call:    func(m routeCmdManager) error { return m.RouteReplace(multi) },
			errs:    []error{errors.New("exit 1")},
			output:  []byte("sudo: a password is required"),
			want:    [][]string{{"change", "-host", "172.31.0.10", "192.168.64.2"}},
			wantErr: true,
		},
		{
			name: "delete removes the host route",
			call: func(m routeCmdManager) error { return m.RouteDel(&netlink.Route{Dst: vip}) },
			want: [][]string{{"delete", "-host", "172.31.0.10"}},
		},
		{
			name:   "delete of an absent route is success",
			call:   func(m routeCmdManager) error { return m.RouteDel(&netlink.Route{Dst: vip}) },
			errs:   []error{errors.New("exit 1")},
			output: notInTable,
			want:   [][]string{{"delete", "-host", "172.31.0.10"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got [][]string
			m := routeCmdManager{run: func(args ...string) ([]byte, error) {
				var err error
				if len(got) < len(tt.errs) {
					err = tt.errs[len(got)]
				}
				got = append(got, args)
				return tt.output, err
			}}
			err := tt.call(m)
			require.Equal(t, tt.want, got)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
