package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testMCPHTTPGatewayToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMCPHTTPGatewayRejectsInvalidHostAndTokenConfiguration(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name         string
		server       *mcp.Server
		verifier     *mcpHTTPTokenVerifier
		expectedHost string
	}{
		{name: "nil server", verifier: verifier, expectedHost: "127.0.0.1:49671"},
		{name: "nil verifier", server: server, expectedHost: "127.0.0.1:49671"},
		{name: "wildcard host", server: server, verifier: verifier, expectedHost: "0.0.0.0:49671"},
		{name: "hostname", server: server, verifier: verifier, expectedHost: "localhost:49671"},
		{name: "missing port", server: server, verifier: verifier, expectedHost: "127.0.0.1"},
		{name: "zero port", server: server, verifier: verifier, expectedHost: "127.0.0.1:0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newAuthenticatedMCPHTTPHandler(test.server, test.verifier, test.expectedHost)
			if err == nil {
				t.Fatal("newAuthenticatedMCPHTTPHandler succeeded for invalid configuration")
			}
		})
	}
	for _, invalidToken := range []string{"short", strings.Repeat("z", 64)} {
		if _, err := newMCPHTTPTokenVerifier(invalidToken); err == nil {
			t.Errorf("newMCPHTTPTokenVerifier accepted invalid token %q", invalidToken)
		}
	}
}

func TestMCPHTTPGatewayTokenVerifierRotationAndRevocation(t *testing.T) {
	const expectedHost = "127.0.0.1:49671"
	rotatedToken := strings.Repeat("a", 64)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(
		mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil),
		verifier,
		expectedHost,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus := func(token string, wantStatus int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "http://"+expectedHost+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		))
		request.Host = expectedHost
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != wantStatus {
			t.Fatalf("HTTP status for token %q = %d, want %d; body=%q", token, response.Code, wantStatus, response.Body.String())
		}
	}

	assertStatus(testMCPHTTPGatewayToken, http.StatusOK)
	if err := verifier.replace("not-a-valid-token"); err == nil {
		t.Fatal("replace accepted invalid token")
	}
	assertStatus(testMCPHTTPGatewayToken, http.StatusOK)
	if err := verifier.replace(rotatedToken); err != nil {
		t.Fatal(err)
	}
	assertStatus(testMCPHTTPGatewayToken, http.StatusUnauthorized)
	assertStatus(rotatedToken, http.StatusOK)
	verifier.revoke()
	assertStatus(testMCPHTTPGatewayToken, http.StatusUnauthorized)
	assertStatus(rotatedToken, http.StatusUnauthorized)
}

func TestMCPHTTPGatewayTokenVerifierConcurrentReplaceAndCheck(t *testing.T) {
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:49671/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+testMCPHTTPGatewayToken)

	const workers = 8
	const iterations = 100
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for range workers {
		go func() {
			defer workersDone.Done()
			for range iterations {
				_ = verifier.allows(request)
			}
		}()
	}
	for range iterations {
		if err := verifier.replace(testMCPHTTPGatewayToken); err != nil {
			t.Fatal(err)
		}
		verifier.revoke()
	}
	workersDone.Wait()
}

func TestMCPHTTPGatewayConcurrentRequestsAndLifecycleChanges(t *testing.T) {
	const expectedHost = "127.0.0.1:49671"
	rotatedToken := strings.Repeat("a", 64)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(
		mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil),
		verifier,
		expectedHost,
	)
	if err != nil {
		t.Fatal(err)
	}
	requestStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://"+expectedHost+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		))
		request.Host = expectedHost
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}

	const workers = 8
	const requestsPerWorker = 20
	start := make(chan struct{})
	var workersReady sync.WaitGroup
	workersReady.Add(workers)
	var requestsDone sync.WaitGroup
	requestsDone.Add(workers)
	for worker := range workers {
		go func() {
			defer requestsDone.Done()
			workersReady.Done()
			<-start
			for requestIndex := range requestsPerWorker {
				token := testMCPHTTPGatewayToken
				if (worker+requestIndex)%2 == 1 {
					token = rotatedToken
				}
				if status := requestStatus(token); status != http.StatusOK && status != http.StatusUnauthorized {
					t.Errorf("concurrent request status = %d, want 200 or 401", status)
				}
			}
		}()
	}
	workersReady.Wait()
	close(start)
	for range requestsPerWorker {
		if err := verifier.replace(rotatedToken); err != nil {
			t.Fatal(err)
		}
		verifier.revoke()
		if err := verifier.replace(testMCPHTTPGatewayToken); err != nil {
			t.Fatal(err)
		}
	}
	requestsDone.Wait()
	verifier.revoke()
	for _, token := range []string{testMCPHTTPGatewayToken, rotatedToken} {
		if status := requestStatus(token); status != http.StatusUnauthorized {
			t.Errorf("post-revoke concurrent handler status = %d, want 401", status)
		}
	}
}

func TestMCPHTTPGatewayInFlightRequestCanFinishAfterRevocation(t *testing.T) {
	listener, err := listenMCPHTTPLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	expectedHost := listener.Addr().String()
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTool) }) }

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool[struct{}, map[string]string](server, &mcp.Tool{Name: "wait"}, func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
		_ struct{},
	) (*mcp.CallToolResult, map[string]string, error) {
		close(toolStarted)
		select {
		case <-releaseTool:
			return nil, map[string]string{"status": "finished"}, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(server, verifier, expectedHost)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() {
		release()
		_ = httpServer.Close()
	})
	postStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://"+expectedHost+"/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`,
		))
		request.Host = expectedHost
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}

	transport := &mcpHTTPBearerRoundTripper{
		base:  &http.Transport{Proxy: nil},
		token: testMCPHTTPGatewayToken,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "revocation-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://" + expectedHost + "/mcp",
		HTTPClient:           &http.Client{Transport: transport, Timeout: 10 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	requestDone := make(chan callResult, 1)
	go func() {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait"})
		requestDone <- callResult{result: result, err: err}
	}()
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP tool did not start before timeout")
	}

	verifier.revoke()
	if status := postStatus(testMCPHTTPGatewayToken); status != http.StatusUnauthorized {
		t.Fatalf("new request after revoke status = %d, want 401", status)
	}
	release()
	select {
	case result := <-requestDone:
		if result.err != nil || result.result == nil || result.result.IsError {
			t.Fatalf("request authenticated before revoke did not finish successfully: result=%+v err=%v", result.result, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish after revoke")
	}
}

func TestMCPHTTPListenerBindsOnlyToIPv4Loopback(t *testing.T) {
	listener, err := listenMCPHTTPLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.Equal(net.ParseIP("127.0.0.1")) || address.Port == 0 {
		t.Fatalf("listener address = %v, want an assigned 127.0.0.1 port", listener.Addr())
	}
	for _, port := range []int{-1, 65536} {
		if _, err := listenMCPHTTPLoopback(port); err == nil {
			t.Errorf("listenMCPHTTPLoopback(%d) succeeded for invalid port", port)
		}
	}
}

func TestMCPHTTPGatewayRejectsUntrustedRequestsBeforeMCPDispatch(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool[struct{}, map[string]string](
		server,
		&mcp.Tool{Name: "probe"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
			calls.Add(1)
			return nil, map[string]string{"status": "called"}, nil
		},
	)
	const expectedHost = "127.0.0.1:49671"
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(server, verifier, expectedHost)
	if err != nil {
		t.Fatal(err)
	}

	validMCPBody := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"probe\",\"arguments\":{}}}"
	for _, test := range []struct {
		name       string
		host       string
		origin     []string
		authorizes []string
		status     int
	}{
		{name: "missing token", host: expectedHost, status: http.StatusUnauthorized},
		{name: "wrong token", host: expectedHost, authorizes: []string{"Bearer " + strings.Repeat("0", 64)}, status: http.StatusUnauthorized},
		{name: "malformed scheme", host: expectedHost, authorizes: []string{"Basic " + testMCPHTTPGatewayToken}, status: http.StatusUnauthorized},
		{name: "duplicate authorization", host: expectedHost, authorizes: []string{"Bearer " + testMCPHTTPGatewayToken, "Bearer " + testMCPHTTPGatewayToken}, status: http.StatusUnauthorized},
		{name: "unexpected host", host: "localhost:49671", authorizes: []string{"Bearer " + testMCPHTTPGatewayToken}, status: http.StatusForbidden},
		{name: "null origin", host: expectedHost, origin: []string{"null"}, authorizes: []string{"Bearer " + testMCPHTTPGatewayToken}, status: http.StatusForbidden},
		{name: "foreign origin", host: expectedHost, origin: []string{"https://evil.example.invalid"}, authorizes: []string{"Bearer " + testMCPHTTPGatewayToken}, status: http.StatusForbidden},
		{name: "duplicate origin", host: expectedHost, origin: []string{"http://" + expectedHost, "http://" + expectedHost}, authorizes: []string{"Bearer " + testMCPHTTPGatewayToken}, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+expectedHost+"/mcp", strings.NewReader(validMCPBody))
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json")
			for _, origin := range test.origin {
				request.Header.Add("Origin", origin)
			}
			for _, authorization := range test.authorizes {
				request.Header.Add("Authorization", authorization)
			}

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("HTTP status = %d, want %d; body=%q", response.Code, test.status, response.Body.String())
			}
			if strings.Contains(response.Body.String(), testMCPHTTPGatewayToken) {
				t.Fatal("rejection response disclosed the gateway token")
			}
			if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("rejection response enabled CORS")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("rejection response is cacheable")
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("untrusted requests dispatched %d MCP tool calls", got)
	}
}

func TestMCPHTTPGatewayRejectsOversizedRequest(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(server, verifier, "127.0.0.1:49671")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:49671/mcp",
		strings.NewReader(strings.Repeat("x", mcpHTTPMaxRequestBytes+1)),
	)
	request.Host = "127.0.0.1:49671"
	request.Header.Set("Authorization", "Bearer "+testMCPHTTPGatewayToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("HTTP status = %d, want %d; body=%q", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
}

func TestMCPHTTPGatewayUsesRegisteredMCPServer(t *testing.T) {
	listener, err := listenMCPHTTPLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()

	secrets := &mcpHTTPTestSecrets{}
	outbound := &mcpHTTPBlockedTransport{}
	a := &app{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		secrets:    secrets,
		client:     &http.Client{Transport: outbound},
	}
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthenticatedMCPHTTPHandler(a.mcpServer(), verifier, host)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		_ = httpServer.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = httpServer.Close()
	})

	for _, originTest := range []struct {
		name   string
		origin string
	}{
		{name: "origin omitted"},
		{name: "exact same origin", origin: "http://" + host},
	} {
		t.Run(originTest.name, func(t *testing.T) {
			transport := &mcpHTTPBearerRoundTripper{
				base:   &http.Transport{Proxy: nil},
				token:  testMCPHTTPGatewayToken,
				origin: originTest.origin,
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "http-test", Version: "1"}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             "http://" + host + "/mcp",
				HTTPClient:           &http.Client{Transport: transport, Timeout: 10 * time.Second},
				DisableStandaloneSSE: true,
				MaxRetries:           -1,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			catalog, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
			if err != nil {
				t.Fatal(err)
			}
			if catalog.IsError {
				t.Fatalf("registered_targets returned an MCP error: %+v", catalog)
			}
			catalogJSON, err := json.Marshal(catalog.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var decoded registeredTargetsResult
			if err := json.Unmarshal(catalogJSON, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Targets) != 0 {
				t.Fatalf("empty settings returned %d registered targets", len(decoded.Targets))
			}

			unregistered, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "github_repository",
				Arguments: map[string]any{"target": "not-registered"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !unregistered.IsError {
				t.Fatalf("unregistered target call was not rejected: %+v", unregistered)
			}
			if got := secrets.loads.Load(); got != 0 {
				t.Fatalf("HTTP MCP calls loaded %d service credentials", got)
			}
			if got := outbound.calls.Load(); got != 0 {
				t.Fatalf("HTTP MCP calls attempted %d outbound service requests", got)
			}
		})
	}
}

type mcpHTTPBearerRoundTripper struct {
	base   http.RoundTripper
	token  string
	origin string
}

func (t *mcpHTTPBearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	if t.origin != "" {
		clone.Header.Set("Origin", t.origin)
	}
	return t.base.RoundTrip(clone)
}

type mcpHTTPTestSecrets struct {
	loads atomic.Int32
}

func (s *mcpHTTPTestSecrets) Load(string) ([]byte, error) {
	s.loads.Add(1)
	return nil, errors.New("unexpected secret load")
}

func (*mcpHTTPTestSecrets) Save(string, []byte) error {
	return errors.New("unexpected secret save")
}

func (*mcpHTTPTestSecrets) Delete(string) error {
	return errors.New("unexpected secret delete")
}

type mcpHTTPBlockedTransport struct {
	calls atomic.Int32
}

func (t *mcpHTTPBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("unexpected outbound request")
}
