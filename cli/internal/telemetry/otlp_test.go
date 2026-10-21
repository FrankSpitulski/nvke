// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeCollector is an in-process OTLP/gRPC logs and traces endpoint.
type fakeCollector struct {
	collogspb.UnimplementedLogsServiceServer

	addr string

	mu           sync.Mutex
	records      []*logspb.LogRecord
	spans        []*tracepb.Span
	resource     map[string]string
	spanResource map[string]string
	metadata     metadata.MD
	spanMetadata metadata.MD
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	collector := &fakeCollector{addr: lis.Addr().String()}
	server := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(server, collector)
	coltracepb.RegisterTraceServiceServer(server, &traceCollector{collector: collector})
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

// exportSpans records the spans of one trace export request.
func (c *fakeCollector) exportSpans(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spanMetadata, _ = metadata.FromIncomingContext(ctx)
	for _, resourceSpans := range req.GetResourceSpans() {
		c.spanResource = map[string]string{}
		for _, kv := range resourceSpans.GetResource().GetAttributes() {
			c.spanResource[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			c.spans = append(c.spans, scopeSpans.GetSpans()...)
		}
	}
}

func (c *fakeCollector) receivedSpans() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...)
}

// traceCollector adapts fakeCollector to the trace service, whose Export method
// would otherwise collide with the logs service's.
type traceCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	collector *fakeCollector
}

// Export implements coltracepb.TraceServiceServer.
func (t *traceCollector) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	t.collector.exportSpans(ctx, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
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

func TestOTLPExportsSpansWithRunTraceID(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)

	run := NewRun("v1.2.3")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	ctx := run.Start(context.Background(), "nvkectl create cluster")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: Transport(srv.Client().Transport)}).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	run.Finish(nil)

	spans := collector.receivedSpans()
	require.Len(t, spans, 2, "the root span and the HTTP client span")
	var root, client *tracepb.Span
	for _, span := range spans {
		if span.GetName() == "nvkectl create cluster" {
			root = span
		} else {
			client = span
		}
	}
	require.NotNil(t, root)
	require.NotNil(t, client)
	for _, span := range spans {
		assert.Equal(t, run.TraceID(), hex.EncodeToString(span.GetTraceId()))
	}
	assert.Equal(t, hex.EncodeToString(root.GetSpanId()), hex.EncodeToString(client.GetParentSpanId()))
	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Equal(t, "nvkectl", collector.spanResource["service.name"])
	assert.Equal(t, "v1.2.3", collector.spanResource["service.version"])
}

func TestOTLPExportsSpansWhenEnabledBeforeStart(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	run.Finish(nil)

	require.Len(t, collector.receivedSpans(), 1)
}

func TestOTLPExportsSpansWhenEnabledAfterStart(t *testing.T) {
	t.Parallel()
	collector := newFakeCollector(t)

	run := NewRun("dev")
	run.Start(context.Background(), "nvkectl get clusters")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Finish(nil)

	spans := collector.receivedSpans()
	require.Len(t, spans, 1, "the root span started before EnableOTLP is still exported")
	assert.Equal(t, run.TraceID(), hex.EncodeToString(spans[0].GetTraceId()))
}

func TestOTLPSpansSendHeaders(t *testing.T) {
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
	assert.Equal(t, []string{"Bearer s3cret"}, collector.spanMetadata.Get("authorization"))
	assert.NotContains(t, stderr.String(), "s3cret")
}

func TestOTLPUnreachableEndpointWarnsOnceForBothSignals(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl create cluster")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("record")

	started := time.Now()
	run.Finish(nil)
	assert.Less(t, time.Since(started), flushTimeout+500*time.Millisecond, "both signals share one flush bound")

	assert.Equal(t, 1, strings.Count(stderr.String(), "could not export telemetry"), "stderr:\n%s", stderr.String())
}

func TestRunOTLPReportsTheEnabledConfig(t *testing.T) {
	t.Parallel()
	run := NewRun("dev")
	cfg, ok := run.OTLP()
	assert.False(t, ok)
	assert.Equal(t, OTLPConfig{}, cfg)

	want := OTLPConfig{Endpoint: "collector:4317", Insecure: true, Headers: map[string]string{"x-team": "nke"}}
	require.NoError(t, run.EnableOTLP(want))
	cfg, ok = run.OTLP()
	assert.True(t, ok)
	assert.Equal(t, want, cfg)
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

// The signal-specific variables the OTLP exporter library would read on its own.
const (
	logsEndpointVar = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	logsHeadersVar  = "OTEL_EXPORTER_OTLP_LOGS_HEADERS"
	logsInsecureVar = "OTEL_EXPORTER_OTLP_LOGS_INSECURE"
)

// The exporter library fills a setting nobody passed from its own environment,
// so these tests set the signal-specific variables and check that none of them
// changes where the records go, what is sent with them, or whether TLS is used.
// They set variables, so neither is parallel.
func TestOTLPExporterIgnoresSignalSpecificVariables(t *testing.T) {
	collector := newFakeCollector(t)
	t.Setenv(logsEndpointVar, "127.0.0.1:1")
	t.Setenv(logsHeadersVar, "x-leaked=yes")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_COMPRESSION", "gzip")

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	require.Len(t, collector.received(), 1, "the records went to the configured endpoint")
	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Empty(t, collector.metadata.Get("x-leaked"), "no header from OTEL_EXPORTER_OTLP_LOGS_HEADERS")
	assert.Empty(t, collector.metadata.Get("grpc-encoding"), "no compression from OTEL_EXPORTER_OTLP_LOGS_COMPRESSION")
}

func TestOTLPSecureConfigIgnoresTheInsecureLogsVariable(t *testing.T) {
	collector := newFakeCollector(t) // serves plaintext, so a TLS client cannot reach it
	t.Setenv(logsInsecureVar, "true")
	stderr := &lockedBuffer{}

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
	run.Finish(nil)

	assert.Empty(t, collector.received(), "the connection used TLS, as configured")
	assert.Contains(t, stderr.String(), "could not export telemetry")
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
			name:   "signal-specific variables are ignored",
			env:    map[string]string{envEndpoint: "general:4317", logsEndpointVar: "logs:4317", logsHeadersVar: "x-logs=1", logsInsecureVar: "true"},
			want:   OTLPConfig{Endpoint: "general:4317", Headers: map[string]string{}},
			wantOK: true,
		},
		{
			name: "only a signal-specific endpoint is no endpoint",
			env:  map[string]string{logsEndpointVar: "logs:4317"},
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
			for _, name := range []string{envEndpoint, envInsecure, envHeaders, logsEndpointVar, logsHeadersVar, logsInsecureVar} {
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
	t.Setenv(envEndpoint, "http://")
	_, ok, err := OTLPConfigFromEnv()
	assert.Error(t, err)
	assert.False(t, ok)
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantHost     string
		wantInsecure bool
		wantErr      bool
	}{
		{name: "bare host and port", raw: "host:4317", wantHost: "host:4317"},
		{name: "http scheme is insecure", raw: "http://host:4317", wantHost: "host:4317", wantInsecure: true},
		{name: "https scheme is secure", raw: "https://host", wantHost: "host"},
		{name: "scheme without a host", raw: "http://", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "bare IPv6 address", raw: "::1", wantErr: true},
		{name: "IPv6 address with a port but no brackets", raw: "fe80::1:4317", wantErr: true},
		{name: "bracketed IPv6 with a port", raw: "[::1]:4317", wantHost: "[::1]:4317"},
		{name: "bracketed IPv6 without a port", raw: "[::1]", wantHost: "[::1]"},
		{name: "IPv6 in a URL", raw: "http://[::1]:4317", wantHost: "[::1]:4317", wantInsecure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, insecure, err := ParseEndpoint(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantInsecure, insecure)
		})
	}
}

func TestRejectLoopback(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "localhost with port", endpoint: "localhost:4317", wantErr: true},
		{name: "localhost without port", endpoint: "localhost", wantErr: true},
		{name: "localhost in capitals", endpoint: "LOCALHOST:4317", wantErr: true},
		{name: "localhost in capitals without port", endpoint: "LOCALHOST", wantErr: true},
		{name: "127.0.0.1", endpoint: "127.0.0.1:4317", wantErr: true},
		{name: "another address in 127.0.0.0/8", endpoint: "127.1.2.3:4317", wantErr: true},
		{name: "bracketed IPv6 loopback", endpoint: "[::1]", wantErr: true},
		{name: "bracketed IPv6 loopback with port", endpoint: "[::1]:4317", wantErr: true},
		{name: "unspecified IPv4", endpoint: "0.0.0.0:4317", wantErr: true},
		{name: "unspecified IPv6", endpoint: "[::]:4317", wantErr: true},
		{name: "localhost with a trailing dot", endpoint: "localhost.:4317", wantErr: true},
		{name: "name under .localhost", endpoint: "foo.localhost:4317", wantErr: true},
		{name: "name under .localhost in capitals", endpoint: "FOO.LOCALHOST:4317", wantErr: true},
		{name: "host that merely starts with localhost", endpoint: "localhost.example.com:4317"},
		{name: "private address", endpoint: "10.0.0.5:4317"},
		{name: "DNS name", endpoint: "collector.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RejectLoopback(tt.endpoint)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.endpoint)
			assert.Contains(t, err.Error(), "temporary controller")
			assert.Contains(t, err.Error(), "cannot reach")
		})
	}
}

// clearOTLPEnv empties every OTEL_EXPORTER_OTLP_* variable the exporters read,
// so a test does not depend on the shell it runs in.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envEndpoint, logsEndpointVar, envInsecure, envHeaders, logsHeadersVar,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_LOGS_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	} {
		t.Setenv(name, "")
	}
}

// The environment must not turn a TLS endpoint into a plaintext one: the
// bearer token would go out in clear. The fake collector speaks plaintext, so
// a TLS export to it delivers nothing.
func TestEnableOTLPKeepsTLSDespiteEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "http endpoint", env: map[string]string{envEndpoint: "http://other.example:4317"}},
		{name: "http traces endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://other.example:4317"}},
		{name: "http logs endpoint", env: map[string]string{logsEndpointVar: "http://other.example:4317"}},
		{name: "insecure variable", env: map[string]string{envInsecure: "true"}},
		{name: "insecure traces variable", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true"}},
		{name: "insecure logs variable", env: map[string]string{"OTEL_EXPORTER_OTLP_LOGS_INSECURE": "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOTLPEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}
			collector := newFakeCollector(t)
			stderr := &lockedBuffer{}

			run := NewRun("dev")
			require.NoError(t, run.EnableOTLP(OTLPConfig{
				Endpoint: collector.addr,
				Insecure: false,
				Headers:  map[string]string{"authorization": "Bearer s3cret"},
			}))
			run.Start(context.Background(), "nvkectl get clusters")
			slog.New(run.LogHandler(slog.NewTextHandler(stderr, nil))).Info("hello")
			run.Finish(nil)

			assert.Empty(t, collector.receivedSpans(), "no span reaches a plaintext collector")
			assert.Empty(t, collector.received(), "no record reaches a plaintext collector")
			collector.mu.Lock()
			defer collector.mu.Unlock()
			assert.Empty(t, collector.spanMetadata.Get("authorization"), "the token is not sent in clear")
			assert.Empty(t, collector.metadata.Get("authorization"), "the token is not sent in clear")
		})
	}
}

func TestEnableOTLPInsecureDeliversDespiteEnvironment(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv(envEndpoint, "https://other.example:4317")
	collector := newFakeCollector(t)

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	run.Finish(nil)

	assert.Len(t, collector.receivedSpans(), 1, "the configured endpoint wins over the environment's")
}

func TestOTLPGeneralEndpointExportsBoth(t *testing.T) {
	clearOTLPEnv(t)
	collector := newFakeCollector(t)
	t.Setenv(envEndpoint, "http://"+collector.addr)

	cfg, ok, err := OTLPConfigFromEnv()
	require.NoError(t, err)
	require.True(t, ok)
	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(cfg))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	assert.Len(t, collector.received(), 1)
	assert.Len(t, collector.receivedSpans(), 1)
}

// The exporter reports a malformed header entry through OpenTelemetry's internal
// logger, which writes the entry to the process's stderr, and a mistyped one is
// a token.
func TestEnableOTLPNeverPrintsAMalformedHeaderEntry(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv(envHeaders, "Bearer s3cret-token")
	collector := newFakeCollector(t)

	// The internal logger writes to the process's stderr; the default error
	// handler writes through the log package. Capture both.
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	realStderr := os.Stderr
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = realStderr })

	run := NewRun("dev")
	require.NoError(t, run.EnableOTLP(OTLPConfig{Endpoint: collector.addr, Insecure: true}))
	run.Start(context.Background(), "nvkectl get clusters")
	slog.New(run.LogHandler(slog.NewTextHandler(&lockedBuffer{}, nil))).Info("hello")
	run.Finish(nil)

	os.Stderr = realStderr
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.NotContains(t, logged.String(), "s3cret-token")
	assert.NotContains(t, string(printed), "s3cret-token")
}
