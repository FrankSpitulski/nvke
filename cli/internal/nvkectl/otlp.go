// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dsx-ai-factory/nvke/cli/internal/telemetry"
)

const (
	otlpEndpointFlag = "otlp-endpoint"
	otlpInsecureFlag = "otlp-insecure"
)

// otlpSource reads one source of OTLP configuration. ok is false when the
// source names no endpoint. strict is true when an error should fail the
// command, because the operator asked for the export explicitly.
type otlpSource func(cmd *cobra.Command) (cfg telemetry.OTLPConfig, ok, strict bool, err error)

// otlpSources are the places OTLP configuration comes from, in order of
// precedence: the first that names an endpoint wins.
var otlpSources = []otlpSource{otlpFromFlags, otlpFromEnv} //nolint:gochecknoglobals // ordered lookup table

// resolveOTLP returns the OTLP configuration for the command about to run, from
// the first source that names an endpoint. strict is true when that endpoint,
// or an error about it, came from a flag, so the caller fails the command
// instead of continuing without export.
func resolveOTLP(cmd *cobra.Command) (cfg telemetry.OTLPConfig, ok, strict bool, err error) {
	for _, source := range otlpSources {
		cfg, ok, strict, err = source(cmd)
		if ok || err != nil {
			return cfg, ok, strict, err
		}
	}
	return telemetry.OTLPConfig{}, false, false, nil
}

// otlpFromFlags reads --otlp-endpoint and --otlp-insecure, which only
// "bootstrap site" registers. The endpoint must be reachable from the temporary
// controller in the bootstrap cluster, so a loopback address is rejected.
func otlpFromFlags(cmd *cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	endpointFlag := cmd.Flags().Lookup(otlpEndpointFlag)
	if endpointFlag == nil {
		return telemetry.OTLPConfig{}, false, false, nil
	}
	insecureFlag := cmd.Flags().Lookup(otlpInsecureFlag)
	if !endpointFlag.Changed {
		if insecureFlag != nil && insecureFlag.Changed {
			return telemetry.OTLPConfig{}, false, true, errors.New("--" + otlpInsecureFlag + " applies to --" + otlpEndpointFlag + "; pass --" +
				otlpEndpointFlag + " too, or drop --" + otlpInsecureFlag)
		}
		return telemetry.OTLPConfig{}, false, false, nil
	}
	endpoint := endpointFlag.Value.String()
	if err := telemetry.RejectLoopback(endpoint); err != nil {
		return telemetry.OTLPConfig{}, false, true, fmt.Errorf("--%s: %w", otlpEndpointFlag, err)
	}
	host, insecure, err := telemetry.ParseEndpoint(endpoint)
	if err != nil {
		return telemetry.OTLPConfig{}, false, true, fmt.Errorf("--%s: %w", otlpEndpointFlag, err)
	}
	if flagInsecure, _ := cmd.Flags().GetBool(otlpInsecureFlag); flagInsecure {
		insecure = true
	}
	cfg := telemetry.OTLPConfig{Endpoint: host, Insecure: insecure, Headers: telemetry.HeadersFromEnv()}
	return cfg, true, true, nil
}

// otlpFromEnv reads the standard OTEL_EXPORTER_OTLP_* variables. A problem with
// them never fails the command.
func otlpFromEnv(*cobra.Command) (telemetry.OTLPConfig, bool, bool, error) {
	cfg, ok, err := telemetry.OTLPConfigFromEnv()
	return cfg, ok, false, err
}

// enableOTLP turns on OTLP export for run from the resolved configuration. It
// does nothing for a nil run. The returned error is fatal only when fatal is
// true; otherwise the caller reports it and carries on.
func enableOTLP(cmd *cobra.Command, run *telemetry.Run) (fatal bool, err error) {
	if run == nil {
		return false, nil
	}
	cfg, ok, strict, err := resolveOTLP(cmd)
	if err != nil || !ok {
		return strict, err
	}
	return strict, run.EnableOTLP(cfg)
}
