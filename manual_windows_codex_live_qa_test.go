//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	codexLiveQAOptInEnv  = "LAH_CODEX_LIVE_QA"
	codexLiveQAAppEnv    = "LAH_CODEX_LIVE_QA_APP"
	codexLiveQACLIEnv    = "LAH_CODEX_LIVE_QA_CODEX"
	codexLiveQAReportEnv = "LAH_CODEX_LIVE_QA_REPORT"
	codexLiveQAServerID  = "local-agent-harness"
	codexLiveQAToolName  = "registered_targets"
	codexLiveQATimeout   = 120 * time.Second
)

type codexLiveQAReport struct {
	Schema                  string         `json:"schema"`
	StartedAt               string         `json:"started_at"`
	FinishedAt              string         `json:"finished_at"`
	OS                      string         `json:"os"`
	CLI                     string         `json:"cli"`
	Outcome                 string         `json:"outcome"`
	CLIExitCode             *int           `json:"cli_exit_code,omitempty"`
	TurnStartedEvents       int            `json:"turn_started_events"`
	JSONLEventCount         int            `json:"jsonl_event_count"`
	MCPServerID             string         `json:"mcp_server_id"`
	MCPTool                 string         `json:"mcp_tool"`
	MCPToolCallCount        int            `json:"mcp_tool_call_count"`
	MCPToolCallCompleted    bool           `json:"mcp_tool_call_completed"`
	EmptyCatalogResultSeen  bool           `json:"empty_catalog_result_seen"`
	UnexpectedToolItemCount int            `json:"unexpected_tool_item_count"`
	EventTypeCounts         map[string]int `json:"event_type_counts"`
	ItemTypeCounts          map[string]int `json:"item_type_counts"`
	ItemTypeIdentifiers     []string       `json:"item_type_identifiers"`
	MCPItemFieldPresence    []string       `json:"mcp_item_field_presence"`
	MCPToolNameMatched      bool           `json:"mcp_tool_name_matched"`
	MCPServerNameMatched    bool           `json:"mcp_server_name_matched"`
	MCPStatusCategories     []string       `json:"mcp_status_categories"`
	MCPResultFieldPresent   bool           `json:"mcp_result_field_present"`
	MCPErrorFieldPresent    bool           `json:"mcp_error_field_present"`
	MCPFailureCategory      string         `json:"mcp_failure_category"`
	TraceSHA256             string         `json:"private_trace_sha256"`
	StderrSHA256            string         `json:"private_stderr_sha256"`
	RunRestrictions         []string       `json:"run_restrictions"`
	UserConfigLoaded        bool           `json:"user_config_loaded"`
	AuthFileInspected       bool           `json:"auth_file_inspected"`
	NormalProfileChanged    bool           `json:"normal_profile_changed"`
	AppDataIsolated         bool           `json:"appdata_isolated"`
	ServiceTargetContacted  bool           `json:"service_target_contacted"`
}

type codexLiveQAEvent struct {
	Type string          `json:"type"`
	Item json.RawMessage `json:"item"`
}

type codexLiveQACall struct {
	ID              string
	Tool            string
	Server          string
	Status          string
	FailureCategory string
	EmptyList       bool
}

func TestManualWindowsCodexLiveMCPToolCall(t *testing.T) {
	if os.Getenv(codexLiveQAOptInEnv) != "1" {
		t.Skip("set LAH_CODEX_LIVE_QA=1 to run the bounded Codex CLI live acceptance check")
	}

	report := codexLiveQAReport{
		Schema:      "lah-codex-live-qa-v1",
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		OS:          "windows",
		Outcome:     "not_started",
		MCPServerID: codexLiveQAServerID,
		MCPTool:     codexLiveQAToolName,
		RunRestrictions: []string{
			"codex exec --ephemeral --ignore-user-config --ignore-rules --sandbox read-only",
			"single required stdio MCP server with enabled_tools=[registered_targets]",
			"shell, shell snapshot, browser, computer use, skill search, and multi-agent features disabled",
			"temporary working directory and process-scoped APPDATA for the MCP server",
			"Codex stdout/stderr captured privately and never copied to test output",
		},
		UserConfigLoaded:       false,
		AuthFileInspected:      false,
		NormalProfileChanged:   false,
		AppDataIsolated:        true,
		ServiceTargetContacted: false,
	}

	reportPath := os.Getenv(codexLiveQAReportEnv)
	if reportPath == "" || !filepath.IsAbs(reportPath) {
		t.Fatal("manual Codex live QA requires an absolute LAH_CODEX_LIVE_QA_REPORT path")
	}

	tempRoot, err := os.MkdirTemp("", "lah-codex-live-qa-")
	if err != nil {
		t.Fatal("manual Codex live QA could not create a private temporary directory")
	}
	defer os.RemoveAll(tempRoot)
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		t.Fatal("manual Codex live QA could not restrict its temporary directory")
	}

	workDir := filepath.Join(tempRoot, "workspace")
	appData := filepath.Join(tempRoot, "appdata")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		failCodexLiveQA(t, "temp_workspace_unavailable", reportPath, &report)
	}
	if err := os.MkdirAll(appData, 0o700); err != nil {
		failCodexLiveQA(t, "temp_appdata_unavailable", reportPath, &report)
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		failCodexLiveQA(t, "temp_workspace_unavailable", reportPath, &report)
	}
	if err := os.Chmod(appData, 0o700); err != nil {
		failCodexLiveQA(t, "temp_appdata_unavailable", reportPath, &report)
	}

	appExe, err := requiredAbsoluteExecutable(os.Getenv(codexLiveQAAppEnv))
	if err != nil {
		failCodexLiveQA(t, "app_executable_unavailable", reportPath, &report)
	}
	codexExe, err := requiredAbsoluteExecutable(os.Getenv(codexLiveQACLIEnv))
	if err != nil {
		failCodexLiveQA(t, "codex_cli_unavailable", reportPath, &report)
	}
	version, err := readCodexVersion(codexExe)
	if err != nil {
		failCodexLiveQA(t, "codex_version_unavailable", reportPath, &report)
	}
	report.CLI = version

	stdoutPath := filepath.Join(tempRoot, "codex-stdout.jsonl")
	stderrPath := filepath.Join(tempRoot, "codex-stderr.log")
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		failCodexLiveQA(t, "private_stdout_capture_unavailable", reportPath, &report)
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdoutFile.Close()
		failCodexLiveQA(t, "private_stderr_capture_unavailable", reportPath, &report)
	}

	ctx, cancel := context.WithTimeout(context.Background(), codexLiveQATimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexExe, codexLiveQAArgs(appExe, appData, workDir)...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.WaitDelay = 3 * time.Second

	report.Outcome = "cli_running"
	if err := cmd.Start(); err != nil {
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		report.Outcome = "codex_start_failed"
		finishCodexLiveQAReport(t, reportPath, report)
		t.Fatal("manual Codex live QA could not start the installed Codex CLI")
	}
	waitErr := cmd.Wait()
	_ = stdoutFile.Sync()
	_ = stderrFile.Sync()
	_ = stdoutFile.Close()
	_ = stderrFile.Close()

	trace, traceHash, traceErr := readPrivateTrace(stdoutPath)
	_, stderrHash, stderrErr := readPrivateTrace(stderrPath)
	report.TraceSHA256 = traceHash
	report.StderrSHA256 = stderrHash
	if traceErr != nil || stderrErr != nil {
		report.Outcome = "private_capture_unreadable"
		finishCodexLiveQAReport(t, reportPath, report)
		t.Fatal("manual Codex live QA could not safely inspect its private captured output")
	}

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		exitCode = -1
	}
	report.CLIExitCode = &exitCode
	parsed, parseErr := summarizeCodexLiveTrace(trace)
	report.JSONLEventCount = parsed.EventCount
	report.TurnStartedEvents = parsed.TurnStarted
	report.MCPToolCallCount = parsed.MCPCallCount
	report.MCPToolCallCompleted = parsed.MCPCallCompleted
	report.EmptyCatalogResultSeen = parsed.EmptyCatalogResult
	report.UnexpectedToolItemCount = parsed.UnexpectedToolItems
	report.EventTypeCounts = parsed.EventTypeCounts
	report.ItemTypeCounts = parsed.ItemTypeCounts
	report.ItemTypeIdentifiers = parsed.ItemTypeIdentifiers
	report.MCPItemFieldPresence = parsed.MCPItemFieldPresence
	report.MCPToolNameMatched = parsed.MCPToolNameMatched
	report.MCPServerNameMatched = parsed.MCPServerNameMatched
	report.MCPStatusCategories = parsed.MCPStatusCategories
	report.MCPResultFieldPresent = parsed.MCPResultFieldPresent
	report.MCPErrorFieldPresent = parsed.MCPErrorFieldPresent
	report.MCPFailureCategory = parsed.MCPFailureCategory

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		report.Outcome = "codex_timeout"
	case waitErr != nil:
		report.Outcome = classifyCodexLiveFailure(trace, stderrPath)
	case parseErr != nil:
		report.Outcome = "jsonl_evidence_unavailable"
	case parsed.MCPCallCount != 1 || !parsed.MCPCallCompleted || !parsed.EmptyCatalogResult:
		report.Outcome = "mcp_tool_call_evidence_incomplete"
	case parsed.UnexpectedToolItems != 0:
		report.Outcome = "unexpected_tool_category_observed"
	default:
		report.Outcome = "pass"
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	finishCodexLiveQAReport(t, reportPath, report)
	if report.Outcome != "pass" {
		t.Fatalf("manual Codex live QA outcome: %s", report.Outcome)
	}
	t.Logf("manual Codex live QA: outcome=pass cli=%s mcp_tool=%s calls=%d empty_catalog=true turns=%d", report.CLI, report.MCPTool, report.MCPToolCallCount, report.TurnStartedEvents)
}

func TestCodexLiveQASanitizerAllowsErrorMetadataButRejectsOtherToolCalls(t *testing.T) {
	validCall := `{"type":"item.completed","item":{"id":"mcp-1","type":"mcp_tool_call","status":"completed","server":"local-agent-harness","tool":"registered_targets","result":{"targets":[]}}}`
	errorMetadata := `{"type":"item.completed","item":{"id":"error-1","type":"error"}}`
	commandCall := `{"type":"item.completed","item":{"id":"command-1","type":"command_execution","status":"completed"}}`

	valid, err := summarizeCodexLiveTrace([]byte(validCall + "\n" + errorMetadata + "\n"))
	if err != nil {
		t.Fatalf("parse valid MCP evidence: %v", err)
	}
	if valid.MCPCallCount != 1 || !valid.MCPCallCompleted || !valid.EmptyCatalogResult || valid.UnexpectedToolItems != 0 {
		t.Fatalf("valid inventory call was not recognized safely: %+v", valid)
	}

	withCommand, err := summarizeCodexLiveTrace([]byte(validCall + "\n" + commandCall + "\n"))
	if err != nil {
		t.Fatalf("parse command evidence: %v", err)
	}
	if withCommand.UnexpectedToolItems == 0 {
		t.Fatal("non-MCP command execution was not rejected")
	}
}

func codexLiveQAArgs(appExe, appData, workDir string) []string {
	server := "mcp_servers." + codexLiveQAServerID
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--sandbox", "read-only",
		"--json",
		"--cd", workDir,
		"--disable", "shell_tool",
		"--disable", "shell_snapshot",
		"--disable", "browser_use",
		"--disable", "browser_use_external",
		"--disable", "computer_use",
		"--disable", "in_app_browser",
		"--disable", "skill_search",
		"--disable", "multi_agent",
		"--disable", "standalone_web_search",
		"--config", "approval_policy=\"never\"",
		"--config", "web_search=\"disabled\"",
		"--config", "tools.web_search=false",
		"--config", "tools.view_image=false",
		"--config", "agents.enabled=false",
		"--config", "shell_environment_policy.inherit=\"none\"",
		"--config", server + ".command=" + tomlString(appExe),
		"--config", server + ".args=[\"--mcp\"]",
		"--config", server + ".env.APPDATA=" + tomlString(appData),
		"--config", server + ".enabled=true",
		"--config", server + ".required=true",
		"--config", server + ".enabled_tools=[\"registered_targets\"]",
		"--config", server + ".tools.registered_targets.approval_mode=\"approve\"",
		"--config", server + ".startup_timeout_sec=15",
		"--config", server + ".tool_timeout_sec=15",
		"Use exactly one tool call: call registered_targets on the local-agent-harness MCP server. This is a local inventory-only call. Do not call any other tool, read or write files, run commands, contact services, or change settings. After the call, return only a short factual summary of the result.",
	}
	return args
}

func requiredAbsoluteExecutable(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("not an absolute executable path")
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return "", errors.New("executable unavailable")
	}
	return resolved, nil
}

func readCodexVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	line := strings.TrimSpace(stdout.String())
	if len(line) > 80 || !strings.HasPrefix(line, "codex-cli ") {
		return "", errors.New("unexpected Codex version response")
	}
	return line, nil
}

func tomlString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func failCodexLiveQA(t *testing.T, outcome, reportPath string, report *codexLiveQAReport) {
	t.Helper()
	report.Outcome = outcome
	finishCodexLiveQAReport(t, reportPath, *report)
	t.Fatalf("manual Codex live QA outcome: %s", outcome)
}

func finishCodexLiveQAReport(t *testing.T, path string, report codexLiveQAReport) {
	t.Helper()
	if report.FinishedAt == "" {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal("manual Codex live QA could not serialize its sanitized report")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal("manual Codex live QA could not write its sanitized report")
	}
}

func readPrivateTrace(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

type codexLiveQATraceSummary struct {
	EventCount            int
	TurnStarted           int
	MCPCallCount          int
	MCPCallCompleted      bool
	EmptyCatalogResult    bool
	UnexpectedToolItems   int
	EventTypeCounts       map[string]int
	ItemTypeCounts        map[string]int
	ItemTypeIdentifiers   []string
	MCPItemFieldPresence  []string
	MCPToolNameMatched    bool
	MCPServerNameMatched  bool
	MCPStatusCategories   []string
	MCPResultFieldPresent bool
	MCPErrorFieldPresent  bool
	MCPFailureCategory    string
}

func summarizeCodexLiveTrace(trace []byte) (codexLiveQATraceSummary, error) {
	summary := codexLiveQATraceSummary{
		EventTypeCounts: make(map[string]int),
		ItemTypeCounts:  make(map[string]int),
	}
	calls := make(map[string]codexLiveQACall)
	fieldPresence := make(map[string]bool)
	statusCategories := make(map[string]bool)
	itemTypeNames := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(trace))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event codexLiveQAEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return summary, errors.New("invalid JSONL event")
		}
		summary.EventCount++
		summary.EventTypeCounts[safeCodexEventType(event.Type)]++
		if event.Type == "turn.started" {
			summary.TurnStarted++
		}
		if len(event.Item) == 0 || string(event.Item) == "null" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(event.Item, &item); err != nil {
			return summary, errors.New("invalid item payload")
		}
		itemType := stringField(item, "type")
		summary.ItemTypeCounts[safeCodexItemType(itemType)]++
		if isSafeCodexIdentifier(itemType) {
			itemTypeNames[itemType] = true
		}
		if strings.Contains(itemType, "mcp") && (strings.Contains(itemType, "tool") || strings.Contains(itemType, "call")) {
			call := codexLiveQACall{
				ID:              stringField(item, "id"),
				Tool:            firstStringField(item, "tool_name", "tool", "name"),
				Server:          firstStringField(item, "server_name", "server", "mcp_server"),
				Status:          stringField(item, "status"),
				FailureCategory: classifyCodexMCPFailure(item),
				EmptyList:       containsEmptyTargets(item),
			}
			collectCodexLiveQAFieldPresence(item, fieldPresence)
			statusCategories[statusCategory(call.Status)] = true
			summary.MCPToolNameMatched = summary.MCPToolNameMatched || isCodexLiveQAToolName(call.Tool)
			summary.MCPServerNameMatched = summary.MCPServerNameMatched || isCodexLiveQAServerName(call.Server)
			summary.MCPResultFieldPresent = summary.MCPResultFieldPresent || hasAnyField(item, "result", "content", "output", "structuredContent")
			summary.MCPErrorFieldPresent = summary.MCPErrorFieldPresent || hasAnyField(item, "error", "isError")
			key := call.ID
			if key == "" {
				key = call.Server + "|" + call.Tool
			}
			if previous, ok := calls[key]; ok {
				if call.Tool == "" {
					call.Tool = previous.Tool
				}
				if call.Server == "" {
					call.Server = previous.Server
				}
				if call.Status == "" || statusCategory(call.Status) == "in_progress" {
					call.Status = previous.Status
				}
				if call.FailureCategory == "none" && previous.FailureCategory != "none" {
					call.FailureCategory = previous.FailureCategory
				}
				call.EmptyList = call.EmptyList || previous.EmptyList
			}
			calls[key] = call
			continue
		}
		if itemType != "" && itemType != "agent_message" && itemType != "reasoning" && itemType != "error" && itemType != "reasoning_summary" && itemType != "plan_update" {
			summary.UnexpectedToolItems++
		}
	}
	for field := range fieldPresence {
		summary.MCPItemFieldPresence = append(summary.MCPItemFieldPresence, field)
	}
	sort.Strings(summary.MCPItemFieldPresence)
	for category := range statusCategories {
		summary.MCPStatusCategories = append(summary.MCPStatusCategories, category)
	}
	sort.Strings(summary.MCPStatusCategories)
	for itemType := range itemTypeNames {
		summary.ItemTypeIdentifiers = append(summary.ItemTypeIdentifiers, itemType)
	}
	sort.Strings(summary.ItemTypeIdentifiers)
	if err := scanner.Err(); err != nil {
		return summary, err
	}
	summary.MCPCallCount = len(calls)
	for _, call := range calls {
		if isCodexLiveQAToolName(call.Tool) && (call.Server == "" || isCodexLiveQAServerName(call.Server)) {
			summary.MCPCallCompleted = summary.MCPCallCompleted || statusCategory(call.Status) == "completed" || (call.EmptyList && call.FailureCategory == "none")
			summary.EmptyCatalogResult = summary.EmptyCatalogResult || call.EmptyList
			if call.FailureCategory != "none" {
				summary.MCPFailureCategory = call.FailureCategory
			}
		} else {
			summary.UnexpectedToolItems++
		}
	}
	return summary, nil
}

func stringField(object map[string]any, key string) string {
	value, ok := object[key].(string)
	if !ok {
		return ""
	}
	return value
}

func classifyCodexMCPFailure(value any) string {
	if !hasTrueMCPError(value) {
		return "none"
	}
	var messages []string
	collectMCPErrorText(value, false, &messages)
	joined := strings.ToLower(strings.Join(messages, " "))
	switch {
	case strings.Contains(joined, "approval"), strings.Contains(joined, "permission denied"), strings.Contains(joined, "not approved"):
		return "approval_rejected"
	case strings.Contains(joined, "not initialized"), strings.Contains(joined, "initializ"):
		return "initialization_failed"
	case strings.Contains(joined, "tool not found"), strings.Contains(joined, "unknown tool"), strings.Contains(joined, "method not found"):
		return "tool_lookup_failed"
	case strings.Contains(joined, "timeout"), strings.Contains(joined, "timed out"), strings.Contains(joined, "deadline"):
		return "timeout"
	case strings.Contains(joined, "eof"), strings.Contains(joined, "closed"), strings.Contains(joined, "transport"):
		return "transport_failed"
	case strings.Contains(joined, "invalid request"), strings.Contains(joined, "protocol"), strings.Contains(joined, "invalid params"):
		return "protocol_failed"
	case strings.Contains(joined, "credential"), strings.Contains(joined, "unauthorized"), strings.Contains(joined, "authentication"):
		return "authentication_failed"
	case strings.Contains(joined, "start"), strings.Contains(joined, "spawn"), strings.Contains(joined, "executable"):
		return "server_process_failed"
	case len(messages) == 0:
		return "unclassified_error"
	default:
		return "other_error"
	}
}

func hasTrueMCPError(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			if strings.EqualFold(key, "isError") && nested == true {
				return true
			}
			if strings.EqualFold(key, "error") && nested != nil && nested != false && nested != "" {
				return true
			}
			if hasTrueMCPError(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range item {
			if hasTrueMCPError(nested) {
				return true
			}
		}
	case string:
		var decoded any
		if json.Unmarshal([]byte(item), &decoded) == nil {
			return hasTrueMCPError(decoded)
		}
	}
	return false
}

func collectMCPErrorText(value any, underError bool, messages *[]string) {
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			collectMCPErrorText(nested, underError || strings.EqualFold(key, "error") || strings.EqualFold(key, "result"), messages)
		}
	case []any:
		for _, nested := range item {
			collectMCPErrorText(nested, underError, messages)
		}
	case string:
		if underError {
			*messages = append(*messages, item)
			return
		}
		var decoded any
		if json.Unmarshal([]byte(item), &decoded) == nil {
			collectMCPErrorText(decoded, underError, messages)
		}
	}
}

func safeCodexEventType(value string) string {
	switch value {
	case "thread.started", "turn.started", "turn.completed", "turn.failed", "item.started", "item.completed", "error":
		return value
	default:
		return "other"
	}
}

func safeCodexItemType(value string) string {
	switch value {
	case "mcp_tool_call", "mcp_tool_result", "mcp_call", "mcp_result", "agent_message", "reasoning", "reasoning_summary", "error", "command_execution", "web_search", "file_change", "plan_update", "collab_wait", "tool_call", "tool_result":
		return value
	case "":
		return "missing"
	default:
		return "other"
	}
}

func isSafeCodexIdentifier(value string) bool {
	if value == "" || len(value) > 48 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func statusCategory(value string) string {
	switch strings.ToLower(value) {
	case "completed", "complete", "success", "succeeded", "ok", "done":
		return "completed"
	case "failed", "failure", "error":
		return "failed"
	case "in_progress", "running", "pending":
		return "in_progress"
	case "":
		return "missing"
	default:
		return "other"
	}
}

func isCodexLiveQAToolName(value string) bool {
	name := strings.ToLower(value)
	return name == codexLiveQAToolName || strings.HasSuffix(name, "__"+codexLiveQAToolName) || strings.HasSuffix(name, "."+codexLiveQAToolName)
}

func isCodexLiveQAServerName(value string) bool {
	return strings.EqualFold(value, codexLiveQAServerID) || strings.Contains(strings.ToLower(value), strings.ToLower(codexLiveQAServerID))
}

func collectCodexLiveQAFieldPresence(value any, found map[string]bool) {
	allowed := map[string]bool{
		"id": true, "type": true, "status": true, "server": true, "server_name": true,
		"mcp_server": true, "tool": true, "tool_name": true, "name": true, "result": true,
		"content": true, "text": true, "error": true, "iserror": true, "arguments": true,
		"call_id": true, "output": true, "structuredcontent": true,
	}
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			lower := strings.ToLower(key)
			if allowed[lower] {
				found[lower] = true
			}
			collectCodexLiveQAFieldPresence(nested, found)
		}
	case []any:
		for _, nested := range item {
			collectCodexLiveQAFieldPresence(nested, found)
		}
	case string:
		var decoded any
		if json.Unmarshal([]byte(item), &decoded) == nil {
			collectCodexLiveQAFieldPresence(decoded, found)
		}
	}
}

func hasAnyField(value any, keys ...string) bool {
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[strings.ToLower(key)] = true
	}
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			if wanted[strings.ToLower(key)] || hasAnyField(nested, keys...) {
				return true
			}
		}
	case []any:
		for _, nested := range item {
			if hasAnyField(nested, keys...) {
				return true
			}
		}
	case string:
		var decoded any
		if json.Unmarshal([]byte(item), &decoded) == nil {
			return hasAnyField(decoded, keys...)
		}
	}
	return false
}

func firstStringField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(object, key); value != "" {
			return value
		}
	}
	return ""
}

func containsEmptyTargets(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			if strings.EqualFold(key, "targets") {
				if targets, ok := nested.([]any); ok && len(targets) == 0 {
					return true
				}
			}
			if containsEmptyTargets(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range item {
			if containsEmptyTargets(nested) {
				return true
			}
		}
	case string:
		var decoded any
		if json.Unmarshal([]byte(item), &decoded) == nil && containsEmptyTargets(decoded) {
			return true
		}
	}
	return false
}

func classifyCodexLiveFailure(trace []byte, stderrPath string) string {
	stderr, _, err := readPrivateTrace(stderrPath)
	if err != nil {
		return "codex_failed"
	}
	combined := strings.ToLower(string(trace) + "\n" + string(stderr))
	for _, marker := range []string{"not logged in", "login required", "authentication required", "unauthorized", "token has expired", "no cached credentials"} {
		if strings.Contains(combined, marker) {
			return "auth_blocked"
		}
	}
	return "codex_failed"
}
