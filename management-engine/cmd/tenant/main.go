// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Tenant-api serves the isolated evaluation Cluster contract in a separate process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	tenantcluster "github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/config"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/server"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		slog.Error("Tenant API stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	var options config.Options
	var showVersion bool
	flags := flag.NewFlagSet("tenant-api", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.Kubeconfig, "kubeconfig", "", "Absolute path to the explicit datastore kubeconfig")
	flags.StringVar(&options.Namespace, "namespace", "", "Datastore namespace containing tenant clusters")
	flags.StringVar(&options.PageTokenKeyFile, "page-token-key-file", "", "Mounted stable 32-to-64-byte pagination key")
	flags.StringVar(&options.HTTPAddress, "http-address", "127.0.0.1:8080", "REST listener address")
	flags.StringVar(&options.GRPCAddress, "grpc-address", "127.0.0.1:9090", "gRPC listener address")
	flags.StringVar(&options.HealthAddress, "health-address", "127.0.0.1:8081", "Process probe listener address")
	flags.BoolVar(&options.EvaluationNoAuth, "evaluation-no-auth", false,
		"Allow unauthenticated access for isolated evaluation only")
	flags.BoolVar(&showVersion, "version", false, "Print version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if showVersion {
		_, err := fmt.Fprintln(output, version)
		return err
	}
	loaded, err := config.Load(options)
	if err != nil {
		return err
	}
	httpClient, err := rest.HTTPClientFor(loaded.Storage)
	if err != nil {
		return fmt.Errorf("construct datastore transport: %w", err)
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := dynamic.NewForConfigAndClient(loaded.Storage, httpClient)
	if err != nil {
		return fmt.Errorf("construct datastore client: %w", err)
	}
	resources, err := tenantcluster.New(client, tenantcluster.Config{
		Namespace:   loaded.Namespace,
		DatastoreID: loaded.DatastoreID, PageTokenKey: loaded.PageTokenKey,
	})
	if err != nil {
		return err
	}
	api, err := server.New(server.Addresses{
		HTTP: loaded.HTTPAddress, GRPC: loaded.GRPCAddress, Health: loaded.HealthAddress,
	}, resources, resources.Ready)
	if err != nil {
		return err
	}
	slog.Warn("Starting Tenant API for isolated evaluation", "authentication", "unavailable",
		"namespace", loaded.Namespace, "version", version)
	return api.Run(ctx)
}
