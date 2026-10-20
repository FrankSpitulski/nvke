// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeCollector is an in-process OTLP/gRPC logs endpoint.
type fakeCollector struct {
	collogspb.UnimplementedLogsServiceServer

	addr string

	mu       sync.Mutex
	records  []*logspb.LogRecord
	resource map[string]string
	metadata metadata.MD
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	collector := &fakeCollector{addr: lis.Addr().String()}
	server := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(server, collector)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return collector
}

// Export implements collogspb.LogsServiceServer.
func (c *fakeCollector) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceLogs := range req.GetResourceLogs() {
		c.resource = map[string]string{}
		for _, kv := range resourceLogs.GetResource().GetAttributes() {
			c.resource[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			c.records = append(c.records, scopeLogs.GetLogRecords()...)
		}
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (c *fakeCollector) received() []*logspb.LogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*logspb.LogRecord(nil), c.records...)
}

// lockedBuffer is a bytes.Buffer that an export goroutine and the test can use together.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func attributes(record *logspb.LogRecord) map[string]string {
	out := map[string]string{}
	for _, kv := range record.GetAttributes() {
		out[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return out
}

func TestOTLPExportsRecordsWithTraceFields(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	stderr := &lockedBuffer{}

	run := NewRun("v1.2.3")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl bootstrap site")
	logger := slog.New(run.LogHandler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	logger.Info("provisioning", "phase", "kind")
	logger.Debug("not shown on stderr, so not exported")
	logger.InfoContext(ctx, "with context", "node", "n1")
	run.Finish(nil)

	records := collector.received()
	require.Len(t, records, 2, "the debug record is below the stderr level")
	wantTraceID, wantSpanID := run.SpanContext().TraceID(), run.SpanContext().SpanID()
	for _, record := range records {
		assert.Equal(t, hex.EncodeToString(wantTraceID[:]), hex.EncodeToString(record.GetTraceId()))
		assert.Equal(t, hex.EncodeToString(wantSpanID[:]), hex.EncodeToString(record.GetSpanId()))
	}
	assert.Equal(t, "provisioning", records[0].GetBody().GetStringValue())
	assert.Equal(t, "kind", attributes(records[0])["phase"])
	assert.Equal(t, "n1", attributes(records[1])["node"])
	assert.Equal(t, "nvkectl", collector.resource["service.name"])
	assert.Equal(t, "v1.2.3", collector.resource["service.version"])

	// stderr is what it is without export: the text handler's line plus the trace fields.
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "msg=provisioning phase=kind trace_id="+run.TraceID())
}

func TestOTLPSendsHeadersAndNeverLogsThem(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{
		Endpoint: collector.addr,
		Insecure: true,
		Headers:  map[string]string{"authorization": "Bearer s3cret"},
	}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
	run.Finish(nil)

	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Equal(t, []string{"Bearer s3cret"}, collector.metadata.Get("authorization"))
	assert.NotContains(t, stderr.String(), "s3cret")
}

func TestOTLPUnreachableEndpointWarnsOnceAndFinishIsBounded(t *testing.T) {
	t.Parallel()
	// A port nothing listens on: bind, note the address, close.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl create cluster")
	logger := slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil)))
	for range 20 {
		logger.Info("record")
	}

	started := time.Now()
	run.Finish(nil)
	assert.Less(t, time.Since(started), flushTimeout+time.Second, "Finish waits at most the flush bound")

	assert.Equal(t, 1, strings.Count(stderr.String(), "could not export telemetry"), "stderr:\n%s", stderr.String())
	assert.Equal(t, 20, strings.Count(stderr.String(), "msg=record"), "local output is unaffected")
}

func TestWithoutOTLPTheHandlerIsUnchanged(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	run.Start(context.Background(), "nvkectl get clusters")
	handler := run.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	assert.IsType(t, &logHandler{}, handler, "no tee, no exporter")
	run.Finish(nil) // flushing with nothing enabled is a no-op
}

func TestEnableOTLPRejectsBadUse(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	assert.Error(t, run.EnableOTLP(OTLPConfig{}), "an empty endpoint")

	run.Start(context.Background(), "nvkectl get clusters")
	run.LogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	assert.Error(t, run.EnableOTLP(OTLPConfig{Endpoint: "localhost:4317"}), "after the handler was built")
}

func TestOTLPConfigFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		want   OTLPConfig
		wantOK bool
	}{
		{name: "nothing set"},
		{
			name:   "host and port",
			env:    map[string]string{envEndpoint: "collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "http URL is insecure",
			env:    map[string]string{envEndpoint: "http://collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "https URL is secure",
			env:    map[string]string{envEndpoint: "https://collector:4317"},
			want:   OTLPConfig{Endpoint: "collector:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "insecure variable",
			env:    map[string]string{envEndpoint: "collector:4317", envInsecure: "true"},
			want:   OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name:   "logs endpoint wins",
			env:    map[string]string{envEndpoint: "general:4317", envLogsEndpoint: "logs:4317"},
			want:   OTLPConfig{Endpoint: "logs:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name: "headers",
			env: map[string]string{
				envEndpoint: "collector:4317",
				envHeaders:  "authorization=Bearer%20abc, x-team=nke,malformed",
			},
			want: OTLPConfig{
				Endpoint: "collector:4317",
				Headers:  map[string]string{"authorization": "Bearer abc", "x-team": "nke"},
			},
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{envEndpoint, envLogsEndpoint, envInsecure, envHeaders, envLogsHeaders} {
				t.Setenv(name, tt.env[name])
			}
			got, ok, err := OTLPConfigFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestOTLPConfigFromEnvRejectsAnUnparsableEndpoint(t *testing.T) {
	t.Setenv(envLogsEndpoint, "")
	t.Setenv(envEndpoint, "http://")
	_, ok, err := OTLPConfigFromEnv()
	assert.Error(t, err)
	assert.False(t, ok)
}
