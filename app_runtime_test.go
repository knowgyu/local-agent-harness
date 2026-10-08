package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResidentMCPRuntimeLifecycleCallsRegisteredServerOverHTTP(t *testing.T) {
	port := availableLoopbackPort(t)
	provider := &fakeMCPGatewayTokenProvider{}
	var serverCreations atomic.Int32
	a := &app{configPath: t.TempDir() + `\config.json`, secrets: &mcpHTTPTestSecrets{}, client: &http.Client{Transport: &mcpHTTPBlockedTransport{}}}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		serverCreations.Add(1)
		return a.mcpServer()
	}, port)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := controller.Start(ctx); err != nil {
		t.Fatalf("runtime Start failed: %v", err)
	}
	status, err := controller.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantEndpoint := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + mcpHTTPPath
	if status.State != mcpRuntimeRunning || status.MCPEndpoint != wantEndpoint {
		t.Fatalf("runtime status = %+v, want running endpoint %q", status, wantEndpoint)
	}
	token := hex.EncodeToString(provider.snapshot())
	if len(token) != 64 {
		t.Fatal("runtime did not provision a 32-byte gateway token")
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statusJSON), token) || strings.Contains(string(statusJSON), gatewayCredentialTargetName) {
		t.Fatal("runtime status disclosed a token or Credential Manager lookup target")
	}
	callResidentRegisteredTargets(t, status.MCPEndpoint, token)
	callResidentRegisteredTargets(t, status.MCPEndpoint, token)
	if got := serverCreations.Load(); got != 1 {
		t.Fatalf("server factory ran %d times for two tool calls, want once per runtime", got)
	}
	if got := provider.saves; got != 1 {
		t.Fatalf("gateway token was stored %d times after two tool calls, want one provisioning", got)
	}

	if err := controller.Stop(ctx); err != nil {
		t.Fatalf("runtime Stop failed: %v", err)
	}
	status, err = controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeStopped {
		t.Fatalf("status after stop = %+v, err=%v", status, err)
	}
	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		t.Fatalf("runtime stop did not release its loopback port: %v", err)
	}
	_ = listener.Close()

	if err := controller.Restart(ctx); err != nil {
		t.Fatalf("runtime Restart failed: %v", err)
	}
	status, err = controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeRunning || !strings.HasSuffix(status.MCPEndpoint, "/mcp") {
		t.Fatalf("status after restart = %+v, err=%v", status, err)
	}
	if got := provider.saves; got != 1 {
		t.Fatalf("runtime restart reprovisioned the gateway token %d times", got-1)
	}
	if got := serverCreations.Load(); got != 2 {
		t.Fatalf("server factory ran %d times across one restart, want two", got)
	}
	if err := controller.RevokeGatewayToken(ctx); err != nil {
		t.Fatalf("runtime token revoke failed: %v", err)
	}
	if len(provider.snapshot()) != 0 {
		t.Fatal("revoke left the gateway token in the provider")
	}
	status, err = controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeStopped {
		t.Fatalf("status after token revoke = %+v, err=%v", status, err)
	}
	clear([]byte(token))
}

func TestResidentMCPRuntimeRotatesTokenWithoutExposingIt(t *testing.T) {
	provider := &fakeMCPGatewayTokenProvider{token: bytesOf(0x11, mcpGatewayTokenSize)}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "rotation-test", Version: "1"}, nil)
	}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oldToken := hex.EncodeToString(provider.snapshot())
	if err := controller.RotateGatewayToken(ctx); err != nil {
		t.Fatalf("RotateGatewayToken failed: %v", err)
	}
	newTokenBytes := provider.snapshot()
	newToken := hex.EncodeToString(newTokenBytes)
	clear(newTokenBytes)
	if oldToken == newToken {
		t.Fatal("rotation retained the previous gateway token")
	}
	status, err := controller.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeResidentMCP(t, status.MCPEndpoint, oldToken); got != http.StatusUnauthorized {
		t.Fatalf("old token status after rotation = %d, want 401", got)
	}
	if got := probeResidentMCP(t, status.MCPEndpoint, newToken); got != http.StatusOK {
		t.Fatalf("new token status after rotation = %d, want 200", got)
	}
	clear([]byte(oldToken))
	clear([]byte(newToken))
	if err := controller.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestResidentMCPRuntimePortConflictAndDuplicateStart(t *testing.T) {
	port := availableLoopbackPort(t)
	blocker, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil { // The helper has closed the reserved socket; a local race is reported as a skip.
		t.Skip("could not reserve a stable loopback port for the conflict test")
	}
	defer blocker.Close()
	provider := &fakeMCPGatewayTokenProvider{}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "conflict-test", Version: "1"}, nil)
	}, port)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := controller.Start(ctx); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("Start on occupied port error = %v, want a generic endpoint error", err)
	}
	status, err := controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeError || status.FailureCode != "endpoint_unavailable" {
		t.Fatalf("status after port conflict = %+v, err=%v", status, err)
	}
	if provider.saves != 0 {
		t.Fatal("port conflict caused a gateway token write")
	}

	_ = blocker.Close()
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("Start after releasing port failed: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := controller.Start(ctx); err != nil {
				t.Errorf("concurrent duplicate Start failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := provider.saves; got != 1 {
		t.Fatalf("duplicate starts provisioned token %d times, want one", got)
	}
	if err := controller.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestResidentMCPRuntimeSecondOwnerFailsBeforeTokenProvision(t *testing.T) {
	port := availableLoopbackPort(t)
	firstProvider := &fakeMCPGatewayTokenProvider{}
	secondProvider := &fakeMCPGatewayTokenProvider{}
	factory := func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "owner-test", Version: "1"}, nil)
	}
	first := newResidentMCPRuntimeController(firstProvider, factory, port)
	second := newResidentMCPRuntimeController(secondProvider, factory, port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer first.Stop(context.Background())
	if err := second.Start(ctx); err == nil {
		t.Fatal("second runtime owner bound an already-owned endpoint")
	}
	if secondProvider.saves != 0 {
		t.Fatal("second owner wrote a gateway token before bind ownership was established")
	}
	status, err := first.Status(ctx)
	if err != nil || status.State != mcpRuntimeRunning {
		t.Fatalf("first owner status = %+v, err=%v", status, err)
	}
}

func TestResidentMCPRuntimeCredentialReadFailureFailsClosed(t *testing.T) {
	port := availableLoopbackPort(t)
	provider := &fakeMCPGatewayTokenProvider{loadErr: errors.New("private credential store detail")}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "credential-error-test", Version: "1"}, nil)
	}, port)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := controller.Start(ctx); err == nil || strings.Contains(err.Error(), "private credential store detail") {
		t.Fatalf("Start error = %v, want generic credential-store failure", err)
	}
	status, err := controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeError || status.FailureCode != "gateway_token_unavailable" {
		t.Fatalf("status after credential read failure = %+v, err=%v", status, err)
	}
	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		t.Fatalf("failed start left loopback port bound: %v", err)
	}
	_ = listener.Close()
	if provider.saves != 0 {
		t.Fatal("credential read failure caused a replacement token to be stored")
	}
}

func TestResidentMCPRuntimeRevokeStoreFailureStillStopsEndpoint(t *testing.T) {
	port := availableLoopbackPort(t)
	provider := &fakeMCPGatewayTokenProvider{token: bytesOf(0x31, mcpGatewayTokenSize)}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "revoke-failure-test", Version: "1"}, nil)
	}, port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.deleteErr = errors.New("private credential store detail")
	provider.mu.Unlock()
	if err := controller.RevokeGatewayToken(ctx); err == nil || strings.Contains(err.Error(), "private credential store detail") {
		t.Fatalf("revoke error = %v, want generic credential-store failure", err)
	}
	status, err := controller.Status(ctx)
	if err != nil || status.State != mcpRuntimeError || status.FailureCode != "gateway_token_revoke_failed" {
		t.Fatalf("status after revoke failure = %+v, err=%v", status, err)
	}
	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		t.Fatalf("failed revoke left HTTP endpoint bound: %v", err)
	}
	_ = listener.Close()
	if len(provider.snapshot()) != mcpGatewayTokenSize {
		t.Fatal("delete failure unexpectedly changed the persisted provider value")
	}
}

func TestResidentMCPRuntimeRotationSaveFailureKeepsOldTokenActive(t *testing.T) {
	provider := &fakeMCPGatewayTokenProvider{token: bytesOf(0x21, mcpGatewayTokenSize)}
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "rotation-failure-test", Version: "1"}, nil)
	}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	oldToken := hex.EncodeToString(provider.snapshot())
	provider.mu.Lock()
	provider.saveErr = errors.New("private provider error")
	provider.mu.Unlock()
	if err := controller.RotateGatewayToken(ctx); err == nil || strings.Contains(err.Error(), "private provider error") {
		t.Fatalf("rotation error = %v, want a generic storage failure", err)
	}
	status, err := controller.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeResidentMCP(t, status.MCPEndpoint, oldToken); got != http.StatusOK {
		t.Fatalf("old token status after failed rotation = %d, want 200", got)
	}
	clear([]byte(oldToken))
	provider.mu.Lock()
	provider.saveErr = nil
	provider.mu.Unlock()
	if err := controller.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func availableLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func callResidentRegisteredTargets(t *testing.T, endpoint, token string) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "resident-runtime-test", Version: "1"}, nil)
	transport := &mcpHTTPBearerRoundTripper{base: &http.Transport{Proxy: nil}, token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 5 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("resident HTTP MCP init failed: %v", err)
	}
	defer session.Close()
	for range 2 {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("resident HTTP MCP tool call failed: result=%+v err=%v", result, err)
		}
	}
}

func probeResidentMCP(t *testing.T, endpoint, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("resident HTTP probe failed: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func bytesOf(value byte, size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}
