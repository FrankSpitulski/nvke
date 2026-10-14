// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package server serves the same cluster service through gRPC and REST.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/limits"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

const (
	maxResponseBytes = 1 << 20
	drainTimeout     = 30 * time.Second
	connectionIdle   = 30 * time.Second
	maxConcurrent    = 64
)

// Addresses separates application listeners from unauthenticated process probes.
type Addresses struct {
	HTTP   string
	GRPC   string
	Health string
}

// Server owns independent REST, gRPC and health listeners without controller state.
type Server struct {
	addresses Addresses
	http      *http.Server
	grpc      *grpc.Server
	health    *http.Server
	draining  atomic.Bool
}

// New registers one tenant implementation on both transports. Ready checks storage.
func New(addresses Addresses, service clusterv1.ClusterServiceServer, ready func(context.Context) error) (*Server, error) {
	if service == nil || ready == nil {
		return nil, fmt.Errorf("cluster service and readiness check are required")
	}
	s := &Server{addresses: addresses}
	admission := make(chan struct{}, maxConcurrent)
	gateway := runtime.NewServeMux(
		runtime.WithDisableHTTPMethodOverride(),
		runtime.WithDisablePathLengthFallback(),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &jsonMarshaler{runtime.JSONPb{
			UseProtoNames: true, DiscardUnknown: false,
		}}),
	)
	if err := clusterv1.RegisterClusterServiceHandlerServer(context.Background(), gateway, service); err != nil {
		return nil, fmt.Errorf("register REST gateway: %w", err)
	}
	rpc := grpc.NewServer(
		grpc.MaxRecvMsgSize(limits.RequestBytes), grpc.MaxSendMsgSize(maxResponseBytes),
		grpc.MaxConcurrentStreams(16), grpc.MaxHeaderListSize(16<<10), grpc.ConnectionTimeout(5*time.Second),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: connectionIdle}),
		grpc.InTapHandle(func(ctx context.Context, _ *tap.Info) (context.Context, error) {
			if s.draining.Load() {
				return nil, status.Error(codes.Unavailable, "server is draining")
			}
			bounded, cancel := context.WithTimeout(ctx, limits.RequestTimeout)
			context.AfterFunc(bounded, cancel)
			return bounded, nil
		}),
		grpc.UnaryInterceptor(func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			select {
			case admission <- struct{}{}:
				defer func() { <-admission }()
			default:
				return nil, status.Error(codes.ResourceExhausted, "too many concurrent requests")
			}
			return handler(ctx, request)
		}),
	)
	clusterv1.RegisterClusterServiceServer(rpc, service)
	handler, err := boundary(gateway)
	if err != nil {
		return nil, err
	}
	probes := http.NewServeMux()
	probes.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	probes.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			http.Error(w, "server is draining", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), limits.ProbeTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "datastore unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.http = httpServer(limitHTTP(handler, admission, &s.draining))
	s.grpc = rpc
	s.health = httpServer(probes)
	return s, nil
}

func httpServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: limits.RequestTimeout,
		WriteTimeout: 10 * time.Second, IdleTimeout: connectionIdle, MaxHeaderBytes: 16 << 10,
	}
}

func limitHTTP(next http.Handler, admission chan struct{}, draining *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if draining.Load() {
			writeError(w, http.StatusServiceUnavailable, codes.Unavailable, "server is draining")
			return
		}
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			writeError(w, http.StatusTooManyRequests, codes.ResourceExhausted, "too many concurrent requests")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), limits.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Run binds all listeners before serving, then drains requests on cancellation.
func (s *Server) Run(ctx context.Context) error {
	addresses := []string{s.addresses.HTTP, s.addresses.GRPC, s.addresses.Health}
	listeners := make([]net.Listener, 0, len(addresses))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, address := range addresses {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("bind API listener: %w", err)
		}
		listeners = append(listeners, listener)
	}
	return s.Serve(ctx, listeners[0], listeners[1], listeners[2])
}

// Serve accepts prebound listeners, also allowing tests to use ephemeral ports.
func (s *Server) Serve(ctx context.Context, rest, rpc, health net.Listener) error {
	rest = netutil.LimitListener(rest, 32)
	rpc = netutil.LimitListener(rpc, 32)
	health = netutil.LimitListener(health, 8)
	failures := make(chan error, 3)
	go func() { failures <- s.http.Serve(rest) }()
	go func() { failures <- s.grpc.Serve(rpc) }()
	go func() { failures <- s.health.Serve(health) }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-failures:
	}
	s.shutdown()
	if result != nil && !errors.Is(result, http.ErrServerClosed) && !errors.Is(result, grpc.ErrServerStopped) {
		return fmt.Errorf("serve cluster API: %w", result)
	}
	return nil
}

func (s *Server) shutdown() {
	s.draining.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	var group sync.WaitGroup
	group.Go(func() {
		if err := s.http.Shutdown(ctx); err != nil {
			_ = s.http.Close()
		}
	})
	stopped := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		s.grpc.Stop()
	}
	group.Wait()
	if err := s.health.Shutdown(ctx); err != nil {
		_ = s.health.Close()
	}
}
