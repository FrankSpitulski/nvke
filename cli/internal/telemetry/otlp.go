// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envInsecure = "OTEL_EXPORTER_OTLP_INSECURE"
	envHeaders  = "OTEL_EXPORTER_OTLP_HEADERS"

	// flushTimeout bounds how long Finish waits for buffered records to reach
	// the endpoint, so an unreachable endpoint cannot hang a command.
	flushTimeout = 2 * time.Second

	serviceName = "nvkectl"
)

// OTLPConfig is where a run exports its telemetry over OTLP/gRPC.
type OTLPConfig struct {
	// Endpoint is the collector's host:port.
	Endpoint string
	// Insecure sends without TLS.
	Insecure bool
	// Headers are sent on every export request, for instance an
	// authorization token. They are never logged.
	Headers map[string]string
}

// OTLPConfigFromEnv reads OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_INSECURE
// and OTEL_EXPORTER_OTLP_HEADERS, and no other OTLP variable. The signal-specific
// ones, such as OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, are ignored so that one
// endpoint and one set of headers apply to everything nvkectl exports. An
// endpoint is a host:port or a URL; the http scheme implies Insecure, as does
// OTEL_EXPORTER_OTLP_INSECURE=true. It returns false when no endpoint is set.
func OTLPConfigFromEnv() (OTLPConfig, bool, error) {
	raw := os.Getenv(envEndpoint)
	if raw == "" {
		return OTLPConfig{}, false, nil
	}
	host, insecure, err := ParseEndpoint(raw)
	if err != nil {
		return OTLPConfig{}, false, err
	}
	if v, err := strconv.ParseBool(os.Getenv(envInsecure)); err == nil && v {
		insecure = true
	}
	return OTLPConfig{Endpoint: host, Insecure: insecure, Headers: HeadersFromEnv()}, true, nil
}

// ParseEndpoint splits an OTLP endpoint, given as a host:port or a URL, into
// the host:port to dial and whether it is sent without TLS, which is the case
// for the http scheme. It returns an error for an empty endpoint or a URL
// without a host, or with a host that cannot be put in a URL: an IPv6 address
// has to be in brackets when it has a port or is bare.
func ParseEndpoint(raw string) (host string, insecure bool, err error) {
	if raw == "" {
		return "", false, errors.New("invalid OTLP endpoint: it is empty")
	}
	if !strings.Contains(raw, "://") {
		bracketed := strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]")
		if _, _, splitErr := net.SplitHostPort(raw); strings.Contains(raw, ":") && !bracketed && splitErr != nil {
			return "", false, fmt.Errorf("invalid OTLP endpoint %q: want host:port or a URL, with an IPv6 address in brackets", raw)
		}
		return raw, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("invalid OTLP endpoint %q: want host:port or a URL", raw)
	}
	return u.Host, u.Scheme == "http", nil
}

// HeadersFromEnv reads the headers to send on every export request from
// OTEL_EXPORTER_OTLP_HEADERS.
func HeadersFromEnv() map[string]string {
	return parseHeaders(os.Getenv(envHeaders))
}

// RejectLoopback returns an error if endpoint, a host:port or a URL, names the
// local machine: localhost or a loopback address. Such an endpoint is
// unreachable from the temporary controller in the bootstrap cluster. The host
// is not resolved. The unspecified addresses 0.0.0.0 and :: are rejected too:
// they dial the local machine.
func RejectLoopback(endpoint string) error {
	hostport, _, err := ParseEndpoint(endpoint)
	if err != nil {
		return err
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if ip := net.ParseIP(host); isLocalName(host) || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return fmt.Errorf("OTLP endpoint %q is a loopback address, which the temporary controller in the bootstrap cluster cannot reach", endpoint)
	}
	return nil
}

// isLocalName reports whether host is localhost or a name under .localhost,
// which resolve to the local machine.
func isLocalName(host string) bool {
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// parseHeaders parses the "key=value,key=value" form of OTEL_EXPORTER_OTLP_HEADERS.
// Values are percent-decoded; an entry without "=" is ignored.
func parseHeaders(raw string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if decoded, err := url.PathUnescape(strings.TrimSpace(value)); err == nil {
			value = decoded
		}
		headers[key] = strings.TrimSpace(value)
	}
	return headers
}

// EnableOTLP makes the run export its log records and spans to cfg. It may be
// called before or after Start, but must be called before LogHandler and
// returns an error afterwards. Creating the exporters contacts nothing: an
// unreachable endpoint is reported by a single warning, never as an error.
func (run *Run) EnableOTLP(cfg OTLPConfig) error {
	if cfg.Endpoint == "" {
		return errors.New("OTLP endpoint is empty")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.handlerBuilt {
		return errors.New("OTLP export must be enabled before the log handler is built")
	}
	// The exporters fill every setting left unset from their own OTEL_EXPORTER_OTLP_*
	// variables, and an http scheme or OTEL_EXPORTER_OTLP_INSECURE there would
	// switch TLS off. Each one is set here, so the configuration is cfg alone.
	// An exporter also reports a malformed OTEL_EXPORTER_OTLP_HEADERS entry
	// through OpenTelemetry's internal logger, which prints the entry to stderr,
	// and that can be a token. The internal logger is silenced. Errors sent to
	// the global error handler lose their text too; the run warns once, later,
	// because this runs with run.mu held and before the warning's logger exists.
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { run.exporterMisconfigured.Store(true) }))
	logOpts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithTLSCredentials(transportCredentials(cfg)),
		otlploggrpc.WithHeaders(headersOrEmpty(cfg)),
		otlploggrpc.WithCompressor("none"),
		otlploggrpc.WithTimeout(flushTimeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	logExporter, err := otlploggrpc.New(context.Background(), logOpts...)
	if err != nil {
		return fmt.Errorf("creating the OTLP log exporter: %w", err)
	}
	processor, err := newSpanProcessor(cfg, run.warnExport)
	if err != nil {
		_ = logExporter.Shutdown(context.Background())
		return err
	}
	run.logProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(run.resource()),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			&guardedExporter{Exporter: logExporter, warn: run.warnExport},
			sdklog.WithExportTimeout(flushTimeout),
		)),
	)
	if run.tracerProvider != nil {
		// Start already ran; a span is handed to the processors registered when it ends.
		run.tracerProvider.RegisterSpanProcessor(processor)
	} else {
		run.providerOptions = append(run.providerOptions, sdktrace.WithSpanProcessor(processor))
	}
	run.otlpConfig, run.otlpEnabled = cfg, true
	return nil
}

// newSpanProcessor returns the processor that exports spans to cfg, with the
// endpoint, transport security and headers explicit for the reason EnableOTLP
// gives. Compression is left to the exporter: it has no way to say "none", and
// a compression variable changes nothing but the payload size.
func newSpanProcessor(cfg OTLPConfig, warn func(error)) (sdktrace.SpanProcessor, error) {
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithTLSCredentials(transportCredentials(cfg)),
		otlptracegrpc.WithHeaders(headersOrEmpty(cfg)),
		otlptracegrpc.WithTimeout(flushTimeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	exporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating the OTLP trace exporter: %w", err)
	}
	return sdktrace.NewBatchSpanProcessor(
		&guardedSpanExporter{SpanExporter: exporter, warn: warn},
		sdktrace.WithExportTimeout(flushTimeout),
	), nil
}

// transportCredentials returns TLS, or plaintext when cfg.Insecure is set.
func transportCredentials(cfg OTLPConfig) credentials.TransportCredentials {
	if cfg.Insecure {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}

// headersOrEmpty returns cfg.Headers, or an empty map, which keeps the exporter
// from reading OTEL_EXPORTER_OTLP_HEADERS itself.
func headersOrEmpty(cfg OTLPConfig) map[string]string {
	if cfg.Headers == nil {
		return map[string]string{}
	}
	return cfg.Headers
}

// OTLP returns the configuration EnableOTLP accepted, and false if it has not
// been called.
func (run *Run) OTLP() (OTLPConfig, bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.otlpConfig, run.otlpEnabled
}

// resource describes nvkectl to the backend, for every signal the run exports.
func (run *Run) resource() *resource.Resource {
	return resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", run.version),
	)
}

// otlpHandler returns the handler that sends records to the OTLP endpoint, or
// nil if EnableOTLP was not called.
func (run *Run) otlpHandler() slog.Handler {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.handlerBuilt = true
	if run.logProvider == nil {
		return nil
	}
	return otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(run.logProvider))
}

// shutdownProviders sends the buffered spans and records, waiting at most
// flushTimeout for both together, and releases the exporters. A failure is
// reported as the run's single export warning.
func (run *Run) shutdownProviders(tracerProvider *sdktrace.TracerProvider, logProvider *sdklog.LoggerProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	var wg sync.WaitGroup
	if tracerProvider != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tracerProvider.Shutdown(ctx); err != nil {
				run.warnExport(err)
			}
		}()
	}
	if logProvider != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := logProvider.Shutdown(ctx); err != nil {
				run.warnExport(err)
			}
		}()
	}
	wg.Wait()
	run.warnMisconfigured()
}

// warnMisconfigured reports the exporter configuration error the global error
// handler saw, if any, without its text.
func (run *Run) warnMisconfigured() {
	if run.exporterMisconfigured.Load() {
		run.warnExport(errors.New("OTLP exporter configuration error"))
	}
}

// warnExport reports, once per run, that telemetry could not be exported. The
// warning goes to the run's own logger and never to the endpoint that failed.
func (run *Run) warnExport(err error) {
	run.exportWarnOnce.Do(func() {
		run.mu.Lock()
		logger := run.warnLogger
		run.mu.Unlock()
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("could not export telemetry to the OTLP endpoint; the command is not affected", "error", err.Error())
	})
}

// guardedExporter keeps an export failure from reaching the OpenTelemetry error
// handler, which would print it for every batch. The failure is reported once
// through warn instead.
type guardedExporter struct {
	sdklog.Exporter
	warn func(error)
}

// Export implements sdklog.Exporter.
func (e *guardedExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := e.Exporter.Export(ctx, records); err != nil {
		e.warn(err)
	}
	return nil
}

// guardedSpanExporter is guardedExporter for spans.
type guardedSpanExporter struct {
	sdktrace.SpanExporter
	warn func(error)
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *guardedSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		e.warn(err)
	}
	return nil
}
