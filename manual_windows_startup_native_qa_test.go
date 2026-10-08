//go:build windows && manual_windows_startup_qa

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

const startupNativeQAOptIn = "LAH_ENABLE_MANUAL_WINDOWS_STARTUP_NATIVE_QA"

func TestManualWindowsStartupNativeBackgroundAcceptance(t *testing.T) {
	if os.Getenv(startupNativeQAOptIn) != "1" {
		t.Skip("manual native startup QA is opt-in")
	}
	if runtime.GOOS != "windows" {
		t.Skip("manual native startup QA requires Windows")
	}

	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("could not create unique synthetic Run value name")
	}
	store := windowsRunLoginStartupStore{valueName: "LocalAgentHarnessStartupQA-" + hex.EncodeToString(suffix[:])}
	if _, exists, err := store.Read(); err != nil || exists {
		t.Fatalf("unique synthetic Run value preflight failed: exists=%t read-error=%t", exists, err != nil)
	}

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate the verified isolated source directory")
	}
	qaRoot := t.TempDir()
	binDir := filepath.Join(qaRoot, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal("could not create isolated QA binary directory")
	}
	productExe := filepath.Join(binDir, "local-agent-harness-startup-qa.exe")
	build := exec.Command("go", "build", "-tags=manual_windows_startup_qa", "-o", productExe, ".")
	build.Dir = workDir
	var buildOutput bytes.Buffer
	build.Stdout = &buildOutput
	build.Stderr = &buildOutput
	if err := build.Run(); err != nil {
		t.Fatalf("isolated QA executable build failed (Go %s; exit=%v)", runtime.Version(), err)
	}

	command := windows.EscapeArg(productExe) + " " + residentBackgroundArg
	controller, err := newManagedLoginStartupController(store, command)
	if err != nil {
		t.Fatal("could not construct controller for the unique synthetic Run value")
	}
	t.Cleanup(func() {
		value, exists, readErr := store.Read()
		if readErr != nil {
			t.Error("final synthetic Run value absence could not be verified")
			return
		}
		if !exists {
			return
		}
		if value != command {
			t.Error("synthetic Run value changed unexpectedly; leaving it untouched")
			return
		}
		if err := controller.Disable(); err != nil {
			t.Error("final synthetic Run value cleanup failed")
			return
		}
		if _, exists, readErr := store.Read(); readErr != nil || exists {
			t.Errorf("final synthetic Run value absence check failed: exists=%t read-error=%t", exists, readErr != nil)
		}
	})

	if err := controller.Enable(); err != nil {
		t.Fatal("native synthetic Run value registration failed")
	}
	value, exists, err := store.Read()
	if err != nil || !exists || value != command || controller.State() != loginStartupEnabled {
		t.Fatalf("native Run value readback failed: exists=%t read-error=%t state=%q", exists, err != nil, controller.State())
	}
	if err := controller.Enable(); err != nil {
		t.Fatal("native synthetic Run value idempotence check failed")
	}

	stateDir := filepath.Join(qaRoot, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal("could not create isolated QA state directory")
	}
	isolatedProfile := filepath.Join(qaRoot, "profile")
	for _, dir := range []string{
		isolatedProfile,
		filepath.Join(isolatedProfile, "AppData", "Local"),
		filepath.Join(isolatedProfile, "AppData", "Roaming"),
		filepath.Join(qaRoot, "temp"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal("could not create isolated child profile directory")
		}
	}
	var tokenBytes [mcpGatewayTokenSize]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		t.Fatal("could not create synthetic in-memory gateway token")
	}
	token := hex.EncodeToString(tokenBytes[:])
	defer clear(tokenBytes[:])

	child := exec.Command(productExe, "--resident", "--background")
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	child.Env = startupQAChildEnvironment(os.Environ(), map[string]string{
		startupQAChildEnv: "1",
		startupQAStateEnv: stateDir,
		startupQATokenEnv: token,
		"USERPROFILE":     isolatedProfile,
		"HOME":            isolatedProfile,
		"LOCALAPPDATA":    filepath.Join(isolatedProfile, "AppData", "Local"),
		"APPDATA":         filepath.Join(isolatedProfile, "AppData", "Roaming"),
		"TEMP":            filepath.Join(qaRoot, "temp"),
		"TMP":             filepath.Join(qaRoot, "temp"),
	})
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		t.Fatal("isolated background process could not start")
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	childExited := false
	var childExitErr error
	awaitExit := func(timeout time.Duration) bool {
		if childExited {
			return true
		}
		select {
		case childExitErr = <-done:
			childExited = true
			return true
		case <-time.After(timeout):
			return false
		}
	}
	t.Cleanup(func() {
		if !childExited {
			_ = os.WriteFile(filepath.Join(stateDir, startupQAStopName), []byte("stop"), 0o600)
			if !awaitExit(8 * time.Second) {
				_ = child.Process.Kill()
				_ = awaitExit(5 * time.Second)
			}
		}
		if !childExited {
			t.Error("owned QA child process could not be reaped")
		} else if childExitErr != nil {
			t.Error("owned QA child process exited with an error")
		}
	})

	ready, err := waitForStartupQAState(done, &childExited, &childExitErr, child, filepath.Join(stateDir, startupQAReadyName), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ready.PID != child.Process.Pid {
		t.Fatalf("reported background PID did not match owned child: got=%d", ready.PID)
	}
	if !strings.HasPrefix(ready.Endpoint, "http://127.0.0.1:") || !strings.HasSuffix(ready.Endpoint, "/mcp") {
		t.Fatal("background process did not report a private loopback MCP endpoint")
	}
	endpoint, err := url.Parse(ready.Endpoint)
	if err != nil || endpoint.Port() == "" || endpoint.Port() == "0" || endpoint.Port() == "49321" {
		t.Fatal("background QA process did not use an OS-assigned private port")
	}
	if ready.ConsoleWindow == 0 || ready.ConsoleVisible {
		t.Fatalf("background console visibility after native hide check: window=%t visible=%t", ready.ConsoleWindow != 0, ready.ConsoleVisible)
	}
	if ownerPID := startupQAConsoleOwner(ready.ConsoleWindow); ownerPID != uint32(child.Process.Pid) {
		t.Fatalf("console HWND was not owned by the isolated QA child: owner=%d", ownerPID)
	}
	if startupQAWindowVisible(ready.ConsoleWindow) {
		t.Fatal("background console remained visible according to IsWindowVisible")
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "startup-native-qa-client", Version: "1"}, nil)
	transport := &mcpHTTPBearerRoundTripper{base: &http.Transport{Proxy: nil}, token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ready.Endpoint,
		HTTPClient:           &http.Client{Transport: transport, Timeout: 8 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		cancel()
		t.Fatal("background resident MCP initialization failed")
	}
	result, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: "startup_qa_health"})
	_ = session.Close()
	cancel()
	if callErr != nil || result == nil || result.IsError {
		t.Fatalf("background resident MCP health call failed: result-present=%t error=%t", result != nil, callErr != nil)
	}
	select {
	case err := <-done:
		childExited = true
		childExitErr = err
		t.Fatal("background process exited before requested shutdown")
	default:
	}

	if err := os.WriteFile(filepath.Join(stateDir, startupQAStopName), []byte("stop"), 0o600); err != nil {
		t.Fatal("could not request shutdown of the owned QA child")
	}
	if !awaitExit(8*time.Second) || childExitErr != nil {
		if !childExited {
			_ = child.Process.Kill()
			_ = awaitExit(5 * time.Second)
		}
		t.Fatal("owned background process did not stop cleanly")
	}
	var stopped startupQAProcessState
	if err := readStartupQAState(filepath.Join(stateDir, startupQAStoppedName), &stopped); err != nil || !stopped.Stopped || stopped.PID != child.Process.Pid {
		t.Fatal("child runtime did not confirm graceful stop")
	}
	if port, err := net.Listen("tcp4", "127.0.0.1:"+endpoint.Port()); err != nil {
		t.Fatal("private background listener was not released after shutdown")
	} else {
		_ = port.Close()
	}

	if err := controller.Disable(); err != nil {
		t.Fatal("native synthetic Run value disable failed")
	}
	if _, exists, err := store.Read(); err != nil || exists || controller.State() != loginStartupDisabled {
		t.Fatalf("native Run value removal failed: exists=%t read-error=%t state=%q", exists, err != nil, controller.State())
	}
	for _, dir := range []string{
		filepath.Join(isolatedProfile, "AppData", "Local"),
		filepath.Join(isolatedProfile, "AppData", "Roaming"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal("isolated child profile directory could not be inspected")
		}
		if len(entries) != 0 {
			t.Fatal("isolated child profile received unexpected application files")
		}
	}
}

func startupQAChildEnvironment(base []string, overrides map[string]string) []string {
	filtered := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, "USERPROFILE") || strings.EqualFold(name, "HOME") ||
			strings.EqualFold(name, "LOCALAPPDATA") || strings.EqualFold(name, "APPDATA") ||
			strings.EqualFold(name, "TEMP") || strings.EqualFold(name, "TMP") ||
			strings.HasPrefix(strings.ToUpper(name), "LAH_STARTUP_NATIVE_QA_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	for name, value := range overrides {
		filtered = append(filtered, name+"="+value)
	}
	return filtered
}

func waitForStartupQAState(done <-chan error, exited *bool, exitErr *error, child *exec.Cmd, path string, timeout time.Duration) (startupQAProcessState, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var state startupQAProcessState
		if err := readStartupQAState(path, &state); err == nil {
			return state, nil
		}
		select {
		case *exitErr = <-done:
			*exited = true
			return startupQAProcessState{}, errorsNewStartupQAChildExit(*exitErr)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	return startupQAProcessState{}, errorsNewStartupQAReadyTimeout()
}

func readStartupQAState(path string, value *startupQAProcessState) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func startupQAConsoleOwner(window uintptr) uint32 {
	var processID uint32
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("GetWindowThreadProcessId")
	_, _, _ = proc.Call(window, uintptr(unsafe.Pointer(&processID)))
	return processID
}

func startupQAWindowVisible(window uintptr) bool {
	visible, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible").Call(window)
	return visible != 0
}

func errorsNewStartupQAChildExit(err error) error {
	if err == nil {
		return errors.New("owned background child exited before becoming ready")
	}
	return errors.New("owned background child exited before becoming ready")
}

func errorsNewStartupQAReadyTimeout() error {
	return errors.New("owned background child did not become ready before timeout")
}
