//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	residentNativeQAGateEnv  = "LAH_ENABLE_WINDOWS_RESIDENT_NATIVE_QA"
	residentNativeQAChildEnv = "LAH_WINDOWS_RESIDENT_NATIVE_QA_CHILD"
	residentNativeQATargetNS = "LocalAgentHarness/MCPGateway/native-qa/"
	residentNativeQATimeout  = 90 * time.Second
	residentNativeQAMaxLine  = 4096
)

type residentNativeQAChildInit struct {
	Target     string `json:"target"`
	ConfigPath string `json:"config_path"`
}

type residentNativeQACredentialProvider struct {
	target string
}

func newResidentNativeQACredentialProvider(target string) (*residentNativeQACredentialProvider, bool) {
	if !validResidentNativeQATarget(target) {
		return nil, false
	}
	return &residentNativeQACredentialProvider{target: target}, true
}

func (p *residentNativeQACredentialProvider) LoadGatewayToken() ([]byte, error) {
	if p == nil || !validResidentNativeQATarget(p.target) {
		return nil, errMCPGatewayTokenStore
	}
	return loadMCPGatewayTokenCredential(p.target)
}

func (p *residentNativeQACredentialProvider) SaveGatewayToken(token []byte) error {
	if p == nil || !validResidentNativeQATarget(p.target) {
		return errMCPGatewayTokenStore
	}
	return saveMCPGatewayTokenCredential(p.target, token)
}

func (p *residentNativeQACredentialProvider) DeleteGatewayToken() error {
	if p == nil || !validResidentNativeQATarget(p.target) {
		return errMCPGatewayTokenStore
	}
	return deleteMCPGatewayTokenCredential(p.target)
}

func validResidentNativeQATarget(target string) bool {
	suffix := strings.TrimPrefix(target, residentNativeQATargetNS)
	if suffix == target || len(suffix) != 32 || strings.ContainsAny(suffix, "/\\\x00") {
		return false
	}
	decoded, err := hex.DecodeString(suffix)
	if err != nil {
		return false
	}
	defer clear(decoded)
	return hex.EncodeToString(decoded) == suffix
}

func TestWindowsResidentGatewayNativeQA(t *testing.T) {
	if os.Getenv(residentNativeQAGateEnv) != "1" {
		t.Skip("opt-in native Windows Credential Manager lifecycle test")
	}

	testCtx, cancel := context.WithTimeout(context.Background(), residentNativeQATimeout)
	defer cancel()

	target, err := newResidentNativeQATarget()
	if err != nil {
		t.Fatal("could not create a synthetic test credential target")
	}
	provider, ok := newResidentNativeQACredentialProvider(target)
	if !ok {
		t.Fatal("synthetic test credential target was invalid")
	}
	if existing, loadErr := provider.LoadGatewayToken(); !errors.Is(loadErr, errMCPGatewayTokenNotFound) {
		clear(existing)
		t.Fatal("synthetic Credential Manager target was not confirmed absent")
	}
	cleanupRegistered := false
	t.Cleanup(func() {
		if !cleanupRegistered {
			return
		}
		if err := provider.DeleteGatewayToken(); err != nil && !errors.Is(err, errMCPGatewayTokenNotFound) {
			t.Error("synthetic Credential Manager cleanup failed")
		}
		leftover, err := provider.LoadGatewayToken()
		clear(leftover)
		if !errors.Is(err, errMCPGatewayTokenNotFound) {
			t.Error("synthetic Credential Manager cleanup could not be verified")
		}
	})
	cleanupRegistered = true

	configPath := filepath.Join(t.TempDir(), "native-qa-config.json")
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary test configuration path was not absent")
	}
	t.Cleanup(func() {
		if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
			t.Error("native gateway test changed its temporary configuration file")
		}
	})

	child, err := startResidentNativeQAChild(testCtx, target, configPath)
	if err != nil {
		t.Fatal("could not start the isolated resident test process")
	}
	defer child.stopAndWait()
	endpoint := child.endpoint(t)

	firstToken := loadResidentNativeQAToken(t, provider)
	secondToken := []byte(nil)
	defer func() {
		clear(firstToken)
		clear(secondToken)
	}()
	if !residentNativeQASDKCall(endpoint, firstToken) {
		t.Fatalf("actual MCP SDK request with the provisioned Credential Manager token failed (HTTP status %d)", residentNativeQAHTTPStatus(endpoint, firstToken))
	}
	child.command(t, "CANCEL_PROBE", "CANCELLED")

	child.command(t, "ROTATE", "ROTATED")
	secondToken = loadResidentNativeQAToken(t, provider)
	if bytes.Equal(firstToken, secondToken) {
		t.Fatal("gateway token rotation did not replace the stored token")
	}
	if residentNativeQAHTTPStatus(endpoint, firstToken) != http.StatusUnauthorized {
		t.Fatal("the previous gateway token remained accepted after rotation")
	}
	if !residentNativeQASDKCall(endpoint, secondToken) {
		t.Fatal("actual MCP SDK request with the rotated token failed")
	}
	port := child.port(t)
	child.command(t, "STOP", "STOPPED")
	if err := child.wait(); err != nil {
		t.Fatal("isolated resident process did not stop cleanly")
	}
	assertResidentNativeQAPortReleased(t, port)
	stoppedToken := loadResidentNativeQAToken(t, provider)
	if !bytes.Equal(secondToken, stoppedToken) {
		clear(stoppedToken)
		t.Fatal("stopping the resident unexpectedly changed its persisted token")
	}
	clear(stoppedToken)

	child, err = startResidentNativeQAChild(testCtx, target, configPath)
	if err != nil {
		t.Fatal("could not restart the resident in a fresh process")
	}
	defer child.stopAndWait()
	endpoint = child.endpoint(t)
	if !residentNativeQASDKCall(endpoint, secondToken) {
		t.Fatal("fresh process did not reload and accept the persisted gateway token")
	}
	if residentNativeQAHTTPStatus(endpoint, firstToken) != http.StatusUnauthorized {
		t.Fatal("fresh process accepted a gateway token superseded before restart")
	}

	port = child.port(t)
	child.command(t, "REVOKE", "REVOKED")
	if err := child.wait(); err != nil {
		t.Fatal("isolated resident process did not revoke and exit cleanly")
	}
	assertResidentNativeQAPortReleased(t, port)
	if residentNativeQAHTTPStatus(endpoint, secondToken) != 0 {
		t.Fatal("revoked resident endpoint still accepted an HTTP request")
	}
	if token, err := provider.LoadGatewayToken(); !errors.Is(err, errMCPGatewayTokenNotFound) {
		clear(token)
		t.Fatal("revocation left a synthetic Credential Manager token behind")
	}
}

func TestWindowsResidentGatewayNativeQAChild(t *testing.T) {
	if os.Getenv(residentNativeQAGateEnv) != "1" || os.Getenv(residentNativeQAChildEnv) != "1" {
		t.Skip("private helper process only")
	}
	runResidentNativeQAChild(t)
}

func newResidentNativeQATarget() (string, error) {
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	defer clear(suffix)
	return residentNativeQATargetNS + hex.EncodeToString(suffix), nil
}

func loadResidentNativeQAToken(t *testing.T, provider gatewayTokenCredentialProvider) []byte {
	t.Helper()
	token, err := provider.LoadGatewayToken()
	if err != nil || len(token) != mcpGatewayTokenSize {
		clear(token)
		t.Fatal("synthetic Credential Manager token could not be read")
	}
	return token
}

type residentNativeQAChildProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	done    chan struct{}
	waitErr error
	portNum int
}

func startResidentNativeQAChild(ctx context.Context, target, configPath string) (*residentNativeQAChildProcess, error) {
	if ctx == nil || !validResidentNativeQATarget(target) || configPath == "" {
		return nil, errors.New("invalid helper startup data")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestWindowsResidentGatewayNativeQAChild$")
	cmd.Env = residentNativeQAChildEnvironment()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	child := &residentNativeQAChildProcess{
		cmd:   cmd,
		stdin: stdin,
		lines: make(chan string, 8),
		done:  make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	go child.readOutput(stdout)
	go func() {
		child.waitErr = cmd.Wait()
		close(child.done)
	}()
	initData, err := json.Marshal(residentNativeQAChildInit{Target: target, ConfigPath: configPath})
	if err != nil {
		child.abort()
		return nil, err
	}
	if len(initData) > 8192 {
		child.abort()
		return nil, errors.New("helper startup data exceeded limit")
	}
	if _, err := stdin.Write(append(initData, '\n')); err != nil {
		child.abort()
		return nil, err
	}
	line, err := child.readLine(ctx)
	if err != nil || !strings.HasPrefix(line, "READY ") {
		child.abort()
		return nil, errors.New("helper did not become ready")
	}
	port, err := strconv.Atoi(strings.TrimPrefix(line, "READY "))
	if err != nil || port < 1 || port > 65535 {
		child.abort()
		return nil, errors.New("helper reported an invalid ephemeral port")
	}
	child.portNum = port
	return child, nil
}

func residentNativeQAChildEnvironment() []string {
	var env []string
	for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, residentNativeQAGateEnv+"=1", residentNativeQAChildEnv+"=1")
	return env
}

func (p *residentNativeQAChildProcess) readOutput(stdout io.ReadCloser) {
	defer close(p.lines)
	defer stdout.Close()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256), residentNativeQAMaxLine)
	for scanner.Scan() {
		p.lines <- scanner.Text()
	}
}

func (p *residentNativeQAChildProcess) readLine(ctx context.Context) (string, error) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case line, ok := <-p.lines:
		if !ok {
			return "", errors.New("helper exited before lifecycle response")
		}
		return line, nil
	case <-timer.C:
		return "", errors.New("helper lifecycle response timed out")
	case <-ctx.Done():
		return "", errors.New("helper lifecycle response canceled")
	}
}

func (p *residentNativeQAChildProcess) command(t *testing.T, command, expected string) {
	t.Helper()
	if command != "ROTATE" && command != "STOP" && command != "REVOKE" && command != "CANCEL_PROBE" {
		t.Fatal("invalid helper lifecycle command")
	}
	if _, err := io.WriteString(p.stdin, command+"\n"); err != nil {
		t.Fatal("could not send helper lifecycle command")
	}
	line, err := p.readLine(context.Background())
	if err != nil || line != expected {
		t.Fatalf("helper did not confirm %s", command)
	}
}

func (p *residentNativeQAChildProcess) endpoint(t *testing.T) string {
	t.Helper()
	if p == nil || p.portNum < 1 || p.portNum > 65535 {
		t.Fatal("helper endpoint was invalid")
	}
	return "http://127.0.0.1:" + strconv.Itoa(p.portNum) + mcpHTTPPath
}

func (p *residentNativeQAChildProcess) port(t *testing.T) int {
	t.Helper()
	if p == nil || p.portNum < 1 || p.portNum > 65535 {
		t.Fatal("helper endpoint was invalid")
	}
	return p.portNum
}

func (p *residentNativeQAChildProcess) wait() error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.waitErr
	case <-timer.C:
		_ = p.terminate()
	}
	secondTimer := time.NewTimer(10 * time.Second)
	defer secondTimer.Stop()
	select {
	case <-p.done:
		return errors.New("helper exit timed out and was terminated")
	case <-secondTimer.C:
		return errors.New("helper process could not be terminated")
	}
}

func (p *residentNativeQAChildProcess) terminate() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if p.cmd.ProcessState != nil {
		return nil
	}
	_ = p.stdin.Close()
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *residentNativeQAChildProcess) stopAndWait() {
	if p == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = ioWriteStop(p.stdin)
	if err := p.wait(); err != nil {
		_ = p.terminate()
	}
}

func (p *residentNativeQAChildProcess) abort() {
	_ = p.terminate()
	_ = p.wait()
}

func ioWriteStop(stdin io.Writer) error {
	_, err := io.WriteString(stdin, "STOP\n")
	return err
}

func assertResidentNativeQAPortReleased(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatal("resident did not release its owned ephemeral loopback port")
	}
	_ = listener.Close()
}

func residentNativeQAHTTPStatus(endpoint string, token []byte) int {
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
	))
	if err != nil {
		return 0
	}
	request.Header.Set("Authorization", "Bearer "+hex.EncodeToString(token))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	return response.StatusCode
}

func residentNativeQASDKCall(endpoint string, token []byte) bool {
	client := mcp.NewClient(&mcp.Implementation{Name: "native-resident-qa", Version: "1"}, nil)
	base := &http.Transport{Proxy: nil}
	defer base.CloseIdleConnections()
	transport := &residentNativeQABearerRoundTripper{base: base, token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 8 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return false
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	return err == nil && result != nil && !result.IsError
}

func runResidentNativeQAChild(t *testing.T) {
	t.Helper()
	fail := func() {
		_, _ = fmt.Fprintln(os.Stdout, "ERROR")
		t.Fail()
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 256), 8192)
	if !scanner.Scan() {
		fail()
		return
	}
	var initData residentNativeQAChildInit
	decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&initData) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validResidentNativeQATarget(initData.Target) || initData.ConfigPath == "" {
		fail()
		return
	}
	if _, err := os.Stat(initData.ConfigPath); !errors.Is(err, os.ErrNotExist) {
		fail()
		return
	}
	provider, ok := newResidentNativeQACredentialProvider(initData.Target)
	if !ok {
		fail()
		return
	}
	var waitStarted sync.Once
	waitToolStarted := make(chan struct{})
	waitToolCanceled := make(chan struct{})
	var waitCanceled sync.Once
	application := &app{configPath: initData.ConfigPath, client: &http.Client{Timeout: 8 * time.Second}}
	server := application.mcpServer()
	mcp.AddTool[struct{}, struct{}](server, &mcp.Tool{Name: "native_qa_wait"}, func(
		ctx context.Context,
		_ *mcp.CallToolRequest,
		_ struct{},
	) (*mcp.CallToolResult, struct{}, error) {
		waitStarted.Do(func() { close(waitToolStarted) })
		select {
		case <-ctx.Done():
			waitCanceled.Do(func() { close(waitToolCanceled) })
			return nil, struct{}{}, ctx.Err()
		case <-time.After(15 * time.Second):
			return nil, struct{}{}, errors.New("native QA wait expired")
		}
	})
	controller := newResidentMCPRuntimeController(provider, func() *mcp.Server { return server }, 0)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	err := controller.Start(startCtx)
	cancelStart()
	if err != nil {
		fail()
		return
	}
	status, err := controller.Status(context.Background())
	if err != nil {
		fail()
		return
	}
	endpointURL := status.MCPEndpoint
	if !strings.HasPrefix(endpointURL, "http://127.0.0.1:") || !strings.HasSuffix(endpointURL, mcpHTTPPath) {
		fail()
		return
	}
	_, portText, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(endpointURL, "http://"), mcpHTTPPath))
	if err != nil {
		fail()
		return
	}
	if _, err := net.LookupPort("tcp", portText); err != nil {
		fail()
		return
	}
	if _, err := fmt.Fprintln(os.Stdout, "READY "+portText); err != nil {
		fail()
		return
	}
	for scanner.Scan() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		switch scanner.Text() {
		case "ROTATE":
			err = controller.RotateGatewayToken(ctx)
			cancel()
			if err != nil {
				fail()
				return
			}
			_, _ = fmt.Fprintln(os.Stdout, "ROTATED")
		case "CANCEL_PROBE":
			cancel()
			if !residentNativeQACancellationProbe(endpointURL, provider, waitToolStarted, waitToolCanceled) {
				fail()
				return
			}
			_, _ = fmt.Fprintln(os.Stdout, "CANCELLED")
		case "STOP":
			err = controller.Stop(ctx)
			cancel()
			if err != nil || !residentNativeQAEndpointReleased(portText) {
				fail()
				return
			}
			_, _ = fmt.Fprintln(os.Stdout, "STOPPED")
			return
		case "REVOKE":
			err = controller.RevokeGatewayToken(ctx)
			cancel()
			if err != nil || !residentNativeQATokenAbsent(provider) || !residentNativeQAEndpointReleased(portText) {
				fail()
				return
			}
			_, _ = fmt.Fprintln(os.Stdout, "REVOKED")
			return
		default:
			cancel()
			fail()
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
	defer cancel()
	_ = controller.Stop(ctx)
}

func residentNativeQACancellationProbe(endpoint string, provider gatewayTokenCredentialProvider, started, canceled <-chan struct{}) bool {
	token, err := provider.LoadGatewayToken()
	if err != nil || len(token) != mcpGatewayTokenSize {
		clear(token)
		return false
	}
	defer clear(token)
	base := &http.Transport{Proxy: nil}
	defer base.CloseIdleConnections()
	transport := &residentNativeQABearerRoundTripper{base: base, token: token}
	client := mcp.NewClient(&mcp.Implementation{Name: "native-resident-cancel-qa", Version: "1"}, nil)
	sessionCtx, cancelSession := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancelSession()
	session, err := client.Connect(sessionCtx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 8 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return false
	}
	defer session.Close()
	callCtx, cancelCall := context.WithCancel(sessionCtx)
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = session.CallTool(callCtx, &mcp.CallToolParams{Name: "native_qa_wait"})
	}()
	select {
	case <-started:
		cancelCall()
	case <-time.After(8 * time.Second):
		cancelCall()
		return false
	}
	select {
	case <-canceled:
	case <-time.After(8 * time.Second):
		return false
	}
	select {
	case <-callDone:
	case <-time.After(8 * time.Second):
		return false
	}
	return true
}

type residentNativeQABearerRoundTripper struct {
	base  http.RoundTripper
	token []byte
}

func (t *residentNativeQABearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil || len(t.token) != mcpGatewayTokenSize {
		return nil, errors.New("test HTTP transport is unavailable")
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	encoded := hex.EncodeToString(t.token)
	clone.Header.Set("Authorization", "Bearer "+encoded)
	response, err := t.base.RoundTrip(clone)
	clone.Header.Del("Authorization")
	return response, err
}

func residentNativeQATokenAbsent(provider gatewayTokenCredentialProvider) bool {
	token, err := provider.LoadGatewayToken()
	clear(token)
	return errors.Is(err, errMCPGatewayTokenNotFound)
}

func residentNativeQAEndpointReleased(portText string) bool {
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return false
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return false
	}
	return listener.Close() == nil
}
