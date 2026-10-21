// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An OTLP endpoint that is unreachable or misconfigured never fails a command
// or holds it up. These tests run commands through execute, so none is parallel.
func TestOTLPEndpointNeverFailsTheCommand(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedPort := lis.Addr().String()
	require.NoError(t, lis.Close())

	tests := map[string]string{
		"unreachable":    "http://" + closedPort,
		"unparsable":     "http://",
		"no endpoint":    "",
		"https endpoint": "https://" + closedPort,
	}
	for name, endpoint := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			root := NewRootCmd("test", "test", "test", "test")
			root.AddCommand(&cobra.Command{
				Use: "probe",
				RunE: func(*cobra.Command, []string) error {
					slog.Info("probe ran")
					return nil
				},
			})
			root.SetArgs([]string{"probe"})
			var stderr bytes.Buffer
			root.SetErr(&stderr)

			started := time.Now()
			require.NoError(t, execute(context.Background(), root, "test", &stderr))

			assert.Less(t, time.Since(started), 4*time.Second, "the flush is bounded")
			assert.Regexp(t, traceIDLineRE, stderr.String(), "the command's output is unchanged")
		})
	}
}
