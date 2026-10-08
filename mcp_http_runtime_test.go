package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPHTTPRuntimeGracefulShutdownRevokesToken(t *testing.T) {
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTool) }) }

	server := mcp.NewServer(&mcp.Implementation{Name: "runtime-test", Version: "1"}, nil)
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
	runtime, err := startMCPHTTPRuntime(server, verifier, 0)
	if err != nil {
		t.Fatal(err)
	}
	port := runtime.listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
		defer cancel()
		_ = runtime.shutdown(ctx)
	})
	if !strings.HasPrefix(runtime.endpoint(), "http://127.0.0.1:") || !strings.HasSuffix(runtime.endpoint(), "/mcp") {
		t.Fatalf("runtime endpoint = %q, want loopback /mcp URL", runtime.endpoint())
	}
	if runtime.server.WriteTimeout != 0 {
		t.Fatalf("runtime WriteTimeout = %s, want disabled for streamable HTTP", runtime.server.WriteTimeout)
	}
	probeClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	wrongPath, err := probeClient.Get(strings.TrimSuffix(runtime.endpoint(), "/mcp") + "/not-mcp")
	if err != nil {
		t.Fatalf("wrong-path probe failed: %v", err)
	}
	defer wrongPath.Body.Close()
	if wrongPath.StatusCode != http.StatusNotFound || wrongPath.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("wrong-path response status=%d cache-control=%q, want 404 and no-store", wrongPath.StatusCode, wrongPath.Header.Get("Cache-Control"))
	}

	transport := &mcpHTTPBearerRoundTripper{base: &http.Transport{Proxy: nil}, token: testMCPHTTPGatewayToken}
	client := mcp.NewClient(&mcp.Implementation{Name: "runtime-test-client", Version: "1"}, nil)
	clientCtx, cancelClient := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClient()
	session, err := client.Connect(clientCtx, &mcp.StreamableClientTransport{
		Endpoint:             runtime.endpoint(),
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
	callDone := make(chan callResult, 1)
	go func() {
		result, err := session.CallTool(clientCtx, &mcp.CallToolParams{Name: "wait"})
		callDone <- callResult{result: result, err: err}
	}()
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP tool did not start before timeout")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
	defer cancelShutdown()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.shutdown(shutdownCtx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("runtime shutdown returned before the active MCP call finished: %v", err)
	default:
	}

	release()
	select {
	case result := <-callDone:
		if result.err != nil || result.result == nil || result.result.IsError {
			t.Fatalf("MCP call accepted before shutdown did not finish: result=%+v err=%v", result.result, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight MCP call did not finish after release")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("runtime shutdown failed: %v", err)
		}
	case <-time.After(2 * mcpHTTPShutdownTimeout):
		t.Fatal("runtime shutdown did not finish after the active call ended")
	}
	request := httptest.NewRequest("POST", runtime.endpoint(), nil)
	request.Header.Set("Authorization", "Bearer "+testMCPHTTPGatewayToken)
	if verifier.allows(request) {
		t.Fatal("runtime shutdown left the gateway token active")
	}

	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		t.Fatalf("loopback port was not released after shutdown: %v", err)
	}
	_ = listener.Close()
	if err := runtime.shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown call failed: %v", err)
	}
}

func TestMCPHTTPRuntimeShutdownBoundsIncompleteRequestHeaders(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "incomplete-header-test", Version: "1"}, nil)
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := startMCPHTTPRuntime(server, verifier, 0)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.DialTimeout("tcp4", runtime.listener.Addr().String(), time.Second)
	if err != nil {
		_ = runtime.shutdown(context.Background())
		t.Fatalf("loopback connection failed: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("POST /mcp HTTP/1.1\r\nHost:")); err != nil {
		_ = runtime.shutdown(context.Background())
		t.Fatalf("partial request header write failed: %v", err)
	}
	// Let Serve accept the TCP connection before shutdown starts. The request
	// deliberately has no complete headers, so the server must bound how long
	// graceful shutdown waits for it.
	time.Sleep(25 * time.Millisecond)
	if runtime.server.ReadHeaderTimeout >= mcpHTTPShutdownTimeout {
		t.Fatalf("ReadHeaderTimeout = %s, want shorter than shutdown timeout %s", runtime.server.ReadHeaderTimeout, mcpHTTPShutdownTimeout)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := runtime.shutdown(shutdownCtx); err != nil {
		t.Fatalf("runtime shutdown with an incomplete request header failed: %v", err)
	}
}

func TestMCPHTTPRuntimeForcedShutdownClosesActiveCall(t *testing.T) {
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTool) }) }

	server := mcp.NewServer(&mcp.Implementation{Name: "runtime-force-test", Version: "1"}, nil)
	mcp.AddTool[struct{}, map[string]string](server, &mcp.Tool{Name: "wait"}, func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
		_ struct{},
	) (*mcp.CallToolResult, map[string]string, error) {
		close(toolStarted)
		select {
		case <-releaseTool:
			return nil, map[string]string{"status": "released"}, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
	verifier, err := newMCPHTTPTokenVerifier(testMCPHTTPGatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := startMCPHTTPRuntime(server, verifier, 0)
	if err != nil {
		t.Fatal(err)
	}
	port := runtime.listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
		defer cancel()
		_ = runtime.shutdown(ctx)
	})

	transport := &mcpHTTPBearerRoundTripper{base: &http.Transport{Proxy: nil}, token: testMCPHTTPGatewayToken}
	client := mcp.NewClient(&mcp.Implementation{Name: "runtime-force-test-client", Version: "1"}, nil)
	clientCtx, cancelClient := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClient()
	session, err := client.Connect(clientCtx, &mcp.StreamableClientTransport{
		Endpoint:             runtime.endpoint(),
		HTTPClient:           &http.Client{Transport: transport, Timeout: 10 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	callDone := make(chan error, 1)
	go func() {
		_, err := session.CallTool(clientCtx, &mcp.CallToolParams{Name: "wait"})
		callDone <- err
	}()
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP tool did not start before timeout")
	}

	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	cancelShutdown()
	if err := runtime.shutdown(shutdownCtx); err == nil || err.Error() != "MCP HTTP server required a forced shutdown." {
		t.Fatalf("forced runtime shutdown error = %v, want fixed forced-shutdown error", err)
	}
	if verifier.allows(httptest.NewRequest("POST", runtime.endpoint(), nil)) {
		t.Fatal("forced runtime shutdown left the gateway token active")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("active MCP call unexpectedly succeeded after forced connection close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active MCP call did not stop after forced connection close")
	}

	listener, err := listenMCPHTTPLoopback(port)
	if err != nil {
		t.Fatalf("loopback port was not released after forced shutdown: %v", err)
	}
	_ = listener.Close()
}
