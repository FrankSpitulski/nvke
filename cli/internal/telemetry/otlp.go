// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	envEndpoint     = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envLogsEndpoint = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	envInsecure     = "OTEL_EXPORTER_OTLP_INSECURE"
	envHeaders      = "OTEL_EXPORTER_OTLP_HEADERS"
	envLogsHeaders  = "OTEL_EXPORTER_OTLP_LOGS_HEADERS"

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

// OTLPConfigFromEnv reads the standard OTEL_EXPORTER_OTLP_* variables. The
// signal-specific OTEL_EXPORTER_OTLP_LOGS_ENDPOINT and
// OTEL_EXPORTER_OTLP_LOGS_HEADERS take precedence over the general ones. An
// endpoint is a host:port or a URL; the http scheme implies Insecure, as does
// OTEL_EXPORTER_OTLP_INSECURE=true. It returns false when no endpoint is set.
func OTLPConfigFromEnv() (OTLPConfig, bool, error) {
	raw := firstNonEmpty(os.Getenv(envLogsEndpoint), os.Getenv(envEndpoint))
	if raw == "" {
		return OTLPConfig{}, false, nil
	}
	cfg := OTLPConfig{Endpoint: raw, Headers: parseHeaders(firstNonEmpty(os.Getenv(envLogsHeaders), os.Getenv(envHeaders)))}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return OTLPConfig{}, false, fmt.Errorf("invalid OTLP endpoint %q: want host:port or a URL", raw)
		}
		cfg.Endpoint = u.Host
		cfg.Insecure = u.Scheme == "http"
	}
	if v, err := strconv.ParseBool(os.Getenv(envInsecure)); err == nil && v {
		cfg.Insecure = true
	}
	return cfg, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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

// EnableOTLP makes the run export its log records to cfg. It must be called
// before LogHandler and returns an error afterwards. Creating the exporter
// contacts nothing: an unreachable endpoint is reported by a single warning,
// never as an error.
func (run *Run) EnableOTLP(cfg OTLPConfig) error {
	if cfg.Endpoint == "" {
		return errors.New("OTLP endpoint is empty")
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.handlerBuilt {
		return errors.New("OTLP export must be enabled before the log handler is built")
	}
	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithTimeout(flushTimeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 200 * time.Millisecond,
			MaxInterval:     500 * time.Millisecond,
			MaxElapsedTime:  flushTimeout / 2,
		}),
	}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}
	exporter, err := otlploggrpc.New(context.Background(), opts...)
	if err != nil {
		return fmt.Errorf("creating the OTLP log exporter: %w", err)
	}
	run.logProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(run.resource()),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(
			&guardedExporter{Exporter: exporter, warn: run.warnExport},
			sdklog.WithExportTimeout(flushTimeout),
		)),
	)
	return nil
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

// flushLogs sends the buffered records, waiting at most flushTimeout, and
// releases the exporter. A failure is reported as the run's single export
// warning.
func (run *Run) flushLogs() {
	run.mu.Lock()
	provider := run.logProvider
	run.logProvider = nil
	run.mu.Unlock()
	if provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		run.warnExport(err)
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
