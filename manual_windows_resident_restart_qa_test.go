//go:build windows && manual_windows_resident_restart_qa

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	residentRestartChildEnv = "LAH_TEST_RESIDENT_RESTART_CHILD"
	residentRestartStoreEnv = "LAH_TEST_RESIDENT_RESTART_STORE"
	residentRestartPortEnv  = "LAH_TEST_RESIDENT_RESTART_PORT"
	residentRestartHealth   = "/.well-known/local-agent-harness/resident-mcp-health"
	residentRestartTool     = "resident_restart_fixture_health"
)

const (
	residentRestartReadyTimeout = 10 * time.Second
	residentRestartExitTimeout  = 7 * time.Second
)

type residentRestartFileTokenProvider struct {
	path string
}

func (p residentRestartFileTokenProvider) LoadGatewayToken() ([]byte, error) {
	if p.path == "" {
		return nil, errMCPGatewayTokenStore
	}
	token, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errMCPGatewayTokenNotFound
	}
	if err != nil {
		return nil, errMCPGatewayTokenStore
	}
	return token, nil
}

func (p residentRestartFileTokenProvider) SaveGatewayToken(token []byte) error {
	if p.path == "" || len(token) != mcpGatewayTokenSize {
		return errMCPGatewayTokenInvalid
	}
	if err := os.WriteFile(p.path, token, 0o600); err != nil {
		return errMCPGatewayTokenStore
	}
	return nil
}

func (p residentRestartFileTokenProvider) DeleteGatewayToken() error {
	if p.path == "" {
		return errMCPGatewayTokenStore
	}
	if err := os.Remove(p.path); errors.Is(err, os.ErrNotExist) {
		return errMCPGatewayTokenNotFound
	} else if err != nil {
		return errMCPGatewayTokenStore
	}
	return nil
}

type residentRestartChild struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	done    chan error
	port    int
	pid     int
	exited  bool
	exitErr error
}

func TestManualWindowsResidentRuntimeForceKillRestart(t *testing.T) {
	stateDir := t.TempDir()
	tokenPath := filepath.Join(stateDir, "synthetic-gateway-token.bin")
	token := make([]byte, mcpGatewayTokenSize)
	if _, err := rand.Read(token); err != nil {
		t.Fatal("could not create a synthetic gateway token")
	}
	defer clear(token)
	if err := os.WriteFile(tokenPath, token, 0o600); err != nil {
		t.Fatal("could not seed the private synthetic token store")
	}
	t.Cleanup(func() {
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("could not remove the temporary synthetic token file")
		}
		if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
			t.Error("temporary synthetic token file remained after cleanup")
		}
	})

	first := startResidentRestartChild(t, tokenPath, 0)
	if first.pid == os.Getpid() || first.pid != first.command.Process.Pid {
		t.Fatal("first runtime was not an independently owned child process")
	}
	assertResidentRestartStoredTokenUnchanged(t, tokenPath, token, "first runtime changed the existing synthetic gateway token")
	checkResidentRestartHealthEndpoint(t, first.port)
	checkResidentRestartMCPRejectsUnauthenticated(t, first.port)
	wrongToken := append([]byte(nil), token...)
	defer clear(wrongToken)
	wrongToken[0] ^= 0xff
	wrongTokenText := hex.EncodeToString(wrongToken)
	checkResidentRestartMCPRejectsWrongToken(t, first.port, wrongTokenText)
	callResidentRestartFixtureTool(t, first.port, hex.EncodeToString(token))

	first.forceKill(t)
	listener, err := listenMCPHTTPLoopback(first.port)
	if err != nil {
		t.Fatal("owned child termination did not release its private loopback port")
	}
	if err := listener.Close(); err != nil {
		t.Fatal("could not close the private loopback rebind probe")
	}

	second := startResidentRestartChild(t, tokenPath, first.port)
	if second.pid == first.pid || second.pid == os.Getpid() || second.port != first.port {
		t.Fatal("restarted runtime did not bind the same port in a distinct process")
	}
	assertResidentRestartStoredTokenUnchanged(t, tokenPath, token, "restart changed the persistent synthetic gateway token")
	checkResidentRestartHealthEndpoint(t, second.port)
	checkResidentRestartMCPRejectsUnauthenticated(t, second.port)
	checkResidentRestartMCPRejectsWrongToken(t, second.port, wrongTokenText)
	callResidentRestartFixtureTool(t, second.port, hex.EncodeToString(token))
	second.stop(t)
	assertResidentRestartStoredTokenUnchanged(t, tokenPath, token, "graceful runtime stop unexpectedly deleted the persistent synthetic token")
}

func TestManualWindowsResidentRuntimeForceKillHelper(t *testing.T) {
	if os.Getenv(residentRestartChildEnv) != "1" {
		return
	}
	tokenPath := os.Getenv(residentRestartStoreEnv)
	portText := os.Getenv(residentRestartPortEnv)
	port, err := strconv.Atoi(portText)
	if tokenPath == "" || err != nil || port < 0 || port > 65535 {
		t.Fatal("isolated runtime helper configuration is invalid")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "resident-restart-test", Version: "1"}, nil)
	mcp.AddTool[struct{}, map[string]string](server, &mcp.Tool{Name: residentRestartTool}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		_ struct{},
	) (*mcp.CallToolResult, map[string]string, error) {
		return nil, map[string]string{"status": "authorized"}, nil
	})
	controller := newResidentMCPRuntimeController(residentRestartFileTokenProvider{path: tokenPath}, func() *mcp.Server {
		return server
	}, port)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	if err := controller.Start(startCtx); err != nil {
		cancelStart()
		t.Fatal("isolated runtime helper could not start")
	}
	cancelStart()
	status, err := controller.Status(context.Background())
	if err != nil || status.State != mcpRuntimeRunning {
		t.Fatal("isolated runtime helper did not report a running listener")
	}
	endpoint, err := url.Parse(status.MCPEndpoint)
	if err != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" {
		t.Fatal("isolated runtime helper did not bind an IPv4 loopback listener")
	}
	actualPort, err := strconv.Atoi(endpoint.Port())
	if err != nil || actualPort < 1 || actualPort > 65535 {
		t.Fatal("isolated runtime helper received an invalid listener port")
	}
	if _, err := fmt.Fprintf(os.Stdout, "%d %d\n", os.Getpid(), actualPort); err != nil {
		t.Fatal("isolated runtime helper could not signal its readiness barrier")
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	stopCtx, cancelStop := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
	defer cancelStop()
	if err := controller.Stop(stopCtx); err != nil {
		t.Fatal("isolated runtime helper could not stop its listener")
	}
}

func startResidentRestartChild(t *testing.T, tokenPath string, port int) *residentRestartChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("could not locate the isolated test executable")
	}
	command := exec.Command(executable, "-test.run=^TestManualWindowsResidentRuntimeForceKillHelper$")
	command.Dir = filepath.Dir(tokenPath)
	command.Env = []string{
		residentRestartChildEnv + "=1",
		residentRestartStoreEnv + "=" + tokenPath,
		residentRestartPortEnv + "=" + strconv.Itoa(port),
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal("could not read the isolated child readiness barrier")
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal("could not own the isolated child shutdown pipe")
	}
	if err := command.Start(); err != nil {
		t.Fatal("could not start the isolated runtime child")
	}
	child := &residentRestartChild{
		command: command,
		stdin:   stdin,
		done:    make(chan error, 1),
	}
	go func() { child.done <- command.Wait() }()
	t.Cleanup(func() { child.cleanup(t) })

	type readyLine struct {
		line string
		err  error
	}
	ready := make(chan readyLine, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(stdout, 64)).ReadString('\n')
		ready <- readyLine{line: line, err: err}
	}()
	timer := time.NewTimer(residentRestartReadyTimeout)
	defer timer.Stop()
	select {
	case result := <-ready:
		if result.err != nil {
			t.Fatal("isolated runtime child exited before its readiness barrier")
		}
		if fields := strings.Fields(result.line); len(fields) == 2 {
			child.pid, err = strconv.Atoi(fields[0])
			if err == nil {
				child.port, err = strconv.Atoi(fields[1])
			}
		} else {
			err = errors.New("invalid readiness record")
		}
		if err != nil || child.pid < 1 || child.port < 1 || child.port > 65535 || (port != 0 && child.port != port) {
			t.Fatal("isolated runtime child returned invalid readiness metadata")
		}
		if child.pid != command.Process.Pid {
			t.Fatal("readiness process identifier did not match the owned child")
		}
	case child.exitErr = <-child.done:
		child.exited = true
		t.Fatal("isolated runtime child exited before its readiness barrier")
	case <-timer.C:
		t.Fatal("isolated runtime child did not reach readiness before the deadline")
	}
	return child
}

func (c *residentRestartChild) forceKill(t *testing.T) {
	t.Helper()
	if c == nil || c.exited || c.command == nil || c.command.Process == nil {
		t.Fatal("owned runtime child was not available for force termination")
	}
	select {
	case c.exitErr = <-c.done:
		c.exited = true
		t.Fatal("owned runtime child exited before force termination")
	default:
	}
	if err := c.command.Process.Kill(); err != nil {
		t.Fatal("could not terminate the owned runtime child")
	}
	if !c.waitForExit(residentRestartExitTimeout) {
		t.Fatal("owned runtime child did not exit after force termination")
	}
	if c.exitErr == nil {
		t.Fatal("force-terminated runtime child exited without an error status")
	}
	_ = c.stdin.Close()
}

func (c *residentRestartChild) stop(t *testing.T) {
	t.Helper()
	if c == nil || c.exited {
		t.Fatal("owned runtime child was not available for graceful stop")
	}
	if _, err := io.WriteString(c.stdin, "stop\n"); err != nil {
		t.Fatal("could not send the isolated child stop barrier")
	}
	_ = c.stdin.Close()
	if !c.waitForExit(residentRestartExitTimeout) || c.exitErr != nil {
		t.Fatal("isolated runtime child did not stop cleanly")
	}
	listener, err := listenMCPHTTPLoopback(c.port)
	if err != nil {
		t.Fatal("gracefully stopped child did not release its private loopback port")
	}
	if err := listener.Close(); err != nil {
		t.Fatal("could not close the private loopback rebind probe")
	}
}

func (c *residentRestartChild) cleanup(t *testing.T) {
	t.Helper()
	if c == nil || c.exited {
		return
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.waitForExit(residentRestartExitTimeout) {
		if c.exitErr != nil {
			t.Error("isolated runtime child did not exit cleanly during cleanup")
		}
		return
	}
	if c.command != nil && c.command.Process != nil {
		_ = c.command.Process.Kill()
		if !c.waitForExit(5 * time.Second) {
			t.Error("owned runtime child could not be reaped")
		}
	}
}

func (c *residentRestartChild) waitForExit(timeout time.Duration) bool {
	if c == nil || c.exited {
		return c != nil && c.exited
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c.exitErr = <-c.done:
		c.exited = true
		return true
	case <-timer.C:
		return false
	}
}

func checkResidentRestartHealthEndpoint(t *testing.T, port int) {
	t.Helper()
	address := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + residentRestartHealth
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal("could not build the local health observation request")
	}
	response, err := privateResidentRestartHTTPClient().Do(request)
	if err != nil {
		t.Fatal("local health endpoint did not respond")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 257))
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" || string(body) != `{"service":"local-agent-harness","protocol":"resident-mcp-health-v1","status":"responding"}` {
		t.Fatal("local health endpoint did not return its fixed response identity")
	}
}

func checkResidentRestartMCPRejectsUnauthenticated(t *testing.T, port int) {
	t.Helper()
	checkResidentRestartMCPStatus(t, port, "", http.StatusUnauthorized)
}

func checkResidentRestartMCPRejectsWrongToken(t *testing.T, port int, token string) {
	t.Helper()
	checkResidentRestartMCPStatus(t, port, token, http.StatusUnauthorized)
}

func checkResidentRestartMCPStatus(t *testing.T, port int, token string, want int) {
	t.Helper()
	address := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + mcpHTTPPath
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal("could not build the private MCP authentication probe")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := privateResidentRestartHTTPClient().Do(request)
	if err != nil {
		t.Fatal("private MCP authentication probe failed to reach the listener")
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("private MCP authentication status = %d, want %d", response.StatusCode, want)
	}
}

func callResidentRestartFixtureTool(t *testing.T, port int, token string) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "resident-restart-test-client", Version: "1"}, nil)
	baseTransport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	transport := &mcpHTTPBearerRoundTripper{base: baseTransport, token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + mcpHTTPPath
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 5 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal("synthetic stored token could not initialize the private MCP fixture")
	}
	defer func() {
		_ = session.Close()
		baseTransport.CloseIdleConnections()
	}()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: residentRestartTool})
	if err != nil || result == nil || result.IsError {
		t.Fatal("synthetic stored token could not call the private MCP fixture tool")
	}
}

func assertResidentRestartStoredTokenUnchanged(t *testing.T, path string, expected []byte, failureMessage string) {
	t.Helper()
	token, err := os.ReadFile(path)
	if err != nil || len(token) != mcpGatewayTokenSize {
		clear(token)
		t.Fatal("synthetic gateway token store did not contain the expected fixed-size value")
	}
	defer clear(token)
	if !bytes.Equal(expected, token) {
		t.Fatal(failureMessage)
	}
}

func privateResidentRestartHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
