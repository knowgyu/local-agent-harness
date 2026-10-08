//go:build windows && integration && parallelmanual

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	manualWindowsClaudeLiveQAGate = "LAH_ENABLE_MANUAL_WINDOWS_CLAUDE_LIVE_QA"
	manualClaudeServerName        = "local-agent-harness"
	manualClaudeToolName          = "mcp__local-agent-harness__registered_targets"
	manualClaudeMaxOutputBytes    = 4 << 20
	manualClaudeMinCLIVersion     = "2.1.268"
)

var (
	errManualClaudeProcessFailed = errors.New("claude process failed")
	errManualClaudeOutputInvalid = errors.New("claude output was invalid")
	manualClaudeVersionPattern   = regexp.MustCompile(`(?:^|\s)(\d+)\.(\d+)\.(\d+)(?:[-+][A-Za-z0-9.-]+)?(?:\s|$)`)
)

type manualClaudeStreamSummary struct {
	initSeen            bool
	serverConnected     bool
	toolAdvertised      bool
	expectedToolCalls   int
	unexpectedToolCalls int
	toolResults         int
	toolErrors          int
	finalResultSeen     bool
}

type manualClaudeAuthStatus struct {
	LoggedIn bool `json:"loggedIn"`
}

type manualClaudePrivateCommand struct {
	name           string
	executable     string
	args           []string
	dir            string
	env            []string
	maxOutputBytes int64
	allowExitCodes []int
}

type manualClaudeBoundedWriter struct {
	file      *os.File
	remaining int64
	exceeded  bool
}

// TestManualWindowsClaudeLiveQACLIToMCP is an opt-in live CLI acceptance run.
// It uses a process-only MCP config, one passive catalog tool, temporary app
// settings, and private process logs. The default Go test suite never starts it.
func TestManualWindowsClaudeLiveQACLIToMCP(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("manual Claude CLI acceptance runs only on native Windows")
	}
	if os.Getenv(manualWindowsClaudeLiveQAGate) != "1" {
		t.Skipf("set %s=1 to start the opt-in Claude CLI acceptance harness", manualWindowsClaudeLiveQAGate)
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude Code CLI is not available in PATH")
	}

	privateDir := t.TempDir()
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 15*time.Second)
	versionOutput, err := manualClaudeRunPrivate(
		probeCtx,
		privateDir,
		manualClaudePrivateCommand{
			name:           "version",
			executable:     claudePath,
			args:           []string{"--version"},
			dir:            privateDir,
			env:            os.Environ(),
			maxOutputBytes: 1 << 20,
		},
	)
	cancelProbe()
	if err != nil {
		t.Skip("Claude Code CLI version could not be verified")
	}
	version, ok := manualClaudeParseVersion(string(versionOutput))
	if !ok || !manualClaudeVersionAtLeast(version, manualClaudeMinCLIVersion) {
		t.Skip("Claude Code CLI does not meet the documented live-test flag requirements")
	}

	appData := filepath.Join(privateDir, "appdata")
	localAppData := filepath.Join(privateDir, "localappdata")
	homeDir := filepath.Join(privateDir, "home")
	claudeConfigDir := filepath.Join(privateDir, "claude-config")
	cacheDir := filepath.Join(privateDir, "cache")
	tempDir := filepath.Join(privateDir, "tmp")
	workDir := filepath.Join(privateDir, "work")
	for _, path := range []string{appData, localAppData, homeDir, claudeConfigDir, cacheDir, tempDir, workDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("could not create isolated Claude test directories")
		}
	}

	authCtx, cancelAuth := context.WithTimeout(context.Background(), 15*time.Second)
	authOutput, err := manualClaudeRunPrivate(
		authCtx,
		privateDir,
		manualClaudePrivateCommand{
			name:       "auth-status",
			executable: claudePath,
			args:       []string{"auth", "status"},
			dir:        privateDir,
			env: manualClaudeProcessEnv(os.Environ(), map[string]string{
				"APPDATA":        appData,
				"LOCALAPPDATA":   localAppData,
				"TEMP":           tempDir,
				"TMP":            tempDir,
				"XDG_CACHE_HOME": cacheDir,
			}),
			maxOutputBytes: 1 << 20,
			allowExitCodes: []int{1},
		},
	)
	cancelAuth()
	if err != nil {
		t.Skip("Claude Code authentication status is unavailable")
	}
	var auth manualClaudeAuthStatus
	if json.Unmarshal(authOutput, &auth) != nil {
		t.Skip("Claude Code CLI authentication status could not be interpreted safely")
	}

	appPath := filepath.Join(privateDir, "local-agent-harness.exe")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	_, err = manualClaudeRunPrivate(
		buildCtx,
		privateDir,
		manualClaudePrivateCommand{
			name:           "app-build",
			executable:     "go",
			args:           []string{"build", "-o", appPath, "."},
			dir:            moduleRoot(t),
			env:            manualClaudeOfflineProcessEnv(os.Environ(), appData, localAppData, claudeConfigDir, privateDir),
			maxOutputBytes: 1 << 20,
		},
	)
	cancelBuild()
	if err != nil {
		t.Fatal("could not build the isolated application executable")
	}

	configBytes, err := json.Marshal(struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}{
		MCPServers: map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}{
			manualClaudeServerName: {
				Type:    "stdio",
				Command: appPath,
				Args:    []string{"--mcp"},
			},
		},
	})
	if err != nil {
		t.Fatal("could not create the isolated MCP configuration")
	}
	mcpConfigPath := filepath.Join(privateDir, "mcp-config.json")
	if err := os.WriteFile(mcpConfigPath, configBytes, 0o600); err != nil {
		t.Fatal("could not write the isolated MCP configuration")
	}

	args := []string{
		"-p",
		"--restricted",
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--setting-sources", "project",
		"--no-session-persistence",
		"--permission-prompts", "none",
		"--tools", "",
		"--allowedTools", manualClaudeToolName,
		"--max-turns", "1",
		"--max-budget-usd", "0.10",
		"--output-format", "stream-json",
		"--verbose",
		"Call only the MCP tool " + manualClaudeToolName + " exactly once. Do not use any other tool. After it returns, output exactly PASS.",
	}
	if !auth.LoggedIn {
		initOnlyArgs := append([]string(nil), args...)
		initOnlyArgs[len(initOnlyArgs)-1] = "Reply exactly PASS."
		initCtx, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
		initOutput, initErr := manualClaudeRunPrivate(
			initCtx,
			privateDir,
			manualClaudePrivateCommand{
				name:           "unauthenticated-init-only",
				executable:     claudePath,
				args:           initOnlyArgs,
				dir:            workDir,
				env:            manualClaudeOfflineProcessEnv(os.Environ(), appData, localAppData, claudeConfigDir, privateDir),
				maxOutputBytes: manualClaudeMaxOutputBytes,
				allowExitCodes: []int{1},
			},
		)
		cancelInit()
		if initErr == nil {
			initSummary, summaryErr := manualClaudeSummarizeStream(initOutput)
			if summaryErr == nil && initSummary.initSeen && initSummary.serverConnected && initSummary.toolAdvertised {
				t.Log("Claude CLI initialized the isolated stdio server; authentication gate prevented a model tool call")
				t.Skip("Claude Code CLI is not authenticated; no model or MCP tool call was made")
			}
		}
		t.Skip("Claude Code CLI is not authenticated; isolated MCP initialization was not proven and no model tool call was made")
	}
	modelCtx, cancelModel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelModel()
	stdout, err := manualClaudeRunPrivate(
		modelCtx,
		privateDir,
		manualClaudePrivateCommand{
			name:       "live-claude",
			executable: claudePath,
			args:       args,
			dir:        workDir,
			env: manualClaudeProcessEnv(os.Environ(), map[string]string{
				"APPDATA":        appData,
				"LOCALAPPDATA":   localAppData,
				"TEMP":           tempDir,
				"TMP":            tempDir,
				"XDG_CACHE_HOME": cacheDir,
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				"CLAUDE_CODE_SKIP_PROMPT_HISTORY":          "1",
				"MAX_MCP_OUTPUT_TOKENS":                    "2048",
				"MCP_TIMEOUT":                              "10000",
				"MCP_TOOL_TIMEOUT":                         "15000",
			}),
			maxOutputBytes: manualClaudeMaxOutputBytes,
		},
	)
	if err != nil {
		t.Fatal("Claude CLI live process did not complete successfully")
	}

	summary, err := manualClaudeSummarizeStream(stdout)
	if err != nil {
		t.Fatal("Claude CLI output could not be validated safely")
	}
	if !summary.initSeen || !summary.serverConnected || !summary.toolAdvertised ||
		summary.expectedToolCalls != 1 || summary.unexpectedToolCalls != 0 ||
		summary.toolResults != 1 || summary.toolErrors != 0 || !summary.finalResultSeen {
		t.Fatal("Claude CLI did not prove one successful passive catalog tool call")
	}
	t.Log("Claude Code CLI initialized the isolated stdio server and completed exactly one registered_targets call")
}

func TestManualWindowsClaudeLiveQASummarizesStreamWithoutReturningPayload(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","mcp_servers":[{"name":"local-agent-harness","status":"connected"}],"tools":["mcp__local-agent-harness__registered_targets"]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"use-1","name":"mcp__local-agent-harness__registered_targets","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"use-1","content":"synthetic-target-name-canary"}]}}`,
		`{"type":"result","result":"PASS"}`,
	}, "\n")
	summary, err := manualClaudeSummarizeStream([]byte(stream))
	if err != nil {
		t.Fatal("synthetic stream was rejected")
	}
	if !summary.initSeen || !summary.serverConnected || !summary.toolAdvertised ||
		summary.expectedToolCalls != 1 || summary.unexpectedToolCalls != 0 ||
		summary.toolResults != 1 || summary.toolErrors != 0 || !summary.finalResultSeen {
		t.Fatal("synthetic stream summary did not match the expected proof")
	}
	encoded, err := json.Marshal(summary)
	if err != nil || bytes.Contains(encoded, []byte("synthetic-target-name-canary")) {
		t.Fatal("summary retained raw tool output")
	}
}

func TestManualWindowsClaudeLiveQASummarizesUnexpectedToolsAsFailure(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","mcp_servers":[{"name":"local-agent-harness","status":"connected"}],"tools":["mcp__local-agent-harness__registered_targets"]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"use-1","name":"mcp__local-agent-harness__run_ssh_operation","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"use-1","is_error":true,"content":"private-error-canary"}]}}`,
		`{"type":"result","result":"Denied"}`,
	}, "\n")
	summary, err := manualClaudeSummarizeStream([]byte(stream))
	if err != nil {
		t.Fatal("synthetic stream was rejected")
	}
	if summary.unexpectedToolCalls != 1 || summary.toolErrors != 1 || summary.expectedToolCalls != 0 {
		t.Fatal("unexpected tool use was not summarized as a fixed failure")
	}
	encoded, err := json.Marshal(summary)
	if err != nil || bytes.Contains(encoded, []byte("private-error-canary")) {
		t.Fatal("summary retained raw tool output")
	}
}

func TestManualWindowsClaudeLiveQAOfflineEnvironmentIsIsolated(t *testing.T) {
	env := manualClaudeOfflineProcessEnv([]string{
		"PATH=C:\\Windows\\System32",
		"ANTHROPIC_API_KEY=private-canary",
		"CLAUDE_CODE_OAUTH_TOKEN=private-canary",
		"AWS_SECRET_ACCESS_KEY=private-canary",
		"GOOGLE_APPLICATION_CREDENTIALS=C:\\private\\credentials.json",
	}, "C:\\private\\appdata", "C:\\private\\localappdata", "C:\\private\\config", "C:\\private")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"private-canary", "credentials.json", "ANTHROPIC_API_KEY=", "CLAUDE_CODE_OAUTH_TOKEN=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("offline process environment retained an authentication value")
		}
	}
	for _, required := range []string{
		"PATH=C:\\Windows\\System32",
		"APPDATA=C:\\private\\appdata",
		"LOCALAPPDATA=C:\\private\\localappdata",
		"TEMP=C:\\private\\tmp",
		"TMP=C:\\private\\tmp",
		"XDG_CACHE_HOME=C:\\private\\cache",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CONFIG_DIR=C:\\private\\config",
		"USERPROFILE=C:\\private\\home",
		"HOME=C:\\private\\home",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:0",
	} {
		if !strings.Contains(joined, required) {
			t.Fatal("offline process environment omitted an isolation setting")
		}
	}
}

func TestManualWindowsClaudeLiveQABoundedWriterCapsPrivateOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-output")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal("could not create private output fixture")
	}
	writer := manualClaudeBoundedWriter{file: file, remaining: 3}
	if written, writeErr := writer.Write([]byte("abcdef")); writeErr != nil || written != 6 || !writer.exceeded {
		_ = file.Close()
		t.Fatal("private output cap did not safely consume and truncate oversized data")
	}
	if err := file.Close(); err != nil {
		t.Fatal("could not close private output fixture")
	}
	output, err := os.ReadFile(path)
	if err != nil || string(output) != "abc" {
		t.Fatal("private output fixture exceeded its configured cap")
	}
}

func manualClaudeRunPrivate(
	ctx context.Context,
	privateDir string,
	process manualClaudePrivateCommand,
) ([]byte, error) {
	stdoutPath := filepath.Join(privateDir, process.name+".stdout")
	stderrPath := filepath.Join(privateDir, process.name+".stderr")
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errManualClaudeProcessFailed
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		closeErr := stdoutFile.Close()
		if closeErr != nil {
			return nil, errManualClaudeProcessFailed
		}
		return nil, errManualClaudeProcessFailed
	}

	cmd := exec.CommandContext(ctx, process.executable, process.args...)
	cmd.Dir = process.dir
	cmd.Env = process.env
	stdoutWriter := manualClaudeBoundedWriter{file: stdoutFile, remaining: process.maxOutputBytes}
	stderrWriter := manualClaudeBoundedWriter{file: stderrFile, remaining: process.maxOutputBytes}
	cmd.Stdout = &stdoutWriter
	cmd.Stderr = &stderrWriter
	runErr := cmd.Run()
	stdoutCloseErr := stdoutFile.Close()
	stderrCloseErr := stderrFile.Close()
	if !manualClaudeExitAllowed(runErr, process.allowExitCodes) || stdoutWriter.exceeded || stderrWriter.exceeded || stdoutCloseErr != nil || stderrCloseErr != nil {
		return nil, errManualClaudeProcessFailed
	}

	stdoutInfo, err := os.Stat(stdoutPath)
	if err != nil || stdoutInfo.Size() > process.maxOutputBytes {
		return nil, errManualClaudeOutputInvalid
	}
	stderrInfo, err := os.Stat(stderrPath)
	if err != nil || stderrInfo.Size() > process.maxOutputBytes {
		return nil, errManualClaudeOutputInvalid
	}
	output, err := os.ReadFile(stdoutPath)
	if err != nil {
		return nil, errManualClaudeOutputInvalid
	}
	return output, nil
}

func (writer *manualClaudeBoundedWriter) Write(payload []byte) (int, error) {
	if writer.remaining <= 0 {
		writer.exceeded = true
		return len(payload), nil
	}
	writeLength := len(payload)
	if int64(writeLength) > writer.remaining {
		writeLength = int(writer.remaining)
		writer.exceeded = true
	}
	written, err := writer.file.Write(payload[:writeLength])
	writer.remaining -= int64(written)
	if written < len(payload) && err == nil {
		writer.exceeded = true
		return len(payload), nil
	}
	if err != nil {
		return written, err
	}
	return len(payload), nil
}

func manualClaudeExitAllowed(runErr error, allowed []int) bool {
	if runErr == nil {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return false
	}
	for _, exitCode := range allowed {
		if exitErr.ExitCode() == exitCode {
			return true
		}
	}
	return false
}

func manualClaudeProcessEnv(current []string, overrides map[string]string) []string {
	result := make([]string, 0, len(current)+len(overrides))
	overrideKeys := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		overrideKeys[strings.ToUpper(key)] = struct{}{}
	}
	for _, item := range current {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, replaced := overrideKeys[strings.ToUpper(key)]; replaced {
			continue
		}
		result = append(result, item)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func manualClaudeOfflineProcessEnv(
	current []string,
	appData string,
	localAppData string,
	claudeConfigDir string,
	privateDir string,
) []string {
	overrides := map[string]string{
		"APPDATA":                         appData,
		"LOCALAPPDATA":                    localAppData,
		"USERPROFILE":                     filepath.Join(privateDir, "home"),
		"HOME":                            filepath.Join(privateDir, "home"),
		"TEMP":                            filepath.Join(privateDir, "tmp"),
		"TMP":                             filepath.Join(privateDir, "tmp"),
		"XDG_CACHE_HOME":                  filepath.Join(privateDir, "cache"),
		"CLAUDE_CONFIG_DIR":               claudeConfigDir,
		"CLAUDE_CODE_SKIP_PROMPT_HISTORY": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"MAX_MCP_OUTPUT_TOKENS":                    "2048",
		"MCP_TIMEOUT":                              "10000",
		"MCP_TOOL_TIMEOUT":                         "15000",
		"ANTHROPIC_BASE_URL":                       "http://127.0.0.1:0",
		"CLAUDE_CODE_USE_BEDROCK":                  "0",
		"CLAUDE_CODE_USE_VERTEX":                   "0",
	}
	overrideKeys := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		overrideKeys[strings.ToUpper(key)] = struct{}{}
	}
	result := make([]string, 0, len(current)+len(overrides))
	for _, item := range current {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		upperKey := strings.ToUpper(key)
		if _, replaced := overrideKeys[upperKey]; replaced {
			continue
		}
		if strings.HasPrefix(upperKey, "ANTHROPIC_") ||
			strings.HasPrefix(upperKey, "CLAUDE_CODE_") ||
			strings.HasPrefix(upperKey, "AWS_") ||
			strings.HasPrefix(upperKey, "GOOGLE_") ||
			strings.HasPrefix(upperKey, "CLOUDSDK_") {
			continue
		}
		result = append(result, item)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func manualClaudeParseVersion(value string) (string, bool) {
	match := manualClaudeVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 4 {
		return "", false
	}
	return match[1] + "." + match[2] + "." + match[3], true
}

func manualClaudeVersionAtLeast(got, minimum string) bool {
	gotParts := strings.Split(got, ".")
	minParts := strings.Split(minimum, ".")
	if len(gotParts) != 3 || len(minParts) != 3 {
		return false
	}
	for index := range gotParts {
		gotPart, gotErr := strconv.Atoi(gotParts[index])
		minPart, minErr := strconv.Atoi(minParts[index])
		if gotErr != nil || minErr != nil {
			return false
		}
		if gotPart != minPart {
			return gotPart > minPart
		}
	}
	return true
}

func manualClaudeSummarizeStream(output []byte) (manualClaudeStreamSummary, error) {
	var summary manualClaudeStreamSummary
	if len(output) == 0 || len(output) > manualClaudeMaxOutputBytes {
		return summary, errManualClaudeOutputInvalid
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal(line, &event) != nil {
			return manualClaudeStreamSummary{}, errManualClaudeOutputInvalid
		}
		if event["type"] == "system" && event["subtype"] == "init" {
			summary.initSeen = true
			summary.serverConnected = manualClaudeInitServerConnected(event)
			summary.toolAdvertised = manualClaudeInitToolAdvertised(event)
		}
		if event["type"] == "result" {
			summary.finalResultSeen = true
		}
		manualClaudeWalk(event, func(value map[string]any) {
			switch value["type"] {
			case "tool_use":
				if value["name"] == manualClaudeToolName {
					summary.expectedToolCalls++
				} else {
					summary.unexpectedToolCalls++
				}
			case "tool_result":
				summary.toolResults++
				if value["is_error"] == true {
					summary.toolErrors++
				}
			}
		})
	}
	if scanner.Err() != nil {
		return manualClaudeStreamSummary{}, errManualClaudeOutputInvalid
	}
	return summary, nil
}

func manualClaudeInitServerConnected(event map[string]any) bool {
	servers, ok := event["mcp_servers"].([]any)
	if !ok {
		return false
	}
	for _, value := range servers {
		server, ok := value.(map[string]any)
		if ok && server["name"] == manualClaudeServerName && server["status"] == "connected" {
			return true
		}
	}
	return false
}

func manualClaudeInitToolAdvertised(event map[string]any) bool {
	tools, ok := event["tools"].([]any)
	if !ok {
		return false
	}
	for _, value := range tools {
		if value == manualClaudeToolName {
			return true
		}
	}
	return false
}

func manualClaudeWalk(value any, visit func(map[string]any)) {
	switch typed := value.(type) {
	case map[string]any:
		visit(typed)
		for _, child := range typed {
			manualClaudeWalk(child, visit)
		}
	case []any:
		for _, child := range typed {
			manualClaudeWalk(child, visit)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate isolated source directory")
	}
	return root
}
