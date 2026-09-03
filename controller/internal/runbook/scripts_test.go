// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUpgradeKubeletKubeadmScript runs the shell-level unit test for
// scripts/upgrade-kubelet-kubeadm.sh, which stubs out dpkg-query, apt-get and
// the rest of the commands the runbook shells out to, then asserts on the
// distro-node and non-distro-node code paths. See
// testdata/upgrade-kubelet-kubeadm_test.sh for the cases it covers.
func TestUpgradeKubeletKubeadmScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found on PATH, skipping shell-level runbook test")
	}

	cmd := exec.CommandContext(t.Context(), "bash", "testdata/upgrade-kubelet-kubeadm_test.sh", "scripts/upgrade-kubelet-kubeadm.sh")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "shell test output:\n%s", output)
	t.Logf("shell test output:\n%s", output)
}
