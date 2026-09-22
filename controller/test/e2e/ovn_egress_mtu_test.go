// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/test/e2e/internal/kube"
)

const (
	ovnBridgeCIDREnv = "NICO_VMS_BRIDGE_CIDR"
	// ovnExpectedMTU is the public OVN v1.4.0 single-node-zone value:
	// https://raw.githubusercontent.com/ovn-kubernetes/ovn-kubernetes/v1.4.0/helm/ovn-kubernetes/values-single-node-zone.yaml
	ovnExpectedMTU        = 1400
	ovnHTTPEgressPort     = 18082
	ovnUDPPort            = 18081
	ovnHTTPProbeContainer = "http-probe"
	ovnUDPProbeContainer  = "udp-probe"
	ovnUDPServerContainer = "udp-echo"

	ovnUDPServerScript = `import socket
import sys

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind(("0.0.0.0", int(sys.argv[1])))
open("/tmp/ready", "w", encoding="utf-8").close()
while True:
    payload, peer = sock.recvfrom(2048)
    sock.sendto(payload, peer)
`

	// IP_PMTUDISC_DO requires Linux to set DF and return EMSGSIZE when a UDP
	// datagram exceeds the discovered path MTU. See
	// https://man7.org/linux/man-pages/man2/IP_MTU_DISCOVER.2const.html.
	ovnUDPProbeScript = `import errno
import socket
import sys

EXPECTED_MTU = %d
IP_MTU_DISCOVER = 10
IP_PMTUDISC_DO = 2
IP_MTU = 14
PAYLOAD = b"x" * 1372
TOO_LARGE = b"x" * 1373

def fail(message):
    with open("/dev/termination-log", "w", encoding="utf-8") as output:
        output.write(message)
    raise SystemExit(1)

def check(target, port):
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.IPPROTO_IP, IP_MTU_DISCOVER, IP_PMTUDISC_DO)
    sock.settimeout(5)
    sock.connect((target, int(port)))
    mtu = sock.getsockopt(socket.IPPROTO_IP, IP_MTU)
    if mtu != EXPECTED_MTU:
        fail(f"{target}:{port} IP_MTU={mtu}, want {EXPECTED_MTU}")
    # Linux can drop the first datagram while discovering the path MTU. Retry
    # only a timeout: a wrong MTU or any other socket error must fail the probe.
    for attempt in range(3):
        sent = sock.send(PAYLOAD)
        if sent != len(PAYLOAD):
            fail(f"{target}:{port} sent {sent} bytes, want {len(PAYLOAD)}")
        try:
            echoed = sock.recv(len(PAYLOAD) + 1)
            break
        except socket.timeout:
            if attempt == 2:
                fail(f"{target}:{port} timed out waiting for 1372-byte echo")
    if echoed != PAYLOAD:
        fail(f"{target}:{port} echo payload mismatch")
    try:
        sock.send(TOO_LARGE)
    except OSError as error:
        if error.errno != errno.EMSGSIZE:
            fail(f"{target}:{port} 1373-byte send error errno={error.errno}, want EMSGSIZE")
    else:
        fail(f"{target}:{port} 1373-byte send unexpectedly succeeded")

check(sys.argv[1], sys.argv[2])
check(sys.argv[3], sys.argv[4])
with open("/dev/termination-log", "w", encoding="utf-8") as output:
    output.write("UDP MTU boundary probes passed")
`
)

func TestOVNBridgeGatewayFromCIDR(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		cidr string
		want netip.Addr
		ok   bool
	}{
		{name: "network aligned", cidr: "172.30.0.0/24", want: netip.MustParseAddr("172.30.0.1"), ok: true},
		{name: "host bits are masked", cidr: "172.30.9.99/24", want: netip.MustParseAddr("172.30.9.1"), ok: true},
		{name: "ipv6", cidr: "fd00::/64"},
		{name: "host route", cidr: "172.30.0.1/32"},
		{name: "invalid", cidr: "not-a-cidr"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ovnBridgeGateway(test.cidr)
			if test.ok {
				if err != nil {
					t.Fatalf("ovnBridgeGateway(%q) error = %v", test.cidr, err)
				}
				if got != test.want {
					t.Fatalf("ovnBridgeGateway(%q) = %s, want %s", test.cidr, got, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ovnBridgeGateway(%q) error = nil, want error", test.cidr)
			}
		})
	}
}

func TestUDPEchoEndpointEchoesAndCloses(t *testing.T) {
	t.Parallel()

	echo, err := startUDPEchoEndpoint(netip.MustParseAddr("127.0.0.1"), 0)
	if err != nil {
		t.Fatalf("startUDPEchoEndpoint() error = %v", err)
	}

	conn, err := net.DialUDP("udp4", nil, echo.Addr())
	if err != nil {
		t.Fatalf("dial UDP echo endpoint: %v", err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set UDP deadline: %v", err)
	}
	if _, err = conn.Write([]byte("echo-me")); err != nil {
		t.Fatalf("write UDP echo request: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read UDP echo response: %v", err)
	}
	if got := string(buf[:n]); got != "echo-me" {
		t.Fatalf("UDP echo response = %q, want %q", got, "echo-me")
	}
	if _, err := startUDPEchoEndpoint(netip.MustParseAddr("127.0.0.1"), echo.Port()); err == nil {
		t.Fatal("startUDPEchoEndpoint() error = nil, want occupied-port error")
	}

	if err := echo.Close(); err != nil {
		t.Fatalf("close UDP echo endpoint: %v", err)
	}
}

func TestOVNProbePodUsesSchedulerPlacementAndMTUBoundaries(t *testing.T) {
	pod := ovnUDPProbePod("test-ns", "source", "worker-a", "172.30.0.1", 18081, "10.244.1.4", ovnUDPPort)
	if pod.Spec.NodeName != "" {
		t.Fatalf("pod.Spec.NodeName = %q, want scheduler placement", pod.Spec.NodeName)
	}
	if got := pod.Spec.NodeSelector[corev1.LabelHostname]; got != "worker-a" {
		t.Fatalf("hostname node selector = %q, want %q", got, "worker-a")
	}
	if got := pod.Spec.Containers[0].Image; got != kube.KubectlImage {
		t.Fatalf("probe image = %q, want %q", got, kube.KubectlImage)
	}
	script := pod.Spec.Containers[0].Args[0]
	for _, value := range []string{"IP_MTU_DISCOVER = 10", "IP_PMTUDISC_DO = 2", "IP_MTU = 14", "b\"x\" * 1372", "b\"x\" * 1373", "errno.EMSGSIZE"} {
		if !strings.Contains(script, value) {
			t.Errorf("UDP probe script does not contain %q", value)
		}
	}
}

func TestOVNBridgeGatewayFromEnvironmentRequiresValue(t *testing.T) {
	t.Setenv(ovnBridgeCIDREnv, "")
	if _, err := ovnBridgeGatewayFromEnvironment(); err == nil {
		t.Fatalf("ovnBridgeGatewayFromEnvironment() error = nil, want missing %s error", ovnBridgeCIDREnv)
	}
}

func TestOVNUDPProbeScriptBoundaries(t *testing.T) {
	for _, test := range []struct {
		name            string
		mode            string
		wantSuccess     bool
		wantTermination string
	}{
		{name: "accepts exact MTU payload and rejects next byte", wantSuccess: true, wantTermination: "UDP MTU boundary probes passed"},
		{name: "fails wrong path MTU", mode: "wrong-mtu", wantTermination: "IP_MTU=1399, want 1400"},
		{name: "fails oversized send that succeeds", mode: "oversize-success", wantTermination: "1373-byte send unexpectedly succeeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminationLog := t.TempDir() + "/termination-log"
			command := exec.CommandContext(t.Context(), "python3", "-c", ovnUDPProbeHarness)
			command.Env = append(os.Environ(),
				"OVN_UDP_PROBE_SCRIPT="+fmt.Sprintf(ovnUDPProbeScript, ovnExpectedMTU),
				"OVN_UDP_PROBE_MODE="+test.mode,
				"OVN_UDP_PROBE_TERMINATION_LOG="+terminationLog,
			)
			output, err := command.CombinedOutput()
			if test.wantSuccess && err != nil {
				t.Fatalf("running UDP probe harness: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("UDP probe harness succeeded, want failure\n%s", output)
			}
			contents, readErr := os.ReadFile(terminationLog)
			if readErr != nil {
				t.Fatalf("reading probe termination log: %v\n%s", readErr, output)
			}
			if got := string(contents); !strings.Contains(got, test.wantTermination) {
				t.Fatalf("termination log = %q, want substring %q", got, test.wantTermination)
			}
		})
	}
}

const ovnUDPProbeHarness = `import errno
import os
import sys
import types

mode = os.environ["OVN_UDP_PROBE_MODE"]
termination_log = os.environ["OVN_UDP_PROBE_TERMINATION_LOG"]
script = os.environ["OVN_UDP_PROBE_SCRIPT"].replace("/dev/termination-log", termination_log)

class Timeout(Exception):
    pass

class Socket:
    def setsockopt(self, *_):
        pass

    def settimeout(self, *_):
        pass

    def connect(self, *_):
        pass

    def getsockopt(self, *_):
        return 1399 if mode == "wrong-mtu" else 1400

    def send(self, payload):
        if len(payload) == 1373:
            if mode == "oversize-success":
                return len(payload)
            raise OSError(errno.EMSGSIZE, "Message too long")
        return len(payload)

    def recv(self, *_):
        return b"x" * 1372

socket = types.SimpleNamespace(
    AF_INET=2,
    SOCK_DGRAM=2,
    IPPROTO_IP=0,
    timeout=Timeout,
    socket=lambda *_: Socket(),
)
sys.modules["socket"] = socket
sys.argv = ["probe", "172.30.0.1", "18081", "10.244.1.4", "18081"]
exec(compile(script, "ovn_udp_probe.py", "exec"), {})
`

// validateOVNEgressAndMTU verifies the public OVN egress path without relying
// on arbitrary Internet access. It is wired only into the NICo VM OVN smoke
// lane, where the Linux runner owns the bridge gateway.
func validateOVNEgressAndMTU(test *tenantTest) {
	It("routes OVN egress and enforces the configured MTU", func(ctx context.Context) {
		gateway := expectSucceed(ovnBridgeGatewayFromEnvironment())
		tenantClient := expectSucceed(test.tenantClients.GetAdminClient(ctx, test.cluster.Name))
		nodes := expectSucceed(ovnEgressNodes(ctx, tenantClient))

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "e2e-ovn-egress-" + ovnRandomSuffix()}}
		Expect(tenantClient.Create(ctx, namespace)).To(Succeed())
		deferDeleteOVNEgressNamespace(ctx, tenantClient, namespace)

		nonce := ovnRandomSuffix()
		httpEndpoint := expectSucceed(startHTTPNonceEndpoint(ctx, gateway, ovnHTTPEgressPort, nonce))
		DeferCleanup(func() {
			Expect(httpEndpoint.Close()).To(Succeed())
		})
		udpEndpoint := expectSucceed(startUDPEchoEndpoint(gateway, ovnUDPPort))
		DeferCleanup(func() {
			Expect(udpEndpoint.Close()).To(Succeed())
		})

		By("Starting UDP echo pods on two distinct Ready tenant nodes")
		firstServer := ovnUDPEchoPod(namespace.Name, "first", nodes[0].hostname, ovnUDPPort)
		secondServer := ovnUDPEchoPod(namespace.Name, "second", nodes[1].hostname, ovnUDPPort)
		for _, server := range []*corev1.Pod{firstServer, secondServer} {
			Expect(tenantClient.Create(ctx, server)).To(Succeed())
		}
		firstServer = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(firstServer))
		secondServer = waitForPodReady(ctx, tenantClient, client.ObjectKeyFromObject(secondServer))
		expectOVNPodOnNode(firstServer, nodes[0].name)
		expectOVNPodOnNode(secondServer, nodes[1].name)

		By("Requesting a runner-bound HTTP nonce through OVN egress")
		httpProbe := ovnHTTPProbePod(namespace.Name, nodes[0].hostname, gateway, httpEndpoint.Port(), nonce)
		finishedHTTPProbe, message, err := kube.RunPod(ctx, tenantClient, httpProbe, ovnHTTPProbeContainer, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(Equal("HTTP egress nonce matched"))
		expectOVNPodOnNode(finishedHTTPProbe, nodes[0].name)
		httpSource := expectSucceed(httpEndpoint.WaitSource(ctx))
		test.log.Info("Observed OVN HTTP egress request at bridge gateway", "source", httpSource)

		By("Probing egress and cross-node UDP MTU from the first tenant node")
		firstProbe := ovnUDPProbePod(namespace.Name, "first", nodes[0].hostname,
			gateway.String(), udpEndpoint.Port(), secondServer.Status.PodIP, ovnUDPPort)
		finishedFirstProbe, message, err := kube.RunPod(ctx, tenantClient, firstProbe, ovnUDPProbeContainer, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(Equal("UDP MTU boundary probes passed"))
		expectOVNPodOnNode(finishedFirstProbe, nodes[0].name)
		udpSource := expectSucceed(udpEndpoint.WaitSource(ctx))
		test.log.Info("Observed OVN UDP egress request at bridge gateway", "source", udpSource)

		By("Probing egress and reverse cross-node UDP MTU from the second tenant node")
		secondProbe := ovnUDPProbePod(namespace.Name, "second", nodes[1].hostname,
			gateway.String(), udpEndpoint.Port(), firstServer.Status.PodIP, ovnUDPPort)
		finishedSecondProbe, message, err := kube.RunPod(ctx, tenantClient, secondProbe, ovnUDPProbeContainer, 2*time.Minute)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(Equal("UDP MTU boundary probes passed"))
		expectOVNPodOnNode(finishedSecondProbe, nodes[1].name)
		udpSource = expectSucceed(udpEndpoint.WaitSource(ctx))
		test.log.Info("Observed OVN UDP egress request at bridge gateway", "source", udpSource)
	}, Label("ovn-egress", "ovn-mtu"), SpecTimeout(15*time.Minute))
}

type ovnEgressNode struct {
	name, hostname string
}

func ovnEgressNodes(ctx context.Context, tenantClient client.Client) ([]ovnEgressNode, error) {
	var nodes corev1.NodeList
	if err := tenantClient.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("listing tenant nodes for OVN egress validation: %w", err)
	}
	ready := make([]ovnEgressNode, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		isReady, _ := kubernetesNodeReady(node)
		if !isReady {
			continue
		}
		hostname := node.Labels[corev1.LabelHostname]
		if hostname == "" {
			return nil, fmt.Errorf("Ready tenant node %q is missing %q", node.Name, corev1.LabelHostname)
		}
		ready = append(ready, ovnEgressNode{name: node.Name, hostname: hostname})
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].name < ready[j].name })
	if len(ready) < 2 {
		return nil, fmt.Errorf("need at least two Ready tenant nodes for bidirectional OVN egress validation, found %d", len(ready))
	}
	return ready[:2], nil
}

func ovnBridgeGatewayFromEnvironment() (netip.Addr, error) {
	value := strings.TrimSpace(os.Getenv(ovnBridgeCIDREnv))
	if value == "" {
		return netip.Addr{}, fmt.Errorf("%s is required for OVN egress validation", ovnBridgeCIDREnv)
	}
	return ovnBridgeGateway(value)
}

func ovnBridgeGateway(cidr string) (netip.Addr, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parsing bridge CIDR %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("bridge CIDR %q is not IPv4", cidr)
	}
	prefix = prefix.Masked()
	if prefix.Bits() == 32 {
		return netip.Addr{}, fmt.Errorf("bridge CIDR %q has no gateway address", cidr)
	}
	gateway := prefix.Addr().Next()
	if !gateway.IsValid() || !prefix.Contains(gateway) {
		return netip.Addr{}, fmt.Errorf("bridge CIDR %q has no usable gateway address", cidr)
	}
	return gateway, nil
}

func ovnRandomSuffix() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("reading random OVN egress suffix: %v", err))
	}
	return hex.EncodeToString(value)
}

func ovnHTTPProbePod(namespace, nodeHostname string, gateway netip.Addr, port int, nonce string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-http-probe-", Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  map[string]string{corev1.LabelHostname: nodeHostname},
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{{
				Name:            ovnHTTPProbeContainer,
				Image:           kube.KubectlImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-ec"},
				Args: []string{`body=$(curl --fail --silent --show-error --max-time 5 "$1")
[ "$body" = "$2" ] || { printf 'HTTP egress response = %s, want nonce\n' "$body" > /dev/termination-log; exit 1; }
printf 'HTTP egress nonce matched' > /dev/termination-log`, "probe", "http://" + net.JoinHostPort(gateway.String(), strconv.Itoa(port)), nonce},
			}},
		},
	}
}

func ovnUDPEchoPod(namespace, role, nodeHostname string, port int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-udp-echo-" + role + "-", Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			NodeSelector:  map[string]string{corev1.LabelHostname: nodeHostname},
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{{
				Name:            ovnUDPServerContainer,
				Image:           kube.KubectlImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"python3", "-c", ovnUDPServerScript, strconv.Itoa(port)},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "test -f /tmp/ready"}}},
					PeriodSeconds:    1,
					FailureThreshold: 10,
				},
			}},
		},
	}
}

func ovnUDPProbePod(namespace, role, nodeHostname, gateway string, gatewayPort int, peer string, peerPort int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "ovn-udp-probe-" + role + "-", Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeSelector:  map[string]string{corev1.LabelHostname: nodeHostname},
			Tolerations:   podCIDRTolerations(),
			Containers: []corev1.Container{{
				Name:            ovnUDPProbeContainer,
				Image:           kube.KubectlImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"python3", "-c"},
				Args:            []string{fmt.Sprintf(ovnUDPProbeScript, ovnExpectedMTU), gateway, strconv.Itoa(gatewayPort), peer, strconv.Itoa(peerPort)},
			}},
		},
	}
}

func expectOVNPodOnNode(pod *corev1.Pod, nodeName string) {
	GinkgoHelper()
	Expect(pod.Spec.NodeName).To(Equal(nodeName), "pod %s should be scheduled on %q", client.ObjectKeyFromObject(pod), nodeName)
}

func deferDeleteOVNEgressNamespace(ctx context.Context, tenantClient client.Client, namespace *corev1.Namespace) {
	GinkgoHelper()
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = tenantClient.Delete(cleanupCtx, namespace)
	})
}

type httpNonceEndpoint struct {
	server  *httptest.Server
	sources chan netip.AddrPort
}

func startHTTPNonceEndpoint(ctx context.Context, address netip.Addr, port int, nonce string) (*httpNonceEndpoint, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", net.JoinHostPort(address.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listening for OVN HTTP egress on %s: %w", address, err)
	}
	endpoint := &httpNonceEndpoint{
		sources: make(chan netip.AddrPort, 4),
	}
	endpoint.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if source, err := netip.ParseAddrPort(request.RemoteAddr); err == nil {
			select {
			case endpoint.sources <- source:
			default:
			}
		}
		_, _ = writer.Write([]byte(nonce))
	}))
	endpoint.server.Listener = listener
	endpoint.server.Start()
	return endpoint, nil
}

func (endpoint *httpNonceEndpoint) Port() int {
	return endpoint.server.Listener.Addr().(*net.TCPAddr).Port
}

func (endpoint *httpNonceEndpoint) WaitSource(ctx context.Context) (netip.AddrPort, error) {
	select {
	case source := <-endpoint.sources:
		return source, nil
	case <-ctx.Done():
		return netip.AddrPort{}, fmt.Errorf("waiting for OVN HTTP egress source: %w", ctx.Err())
	}
}

func (endpoint *httpNonceEndpoint) Close() error {
	endpoint.server.Close()
	return nil
}

type udpEchoEndpoint struct {
	conn    *net.UDPConn
	sources chan netip.AddrPort
	done    chan error
}

func startUDPEchoEndpoint(address netip.Addr, port int) (*udpEchoEndpoint, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP(address.AsSlice()), Port: port})
	if err != nil {
		return nil, fmt.Errorf("listening for OVN UDP egress on %s: %w", address, err)
	}
	endpoint := &udpEchoEndpoint{
		conn:    conn,
		sources: make(chan netip.AddrPort, 8),
		done:    make(chan error, 1),
	}
	go endpoint.serve()
	return endpoint, nil
}

func (endpoint *udpEchoEndpoint) Addr() *net.UDPAddr {
	return endpoint.conn.LocalAddr().(*net.UDPAddr)
}

func (endpoint *udpEchoEndpoint) Port() int {
	return endpoint.Addr().Port
}

func (endpoint *udpEchoEndpoint) WaitSource(ctx context.Context) (netip.AddrPort, error) {
	select {
	case source := <-endpoint.sources:
		return source, nil
	case <-ctx.Done():
		return netip.AddrPort{}, fmt.Errorf("waiting for OVN UDP egress source: %w", ctx.Err())
	}
}

func (endpoint *udpEchoEndpoint) Close() error {
	closeErr := endpoint.conn.Close()
	if serveErr := <-endpoint.done; serveErr != nil && closeErr == nil {
		return serveErr
	}
	return closeErr
}

func (endpoint *udpEchoEndpoint) serve() {
	buffer := make([]byte, 2048)
	for {
		n, source, err := endpoint.conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				endpoint.done <- nil
				return
			}
			endpoint.done <- err
			return
		}
		select {
		case endpoint.sources <- source:
		default:
		}
		if _, err := endpoint.conn.WriteToUDPAddrPort(buffer[:n], source); err != nil {
			if errors.Is(err, net.ErrClosed) {
				endpoint.done <- nil
				return
			}
			endpoint.done <- err
			return
		}
	}
}
