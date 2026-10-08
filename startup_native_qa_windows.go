//go:build windows && manual_windows_startup_qa

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

const (
	startupQAChildEnv    = "LAH_STARTUP_NATIVE_QA_CHILD"
	startupQAStateEnv    = "LAH_STARTUP_NATIVE_QA_STATE_DIR"
	startupQATokenEnv    = "LAH_STARTUP_NATIVE_QA_TOKEN"
	startupQAStopName    = "shutdown.request"
	startupQAReadyName   = "ready.json"
	startupQAStoppedName = "stopped.json"
)

type startupQATokenProvider struct {
	token []byte
}

func (p *startupQATokenProvider) LoadGatewayToken() ([]byte, error) {
	if p == nil || len(p.token) != mcpGatewayTokenSize {
		return nil, errMCPGatewayTokenNotFound
	}
	return append([]byte(nil), p.token...), nil
}

func (p *startupQATokenProvider) SaveGatewayToken(token []byte) error {
	if p == nil || len(token) != mcpGatewayTokenSize {
		return errors.New("startup QA token provider rejected invalid data")
	}
	clear(p.token)
	p.token = append([]byte(nil), token...)
	return nil
}

func (p *startupQATokenProvider) DeleteGatewayToken() error {
	if p == nil {
		return errMCPGatewayTokenNotFound
	}
	clear(p.token)
	p.token = nil
	return nil
}

type startupQAProcessState struct {
	PID            int     `json:"pid"`
	Endpoint       string  `json:"endpoint"`
	ConsoleWindow  uintptr `json:"console_window"`
	ConsoleVisible bool    `json:"console_visible"`
	Stopped        bool    `json:"stopped,omitempty"`
}

func init() {
	if os.Getenv(startupQAChildEnv) == "1" {
		runResidentMCPEntrypoint = runStartupNativeQARuntime
	}
}

func runStartupNativeQARuntime() error {
	stateDir := os.Getenv(startupQAStateEnv)
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return errors.New("Native startup QA state directory is unavailable.")
	}
	tokenHex := os.Getenv(startupQATokenEnv)
	token, err := hex.DecodeString(tokenHex)
	tokenHex = ""
	if err != nil || len(token) != mcpGatewayTokenSize {
		clear(token)
		return errors.New("Native startup QA runtime credentials are unavailable.")
	}
	provider := &startupQATokenProvider{token: token}
	defer func() { clear(provider.token) }()
	server := newStartupQAMCPServer()
	runtime := newResidentMCPRuntimeController(provider, func() *mcp.Server { return server }, 0)
	ctx := context.Background()
	if err := runtime.Start(ctx); err != nil {
		return errors.New("Native startup QA runtime could not start.")
	}
	status, err := runtime.Status(ctx)
	if err != nil || status.State != mcpRuntimeRunning || status.MCPEndpoint == "" {
		shutdownStartupQARuntime(runtime)
		return errors.New("Native startup QA runtime health is unavailable.")
	}
	window, visible := startupQAConsoleVisibility()
	ready := startupQAProcessState{
		PID:            os.Getpid(),
		Endpoint:       status.MCPEndpoint,
		ConsoleWindow:  window,
		ConsoleVisible: visible,
	}
	if err := writeStartupQAState(filepath.Join(stateDir, startupQAReadyName), ready); err != nil {
		shutdownStartupQARuntime(runtime)
		return errors.New("Native startup QA readiness could not be recorded.")
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if _, err := os.Stat(filepath.Join(stateDir, startupQAStopName)); err != nil {
			continue
		}
		if err := shutdownStartupQARuntime(runtime); err != nil {
			return errors.New("Native startup QA runtime could not stop cleanly.")
		}
		stopped := startupQAProcessState{PID: os.Getpid(), Stopped: true}
		if err := writeStartupQAState(filepath.Join(stateDir, startupQAStoppedName), stopped); err != nil {
			return errors.New("Native startup QA stop state could not be recorded.")
		}
		return nil
	}
	return nil
}

func newStartupQAMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "local-agent-harness-startup-qa", Version: "1"}, nil)
	mcp.AddTool[struct{}, map[string]string](server, &mcp.Tool{Name: "startup_qa_health"}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		_ struct{},
	) (*mcp.CallToolResult, map[string]string, error) {
		return nil, map[string]string{"state": "running"}, nil
	})
	return server
}

func shutdownStartupQARuntime(runtime *residentMCPRuntimeController) error {
	ctx, cancel := context.WithTimeout(context.Background(), mcpHTTPShutdownTimeout)
	defer cancel()
	return runtime.Stop(ctx)
}

func startupQAConsoleVisibility() (uintptr, bool) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	window, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	if window == 0 {
		return 0, false
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	visible, _, _ := user32.NewProc("IsWindowVisible").Call(window)
	return window, visible != 0
}

func writeStartupQAState(path string, value startupQAProcessState) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporaryPath := path + ".tmp"
	if err := os.WriteFile(temporaryPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}
