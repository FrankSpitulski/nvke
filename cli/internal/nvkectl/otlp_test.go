// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

// otlpCollector is an in-process OTLP/gRPC endpoint that keeps the spans it
// receives and the headers they came with.
type otlpCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	addr string

	mu       sync.Mutex
	spans    []*tracepb.Span
	metadata metadata.MD
}

// nonLoopbackIPv4 returns an IPv4 address of this machine that is not a
// loopback address and that the machine can open a TCP connection to itself on,
// so a test can pass it to a flag that rejects loopback and still reach a
// collector there; with mustConnect false, any such address will do. A host
// firewall or VPN can reset these connections; the test is skipped when no
// address works.
func nonLoopbackIPv4(t *testing.T, mustConnect bool) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() && (!mustConnect || canDialSelf(ipNet.IP.String())) {
				return ipNet.IP.String()
			}
		}
	}
	t.Skip("this machine has no non-loopback IPv4 address it can open a TCP connection to itself on")
	return ""
}

// canDialSelf reports whether a connection to a listener on ip reaches it and
// carries data both ways.
func canDialSelf(ip string) bool {
	lis, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return false
	}
	defer lis.Close()
	go func() {
		if conn, err := lis.Accept(); err == nil {
			_, _ = conn.Write([]byte{1})
			conn.Close()
		}
	}()
	conn, err := net.DialTimeout("tcp", lis.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = conn.Read(make([]byte, 1))
	return err == nil
}

// newOTLPCollector starts a collector on a non-loopback address of this
// machine and returns it with that address as host:port.
func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()
	lis, err := net.Listen("tcp", nonLoopbackIPv4(t, true)+":0")
	require.NoError(t, err)
	c := &otlpCollector{addr: lis.Addr().String()}
	server := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(server, c)
	collogspb.RegisterLogsServiceServer(server, &otlpLogsSink{})
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return c
}

// Export implements coltracepb.TraceServiceServer.
func (c *otlpCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceSpans := range req.GetResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			c.spans = append(c.spans, scopeSpans.GetSpans()...)
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (c *otlpCollector) received() ([]*tracepb.Span, metadata.MD) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...), c.metadata.Copy()
}

// otlpLogsSink accepts log exports, which the tests here do not inspect. The
// logs and trace services both name their method Export, so they cannot share a type.
type otlpLogsSink struct {
	collogspb.UnimplementedLogsServiceServer
}

// Export implements collogspb.LogsServiceServer.
func (*otlpLogsSink) Export(context.Context, *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	return &collogspb.ExportLogsServiceResponse{}, nil
}

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

// resolveFor parses args as the flags of "bootstrap site" and resolves the OTLP
// configuration for it, without touching the network.
func resolveFor(t *testing.T, args ...string) (cfg telemetry.OTLPConfig, ok, strict bool, err error) {
	t.Helper()
	cmd := newBootstrapSiteCmd()
	require.NoError(t, cmd.ParseFlags(args))
	return resolveOTLP(cmd)
}

func TestResolveOTLPPrecedence(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "other.example:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer x")

	t.Run("the flag wins over the environment and keeps its headers", func(t *testing.T) {
		cfg, ok, strict, err := resolveFor(t, "--otlp-endpoint", "collector.example:4317")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, strict)
		assert.Equal(t, "collector.example:4317", cfg.Endpoint)
		assert.False(t, cfg.Insecure)
		assert.Equal(t, "Bearer x", cfg.Headers["authorization"])
	})

	t.Run("without the flag the environment applies and is not strict", func(t *testing.T) {
		cfg, ok, strict, err := resolveFor(t)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, strict)
		assert.Equal(t, "other.example:4317", cfg.Endpoint)
		assert.Equal(t, "Bearer x", cfg.Headers["authorization"])
	})

	t.Run("--otlp-insecure sets Insecure", func(t *testing.T) {
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "collector.example:4317", "--otlp-insecure")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, cfg.Insecure)
	})

	t.Run("an http URL gives the host and implies insecure", func(t *testing.T) {
		cfg, ok, _, err := resolveFor(t, "--otlp-endpoint", "http://collector.example:4317")
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "collector.example:4317", cfg.Endpoint)
		assert.True(t, cfg.Insecure)
	})

	t.Run("a flag error is strict", func(t *testing.T) {
		_, ok, strict, err := resolveFor(t, "--otlp-endpoint", "localhost:4317")
		require.Error(t, err)
		assert.False(t, ok)
		assert.True(t, strict)
	})
}
