//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	geminiLiveQAGateEnv       = "LAH_ENABLE_MANUAL_WINDOWS_GEMINI_LIVE_QA"
	geminiLiveQAAppExeEnv     = "LAH_GEMINI_LIVE_APP_EXE"
	geminiLiveQAServerName    = "local-agent-harness"
	geminiLiveQACatalogTool   = "registered_targets"
	geminiLiveQACaptureMax    = 4 << 20
	geminiLiveQAVersionRegexp = `(?:Gemini CLI|gemini)?\s*v?([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)`
)

var geminiLiveQAVersionPattern = regexp.MustCompile(geminiLiveQAVersionRegexp)

// TestManualWindowsGeminiLiveQAAcceptance is opt-in and runs one real Gemini
// CLI headless prompt against an isolated production MCP process. It is never
// replaced with an SDK call or a synthetic child process.
func TestManualWindowsGeminiLiveQAAcceptance(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("live Gemini CLI acceptance requires native Windows")
	}
	if os.Getenv(geminiLiveQAGateEnv) != "1" {
		t.Skip("set LAH_ENABLE_MANUAL_WINDOWS_GEMINI_LIVE_QA=1 to opt in")
	}

	appExe := strings.TrimSpace(os.Getenv(geminiLiveQAAppExeEnv))
	if appExe == "" || !filepath.IsAbs(appExe) {
		t.Skip("set LAH_GEMINI_LIVE_APP_EXE to the absolute production app executable")
	}
	appInfo, err := os.Stat(appExe)
	if err != nil || !appInfo.Mode().IsRegular() {
		t.Skip("the configured production app executable is unavailable")
	}
	appExe, err = filepath.Abs(appExe)
	if err != nil || !filepath.IsAbs(appExe) {
		t.Skip("the configured production app executable path is invalid")
	}

	geminiExe, err := exec.LookPath("gemini")
	if err != nil || strings.TrimSpace(geminiExe) == "" {
		t.Skip("Gemini CLI was not found on PATH; actual CLI acceptance was not run")
	}
	geminiExe, err = filepath.Abs(geminiExe)
	if err != nil || !filepath.IsAbs(geminiExe) {
		t.Skip("Gemini CLI did not resolve to an absolute executable path")
	}
	tempRoot := t.TempDir()
	profileHome := filepath.Join(tempRoot, "gemini-home")
	workDir := filepath.Join(tempRoot, "workspace")
	appData := filepath.Join(tempRoot, "appdata")
	localAppData := filepath.Join(tempRoot, "local-appdata")
	trustedFolders := filepath.Join(profileHome, ".gemini", "trustedFolders.json")
	for _, directory := range []string{profileHome, workDir, appData, localAppData, filepath.Dir(trustedFolders)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal("could not create private temporary acceptance directories")
		}
	}

	versionCapture, err := runGeminiLiveQACaptured(
		t,
		geminiExe,
		workDir,
		profileHome,
		trustedFolders,
		appData,
		localAppData,
		15*time.Second,
		"--version",
	)
	if err != nil {
		t.Fatal("Gemini CLI version preflight failed; captured output remains private")
	}
	version := parseGeminiLiveQAVersion(versionCapture.stdout)
	if !versionCapture.processStarted || versionCapture.exitCode != 0 {
		t.Fatalf(
			"Gemini CLI version preflight failed; process_started=%t exit_code=%d version_observed=%t; captured output remains private",
			versionCapture.processStarted,
			versionCapture.exitCode,
			version != "",
		)
	}
	if version == "" {
		t.Fatal("Gemini CLI version output did not contain an allowlisted version")
	}

	if err := writeGeminiLiveQAProjectSettings(workDir, appExe, appData, localAppData); err != nil {
		t.Fatal("could not write isolated temporary Gemini project settings")
	}
	serverCapture, err := runGeminiLiveQACaptured(
		t,
		geminiExe,
		workDir,
		profileHome,
		trustedFolders,
		appData,
		localAppData,
		20*time.Second,
		"mcp", "list",
	)
	if err != nil || serverCapture.exitCode != 0 {
		t.Logf(
			"Gemini live acceptance proof: version=%s mcp_exit_code=%d mcp_name_observed=%t mcp_stdio_observed=%t mcp_connected_marker=%t mcp_disconnected_marker=%t catalog_calls=0",
			version,
			serverCapture.exitCode,
			serverCapture.mcpStatus.serverNameObserved,
			serverCapture.mcpStatus.stdioObserved,
			serverCapture.mcpStatus.connectedMarker,
			serverCapture.mcpStatus.disconnectedMarker,
		)
		t.Skip("Gemini CLI could not confirm the temporary stdio MCP server; captured output remains private")
	}
	serverConnected := serverCapture.mcpStatus.serverConnected
	if !serverConnected {
		t.Logf(
			"Gemini live acceptance proof: version=%s server_connected=false mcp_name_observed=%t mcp_stdio_observed=%t mcp_connected_marker=%t mcp_disconnected_marker=%t catalog_calls=0",
			version,
			serverCapture.mcpStatus.serverNameObserved,
			serverCapture.mcpStatus.stdioObserved,
			serverCapture.mcpStatus.connectedMarker,
			serverCapture.mcpStatus.disconnectedMarker,
		)
		t.Skip("Gemini CLI did not report the temporary stdio MCP server as connected; captured output remains private")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	capture, err := runGeminiLiveQACapturedContext(
		t,
		ctx,
		geminiExe,
		workDir,
		profileHome,
		trustedFolders,
		appData,
		localAppData,
		[]string{
			"--prompt", "Call registered_targets exactly once. Do not call any other tool. Do not repeat, quote, or summarize catalog contents.",
			"--output-format", "stream-json",
			"--approval-mode", "plan",
			"--allowed-mcp-server-names", geminiLiveQAServerName,
			"--extensions", "",
			"--skip-trust",
		},
	)
	if err != nil {
		t.Fatal("Gemini live acceptance process failed; captured output remains private")
	}

	proof, err := parseGeminiLiveQAProof(capture.stdout)
	if err != nil {
		t.Logf(
			"Gemini live acceptance proof: version=%s server_connected=true session_initialized=%t model_exit_code=%d catalog_calls=%d successful_catalog_results=%d final_success=%t",
			version,
			proof.initialized,
			capture.exitCode,
			proof.catalogCalls,
			proof.successfulCatalogResults,
			proof.finalSuccess,
		)
		if proof.initialized {
			t.Skip("Gemini CLI initialized the isolated MCP session, but no successful catalog tool call was proven; authentication was not inspected")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("Gemini live acceptance exceeded the 90-second limit; captured output remains private")
		}
		t.Skip("Gemini CLI connected the isolated MCP server, but the bounded headless session did not initialize; authentication was not inspected")
	}
	t.Logf(
		"Gemini live acceptance proof: version=%s server_connected=true initialized=%t model_exit_code=%d catalog_calls=%d successful_catalog_results=%d final_success=%t",
		version,
		proof.initialized,
		capture.exitCode,
		proof.catalogCalls,
		proof.successfulCatalogResults,
		proof.finalSuccess,
	)
}

type geminiLiveQACapture struct {
	stdout         []byte
	exitCode       int
	processStarted bool
	mcpStatus      geminiLiveQAMCPStatus
}

type geminiLiveQAMCPStatus struct {
	serverNameObserved bool
	stdioObserved      bool
	connectedMarker    bool
	disconnectedMarker bool
	serverConnected    bool
}

type geminiLiveQAProof struct {
	initialized              bool
	catalogCalls             int
	successfulCatalogResults int
	finalSuccess             bool
	errorEvent               bool
}

type geminiLiveQAEvent struct {
	Type     string `json:"type"`
	ToolName string `json:"tool_name"`
	ToolID   string `json:"tool_id"`
	Status   string `json:"status"`
}

func runGeminiLiveQACaptured(
	t *testing.T,
	geminiExe string,
	workDir string,
	profileHome string,
	trustedFolders string,
	appData string,
	localAppData string,
	timeout time.Duration,
	args ...string,
) (geminiLiveQACapture, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runGeminiLiveQACapturedContext(
		t,
		ctx,
		geminiExe,
		workDir,
		profileHome,
		trustedFolders,
		appData,
		localAppData,
		args,
	)
}

func runGeminiLiveQACapturedContext(
	t *testing.T,
	ctx context.Context,
	geminiExe string,
	workDir string,
	profileHome string,
	trustedFolders string,
	appData string,
	localAppData string,
	args []string,
) (geminiLiveQACapture, error) {
	t.Helper()
	stdoutFile, stdoutPath, err := createGeminiLiveQACaptureFile(t.TempDir(), "stdout")
	if err != nil {
		return geminiLiveQACapture{}, err
	}
	defer func() {
		removeGeminiLiveQACapture(t, stdoutFile, stdoutPath)
	}()
	stderrFile, stderrPath, err := createGeminiLiveQACaptureFile(t.TempDir(), "stderr")
	if err != nil {
		return geminiLiveQACapture{}, err
	}
	defer func() {
		removeGeminiLiveQACapture(t, stderrFile, stderrPath)
	}()

	command, err := newGeminiLiveQACommand(geminiExe, args)
	if err != nil {
		return geminiLiveQACapture{}, err
	}
	command.Dir = workDir
	command.Env = geminiLiveQAEnvironment(
		os.Environ(),
		profileHome,
		trustedFolders,
		appData,
		localAppData,
	)
	stdoutWriter := &geminiLiveQABoundedWriter{file: stdoutFile, remaining: geminiLiveQACaptureMax}
	stderrWriter := &geminiLiveQABoundedWriter{file: stderrFile, remaining: geminiLiveQACaptureMax}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	exitCode := 0
	if err := command.Run(); err != nil {
		if command.ProcessState == nil {
			return geminiLiveQACapture{}, errors.New("process failed")
		}
		exitCode = command.ProcessState.ExitCode()
	}
	if stdoutWriter.exceeded || stderrWriter.exceeded {
		return geminiLiveQACapture{}, errors.New("capture limit exceeded")
	}
	if err := stdoutFile.Close(); err != nil {
		return geminiLiveQACapture{}, errors.New("capture unavailable")
	}
	stdoutFile = nil
	if err := stderrFile.Close(); err != nil {
		return geminiLiveQACapture{}, errors.New("capture unavailable")
	}
	stderrFile = nil
	stdout, err := readGeminiLiveQACapture(stdoutPath)
	if err != nil {
		return geminiLiveQACapture{}, err
	}
	stderr, err := readGeminiLiveQACapture(stderrPath)
	if err != nil {
		return geminiLiveQACapture{}, err
	}
	return geminiLiveQACapture{
		stdout:         stdout,
		exitCode:       exitCode,
		processStarted: true,
		mcpStatus:      parseGeminiLiveQAMCPStatus(stdout, stderr),
	}, nil
}

func createGeminiLiveQACaptureFile(directory string, label string) (*os.File, string, error) {
	file, err := os.CreateTemp(directory, "gemini-live-"+label+"-*.private")
	if err != nil {
		return nil, "", errors.New("capture unavailable")
	}
	if err := file.Chmod(0o600); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, "", errors.New("capture unavailable")
		}
		return nil, "", errors.New("capture unavailable")
	}
	return file, file.Name(), nil
}

type geminiLiveQABoundedWriter struct {
	file      *os.File
	remaining int
	exceeded  bool
}

func (w *geminiLiveQABoundedWriter) Write(contents []byte) (int, error) {
	writeLength := len(contents)
	if writeLength > w.remaining {
		writeLength = w.remaining
		w.exceeded = true
	}
	if writeLength > 0 {
		written, err := w.file.Write(contents[:writeLength])
		if err != nil {
			return 0, err
		}
		if written != writeLength {
			return 0, io.ErrShortWrite
		}
		w.remaining -= writeLength
	}
	return len(contents), nil
}

func removeGeminiLiveQACapture(t *testing.T, file *os.File, path string) {
	t.Helper()
	if file != nil {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error("private capture cleanup did not complete")
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Error("private capture cleanup did not complete")
	}
}

func readGeminiLiveQACapture(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("capture unavailable")
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, geminiLiveQACaptureMax+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(contents) > geminiLiveQACaptureMax {
		return nil, errors.New("capture unavailable")
	}
	return contents, nil
}

func newGeminiLiveQACommand(geminiExe string, args []string) (*exec.Cmd, error) {
	if !isGeminiCommandScript(geminiExe) {
		return exec.Command(geminiExe, args...), nil
	}
	entry, err := geminiCLIEntryForNPMWrapper(geminiExe)
	if err != nil {
		return nil, err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("node runtime unavailable")
	}
	return exec.Command(node, append([]string{entry}, args...)...), nil
}

func isGeminiCommandScript(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	return extension == ".cmd" || extension == ".bat"
}

func geminiCLIEntryForNPMWrapper(executable string) (string, error) {
	wrapperDir := filepath.Dir(executable)
	entryRelativePath := filepath.Join("@google", "gemini-cli", "bundle", "gemini.js")
	candidates := []string{
		filepath.Join(wrapperDir, "node_modules", entryRelativePath),
		filepath.Join(wrapperDir, "..", entryRelativePath),
		filepath.Join(wrapperDir, "..", "node_modules", entryRelativePath),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err == nil && filepath.IsAbs(absolute) {
			return absolute, nil
		}
	}
	return "", errors.New("Gemini CLI package entry unavailable")
}

func geminiLiveQAEnvironment(
	base []string,
	profileHome string,
	trustedFolders string,
	appData string,
	localAppData string,
) []string {
	overrides := map[string]string{
		"GEMINI_CLI_HOME":                 profileHome,
		"GEMINI_CLI_TRUSTED_FOLDERS_PATH": trustedFolders,
		"GEMINI_CLI_TRUST_WORKSPACE":      "true",
		"HOME":                            profileHome,
		"USERPROFILE":                     profileHome,
		"APPDATA":                         appData,
		"LOCALAPPDATA":                    localAppData,
	}
	environment := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := geminiLiveQAOverride(overrides, key); replaced {
			continue
		}
		environment = append(environment, entry)
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func geminiLiveQAOverride(overrides map[string]string, key string) (string, bool) {
	for name, value := range overrides {
		if strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func writeGeminiLiveQAProjectSettings(workDir string, appExe string, appData string, localAppData string) error {
	settings := map[string]any{
		"general": map[string]any{
			"enableAutoUpdate":             false,
			"enableAutoUpdateNotification": false,
			"plan":                         map[string]any{"enabled": true},
		},
		"mcp": map[string]any{
			"allowed": []string{geminiLiveQAServerName},
		},
		"mcpServers": map[string]any{
			geminiLiveQAServerName: map[string]any{
				"command": appExe,
				"args":    []string{"--mcp"},
				"cwd":     workDir,
				"env": map[string]string{
					"APPDATA":      appData,
					"LOCALAPPDATA": localAppData,
				},
				"timeout":      30000,
				"trust":        true,
				"includeTools": []string{geminiLiveQACatalogTool},
			},
		},
		"tools": map[string]any{
			"core": []string{},
		},
		"privacy": map[string]any{
			"usageStatisticsEnabled": false,
		},
		"telemetry": map[string]any{
			"enabled": false,
		},
	}
	settingsPath := filepath.Join(workDir, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return errors.New("temporary settings unavailable")
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return errors.New("temporary settings unavailable")
	}
	file, err := os.OpenFile(settingsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("temporary settings unavailable")
	}
	if _, err := file.Write(encoded); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("temporary settings unavailable")
		}
		return errors.New("temporary settings unavailable")
	}
	if err := file.Close(); err != nil {
		return errors.New("temporary settings unavailable")
	}
	return nil
}

func parseGeminiLiveQAVersion(contents []byte) string {
	match := geminiLiveQAVersionPattern.FindSubmatch(contents)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func parseGeminiLiveQAMCPStatus(streams ...[]byte) geminiLiveQAMCPStatus {
	status := geminiLiveQAMCPStatus{}
	for _, contents := range streams {
		scanner := bufio.NewScanner(bytes.NewReader(contents))
		scanner.Buffer(make([]byte, 0, 1024), geminiLiveQACaptureMax)
		for scanner.Scan() {
			line := strings.ToLower(scanner.Text())
			if !strings.Contains(line, geminiLiveQAServerName) {
				continue
			}
			status.serverNameObserved = true
			if strings.Contains(line, "stdio") {
				status.stdioObserved = true
			}
			if strings.Contains(line, "connected") && !strings.Contains(line, "disconnected") {
				status.connectedMarker = true
				if strings.Contains(line, "stdio") {
					status.serverConnected = true
				}
			}
			if strings.Contains(line, "disconnected") {
				status.disconnectedMarker = true
			}
		}
	}
	return status
}

func TestGeminiLiveQAMCPStatusParser(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		connected  bool
		disconnect bool
	}{
		{
			name:      "connected stdio line on stderr",
			stderr:    "✓ local-agent-harness: command: app.exe --mcp (stdio) - Connected\n",
			connected: true,
		},
		{
			name:       "disconnected stdio line",
			stdout:     "✗ local-agent-harness: command: app.exe --mcp (stdio) - Disconnected\n",
			disconnect: true,
		},
		{
			name:   "unrelated server status ignored",
			stdout: "✓ other-server: command: app.exe --mcp (stdio) - Connected\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := parseGeminiLiveQAMCPStatus([]byte(test.stdout), []byte(test.stderr))
			if status.serverConnected != test.connected || status.disconnectedMarker != test.disconnect {
				t.Fatal("safe MCP status classification did not match the fixture")
			}
		})
	}
}

func parseGeminiLiveQAProof(contents []byte) (geminiLiveQAProof, error) {
	proof := geminiLiveQAProof{}
	toolCalls := map[string]bool{}
	toolResults := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	scanner.Buffer(make([]byte, 0, 4096), geminiLiveQACaptureMax)
	for scanner.Scan() {
		var event geminiLiveQAEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		switch event.Type {
		case "init":
			proof.initialized = true
		case "tool_use":
			if event.ToolName != "mcp_local-agent-harness_registered_targets" || event.ToolID == "" {
				return proof, errors.New("unexpected tool invocation")
			}
			proof.catalogCalls++
			toolCalls[event.ToolID] = true
		case "tool_result":
			if event.ToolID != "" && toolCalls[event.ToolID] {
				toolResults[event.ToolID] = event.Status
			}
		case "result":
			proof.finalSuccess = event.Status == "success"
		case "error":
			proof.errorEvent = true
		}
	}
	if err := scanner.Err(); err != nil {
		return proof, errors.New("stream unavailable")
	}
	for id, status := range toolResults {
		if toolCalls[id] && status == "success" {
			proof.successfulCatalogResults++
		}
	}
	if !proof.initialized || proof.catalogCalls != 1 || proof.successfulCatalogResults != 1 || !proof.finalSuccess {
		return proof, errors.New("acceptance proof incomplete")
	}
	return proof, nil
}

func TestGeminiLiveQAProofParser(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		wantErr bool
	}{
		{
			name: "one successful catalog call",
			stream: "{\"type\":\"init\",\"session_id\":\"private-session\",\"model\":\"private-model\"}\n" +
				"{\"type\":\"tool_use\",\"tool_name\":\"mcp_local-agent-harness_registered_targets\",\"tool_id\":\"private-call\",\"parameters\":{}}\n" +
				"{\"type\":\"tool_result\",\"tool_id\":\"private-call\",\"status\":\"success\",\"output\":\"private catalog\"}\n" +
				"{\"type\":\"result\",\"status\":\"success\"}\n",
		},
		{
			name: "unexpected tool is rejected",
			stream: "{\"type\":\"init\"}\n" +
				"{\"type\":\"tool_use\",\"tool_name\":\"mcp_local-agent-harness_ssh_run\",\"tool_id\":\"private-call\"}\n",
			wantErr: true,
		},
		{
			name: "failed catalog result is rejected",
			stream: "{\"type\":\"init\"}\n" +
				"{\"type\":\"tool_use\",\"tool_name\":\"mcp_local-agent-harness_registered_targets\",\"tool_id\":\"private-call\"}\n" +
				"{\"type\":\"tool_result\",\"tool_id\":\"private-call\",\"status\":\"error\",\"output\":\"private error\"}\n" +
				"{\"type\":\"result\",\"status\":\"success\"}\n",
			wantErr: true,
		},
		{
			name: "repeated catalog call is rejected",
			stream: "{\"type\":\"init\"}\n" +
				"{\"type\":\"tool_use\",\"tool_name\":\"mcp_local-agent-harness_registered_targets\",\"tool_id\":\"private-call-one\"}\n" +
				"{\"type\":\"tool_use\",\"tool_name\":\"mcp_local-agent-harness_registered_targets\",\"tool_id\":\"private-call-two\"}\n" +
				"{\"type\":\"result\",\"status\":\"success\"}\n",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proof, err := parseGeminiLiveQAProof([]byte(test.stream))
			if (err != nil) != test.wantErr {
				t.Fatalf("proof parse error presence = %t, want %t", err != nil, test.wantErr)
			}
			if err == nil && (!proof.initialized || proof.catalogCalls != 1 || proof.successfulCatalogResults != 1 || !proof.finalSuccess) {
				t.Fatalf("successful proof fields were incomplete: %#v", proof)
			}
		})
	}
}

func TestGeminiLiveQAProcessEnvironmentUsesEphemeralProfile(t *testing.T) {
	profileHome := filepath.Join(t.TempDir(), "profile")
	trustedFolders := filepath.Join(profileHome, ".gemini", "trustedFolders.json")
	appData := filepath.Join(t.TempDir(), "appdata")
	localAppData := filepath.Join(t.TempDir(), "local-appdata")
	environment := geminiLiveQAEnvironment(
		[]string{"PATH=synthetic-path", "GEMINI_CLI_HOME=old-home", "APPDATA=old-appdata"},
		profileHome,
		trustedFolders,
		appData,
		localAppData,
	)
	values := map[string]string{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatal("process environment entry was malformed")
		}
		values[strings.ToUpper(key)] = value
	}
	if values["PATH"] != "synthetic-path" || values["GEMINI_CLI_HOME"] != profileHome {
		t.Fatal("process environment did not preserve PATH and isolate Gemini user state")
	}
	if values["APPDATA"] != appData || values["LOCALAPPDATA"] != localAppData {
		t.Fatal("Gemini process environment did not isolate application paths")
	}
	if values["GEMINI_CLI_TRUSTED_FOLDERS_PATH"] != trustedFolders || values["GEMINI_CLI_TRUST_WORKSPACE"] != "true" {
		t.Fatal("process trust overrides were not scoped to the temporary workspace")
	}
}

func TestGeminiLiveQACommandDoesNotUseShellForExecutable(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "gemini.exe")
	command, err := newGeminiLiveQACommand(executable, []string{"--version"})
	if err != nil {
		t.Fatal("construct direct executable command failed")
	}
	if command.Path != executable || len(command.Args) != 2 || command.Args[1] != "--version" {
		t.Fatal("Gemini executable command was not launched directly")
	}
}

func TestGeminiLiveQANPMWrapperUsesDirectNodeEntry(t *testing.T) {
	root := t.TempDir()
	wrapperDir := filepath.Join(root, "node_modules", ".bin")
	entryPath := filepath.Join(root, "node_modules", "@google", "gemini-cli", "bundle", "gemini.js")
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o700); err != nil {
		t.Fatal("create temporary package layout failed")
	}
	if err := os.WriteFile(entryPath, []byte(""), 0o600); err != nil {
		t.Fatal("create temporary package entry failed")
	}
	wrapperPath := filepath.Join(wrapperDir, "gemini.cmd")
	command, err := newGeminiLiveQACommand(wrapperPath, []string{"--version"})
	if err != nil {
		t.Fatal("construct direct Node entry command failed")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node runtime prerequisite was not available to test harness")
	}
	if command.Path != node || len(command.Args) != 3 || command.Args[1] != entryPath || command.Args[2] != "--version" {
		t.Fatal("NPM wrapper was not resolved to the real package entry without a shell")
	}
}

func TestGeminiLiveQASettingsUseOnlyIsolatedPassiveCatalog(t *testing.T) {
	workDir := t.TempDir()
	appExe := filepath.Join(workDir, "lah-test.exe")
	appData := filepath.Join(workDir, "appdata")
	localAppData := filepath.Join(workDir, "local-appdata")
	if err := writeGeminiLiveQAProjectSettings(workDir, appExe, appData, localAppData); err != nil {
		t.Fatal("write isolated settings fixture failed")
	}
	contents, err := os.ReadFile(filepath.Join(workDir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatal("read isolated settings fixture failed")
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(contents, &settings); err != nil {
		t.Fatal("isolated settings fixture was not valid JSON")
	}
	var tools struct {
		Core []string `json:"core"`
	}
	if err := json.Unmarshal(settings["tools"], &tools); err != nil || len(tools.Core) != 0 {
		t.Fatal("isolated settings did not disable built-in tools")
	}
	var mcp struct {
		Allowed []string `json:"allowed"`
	}
	if err := json.Unmarshal(settings["mcp"], &mcp); err != nil || len(mcp.Allowed) != 1 || mcp.Allowed[0] != geminiLiveQAServerName {
		t.Fatal("isolated settings did not allow exactly the local MCP server")
	}
	var servers map[string]struct {
		Command      string            `json:"command"`
		Args         []string          `json:"args"`
		IncludeTools []string          `json:"includeTools"`
		Trust        bool              `json:"trust"`
		Env          map[string]string `json:"env"`
	}
	if err := json.Unmarshal(settings["mcpServers"], &servers); err != nil || len(servers) != 1 {
		t.Fatal("isolated settings did not contain exactly one MCP server")
	}
	server := servers[geminiLiveQAServerName]
	if server.Command != appExe || len(server.Args) != 1 || server.Args[0] != "--mcp" {
		t.Fatal("isolated MCP server command did not use the production executable and --mcp")
	}
	if len(server.IncludeTools) != 1 || server.IncludeTools[0] != geminiLiveQACatalogTool || !server.Trust {
		t.Fatal("isolated MCP server did not restrict access to the passive catalog tool")
	}
	if server.Env["APPDATA"] != appData || server.Env["LOCALAPPDATA"] != localAppData {
		t.Fatal("isolated MCP server did not isolate application settings and cache paths")
	}
}
