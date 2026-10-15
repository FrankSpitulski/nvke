// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/validate/pkg/checks"
)

// fakeProber maps commands to scripted results; an unmapped command is a
// transport error, so a test cannot silently pass on a command it never
// anticipated.
type fakeProber struct {
	results map[string]checks.ProbeResult
}

func (f *fakeProber) Probe(_ context.Context, command string) (checks.ProbeResult, error) {
	if res, ok := f.results[command]; ok {
		return res, nil
	}
	return checks.ProbeResult{}, fmt.Errorf("unscripted probe: %q", command)
}

// healthyNode scripts a post-rename image: nvke-* unit names.
func healthyNode() *fakeProber {
	return &fakeProber{results: map[string]checks.ProbeResult{
		"systemctl show -p LoadState --value nvke-agent.service":           {Stdout: "loaded\n"},
		"systemctl show -p LoadState --value nvke-spire-configure.service": {Stdout: "loaded\n"},
		"systemctl is-enabled nvke-agent.service":                          {Stdout: "enabled\n"},
		"systemctl is-enabled spire-agent.service":                         {Stdout: "enabled\n"},
		"systemctl is-enabled luks-tpm-firstboot.service":                  {Stdout: "enabled\n"},
		"systemctl show -p After --value spire-agent.service":              {Stdout: "basic.target nvke-spire-configure.service sysinit.target\n"},
		"systemctl show -p After --value nvke-agent.service":               {Stdout: "network-online.target spire-agent.service\n"},
		"sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait":       {Stdout: "60\n"},
	}}
}

// preRenameNode scripts a release up to v0.2.0: nke-* unit names, with the
// nvke-* names reporting not-found the way a real systemd answers.
func preRenameNode() *fakeProber {
	return &fakeProber{results: map[string]checks.ProbeResult{
		"systemctl show -p LoadState --value nvke-agent.service":           {Stdout: "not-found\n"},
		"systemctl show -p LoadState --value nke-agent.service":            {Stdout: "loaded\n"},
		"systemctl show -p LoadState --value nvke-spire-configure.service": {Stdout: "not-found\n"},
		"systemctl show -p LoadState --value nke-spire-configure.service":  {Stdout: "loaded\n"},
		"systemctl is-enabled nke-agent.service":                           {Stdout: "enabled\n"},
		"systemctl is-enabled spire-agent.service":                         {Stdout: "enabled\n"},
		"systemctl is-enabled luks-tpm-firstboot.service":                  {Stdout: "enabled\n"},
		"systemctl show -p After --value spire-agent.service":              {Stdout: "basic.target nke-spire-configure.service sysinit.target\n"},
		"systemctl show -p After --value nke-agent.service":                {Stdout: "network-online.target spire-agent.service\n"},
		"sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait":       {Stdout: "60\n"},
	}}
}

func run(t *testing.T, id string, prober checks.Prober) checks.Outcome {
	t.Helper()
	impl, ok := Impls()[id]
	require.True(t, ok, "no implementation for %s", id)
	out, err := impl(context.Background(), checks.Target{}, checks.Inputs{Boot: prober})
	require.NoError(t, err)
	return out
}

func TestUnitEnabledChecks(t *testing.T) {
	for _, id := range []string{"SPEC-001", "SPEC-002", "FDE-002"} {
		assert.Equal(t, checks.StatusPass, run(t, id, healthyNode()).Status, id)
		assert.Equal(t, checks.StatusPass, run(t, id, preRenameNode()).Status, id+" on a pre-rename image")
	}

	p := healthyNode()
	p.results["systemctl is-enabled nvke-agent.service"] = checks.ProbeResult{Stdout: "disabled\n", ExitCode: 1}
	out := run(t, "SPEC-001", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "disabled")

	p = healthyNode()
	p.results["systemctl show -p LoadState --value nvke-agent.service"] = checks.ProbeResult{Stdout: "not-found\n"}
	p.results["systemctl show -p LoadState --value nke-agent.service"] = checks.ProbeResult{Stdout: "not-found\n"}
	out = run(t, "SPEC-001", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "no node agent unit is present", "neither name generation present is a fail naming both")
	assert.Contains(t, out.Evidence, "nke-agent.service not-found")

	p = healthyNode()
	p.results["systemctl is-enabled spire-agent.service"] = checks.ProbeResult{Stdout: "", ExitCode: 4}
	out = run(t, "SPEC-002", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "exit 4", "a unit systemd does not know is a fail naming the exit")
}

func TestCheckUnitOrdering(t *testing.T) {
	assert.Equal(t, checks.StatusPass, run(t, "SPEC-003", healthyNode()).Status)
	out := run(t, "SPEC-003", preRenameNode())
	assert.Equal(t, checks.StatusPass, out.Status, "pre-rename unit names satisfy the ordering: %s", out.Evidence)

	p := healthyNode()
	p.results["systemctl show -p After --value nvke-agent.service"] = checks.ProbeResult{Stdout: "network-online.target\n"}
	out = run(t, "SPEC-003", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "does not order After=spire-agent.service")

	p = healthyNode()
	p.results["systemctl show -p After --value spire-agent.service"] = checks.ProbeResult{Stdout: "nvke-spire-configure.servicex\n"}
	out = run(t, "SPEC-003", p)
	assert.Equal(t, checks.StatusFail, out.Status, "substring of another unit name must not satisfy the ordering")
}

func TestCheckConntrackApplied(t *testing.T) {
	assert.Equal(t, checks.StatusPass, run(t, "SPEC-005", healthyNode()).Status)

	p := healthyNode()
	p.results["sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait"] = checks.ProbeResult{Stdout: "120\n"}
	out := run(t, "SPEC-005", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "120")

	p = healthyNode()
	p.results["sysctl -n net.netfilter.nf_conntrack_tcp_timeout_time_wait"] = checks.ProbeResult{ExitCode: 255}
	out = run(t, "SPEC-005", p)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "not loaded")
}

func TestTransportErrorIsAnError(t *testing.T) {
	impl := Impls()["SPEC-001"]
	_, err := impl(context.Background(), checks.Target{}, checks.Inputs{Boot: &fakeProber{results: map[string]checks.ProbeResult{}}})
	require.Error(t, err, "a transport failure must surface as StatusError, never as pass or fail")
}

func TestCheckBOMBooted(t *testing.T) {
	bomQuery := func(name string) string {
		return "dpkg-query -W -f='${db:Status-Status} ${Version}' " + name
	}
	withBOM := func(p *fakeProber, bom map[string]string) checks.Inputs {
		return checks.Inputs{Boot: p, BOM: bom}
	}

	out, err := checkBOMBooted(context.Background(), checks.Target{}, checks.Inputs{Boot: healthyNode()})
	require.NoError(t, err)
	assert.Equal(t, checks.StatusSkip, out.Status, "no BOM is a visible skip")
	assert.Contains(t, out.Evidence, "--target-bom")

	p := healthyNode()
	p.results[bomQuery("nvke-agent")] = checks.ProbeResult{Stdout: "installed 0.4.0\n"}
	p.results[bomQuery("spire-agent")] = checks.ProbeResult{Stdout: "installed 1.14.1+tpm1.9.0-2\n"}
	out, err = checkBOMBooted(context.Background(), checks.Target{}, withBOM(p, map[string]string{"nvke-agent": "0.4.0", "spire-agent": "1.14.1+tpm1.9.0-2"}))
	require.NoError(t, err)
	assert.Equal(t, checks.StatusPass, out.Status, out.Evidence)

	p = healthyNode()
	p.results[bomQuery("nvke-agent")] = checks.ProbeResult{Stdout: "installed 0.3.0-rc.1\n"}
	out, err = checkBOMBooted(context.Background(), checks.Target{}, withBOM(p, map[string]string{"nvke-agent": "0.4.0"}))
	require.NoError(t, err)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "nvke-agent: bom 0.4.0, booted image 0.3.0-rc.1")

	p = healthyNode()
	p.results[bomQuery("nvke-agent")] = checks.ProbeResult{Stdout: "", ExitCode: 1}
	out, err = checkBOMBooted(context.Background(), checks.Target{}, withBOM(p, map[string]string{"nvke-agent": "0.4.0"}))
	require.NoError(t, err)
	assert.Equal(t, checks.StatusFail, out.Status)
	assert.Contains(t, out.Evidence, "nvke-agent: not installed")

	p = healthyNode()
	p.results[bomQuery("nvke-agent")] = checks.ProbeResult{Stdout: "config-files 0.4.0\n"}
	out, err = checkBOMBooted(context.Background(), checks.Target{}, withBOM(p, map[string]string{"nvke-agent": "0.4.0"}))
	require.NoError(t, err)
	assert.Equal(t, checks.StatusFail, out.Status, "removed-but-not-purged must not count as installed")
	assert.Contains(t, out.Evidence, "dpkg state config-files")
}
