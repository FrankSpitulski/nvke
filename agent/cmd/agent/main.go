// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	server "github.com/NVIDIA/nke/agent/internal/agent"
	"github.com/NVIDIA/nke/agent/internal/grpcserver"
	"github.com/NVIDIA/nke/agent/internal/identity"
)

const (
	DefaultNodeServerListenPort = "15005"
)

// VERSION is set at build time via: -ldflags "-X main.VERSION=<version>"
// Keep it as variable or things are going to break
//
// "dev", not the zero value, so a build with no -X says so. Empty, it printed
// "NKE Agent Version: " and registered an empty version, which reads as a
// display bug instead of a missing stamp.
var VERSION = "dev"

// envOr returns the value of the named environment variable, or fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// AgentConfig centralizes configuration previously stored in globals.
// It is passed explicitly to functions instead of relying on package-level state.
// TODO:  May want to move this and define it in the agent package, and just
// pass the full config to NewAgent but for now beyond the scope of this commit :)
type AgentConfig struct {
	Version               string
	NodeServerListenPort  string
	MLX5Rescan            bool
	BundleServerURL       string
	IndexFileURL          string
	CniImageRegistry      string
	AuditLogger           *slog.Logger
	ScriptExecutionLogger *slog.Logger
	DisableMTLS           bool
	SPIFFESocketPath      string
}

func Bootstrap(kubeVersion string) error {
	versionResponse, err := GetVersionInfo()
	if err == nil {
		slog.Info("existing kubeadm version info", "version", versionResponse)
		return nil
	}

	if !strings.Contains(err.Error(), "not found") {
		slog.Error("kubeadm version not found")
		return err
	}

	slog.Info("executing /usr/local/nke/bin/bootstrap.sh", "kubeVersion", kubeVersion)
	cmd := exec.Command("/usr/local/nke/bin/bootstrap.sh", kubeVersion)

	output, err := cmd.CombinedOutput()
	if err != nil {
		bootstrapErr := fmt.Errorf("kubeadm bootstrap failed.  Error: %v, Output: %s", err, output)
		slog.Error("kubeadm bootstrap failed", "err", bootstrapErr)
		return bootstrapErr
	}

	// We need to rotate any state file that exists, since kubeadm not-found we start over...
	slog.Info("kubeadm installed successfully, node is initialized and ready to join a cluster.")
	return nil
}

func GetVersionInfo() (string, error) {
	// Make sure kubeadm exists...
	cmd := exec.Command("kubeadm", "version", "-o", "short")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "not found") {
			return "", errors.New("kubeadm not found")
		}
		return "", err
	}
	version := bytes.TrimSpace(out)
	return string(version), nil
}

// callerPathPrefixes are the SPIFFE ID paths permitted to drive this agent, matched
// as prefixes: "/nke-service/" does not admit "/nke-service-evil/". Both name the
// same callers -- NKE is migrating its IDs onto the tenant grammar, and agents have
// to accept the old form until the ClusterSPIFFEID template changes and the new one
// after. Drop "/nke-service/" once nothing issues it.
var callerPathPrefixes = []string{
	"/nke-service/",
	"/tenant/nke/service/",
}

// authorizeClient returns the matcher deciding which peer SPIFFE IDs may call this
// agent: same trust domain, and a path under callerPathPrefixes.
func authorizeClient(agentTrustDomain string) func(spiffeid.ID) error {
	return func(clientID spiffeid.ID) error {
		if clientID.TrustDomain().Name() != agentTrustDomain {
			return errors.New("unauthorized: trust domain mismatch")
		}
		for _, prefix := range callerPathPrefixes {
			if strings.HasPrefix(clientID.Path(), prefix) {
				return nil
			}
		}
		return fmt.Errorf("unauthorized: path %q does not start with one of %s",
			clientID.Path(), strings.Join(callerPathPrefixes, ", "))
	}
}

// buildSpiffeMtlsConfig waits for the Workload API to issue this agent an SVID
// and builds the listener's mTLS config around it.
//
// It returns an error only when ctx is done. A host whose SPIRE agent has not
// completed node attestation retries instead of failing, because attestation
// can finish long after the agent starts and exiting would only lose the
// diagnostics.
func buildSpiffeMtlsConfig(
	ctx context.Context,
	socketPath string,
	observe func(identity.Attempt),
) (*tls.Config, spiffeid.ID, error) {
	acquirer := &identity.Acquirer{
		NewSource: identity.WorkloadAPISource(socketPath),
		Observe:   observe,
	}

	source, id, err := acquirer.Acquire(ctx)
	if err != nil {
		return nil, spiffeid.ID{}, err
	}

	authorizer := tlsconfig.AdaptMatcher(authorizeClient(id.TrustDomain().Name()))

	return tlsconfig.MTLSServerConfig(source, source, authorizer), id, nil
}

// spiffeStatusObserver reports failed acquisition attempts into the node status
// file. Successful attempts are left alone: the ready state carries the SPIFFE
// ID, and only Acquire returns it.
func spiffeStatusObserver(agentServer *server.NodeAgentServer) func(identity.Attempt) {
	return func(attempt identity.Attempt) {
		if attempt.Err == nil {
			return
		}

		slog.Warn("waiting for a SPIFFE identity",
			"attempt", attempt.N,
			"retryIn", attempt.RetryIn,
			"err", attempt.Err,
		)
		agentServer.SetSPIFFEStatus(server.SPIFFEStatus{
			State:        server.SPIFFEStateWaiting,
			Attempts:     attempt.N,
			LastError:    attempt.Err.Error(),
			RetrySeconds: int(attempt.RetryIn.Seconds()),
		})
	}
}

// newAuditLogInterceptor returns an interceptor that logs audit events using the provided logger.
func newAuditLogInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		method *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		client := "Unavailable"
		errorResponse := "nil"
		clientAddress := "Unavailable"

		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			ua := md.Get("user-agent")
			if len(ua) > 0 {
				client = ua[0]
			}
		}

		m, err := handler(ctx, req)
		if err != nil {
			errorResponse = err.Error()
		}

		p, ok := peer.FromContext(ctx)
		if ok {
			clientAddress = p.Addr.String()
		}

		logger.Info(
			"grpc request received",
			slog.String("method", method.FullMethod),
			slog.String("error", errorResponse),
			slog.String("user-agent", client),
			slog.String("client-address", clientAddress),
		)
		return m, err
	}
}

func startSecureServer(ctx context.Context, cfg AgentConfig, agentServer *server.NodeAgentServer, auditLogger *slog.Logger) {
	serverOptions := []grpc.ServerOption{
		grpc.UnaryInterceptor(newAuditLogInterceptor(auditLogger)),
	}

	if cfg.DisableMTLS {
		agentServer.SetSPIFFEStatus(server.SPIFFEStatus{State: server.SPIFFEStateDisabled})
	} else {
		slog.Info("setting up SPIFFE mTLS for gRPC")
		agentServer.SetSPIFFEStatus(server.SPIFFEStatus{State: server.SPIFFEStateWaiting})

		tlsCfg, id, err := buildSpiffeMtlsConfig(ctx, cfg.SPIFFESocketPath, spiffeStatusObserver(agentServer))
		if err != nil {
			// Acquisition retries forever, so shutdown is the only way here.
			slog.Info("stopped waiting for a SPIFFE identity", "err", err)
			return
		}

		slog.Info("acquired a SPIFFE identity", "spiffeID", id.String())
		agentServer.SetSPIFFEStatus(server.SPIFFEStatus{
			State:    server.SPIFFEStateReady,
			SpiffeID: id.String(),
		})
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}

	s := grpc.NewServer(serverOptions...)

	grpcserver.Register(s, agentServer)
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", cfg.NodeServerListenPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	agentServer.NodeStatus.ListenStatus = "listening on port " + cfg.NodeServerListenPort
	slog.Info("persisting status with listenStatus update")
	agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
	agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
}

func initializeNodeServer(cfg AgentConfig) *server.NodeAgentServer {
	slog.Info("running initializeNodeServer with the following config",
		"CONFIG",
		cfg)
	agentServer := server.NewNodeAgentServer(cfg.MLX5Rescan, cfg.BundleServerURL, cfg.IndexFileURL, cfg.CniImageRegistry)
	agentServer.ScriptExecutionLogger = cfg.ScriptExecutionLogger
	agentServer.NodeStatus.ListenStatus = "pending"
	if err := agentServer.Persister.Persist(agentServer.NodeStatus); err != nil {
		slog.Error("failed to persist initial status", "err", err)
	}

	agentServer.NodeStatus.AgentVersion = cfg.Version
	return agentServer
}

func RunNodeStartup(cfg AgentConfig) (*server.NodeAgentServer, error) {
	slog.Info("running NodeStartup...")
	agentServer := initializeNodeServer(cfg)

	cmd := exec.Command("swapoff", "-a")
	_, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("failed to disable swap", "err", err)
	}
	return agentServer, nil
}

func main() {
	disableMLX5Rescan := flag.Bool("disable-mlx5-rescan", false, "Disable mlx5 rescan (disables the init routine mlx5 reset proc)")
	disableMTLS := flag.Bool("disable-mtls", false, "Disable mTLS / use plaintext gRPC (development only)")
	spiffeSocketPath := flag.String("spiffe-socket-path", "unix:///tmp/spire-agent/public/api.sock", "Path to the SPIFFE workload API socket (--disable-mtls ignores this)")
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	auditLogFileName := flag.String(
		"audit-log-file",
		"/var/log/nke-agent-audit.log",
		"The audit log file name",
	)
	secureServerPort := flag.String("secure-server-port", DefaultNodeServerListenPort, "The port for the gRPC server")
	bundleServerURL := flag.String(
		"bundle-server-url",
		envOr("NKE_BUNDLE_SERVER_URL", ""),
		"Optional bundle source URL for InstallBundle/InstallCollection(.json). Supports http://, https://, file://, or oci://. Also set via NKE_BUNDLE_SERVER_URL. Prefer apt repos when unset. Example: https://artifacts.example.com/ or oci://registry.example.com/nke/bundles",
	)
	indexFileURL := flag.String(
		"index-file-url",
		envOr("NKE_BUNDLE_INDEX_URL", ""),
		"Full URL to a custom index file. When empty, derived as <bundle-server-url>/nke_bundles/ubuntu_<version>_index.json. Also set via NKE_BUNDLE_INDEX_URL",
	)
	cniImageRegistry := flag.String(
		"cni-image-registry",
		envOr("NKE_CNI_IMAGE_REGISTRY", ""),
		"OCI registry prefix for CNI component images (e.g. registry.example.com/nke). When set, passed to the CNI helm chart as --set global.registry=<value>. Also set via NKE_CNI_IMAGE_REGISTRY",
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("NKE Agent Version: %s\n", VERSION)
		os.Exit(0)
	}

	auditFile, err := os.OpenFile(*auditLogFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open audit log file: %v", err)
	}
	scriptExecLogFile, err := os.OpenFile("/var/log/nke-execute-scripts.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("failed to open script execution log file: %v", err)
	}
	// Create audit logger, script execution logger, and default logger
	auditLogger := slog.New(slog.NewJSONHandler(auditFile, nil))
	scriptExecutionLogger := slog.New(slog.NewJSONHandler(scriptExecLogFile, nil))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Build config from flags and build-time vars
	cfg := AgentConfig{
		Version:               VERSION,
		NodeServerListenPort:  *secureServerPort,
		MLX5Rescan:            !*disableMLX5Rescan,
		BundleServerURL:       *bundleServerURL,
		IndexFileURL:          *indexFileURL,
		CniImageRegistry:      *cniImageRegistry,
		AuditLogger:           auditLogger,
		ScriptExecutionLogger: scriptExecutionLogger,
		DisableMTLS:           *disableMTLS,
		SPIFFESocketPath:      *spiffeSocketPath,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("initializing node agent and setting status")
	agentServer, err := RunNodeStartup(cfg)
	if err != nil {
		slog.Error("failed to run node startup", "err", err)
	} else {
		agentServer.Persister.Persist(agentServer.NodeStatus) //nolint:errcheck
	}
	go func() {
		slog.Info("starting server")
		startSecureServer(ctx, cfg, agentServer, cfg.AuditLogger)
	}()

	<-ctx.Done()
}
