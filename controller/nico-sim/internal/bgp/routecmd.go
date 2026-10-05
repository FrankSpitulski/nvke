// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
)

// routeCmdManager is the darwin RouteManager. macOS has no netlink and only
// root may change routes, so it shells out to `sudo -n /sbin/route -n`, which
// the sudoers rule in the nico-sim README allows for 172.31.0.x host routes.
// The argument order below must match that rule.
type routeCmdManager struct {
	run func(args ...string) ([]byte, error)
}

// sudoRoute runs `sudo -n /sbin/route -n args...`, returning combined output.
func sudoRoute(args ...string) ([]byte, error) {
	return exec.Command("sudo", append([]string{"-n", "/sbin/route", "-n"}, args...)...).CombinedOutput()
}

// CheckRouteSudo reports whether the sudoers rule for route installs is in
// place. `sudo -n -l` runs nothing and never prompts.
func CheckRouteSudo() error {
	args := []string{"-n", "-l", "/sbin/route", "-n", "add", "-host", "172.31.0.10", "192.168.64.2"}
	if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("passwordless sudo for /sbin/route host routes is not configured, "+
			"see the \"Full NKE stack on macOS\" section of controller/nico-sim/README.md: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteReplace installs a host route via the first next hop: macOS has no ECMP.
// The speaker sorts next hops, so the choice is stable and moves only when the
// announcing control plane changes.
func (m routeCmdManager) RouteReplace(route *netlink.Route) error {
	vip, gw := route.Dst.IP.String(), route.MultiPath[0].Gw.String()
	out, err := m.run("change", "-host", vip, gw)
	if err != nil && strings.Contains(string(out), "not in table") {
		out, err = m.run("add", "-host", vip, gw)
	}
	if err != nil {
		return fmt.Errorf("route %s via %s: %w: %s", vip, gw, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RouteDel removes a host route; a route that is already gone is success, like
// ESRCH from netlink.
func (m routeCmdManager) RouteDel(route *netlink.Route) error {
	vip := route.Dst.IP.String()
	out, err := m.run("delete", "-host", vip)
	if err != nil && !strings.Contains(string(out), "not in table") {
		return fmt.Errorf("route delete %s: %w: %s", vip, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LinkByName returns a placeholder: routes are keyed by gateway, not interface.
func (routeCmdManager) LinkByName(string) (netlink.Link, error) {
	return &netlink.Dummy{}, nil
}
