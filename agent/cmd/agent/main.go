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
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	server "github.com/NVIDIA/nke/agent/internal/agent"
	"github.com/NVIDIA/nke/agent/internal/attest"
	"github.com/NVIDIA/nke/agent/internal/grpcserver"
)

const (
	DefaultNodeServerListenPort = "15005"

	// identityModeSPIFFE is the default: X.509 mTLS from the Workload API
	// (TPM node attestation lives on the SPIRE agent).
	identityModeSPIFFE = "spiffe"
	// identityModeControllerTLS is opt-in: JWT-SVID from the Workload API
	// (DPU-attested SPIRE agent), exchanged at the controller for a server
	// TLS certificate. gRPC is server-TLS only, not mTLS.
	identityModeControllerTLS = "controller-tls"
	// identityModePlaintext is development-only unauthenticated gRPC.
	identityModePlaintext = "plaintext"
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
	IdentityMode          string
	SPIFFESocketPath      string
	JWTAudience           string
	// ControllerEndpoint is required for identity-mode=controller-tls.
	ControllerEndpoint   string
	ControllerAttestPath string
	// StubAttestJWT, when set with controller-tls, skips the Workload API
	// (tests / bring-up only). Production leaves this empty.
	StubAttestJWT string
	// ServerTLS is the controller-issued server certificate config for
	// controller-tls. Nil for other modes.
	ServerTLS *tls.Config
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

func buildSpiffeMtlsConfig(ctx context.Context, socketPath string) (*tls.Config, error) {
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	if err != nil {
		return nil, fmt.Errorf("failed to create x509 source: %w", err)
	}

	svid, err := source.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("failed to get x509 SVID: %w", err)
	}

	agentTrustDomain := svid.ID.TrustDomain().Name()

	authorizer := tlsconfig.AdaptMatcher(authorizeClient(agentTrustDomain))

	return tlsconfig.MTLSServerConfig(source, source, authorizer), nil
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

	switch cfg.IdentityMode {
	case identityModePlaintext:
		slog.Warn("gRPC plaintext; development only")
	case identityModeControllerTLS:
		if cfg.ServerTLS == nil {
			log.Fatalf("identity-mode controller-tls requires a server certificate from the controller")
		}
		slog.Info("setting up controller-issued server TLS for gRPC (no client certificates)")
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(cfg.ServerTLS)))
	default:
		slog.Info("setting up SPIFFE mTLS for gRPC")
		tlsCfg, err := buildSpiffeMtlsConfig(ctx, cfg.SPIFFESocketPath)
		if err != nil {
			log.Fatalf("failed to build SPIFFE mTLS config: %v", err)
		}
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
	logCfg := cfg
	logCfg.ServerTLS = nil
	if logCfg.StubAttestJWT != "" {
		logCfg.StubAttestJWT = "(set)"
	}
	slog.Info("running initializeNodeServer with the following config",
		"CONFIG",
		logCfg)
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

func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func resolveIdentityMode(mode string, modeSet, disableMTLS bool) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = identityModeSPIFFE
	}
	switch mode {
	case identityModeSPIFFE, identityModeControllerTLS, identityModePlaintext:
	default:
		return "", fmt.Errorf("unknown identity-mode %q (want spiffe, controller-tls, or plaintext)", mode)
	}
	if disableMTLS {
		if modeSet && mode != identityModePlaintext {
			return "", fmt.Errorf("--disable-mtls conflicts with --identity-mode=%s", mode)
		}
		return identityModePlaintext, nil
	}
	return mode, nil
}

func serverTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.NoClientCert,
	}
}

func redactAttestBody(s string) string {
	if strings.Contains(s, "PRIVATE KEY") {
		return "(redacted: contains private key material)"
	}
	return s
}

func controllerTLSAttestor(cfg AgentConfig) attest.Attestor {
	if stub := strings.TrimSpace(cfg.StubAttestJWT); stub != "" {
		slog.Warn("using --stub-attest-jwt; skipping SPIRE workload API JWT-SVID")
		return attest.StubAttestor{JWTValue: stub}
	}
	audience := strings.TrimSpace(cfg.JWTAudience)
	if audience == "" {
		audience = strings.TrimSpace(cfg.ControllerEndpoint)
	}
	return attest.WorkloadAPIAttestor{
		SocketPath: cfg.SPIFFESocketPath,
		Audience:   audience,
	}
}

// maybeControllerAttest runs only for identity-mode=controller-tls: JWT-SVID
// (or a test stub JWT) is POSTed to the controller; a 2xx body must be a
// TLS certificate + key used as gRPC server TLS (not mTLS).
func maybeControllerAttest(ctx context.Context, cfg AgentConfig) (*tls.Config, error) {
	if cfg.IdentityMode != identityModeControllerTLS {
		return nil, nil
	}
	if strings.TrimSpace(cfg.ControllerEndpoint) == "" {
		return nil, errors.New("identity-mode controller-tls requires --controller-endpoint")
	}

	nodeName, err := os.Hostname()
	if err != nil {
		slog.Warn("hostname unavailable for attest request", "err", err)
		nodeName = ""
	}

	path := cfg.ControllerAttestPath
	if strings.TrimSpace(path) == "" {
		path = attest.DefaultPath
	}
	slog.Info("offering jwt-svid to controller for server tls certificate",
		"endpoint", cfg.ControllerEndpoint,
		"path", path,
		"nodeName", nodeName,
	)

	client := &attest.Client{
		Endpoint: cfg.ControllerEndpoint,
		Path:     cfg.ControllerAttestPath,
		Attestor: controllerTLSAttestor(cfg),
	}

	result, err := client.Attest(ctx, attest.Request{NodeName: nodeName})
	if err != nil {
		if result.StatusCode != 0 {
			slog.Error("controller attest failed",
				"url", result.URL,
				"status", result.StatusCode,
				"body", redactAttestBody(result.Body),
				"err", err,
			)
		}
		return nil, err
	}
	if result.Certificate == nil {
		return nil, errors.New("controller attest response missing tls certificate")
	}

	slog.Info("controller issued server tls certificate",
		"url", result.URL,
		"status", result.StatusCode,
	)
	return serverTLSConfig(*result.Certificate), nil
}

func main() {
	disableMLX5Rescan := flag.Bool("disable-mlx5-rescan", false, "Disable mlx5 rescan (disables the init routine mlx5 reset proc)")
	disableMTLS := flag.Bool("disable-mtls", false, "Deprecated alias for --identity-mode=plaintext (development only)")
	identityModeFlag := flag.String(
		"identity-mode",
		identityModeSPIFFE,
		"Identity and gRPC transport: spiffe (default, X.509 mTLS via Workload API), controller-tls (JWT-SVID via Workload API; controller issues server TLS cert; not mTLS), plaintext (development). Also NKE_IDENTITY_MODE",
	)
	spiffeSocketPath := flag.String("spiffe-socket-path", "unix:///tmp/spire-agent/public/api.sock", "Path to the SPIFFE workload API socket (used by spiffe and controller-tls; ignored for plaintext)")
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
	controllerEndpoint := flag.String(
		"controller-endpoint",
		envOr("NKE_CONTROLLER_ENDPOINT", ""),
		"Absolute controller URL for --identity-mode=controller-tls (JWT exchange). Ignored in other modes. Also NKE_CONTROLLER_ENDPOINT",
	)
	controllerAttestPath := flag.String(
		"controller-attest-path",
		attest.DefaultPath,
		"Path on --controller-endpoint for the attest POST. Not a CWE API",
	)
	jwtAudience := flag.String(
		"jwt-audience",
		envOr("NKE_JWT_AUDIENCE", ""),
		"JWT-SVID audience for --identity-mode=controller-tls. Defaults to --controller-endpoint when empty. Also NKE_JWT_AUDIENCE",
	)
	stubAttestJWT := flag.String(
		"stub-attest-jwt",
		envOr("NKE_STUB_ATTEST_JWT", ""),
		"Test-only JWT for controller-tls; skips the Workload API when set. Also NKE_STUB_ATTEST_JWT",
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("NKE Agent Version: %s\n", VERSION)
		os.Exit(0)
	}

	mode := *identityModeFlag
	modeSet := flagWasSet("identity-mode")
	if !modeSet {
		if v := strings.TrimSpace(os.Getenv("NKE_IDENTITY_MODE")); v != "" {
			mode = v
			modeSet = true
		}
	}
	identityMode, err := resolveIdentityMode(mode, modeSet, *disableMTLS)
	if err != nil {
		log.Fatalf("%v", err)
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
		DisableMTLS:           identityMode == identityModePlaintext,
		IdentityMode:          identityMode,
		SPIFFESocketPath:      *spiffeSocketPath,
		JWTAudience:           *jwtAudience,
		ControllerEndpoint:    *controllerEndpoint,
		ControllerAttestPath:  *controllerAttestPath,
		StubAttestJWT:         *stubAttestJWT,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverTLS, err := maybeControllerAttest(ctx, cfg)
	if err != nil {
		log.Fatalf("controller attest: %v", err)
	}
	cfg.ServerTLS = serverTLS

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
