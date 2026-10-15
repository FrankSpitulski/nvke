// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/limits"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

type testService struct {
	clusterv1.UnimplementedClusterServiceServer
	calls   atomic.Int32
	bounded atomic.Bool
	observe func(proto.Message)
	rpc     atomic.Bool
}

func (s *testService) record(request proto.Message) {
	s.calls.Add(1)
	if s.observe != nil {
		s.observe(request)
	}
}

func (s *testService) CreateCluster(ctx context.Context, request *clusterv1.CreateClusterRequest) (*clusterv1.Cluster, error) {
	s.record(request)
	deadline, ok := ctx.Deadline()
	s.bounded.Store(ok && time.Until(deadline) <= limits.RequestTimeout)
	_, fromRPC := peer.FromContext(ctx)
	s.rpc.Store(fromRPC)
	return &clusterv1.Cluster{Name: request.GetName(), ResourceVersion: "12"}, nil
}

func (s *testService) ListClusters(_ context.Context, request *clusterv1.ListClustersRequest) (*clusterv1.ListClustersResponse, error) {
	s.record(request)
	return &clusterv1.ListClustersResponse{NextPageToken: request.GetPageToken()}, nil
}

func (s *testService) GetCluster(_ context.Context, request *clusterv1.GetClusterRequest) (*clusterv1.Cluster, error) {
	s.record(request)
	return &clusterv1.Cluster{Name: request.GetName()}, nil
}

func (s *testService) ReplaceCluster(_ context.Context, request *clusterv1.ReplaceClusterRequest) (*clusterv1.Cluster, error) {
	s.record(request)
	return &clusterv1.Cluster{Name: request.GetName(), ResourceVersion: request.GetResourceVersion()}, nil
}

func TestGatewayRejectsAmbiguousAndUnboundedRequests(t *testing.T) {
	for name, test := range map[string]struct {
		method      string
		path        string
		body        string
		contentType string
		encoding    string
		override    string
		readError   error
		wantHTTP    int
		wantCode    codes.Code
	}{
		"duplicate key":    {body: `{"name":"sample","name":"other"}`},
		"nested duplicate": {body: `{"name":"sample","spec":{"control_plane_class":"a","control_plane_class":"b"}}`},
		"unknown field":    {body: `{"name":"sample","secret":"value"}`},
		"camel alias":      {body: `{"name":"sample","spec":{"controlPlaneClass":"a"}}`},
		"explicit null":    {body: `{"name":"sample","spec":null}`},
		"scalar null":      {body: `{"name":null}`},
		"nested null":      {body: `{"spec":{"cluster_network":{"pods":{"cidr_blocks":[null]}}}}`},
		"trailing JSON":    {body: `{"name":"sample"}{}`},
		"trailing scalar":  {body: `{"name":"sample"}false`},
		"empty body":       {},
		"removed org":      {body: `{"name":"sample","org":"local"}`},
		"project body":     {body: `{"name":"sample","project":"example"}`},
		"name override":    {method: "PUT", path: "/v1/clusters/sample", body: `{"name":"other"}`},
		"empty name":       {method: "PUT", path: "/v1/clusters/sample", body: `{"name":""}`},
		"late override":    {method: "PUT", path: "/v1/clusters/sample", body: `{"spec":{"release":{"channel":{"name":"stable"}}},"name":"other"}`},
		"form":             {body: "name=sample", contentType: "application/x-www-form-urlencoded", wantHTTP: http.StatusUnsupportedMediaType},
		"compression":      {body: `{"name":"sample"}`, encoding: "gzip", wantHTTP: http.StatusUnsupportedMediaType},
		"method override":  {body: `{"name":"sample"}`, override: "GET"},
		"huge body": {
			body: strings.Repeat(" ", limits.RequestBytes+1), wantHTTP: http.StatusRequestEntityTooLarge, wantCode: codes.ResourceExhausted,
		},
		"GET huge body": {
			method: "GET", body: strings.Repeat(" ", limits.RequestBytes+1), wantHTTP: http.StatusRequestEntityTooLarge, wantCode: codes.ResourceExhausted,
		},
		"truncated body": {readError: io.ErrUnexpectedEOF},
		"malformed body": {readError: errors.New("private-body-fragment")},
		"body timeout": {
			readError: &net.OpError{Op: "read", Err: context.DeadlineExceeded}, wantHTTP: http.StatusRequestTimeout, wantCode: codes.DeadlineExceeded,
		},
		"POST query":           {path: "/v1/clusters?org=other", body: `{"name":"sample"}`},
		"org query":            {method: "GET", path: "/v1/clusters?org=local"},
		"project query":        {method: "GET", path: "/v1/clusters?project=example"},
		"query alias":          {method: "GET", path: "/v1/clusters?pageSize=1"},
		"duplicate query":      {method: "GET", path: "/v1/clusters?page_size=1&page_size=2"},
		"bound query":          {method: "GET", path: "/v1/clusters/sample?name=other"},
		"GET body":             {method: "GET", body: `{}`},
		"path encoding":        {path: "/v1/clusters/sam%70le", body: `{"name":"sample"}`},
		"org collection route": {method: "GET", path: "/v1/orgs/local/clusters", wantHTTP: http.StatusNotFound, wantCode: codes.NotFound},
		"org resource route":   {method: "GET", path: "/v1/orgs/local/clusters/sample", wantHTTP: http.StatusNotFound, wantCode: codes.NotFound},
		"project route":        {method: "GET", path: "/v1/projects/example/clusters", wantHTTP: http.StatusNotFound, wantCode: codes.NotFound},
	} {
		t.Run(name, func(t *testing.T) {
			service := &testService{}
			server, err := New(Addresses{}, service, func(context.Context) error { return nil })
			require.NoError(t, err)
			startGateway(t, server)
			if test.method == "" {
				test.method = http.MethodPost
			}
			if test.path == "" {
				test.path = "/v1/clusters"
			}
			if test.contentType == "" {
				test.contentType = "application/json"
			}
			if test.wantHTTP == 0 {
				test.wantHTTP = http.StatusBadRequest
			}
			if test.wantCode == codes.OK {
				test.wantCode = codes.InvalidArgument
			}
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.readError != nil {
				request.Body = io.NopCloser(iotest.ErrReader(test.readError))
			}
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Content-Encoding", test.encoding)
			if test.override != "" {
				request.Header.Set("X-HTTP-Method-Override", test.override)
			}
			response := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(response, request)
			require.Equal(t, test.wantHTTP, response.Code, response.Body.String())
			var result struct {
				Code uint32 `json:"code"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.Equal(t, uint32(test.wantCode), result.Code)
			require.NotContains(t, response.Body.String(), "private-body-fragment")
			require.Zero(t, service.calls.Load())
		})
	}
}

func TestIdleGRPCConnectionsReleaseCapacity(t *testing.T) {
	// Exercise grpc-go's actual idle timer with an in-memory transport and fake
	// time, avoiding a production timeout override or a 30-second test sleep.
	synctest.Test(t, func(t *testing.T) {
		api, err := New(Addresses{}, &testService{}, func(context.Context) error { return nil })
		require.NoError(t, err)
		listener := bufconn.Listen(limits.RequestBytes)
		defer api.grpc.Stop()
		go func() { _ = api.grpc.Serve(netutil.LimitListener(listener, 1)) }()
		var clients [2]clusterv1.ClusterServiceClient
		for i := range clients {
			connection, err := grpc.NewClient("passthrough:///memory",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
			)
			require.NoError(t, err)
			defer func() { _ = connection.Close() }()
			clients[i] = clusterv1.NewClusterServiceClient(connection)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*connectionIdle)
		defer cancel()
		_, err = clients[0].GetCluster(ctx, &clusterv1.GetClusterRequest{Name: "sample"})
		require.NoError(t, err)
		started := time.Now()
		_, err = clients[1].GetCluster(ctx, &clusterv1.GetClusterRequest{Name: "sample"}, grpc.WaitForReady(true))
		require.NoError(t, err, "idle connection must release the only slot for the waiting client")
		require.GreaterOrEqual(t, time.Since(started), connectionIdle)
		require.Less(t, time.Since(started), 2*connectionIdle)
	})
}

func TestGRPCDeadlineIncludesIncompleteMessage(t *testing.T) {
	api, err := New(Addresses{}, &testService{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	listener := bufconn.Listen(1 << 20)
	t.Cleanup(api.grpc.Stop)
	go func() { _ = api.grpc.Serve(listener) }()
	connection, err := listener.Dial()
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	_, err = io.WriteString(connection, http2.ClientPreface)
	require.NoError(t, err)
	framer := http2.NewFramer(connection, connection)
	require.NoError(t, framer.WriteSettings())
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: clusterv1.ClusterService_GetCluster_FullMethodName},
		{Name: ":authority", Value: "local"}, {Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	} {
		require.NoError(t, encoder.WriteField(field))
	}
	require.NoError(t, framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID: 1, BlockFragment: encoded.Bytes(), EndHeaders: true,
	}))
	// The envelope promises ten message bytes, but the peer sends only one.
	require.NoError(t, framer.WriteData(1, false, []byte{0, 0, 0, 0, 10, 10}))
	started := time.Now()
	require.NoError(t, connection.SetReadDeadline(started.Add(limits.RequestTimeout+2*time.Second)))
	for {
		frame, err := framer.ReadFrame()
		require.NoError(t, err, "server must terminate the incomplete message before the peer deadline")
		if headers, ok := frame.(*http2.HeadersFrame); ok && headers.StreamEnded() {
			break
		}
		if reset, ok := frame.(*http2.RSTStreamFrame); ok && reset.StreamID == 1 {
			break
		}
	}
	require.GreaterOrEqual(t, time.Since(started), limits.RequestTimeout/2, "valid request headers must be accepted")
}

func TestGatewayRoutesAndSerializesRequests(t *testing.T) {
	for name, test := range map[string]struct {
		method      string
		path        string
		body        string
		wantRequest proto.Message
		wantBody    string
	}{
		"create": {
			method: "POST", path: "/v1/clusters", body: `{"name":"sample"}`,
			wantRequest: &clusterv1.CreateClusterRequest{Name: "sample"}, wantBody: `"resource_version":"12"`,
		},
		"list": {
			method: "GET", path: "/v1/clusters?page_size=1&page_token=example",
			wantRequest: &clusterv1.ListClustersRequest{PageSize: 1, PageToken: "example"}, wantBody: `"next_page_token":"example"`,
		},
		"get": {
			method: "GET", path: "/v1/clusters/sample",
			wantRequest: &clusterv1.GetClusterRequest{Name: "sample"}, wantBody: `"name":"sample"`,
		},
		"replace matching body name": {
			method: "PUT", path: "/v1/clusters/sample",
			body: `{"spec":{"release":{"channel":{"name":"null"}}},"name":"sample","resource_version":"12"} `,
			wantRequest: &clusterv1.ReplaceClusterRequest{
				Name: "sample", ResourceVersion: "12",
				Spec: &clusterv1.ReplaceClusterSpec{Release: &clusterv1.ReplaceRelease{
					Channel: &clusterv1.ReplaceReleaseChannel{Name: new("null")},
				}},
			},
			wantBody: `"name":"sample"`,
		},
		"replace path name": {
			method: "PUT", path: "/v1/clusters/sample", body: `{"resource_version":"12"}`,
			wantRequest: &clusterv1.ReplaceClusterRequest{Name: "sample", ResourceVersion: "12"}, wantBody: `"name":"sample"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			received := make(chan proto.Message, 1)
			service := &testService{observe: func(request proto.Message) { received <- request }}
			server, err := New(Addresses{}, service, func(context.Context) error { return nil })
			require.NoError(t, err)
			startGateway(t, server)
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json; charset=utf-8")
			response := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), test.wantBody)
			require.NotContains(t, response.Body.String(), "resourceVersion")
			require.Equal(t, int32(1), service.calls.Load())
			got := <-received
			require.True(t, proto.Equal(test.wantRequest, got), "decoded request: got %v, want %v", got, test.wantRequest)
			if test.method == http.MethodPost {
				require.True(t, service.bounded.Load())
				require.True(t, service.rpc.Load(), "HTTP must use the real RPC transport")
			}
		})
	}
}

// TestAdmissionRejectsExcessWithoutCallingService proves both transports share one admission budget.
func TestAdmissionRejectsExcessWithoutCallingService(t *testing.T) {
	service := &testService{}
	server, err := New(Addresses{}, service, func(context.Context) error { return nil })
	require.NoError(t, err)
	client := clusterv1.NewClusterServiceClient(startGateway(t, server))
	for range maxConcurrent - 1 {
		server.admission <- struct{}{}
	}
	for _, full := range []bool{false, true} {
		if full {
			server.admission <- struct{}{}
		}
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/clusters/sample", nil))
		ctx, cancel := context.WithTimeout(t.Context(), limits.RequestTimeout)
		_, err := client.GetCluster(ctx, &clusterv1.GetClusterRequest{Name: "sample"})
		cancel()
		if full {
			require.Equal(t, http.StatusTooManyRequests, response.Code)
			require.Equal(t, codes.ResourceExhausted, status.Code(err))
		} else {
			require.Equal(t, http.StatusOK, response.Code, "HTTP must consume only the last available RPC slot")
			require.NoError(t, err)
		}
		require.Equal(t, int32(2), service.calls.Load(), "rejected requests must not reach the service")
	}
}

// startGateway connects the production gateway to a real RPC listener for transport tests.
func startGateway(t *testing.T, server *Server) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(server.grpc.Stop)
	go func() { _ = server.grpc.Serve(listener) }()
	connection, err := server.connectGateway(listener.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestListenersReadinessAndGracefulStop(t *testing.T) {
	service := &testService{}
	var available atomic.Bool
	server, err := New(Addresses{}, service, func(context.Context) error {
		if !available.Load() {
			return errors.New("private storage diagnostic")
		}
		return nil
	})
	require.NoError(t, err)
	listeners := make([]net.Listener, 3)
	for i := range listeners {
		listeners[i], err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listeners[i].Close() })
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listeners[0], listeners[1], listeners[2]) }()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/healthz", "/readyz"} {
		response, err := client.Get("http://" + listeners[2].Addr().String() + path)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		if path == "/readyz" {
			require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
		} else {
			require.Equal(t, http.StatusOK, response.StatusCode)
		}
		require.NotContains(t, string(body), "private storage diagnostic")
	}
	available.Store(true)
	response, err := client.Get("http://" + listeners[2].Addr().String() + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	connection, err := grpc.NewClient(listeners[1].Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	rpcContext, rpcCancel := context.WithTimeout(ctx, 2*limits.RequestTimeout)
	defer rpcCancel()
	result, err := clusterv1.NewClusterServiceClient(connection).CreateCluster(rpcContext, &clusterv1.CreateClusterRequest{Name: "sample"})
	require.NoError(t, err)
	require.Equal(t, "sample", result.GetName())
	require.True(t, service.bounded.Load())
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("API did not drain on cancellation")
	}
}

type blockingService struct {
	testService
	entered chan struct{}
	release chan struct{}
}

func (s *blockingService) GetCluster(ctx context.Context, request *clusterv1.GetClusterRequest) (*clusterv1.Cluster, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.testService.GetCluster(ctx, request)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDrainRejectsReadinessAndNewRequestsWhileCompletingAcceptedRequest(t *testing.T) {
	service := &blockingService{entered: make(chan struct{}), release: make(chan struct{}, 1)}
	t.Cleanup(func() { close(service.release) })
	server, err := New(Addresses{}, service, func(context.Context) error { return nil })
	require.NoError(t, err)
	listeners := make([]net.Listener, 3)
	for i := range listeners {
		listeners[i], err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listeners[i].Close() })
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listeners[0], listeners[1], listeners[2]) }()
	client := &http.Client{Timeout: 3 * time.Second}
	responseReady := make(chan *http.Response, 1)
	requestErrors := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listeners[0].Addr().String() + "/v1/clusters/sample")
		if err != nil {
			requestErrors <- err
			return
		}
		responseReady <- response
	}()
	select {
	case <-service.entered:
	case err := <-requestErrors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach service")
	}
	cancel()
	require.Eventually(t, server.draining.Load, time.Second, time.Millisecond)
	response, err := client.Get("http://" + listeners[2].Addr().String() + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.NoError(t, response.Body.Close())
	newRequest := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(newRequest, httptest.NewRequest(http.MethodGet, "/v1/clusters/sample", nil))
	require.Equal(t, http.StatusServiceUnavailable, newRequest.Code)
	service.release <- struct{}{}
	select {
	case response := <-responseReady:
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, response.Body.Close())
	case err := <-requestErrors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("accepted request did not finish")
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish after active request")
	}
}
