//go:build windows && integration && parallelmanual

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	q01ProcessChildRole = "LAH_Q01_PROCESS_ROLE"
	q01ProcessConfig    = "LAH_Q01_PROCESS_CONFIG"
	q01ProcessQueue     = "LAH_Q01_PROCESS_QUEUE"
	q01ProcessReady     = "LAH_Q01_PROCESS_READY"
	q01ProcessCSRF      = "LAH_Q01_PROCESS_CSRF"
	q01ProcessEvents    = "LAH_Q01_PROCESS_EVENTS"
	q01ProcessGate      = "LAH_ENABLE_MANUAL_WINDOWS_PARALLEL_INTEGRATION_QA"
)

type q01ProcessReadyInfo struct {
	URL  string `json:"url"`
	Host string `json:"host"`
}

type q01ProcessEvent struct {
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	ValueLength   int    `json:"value_length,omitempty"`
	ValueSHA256   string `json:"value_sha256,omitempty"`
	Alias         string `json:"alias,omitempty"`
	CommandSHA256 string `json:"command_sha256,omitempty"`
}

type q01ProcessEventSink struct {
	mu   sync.Mutex
	path string
}

func (s *q01ProcessEventSink) add(event q01ProcessEvent) {
	if s == nil || s.path == "" {
		return
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

type q01ProcessNamedSecrets struct {
	mu     sync.Mutex
	events *q01ProcessEventSink
	items  []NamedSecretView
}

func (f *q01ProcessNamedSecrets) ListNamedSecrets(context.Context) ([]NamedSecretView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]NamedSecretView(nil), f.items...), nil
}

func (f *q01ProcessNamedSecrets) SaveNamedSecret(_ context.Context, write NamedSecretWrite) (NamedSecretView, error) {
	digest := sha256.Sum256(write.Value)
	f.events.add(q01ProcessEvent{Kind: "named_secret_save", Name: write.Name, ValueLength: len(write.Value), ValueSHA256: hex.EncodeToString(digest[:])})
	f.mu.Lock()
	defer f.mu.Unlock()
	view := NamedSecretView{ID: "secret:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: write.Name, Purpose: write.Purpose, Configured: true}
	f.items = []NamedSecretView{view}
	return view, nil
}

func (f *q01ProcessNamedSecrets) DeleteNamedSecret(_ context.Context, id string) error {
	f.events.add(q01ProcessEvent{Kind: "named_secret_delete", Name: id})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = nil
	return nil
}

type q01ProcessEnvironment struct {
	mu      sync.Mutex
	events  *q01ProcessEventSink
	entries []UserEnvironmentEntry
}

func (f *q01ProcessEnvironment) ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]UserEnvironmentEntry{}, f.entries...), nil
}

func (f *q01ProcessEnvironment) SetUserEnvironment(_ context.Context, write UserEnvironmentWrite) (UserEnvironmentResult, error) {
	digest := sha256.Sum256(write.Value)
	f.events.add(q01ProcessEvent{Kind: "user_environment_set", Name: write.Name, ValueLength: len(write.Value), ValueSHA256: hex.EncodeToString(digest[:])})
	f.mu.Lock()
	f.entries = []UserEnvironmentEntry{{Name: write.Name, Configured: true}}
	f.mu.Unlock()
	return UserEnvironmentResult{Name: write.Name, Operation: "set", Applied: true, RequiresNewSession: true, NextAction: "Start a new test session."}, nil
}

func (f *q01ProcessEnvironment) DeleteUserEnvironment(_ context.Context, name string) (UserEnvironmentResult, error) {
	f.events.add(q01ProcessEvent{Kind: "user_environment_delete", Name: name})
	f.mu.Lock()
	f.entries = nil
	f.mu.Unlock()
	return UserEnvironmentResult{Name: name, Operation: "delete", Applied: true, RequiresNewSession: true}, nil
}

type q01ProcessSSHRunner struct{ events *q01ProcessEventSink }

func (r *q01ProcessSSHRunner) Start(_ context.Context, alias, command string) (sshRunningProcess, error) {
	digest := sha256.Sum256([]byte(command))
	r.events.add(q01ProcessEvent{Kind: "synthetic_ssh_start", Alias: alias, CommandSHA256: hex.EncodeToString(digest[:])})
	return &fakeSSHRunningProcess{result: sshProcessResult{Stdout: []byte("synthetic-list-ok"), ExitCode: 0, ExitKnown: true}}, nil
}

// TestQ01ParallelProcessChild is a test-binary entry point for isolated child
// roles. MCP and UI run in distinct processes against only the temp v9 config
// and test-injected queue file.
func TestQ01ParallelProcessChild(t *testing.T) {
	role := os.Getenv(q01ProcessChildRole)
	if role == "" {
		return
	}
	configPath, queuePath := os.Getenv(q01ProcessConfig), os.Getenv(q01ProcessQueue)
	if configPath == "" || queuePath == "" {
		t.Fatal("test child is missing its isolated config or queue path")
	}
	now := time.Now
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, now)
	if err != nil {
		t.Fatalf("create isolated queue: %v", err)
	}
	drafts := newSetupDraftControllerWithQueue(newSetupDraftConfigStore(configPath), queue)
	events := &q01ProcessEventSink{path: os.Getenv(q01ProcessEvents)}
	runner := &q01ProcessSSHRunner{events: events}
	ssh := newSSHOperationController(newSSHConfigStore(configPath), runner, nil)
	a := &app{configPath: configPath, setupDrafts: drafts, ssh: ssh}
	switch role {
	case "mcp":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := a.mcpServer().Run(ctx, &mcp.StdioTransport{MaxLineLength: 1 << 20}); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run production stdio MCP server: %v", err)
		}
	case "ui":
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen on synthetic IPv4 loopback: %v", err)
		}
		a.host = listener.Addr().String()
		a.csrf = os.Getenv(q01ProcessCSRF)
		a.namedSecrets = &q01ProcessNamedSecrets{events: events}
		a.userEnvironment = &q01ProcessEnvironment{events: events}
		server := &http.Server{Handler: newLocalUIHandler(a), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
		ready := q01ProcessReadyInfo{URL: "http://" + a.host, Host: a.host}
		encoded, err := json.Marshal(ready)
		if err != nil {
			t.Fatalf("encode isolated UI endpoint: %v", err)
		}
		if err := os.WriteFile(os.Getenv(q01ProcessReady), encoded, 0o600); err != nil {
			t.Fatalf("publish isolated UI endpoint: %v", err)
		}
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve production local UI handler: %v", err)
		}
	default:
		t.Fatalf("unsupported Q01 child role %q", role)
	}
}

// TestManualWindowsParallelProcessBrowserAcceptance runs the production MCP
// stdio server and production local UI handler in separate processes. It
// rejects a synthetic setup draft through a headed browser and confirms that
// rejection survives process restart without changing the config file.
func TestManualWindowsParallelProcessBrowserAcceptance(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Q01 process/browser acceptance requires native Windows")
	}
	if os.Getenv(q01ProcessChildRole) != "" {
		t.Skip("child process role is handled by TestQ01ParallelProcessChild")
	}
	if os.Getenv(q01ProcessGate) != "1" {
		t.Skipf("set %s=1 in this test process to run the isolated process/browser acceptance", q01ProcessGate)
	}

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "settings-v9.json")
	queuePath := filepath.Join(workDir, "queue-home", setupDraftQueueFileName)
	eventsPath := filepath.Join(workDir, "events.jsonl")
	readyPath := filepath.Join(workDir, "ui-ready.json")
	csrf := "q01-synthetic-csrf-" + strings.Repeat("7", 24)
	if err := writeConfig(configPath, config{Version: configVersion, NamedSecrets: []namedSecretMetadata{{
		ID: "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "Q01 Synthetic Credential", Purpose: "Acceptance fixture only",
		CredentialRef: "cred:cccccccccccccccccccccccccccccccc",
	}}}); err != nil {
		t.Fatalf("write synthetic v9 settings: %v", err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read synthetic v9 settings: %v", err)
	}
	snapshot, err := newSetupDraftConfigStore(configPath).Snapshot()
	if err != nil {
		t.Fatalf("read synthetic settings revision: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	mcpCommand := q01ChildCommand(t, "mcp", configPath, queuePath, readyPath, csrf, eventsPath)
	stdio, err := mcp.NewClient(&mcp.Implementation{Name: "q01-separate-process-acceptance", Version: "1"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: mcpCommand}, nil)
	if err != nil {
		t.Fatalf("start production MCP stdio child: %v", err)
	}
	defer func() { _ = stdio.Close() }()
	tools, err := stdio.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools from production stdio child: %v", err)
	}
	toolNames := make(map[string]bool)
	for _, tool := range tools.Tools {
		toolNames[tool.Name] = true
	}
	if !toolNames["submit_setup_draft"] || !toolNames[mcpToolRegisteredSSHTargets] || !toolNames[mcpToolRunSSHOperation] {
		t.Fatalf("production stdio child did not expose the fixed draft/SSH tools: %#v", toolNames)
	}

	submission := SetupDraftSubmission{
		BaseRevision: snapshot.Revision,
		Sources:      []SetupDraftSource{{Kind: "user_provided", Label: "Q01 synthetic setup fixture", Confidence: "high"}},
		Missing:      []string{}, Connections: []SetupDraftConnection{{
			Kind: SetupServiceGitHub, Action: SetupDraftAdd, Name: "Q01 Synthetic GitHub target",
			Origin: "https://github.example.invalid", Repository: "qa/synthetic-repository", CredentialName: "Q01 Synthetic Credential",
		}}, Bundles: []SetupDraftBundle{}, SSHTargets: []SetupDraftSSHTarget{},
	}
	if _, normalizeErr := normalizeSetupDraftSubmission(submission); normalizeErr != nil {
		t.Fatalf("synthetic proposal fixture failed production normalization: %v", normalizeErr)
	}
	if _, planErr := buildSetupDraftPlan(snapshot, submission, setupDraftResourceIDGenerator("setupdraft:11111111111111111111111111111111")); planErr != nil {
		t.Fatalf("synthetic proposal fixture failed production planning: %v", planErr)
	}
	argumentBytes, err := json.Marshal(submission)
	if err != nil {
		t.Fatalf("encode synthetic setup proposal: %v", err)
	}
	var arguments map[string]any
	if err := json.Unmarshal(argumentBytes, &arguments); err != nil {
		t.Fatalf("decode synthetic proposal into MCP arguments: %v", err)
	}
	toolResult, err := stdio.CallTool(ctx, &mcp.CallToolParams{Name: "submit_setup_draft", Arguments: arguments})
	if err != nil || toolResult == nil || toolResult.IsError {
		t.Fatalf("submit synthetic proposal over production stdio: err=%v result-error=%t safe-result=%q", err, toolResult != nil && toolResult.IsError, q01TextFromMCPResult(toolResult))
	}
	reviewText := q01TextFromMCPResult(toolResult)
	var review SetupDraftReview
	if err := json.Unmarshal([]byte(reviewText), &review); err != nil || review.ID == "" || review.Digest == "" || review.BaseRevision != snapshot.Revision {
		t.Fatalf("production stdio did not return a usable safe review receipt: err=%v", err)
	}
	for _, forbidden := range []string{"password", "token", "credential_ref", "secret:"} {
		if strings.Contains(strings.ToLower(reviewText), forbidden) {
			t.Fatalf("synthetic review receipt contained forbidden secret metadata marker %q", forbidden)
		}
	}
	afterSubmit, err := os.ReadFile(configPath)
	if err != nil || !equalBytes(configBefore, afterSubmit) {
		t.Fatalf("draft submission changed config bytes: read err=%v", err)
	}

	uiProcess := q01StartUIChild(t, configPath, queuePath, readyPath, csrf, eventsPath)
	ready := q01WaitForReady(t, uiProcess, readyPath, 15*time.Second)
	browserSession := fmt.Sprintf("q01-draft-%d", time.Now().UnixNano())
	profilePath := filepath.Join(workDir, "playwright-profile")
	q01Playwright(t, browserSession, "open", ready.URL+"/ssh-settings", "--headed", "--profile", profilePath)
	page := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(page, "Review an AI setup draft") {
		t.Fatal("headed browser did not render the production setup-draft review route")
	}
	q01Playwright(t, browserSession, "fill", "#setup-draft-id", review.ID)
	q01Playwright(t, browserSession, "fill", "#setup-draft-revision", review.BaseRevision)
	q01Playwright(t, browserSession, "fill", "#setup-draft-digest", review.Digest)
	q01Playwright(t, browserSession, "click", "form[action='/review-setup-draft'] button[type='submit']")
	reviewPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(reviewPage, "Q01 Synthetic GitHub target") || !strings.Contains(reviewPage, "qa/synthetic-repository") {
		t.Fatal("production browser review page did not show the exact synthetic proposal")
	}
	q01Playwright(t, browserSession, "check", "input[name='confirm_reject']")
	q01Playwright(t, browserSession, "click", "form[action='/reject-setup-draft'] button[type='submit']")
	rejectedPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(rejectedPage, "setup draft was rejected and discarded") {
		t.Fatal("headed browser did not show the production rejection confirmation")
	}
	stopQ01Child(uiProcess)
	uiProcess = q01StartUIChild(t, configPath, queuePath, readyPath, csrf, eventsPath)
	ready = q01WaitForReady(t, uiProcess, readyPath, 15*time.Second)
	q01Playwright(t, browserSession, "goto", ready.URL+"/ssh-settings")
	q01Playwright(t, browserSession, "fill", "#setup-draft-id", review.ID)
	q01Playwright(t, browserSession, "fill", "#setup-draft-revision", review.BaseRevision)
	q01Playwright(t, browserSession, "fill", "#setup-draft-digest", review.Digest)
	q01Playwright(t, browserSession, "click", "form[action='/review-setup-draft'] button[type='submit']")
	replayPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(replayPage, "already been used") && !strings.Contains(replayPage, "already used") {
		t.Fatal("restarted production UI did not reject the already-discarded review")
	}

	// Exercise the real browser forms with fake controllers only. Values are
	// synthetic canaries; the child records their hashes and lengths, never the
	// values themselves, and the production config file must remain unchanged.
	q01Playwright(t, browserSession, "goto", ready.URL+"/")
	secretsPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(secretsPage, "Q01 Synthetic Credential") {
		t.Fatal("production root page did not show the synthetic named-secret form data")
	}
	secretCanary := "Q01_SYNTHETIC_SECRET_CANARY_7391"
	environmentCanary := "Q01_SYNTHETIC_ENVIRONMENT_CANARY_4826"
	q01Playwright(t, browserSession, "fill", "#lah-named-secret-name", "Q01 Browser Secret")
	q01Playwright(t, browserSession, "fill", "#lah-named-secret-purpose", "Synthetic browser acceptance")
	q01Playwright(t, browserSession, "fill", "#lah-named-secret-value", secretCanary)
	q01Playwright(t, browserSession, "click", "#lah-named-secret-save")
	secretSavedPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(secretSavedPage, "Q01 Browser Secret") || strings.Contains(secretSavedPage, secretCanary) {
		t.Fatal("named-secret form did not render safe saved metadata or exposed its synthetic value")
	}
	q01Playwright(t, browserSession, "fill", "#lah-user-environment-name", "Q01_SYNTHETIC_ENV")
	q01Playwright(t, browserSession, "fill", "#lah-user-environment-value", environmentCanary)
	q01Playwright(t, browserSession, "click", "#lah-user-environment-save")
	environmentSavedPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(environmentSavedPage, "Q01_SYNTHETIC_ENV") || strings.Contains(environmentSavedPage, environmentCanary) {
		t.Fatal("user-environment form did not render safe saved state or exposed its synthetic value")
	}
	eventsBytes, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read synthetic controller event receipt: %v", err)
	}
	if strings.Contains(string(eventsBytes), secretCanary) || strings.Contains(string(eventsBytes), environmentCanary) {
		t.Fatal("synthetic controller event receipt contained a submitted value")
	}
	wantEventDigests := map[string]struct {
		value  string
		length int
	}{
		"named_secret_save":    {value: secretCanary, length: len(secretCanary)},
		"user_environment_set": {value: environmentCanary, length: len(environmentCanary)},
	}
	seenEvents := make(map[string]bool, len(wantEventDigests))
	for _, line := range strings.Split(strings.TrimSpace(string(eventsBytes)), "\n") {
		if line == "" {
			continue
		}
		var event q01ProcessEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode synthetic controller event: %v", err)
		}
		want, ok := wantEventDigests[event.Kind]
		if !ok {
			continue
		}
		digest := sha256.Sum256([]byte(want.value))
		if event.ValueLength != want.length || event.ValueSHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("synthetic %s event did not match the submitted value digest/length", event.Kind)
		}
		seenEvents[event.Kind] = true
	}
	for kind := range wantEventDigests {
		if !seenEvents[kind] {
			t.Fatalf("production browser did not invoke the fake %s controller", kind)
		}
	}
	configAfterU01, err := os.ReadFile(configPath)
	if err != nil || !equalBytes(configBefore, configAfterU01) {
		t.Fatalf("fake-controller U01 browser flow changed config bytes: read err=%v", err)
	}

	// Submit a fresh draft over production stdio, explicitly approve its exact
	// reviewed proposal in the browser, restart the UI, and verify review replay
	// is rejected before a fresh stdio catalog reads the saved target.
	approvedResult, err := stdio.CallTool(ctx, &mcp.CallToolParams{Name: "submit_setup_draft", Arguments: arguments})
	if err != nil || approvedResult == nil || approvedResult.IsError {
		t.Fatalf("submit second synthetic proposal over production stdio: err=%v result-error=%t", err, approvedResult != nil && approvedResult.IsError)
	}
	var approvedReview SetupDraftReview
	if err := json.Unmarshal([]byte(q01TextFromMCPResult(approvedResult)), &approvedReview); err != nil || approvedReview.ID == "" || approvedReview.Digest == "" {
		t.Fatalf("production stdio did not return the second safe review receipt: err=%v", err)
	}
	q01Playwright(t, browserSession, "goto", ready.URL+"/ssh-settings")
	q01Playwright(t, browserSession, "fill", "#setup-draft-id", approvedReview.ID)
	q01Playwright(t, browserSession, "fill", "#setup-draft-revision", approvedReview.BaseRevision)
	q01Playwright(t, browserSession, "fill", "#setup-draft-digest", approvedReview.Digest)
	q01Playwright(t, browserSession, "click", "form[action='/review-setup-draft'] button[type='submit']")
	approvedReviewPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(approvedReviewPage, "Q01 Synthetic GitHub target") || !strings.Contains(approvedReviewPage, "qa/synthetic-repository") {
		t.Fatal("browser approval page did not show the exact second synthetic proposal")
	}
	q01Playwright(t, browserSession, "check", "input[name='confirm_approve']")
	q01Playwright(t, browserSession, "click", "form[action='/approve-setup-draft'] button[type='submit']")
	approvalPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(approvalPage, "The reviewed setup draft was saved") {
		t.Fatal("headed browser did not show production setup-draft approval confirmation")
	}
	stopQ01Child(uiProcess)
	uiProcess = q01StartUIChild(t, configPath, queuePath, readyPath, csrf, eventsPath)
	ready = q01WaitForReady(t, uiProcess, readyPath, 15*time.Second)
	q01Playwright(t, browserSession, "goto", ready.URL+"/ssh-settings")
	q01Playwright(t, browserSession, "fill", "#setup-draft-id", approvedReview.ID)
	q01Playwright(t, browserSession, "fill", "#setup-draft-revision", approvedReview.BaseRevision)
	q01Playwright(t, browserSession, "fill", "#setup-draft-digest", approvedReview.Digest)
	q01Playwright(t, browserSession, "click", "form[action='/review-setup-draft'] button[type='submit']")
	approvedReplayPage := q01Playwright(t, browserSession, "snapshot")
	if !strings.Contains(approvedReplayPage, "already been used") && !strings.Contains(approvedReplayPage, "already used") {
		t.Fatal("restarted production UI did not reject replay of the approved and claimed draft")
	}
	q01Playwright(t, browserSession, "close")
	stopQ01Child(uiProcess)

	if err := stdio.Close(); err != nil {
		t.Fatalf("close original production stdio child before fresh readback: %v", err)
	}
	freshCommand := q01ChildCommand(t, "mcp", configPath, queuePath, readyPath, csrf, eventsPath)
	freshStdio, err := mcp.NewClient(&mcp.Implementation{Name: "q01-fresh-catalog-readback", Version: "1"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: freshCommand}, nil)
	if err != nil {
		t.Fatalf("start fresh production stdio child after approval: %v", err)
	}
	defer func() { _ = freshStdio.Close() }()
	catalogResult, err := freshStdio.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	if err != nil || catalogResult == nil || catalogResult.IsError {
		t.Fatalf("fresh production stdio catalog call failed: err=%v result-error=%t", err, catalogResult != nil && catalogResult.IsError)
	}
	catalogBytes, err := json.Marshal(catalogResult)
	if err != nil || !strings.Contains(string(catalogBytes), "Q01 Synthetic GitHub target") {
		t.Fatalf("fresh production stdio catalog did not read the approved target: marshal err=%v", err)
	}

	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, time.Now)
	if err != nil {
		t.Fatalf("reopen test-injected queue after approval UI restart: %v", err)
	}
	pending, err := queue.Load(time.Now())
	if err != nil || len(pending) != 0 {
		t.Fatalf("consumed reviews survived queue restart: pending=%d err=%v", len(pending), err)
	}
	configAfterApproval, err := readConfig(configPath)
	if err != nil || len(configAfterApproval.GitHubTargets) != 1 || configAfterApproval.GitHubTargets[0].Name != "Q01 Synthetic GitHub target" {
		t.Fatalf("approved synthetic GitHub target was not persisted: target-count=%d err=%v", len(configAfterApproval.GitHubTargets), err)
	}
	t.Logf("Native Windows separate-process acceptance passed: production stdio submitted synthetic reviews; headed Chromium rejected one, approved another, restarted UI rejected both replays, fresh stdio catalog read the approved GitHub target, queue=%d; U01 forms used fake controllers and neither value was rendered or persisted in config.", len(pending))
}

func q01TextFromMCPResult(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, content := range result.Content {
		if block, ok := content.(*mcp.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func q01ChildCommand(t *testing.T, role, configPath, queuePath, readyPath, csrf, eventsPath string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestQ01ParallelProcessChild$")
	command.Env = q01ChildEnvironment(role, configPath, queuePath, readyPath, csrf, eventsPath)
	return command
}

func q01ChildEnvironment(role, configPath, queuePath, readyPath, csrf, eventsPath string) []string {
	keys := []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"}
	env := make([]string, 0, len(keys)+6)
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	for _, item := range []string{
		q01ProcessChildRole + "=" + role, q01ProcessConfig + "=" + configPath,
		q01ProcessQueue + "=" + queuePath, q01ProcessReady + "=" + readyPath,
		q01ProcessCSRF + "=" + csrf, q01ProcessEvents + "=" + eventsPath,
	} {
		env = append(env, item)
	}
	return env
}

type q01ManagedChild struct {
	command *exec.Cmd
	stderr  strings.Builder
}

func q01StartUIChild(t *testing.T, configPath, queuePath, readyPath, csrf, eventsPath string) *q01ManagedChild {
	t.Helper()
	_ = os.Remove(readyPath)
	child := &q01ManagedChild{command: q01ChildCommand(t, "ui", configPath, queuePath, readyPath, csrf, eventsPath)}
	child.command.Stderr = &child.stderr
	if err := child.command.Start(); err != nil {
		t.Fatalf("start production local UI child: %v", err)
	}
	t.Cleanup(func() { stopQ01Child(child) })
	return child
}

func q01WaitForReady(t *testing.T, child *q01ManagedChild, readyPath string, timeout time.Duration) q01ProcessReadyInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if child.command.ProcessState != nil {
			t.Fatalf("production UI child exited before publishing its endpoint: %s", child.stderr.String())
		}
		if encoded, err := os.ReadFile(readyPath); err == nil {
			var info q01ProcessReadyInfo
			if json.Unmarshal(encoded, &info) == nil && strings.HasPrefix(info.URL, "http://127.0.0.1:") && info.Host != "" {
				return info
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for production UI child endpoint: %s", child.stderr.String())
	return q01ProcessReadyInfo{}
}

func stopQ01Child(child *q01ManagedChild) {
	if child == nil || child.command == nil || child.command.Process == nil {
		return
	}
	_ = child.command.Process.Kill()
	_, _ = child.command.Process.Wait()
}

func q01Playwright(t *testing.T, session string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	parts := []string{"--yes", "--package", "@playwright/cli", "playwright-cli", "--session", session}
	parts = append(parts, args...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, "'"+strings.ReplaceAll(part, "'", "''")+"'")
	}
	script := "& npx " + strings.Join(quoted, " ")
	command := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	command.Dir = filepath.Dir(os.Args[0])
	outputFile, err := os.CreateTemp(t.TempDir(), "playwright-command-*.log")
	if err != nil {
		t.Fatalf("create temporary Playwright output file: %v", err)
	}
	outputPath := outputFile.Name()
	command.Stdout = outputFile
	command.Stderr = outputFile
	runErr := command.Run()
	closeErr := outputFile.Close()
	output, readErr := os.ReadFile(outputPath)
	if runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if runErr == nil && readErr != nil {
		runErr = readErr
	}
	if runErr != nil {
		// Do not include browser/network output, which could contain synthetic
		// form values; preserve only the command verb and process error.
		verb := "command"
		if len(args) > 0 {
			verb = args[0]
		}
		t.Fatalf("headed Playwright %s failed: %v (output redacted)", verb, runErr)
	}
	return string(output)
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
