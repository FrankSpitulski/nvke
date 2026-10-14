// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInformationalFlagsDoNotLoadCredentials(t *testing.T) {
	for _, flag := range []string{"--version", "-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, run(context.Background(), []string{flag}, &output))
			if flag == "--version" {
				require.Equal(t, version+"\n", output.String())
				return
			}
			require.Contains(t, output.String(), "Usage of tenant-api:")
			require.Contains(t, output.String(), "-kubeconfig")
		})
	}
}

func TestAuthenticationMustBeExplicitlyDeferred(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), nil, &output)
	require.ErrorContains(t, err, "--evaluation-no-auth")
}
