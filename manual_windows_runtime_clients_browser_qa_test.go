//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const manualWindowsRuntimeClientsBrowserQAGate = "LAH_ENABLE_MANUAL_WINDOWS_RUNTIME_CLIENTS_BROWSER_QA"

const (
	manualRuntimeClientsCSRF         = "manual-runtime-clients-synthetic-csrf"
	manualRuntimeClientsSecretCanary = "manual-runtime-clients-settings-secret-canary"
	manualRuntimeClientsPathCanary   = "C:\\synthetic\\manual-runtime-clients-path-canary"
	manualRuntimeClientsErrorCanary  = "manual-runtime-clients-private-error-canary"
	manualRuntimeClientsBackupID     = "qa-backup-0000000000000001"
	manualRuntimeClientsPlanID       = "qa-plan-0000000000000001"
	manualRuntimeClientsRevision     = "qa-revision-00000000000001"
)

type manualRuntimeClientsBrowserController struct {
	mu               sync.Mutex
	configPath       string
	backupPath       string
	executable       string
	originalConfig   []byte
	applied          bool
	restored         bool
	operationEvents  []string
	operationEventCh chan string
	planClients      []string
}

func newManualRuntimeClientsBrowserController(
	configPath string,
	backupPath string,
	executable string,
	initial []byte,
) *manualRuntimeClientsBrowserController {
	return &manualRuntimeClientsBrowserController{
		configPath:       configPath,
		backupPath:       backupPath,
		executable:       executable,
		originalConfig:   append([]byte(nil), initial...),
		operationEventCh: make(chan string, 8),
		planClients:      []string{},
	}
}

func (f *manualRuntimeClientsBrowserController) InspectClient(
	_ context.Context,
	clientID string,
) (MCPClientStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if clientID == mcpClientIDClaude {
		return MCPClientStatus{
			ClientID:       clientID,
			Installed:      mcpClientInstalledNotObserved,
			Registration:   mcpClientRegistrationNotObserved,
			Connection:     mcpClientConnectionUnverified,
			ToolCall:       mcpClientConnectionUnverified,
			Backup:         mcpClientBackupNotCreated,
			EvidenceSource: mcpClientEvidenceNotObserved,
		}, nil
	}
	if clientID != mcpClientIDCodex {
		return MCPClientStatus{
			ClientID:       clientID,
			Installed:      mcpClientInstalled,
			Registration:   mcpClientRegistrationNotRegistered,
			Connection:     mcpClientConnectionUnverified,
			ToolCall:       mcpClientConnectionUnverified,
			Backup:         mcpClientBackupNotCreated,
			EvidenceSource: mcpClientEvidenceConfigObserved,
		}, nil
	}

	registration := mcpClientRegistrationNotRegistered
	backup := mcpClientBackupNotCreated
	if f.applied && !f.restored {
		registration = mcpClientRegistrationRegistered
		backup = mcpClientBackupApplied
	}
	if f.restored {
		backup = mcpClientBackupRestored
	}
	return MCPClientStatus{
		ClientID:       clientID,
		Installed:      mcpClientInstalled,
		Version:        manualRuntimeClientsPathCanary,
		Registration:   registration,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         backup,
		EvidenceSource: mcpClientEvidenceConfigObserved,
	}, nil
}

func (f *manualRuntimeClientsBrowserController) PlanClientRegistration(
	_ context.Context,
	spec MCPRegistrationSpec,
) (MCPClientRegistrationPlan, error) {
	f.mu.Lock()
	f.planClients = append(f.planClients, spec.ClientID)
	f.mu.Unlock()
	if spec.ClientID != mcpClientIDCodex && spec.ClientID != mcpClientIDGemini {
		return MCPClientRegistrationPlan{}, errors.New("unexpected synthetic client")
	}
	if spec.DisplayName != mcpClientRegistrationDisplayName || spec.Transport != MCPTransportStdio ||
		spec.Endpoint != "" || spec.Command != f.executable ||
		!slices.Equal(spec.Args, []string{"--mcp"}) || !slices.Equal(spec.Scope, []string{"user"}) {
		return MCPClientRegistrationPlan{}, errors.New("unexpected registration spec")
	}
	if spec.ClientID == mcpClientIDGemini {
		return MCPClientRegistrationPlan{}, fmt.Errorf("%s at %s", manualRuntimeClientsErrorCanary, manualRuntimeClientsPathCanary)
	}
	return MCPClientRegistrationPlan{
		ID:           manualRuntimeClientsPlanID,
		ClientID:     spec.ClientID,
		BaseRevision: manualRuntimeClientsRevision,
		Changes:      []string{mcpRegistrationChangeAdded},
	}, nil
}

func (f *manualRuntimeClientsBrowserController) BackupClientRegistration(
	_ context.Context,
	clientID string,
) (MCPClientBackupReceipt, error) {
	if clientID != mcpClientIDCodex {
		return MCPClientBackupReceipt{}, errors.New("unexpected synthetic backup client")
	}
	current, err := os.ReadFile(f.configPath)
	if err != nil || !bytes.Equal(current, f.originalConfig) {
		return MCPClientBackupReceipt{}, errors.New("synthetic client settings changed before backup")
	}
	if err := os.WriteFile(f.backupPath, f.originalConfig, 0o600); err != nil {
		return MCPClientBackupReceipt{}, errors.New("synthetic backup write failed")
	}
	f.recordOperation("backup")
	return MCPClientBackupReceipt{
		ID:        manualRuntimeClientsBackupID,
		Status:    mcpClientBackupCreated,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (f *manualRuntimeClientsBrowserController) ApplyClientRegistration(
	_ context.Context,
	planID string,
	revision string,
) (MCPClientStatus, error) {
	if planID != manualRuntimeClientsPlanID || revision != manualRuntimeClientsRevision {
		return MCPClientStatus{}, errors.New("synthetic plan was invalid")
	}
	if _, err := os.Stat(f.backupPath); err != nil {
		return MCPClientStatus{}, errClientRegistrationBackupRequired
	}
	updated := append(append([]byte(nil), f.originalConfig...), []byte("\n[qa-registration]\nregistered=true\n")...)
	if err := os.WriteFile(f.configPath, updated, 0o600); err != nil {
		return MCPClientStatus{}, errors.New("synthetic client write failed")
	}
	f.mu.Lock()
	f.applied = true
	f.restored = false
	f.mu.Unlock()
	f.recordOperation("apply")
	return MCPClientStatus{
		ClientID:       mcpClientIDCodex,
		Installed:      mcpClientInstalled,
		Registration:   mcpClientRegistrationRegistered,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         mcpClientBackupApplied,
		EvidenceSource: mcpClientEvidenceConfigObserved,
	}, nil
}

func (f *manualRuntimeClientsBrowserController) VerifyClientRegistration(
	ctx context.Context,
	clientID string,
) (MCPClientStatus, error) {
	return f.InspectClient(ctx, clientID)
}

func (f *manualRuntimeClientsBrowserController) RestoreClientRegistration(
	_ context.Context,
	clientID string,
	receiptID string,
) (MCPClientStatus, error) {
	if clientID != mcpClientIDCodex || receiptID != manualRuntimeClientsBackupID {
		return MCPClientStatus{}, errors.New("synthetic backup receipt was invalid")
	}
	original, err := os.ReadFile(f.backupPath)
	if err != nil {
		return MCPClientStatus{}, errors.New("synthetic backup read failed")
	}
	if err := os.WriteFile(f.configPath, original, 0o600); err != nil {
		return MCPClientStatus{}, errors.New("synthetic client restore failed")
	}
	f.mu.Lock()
	f.restored = true
	f.mu.Unlock()
	f.recordOperation("restore")
	return MCPClientStatus{
		ClientID:       mcpClientIDCodex,
		Installed:      mcpClientInstalled,
		Registration:   mcpClientRegistrationNotRegistered,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         mcpClientBackupRestored,
		EvidenceSource: mcpClientEvidenceConfigObserved,
	}, nil
}

func (f *manualRuntimeClientsBrowserController) recordOperation(name string) {
	f.mu.Lock()
	f.operationEvents = append(f.operationEvents, name)
	f.mu.Unlock()
	f.operationEventCh <- name
}

func (f *manualRuntimeClientsBrowserController) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.operationEvents...), append([]string(nil), f.planClients...)
}

type manualRuntimeClientsBrowserRuntime struct{}

func (manualRuntimeClientsBrowserRuntime) Status(context.Context) (MCPRuntimeStatus, error) {
	return MCPRuntimeStatus{State: "stopped"}, nil
}

func (manualRuntimeClientsBrowserRuntime) Start(context.Context) error { return nil }
func (manualRuntimeClientsBrowserRuntime) Stop(context.Context) error  { return nil }

type manualRuntimeClientsBrowserObservation struct {
	method string
	path   string
	status int
	origin string
}

// TestManualWindowsRuntimeClientsBrowserQA serves the production local UI
// handler against a temporary settings file and a synthetic client controller.
// The headed browser remains under manual control so visible labels and states
// can be checked independently of server-side route assertions.
func TestManualWindowsRuntimeClientsBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("manual browser acceptance runs only on native Windows")
	}
	if os.Getenv(manualWindowsRuntimeClientsBrowserQAGate) != "1" {
		t.Skipf("set %s=1 to start the opt-in headed browser harness", manualWindowsRuntimeClientsBrowserQAGate)
	}

	tempDir := t.TempDir()
	settingsPath := filepath.Join(tempDir, "app-settings.json")
	if err := writeConfig(settingsPath, config{Version: configVersion}); err != nil {
		t.Fatal("write isolated app settings")
	}
	initialSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal("read isolated app settings")
	}
	clientConfigPath := filepath.Join(tempDir, "synthetic-client-settings.txt")
	backupPath := filepath.Join(tempDir, "synthetic-client-backup.txt")
	initialClientConfig := []byte("synthetic-local-client-settings=" + manualRuntimeClientsSecretCanary + "\n")
	if err := os.WriteFile(clientConfigPath, initialClientConfig, 0o600); err != nil {
		t.Fatal("write isolated synthetic client settings")
	}
	executable := filepath.Join(tempDir, "synthetic-local-agent-harness.exe")
	controller := newManualRuntimeClientsBrowserController(
		clientConfigPath,
		backupPath,
		executable,
		initialClientConfig,
	)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen on product loopback origin")
	}
	appHost := listener.Addr().String()
	productURL := "http://" + appHost
	a := &app{
		configPath:         settingsPath,
		clientRegistration: controller,
		runtime:            manualRuntimeClientsBrowserRuntime{},
		executable:         executable,
		host:               appHost,
		csrf:               manualRuntimeClientsCSRF,
	}
	productHandler := newLocalUIHandler(a)

	completion := make(chan struct{})
	var completionOnce sync.Once
	var observationMu sync.Mutex
	observations := []manualRuntimeClientsBrowserObservation{}
	blockedRequests := 0
	responseLeaks := []string{}
	observedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != appHost || r.URL.RawQuery != "" {
			observationMu.Lock()
			blockedRequests++
			observationMu.Unlock()
			http.Error(w, "This synthetic browser request is outside the QA allowlist.", http.StatusConflict)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/__qa/complete" {
			completionOnce.Do(func() { close(completion) })
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/__qa/probes" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html lang="en"><meta charset="utf-8"><title>Local QA probes</title><h1>Local QA request probes</h1><button id="csrf-probe" type="button">Send same-origin POST without CSRF</button><button id="unsupported-probe" type="button">Send unsupported-client request</button><pre id="result" role="status" aria-live="polite"></pre><script>
const send = async (values) => {
  const response = await fetch(%q, {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/x-www-form-urlencoded'}, body: new URLSearchParams(values)});
  document.getElementById('result').textContent = response.status + ' ' + await response.text();
};
document.getElementById('csrf-probe').addEventListener('click', () => send({client_id: 'codex'}));
document.getElementById('unsupported-probe').addEventListener('click', () => send({csrf: %q, client_id: 'unsupported'}));
</script></html>`,
				runtimeClientsUIPlanPath,
				manualRuntimeClientsCSRF,
			)
			return
		}
		if !manualRuntimeClientsBrowserAllowedRequest(r) {
			observationMu.Lock()
			blockedRequests++
			observationMu.Unlock()
			http.Error(w, "This synthetic browser request is outside the QA allowlist.", http.StatusConflict)
			return
		}

		recorder := httptest.NewRecorder()
		productHandler.ServeHTTP(recorder, r)
		body := recorder.Body.Bytes()
		leakCanaries := []string{
			manualRuntimeClientsSecretCanary,
			manualRuntimeClientsPathCanary,
			manualRuntimeClientsErrorCanary,
		}
		for _, canary := range leakCanaries {
			if bytes.Contains(body, []byte(canary)) {
				observationMu.Lock()
				responseLeaks = append(responseLeaks, r.URL.Path)
				observationMu.Unlock()
				break
			}
		}
		observationMu.Lock()
		observations = append(observations, manualRuntimeClientsBrowserObservation{
			method: r.Method,
			path:   r.URL.Path,
			status: recorder.Code,
			origin: strings.Join(r.Header.Values("Origin"), ","),
		})
		observationMu.Unlock()
		for name, values := range recorder.Header() {
			w.Header()[name] = append([]string(nil), values...)
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(body)
	})

	productServer := &http.Server{
		Handler:           observedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	productServeErrors := make(chan error, 1)
	go func() { productServeErrors <- productServer.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := productServer.Shutdown(ctx); err != nil {
			_ = productServer.Close()
		}
	})

	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><body><h1>QA cross-origin form</h1><form method="post" action="http://%s%s"><input type="hidden" name="csrf" value="%s"><input type="hidden" name="client_id" value="codex"><button type="submit">Send cross-origin request</button></form></body></html>`,
			appHost,
			runtimeClientsUIPlanPath,
			manualRuntimeClientsCSRF,
		)
	}))
	t.Cleanup(attacker.Close)

	fmt.Fprintf(os.Stderr, "U02 isolated headed-browser product page: %s/\n", productURL)
	fmt.Fprintf(os.Stderr, "U02 isolated cross-origin form page: %s/\n", attacker.URL)
	fmt.Fprintf(os.Stderr, "Same-origin CSRF/unsupported-client probes: %s/__qa/probes\n", productURL)
	fmt.Fprintln(os.Stderr, "Use a headed Windows browser. Confirm Codex is observed and Claude is not observed/disabled; review Codex, create backup, apply, then confirm and restore. Click both same-origin probe buttons and submit the cross-origin form. Gemini's fixed failure is synthetic and should show only a generic error. Finish only after the Codex page visibly shows restored backup and unverified connection/tool-call status; then request /__qa/complete from the product page.")
	_ = os.Stderr.Sync()

	select {
	case <-completion:
	case err := <-productServeErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("synthetic product browser server stopped unexpectedly: %v", err)
		}
		t.Fatal("synthetic product browser server stopped before browser QA completed")
	case <-time.After(5 * time.Minute):
		t.Fatal("timed out waiting for the headed browser to finish isolated registration QA")
	}

	operations, planClients := controller.snapshot()
	if !slices.Equal(operations, []string{"backup", "apply", "restore"}) {
		t.Errorf("synthetic client mutations=%v; want backup/apply/restore in order", operations)
	}
	if !slices.Equal(planClients, []string{mcpClientIDGemini, mcpClientIDCodex}) {
		t.Errorf("synthetic plans=%v; want one safe-error probe followed by Codex plan", planClients)
	}
	select {
	case operation := <-controller.operationEventCh:
		if operation != "backup" {
			t.Errorf("first browser mutation=%q; want backup", operation)
		}
	default:
		t.Error("browser did not reach the synthetic backup operation")
	}
	for _, want := range []string{"apply", "restore"} {
		select {
		case operation := <-controller.operationEventCh:
			if operation != want {
				t.Errorf("next browser mutation=%q; want %q", operation, want)
			}
		case <-time.After(time.Second):
			t.Errorf("browser did not reach the synthetic %s operation", want)
		}
	}

	settings, err := os.ReadFile(settingsPath)
	if err != nil || !bytes.Equal(settings, initialSettings) {
		t.Error("browser registration flow changed the isolated app settings")
	}
	backup, backupErr := os.ReadFile(backupPath)
	if backupErr != nil || !bytes.Equal(backup, initialClientConfig) {
		t.Error("synthetic private backup did not preserve only the initial temp config bytes")
	}
	clientConfig, clientConfigErr := os.ReadFile(clientConfigPath)
	if clientConfigErr != nil || !bytes.Equal(clientConfig, initialClientConfig) {
		t.Error("restore did not return the synthetic temp client settings to their initial bytes")
	}

	observationMu.Lock()
	defer observationMu.Unlock()
	if blockedRequests != 0 {
		t.Errorf("browser attempted %d requests outside the local synthetic allowlist", blockedRequests)
	}
	if len(responseLeaks) != 0 {
		t.Errorf("synthetic canary appeared in response bodies on routes: %v", responseLeaks)
	}
	if !manualRuntimeClientsSawStatus(observations, runtimeClientsUIPlanPath, http.StatusForbidden, "http://"+appHost) {
		t.Error("same-origin browser request without CSRF did not receive 403")
	}
	if !manualRuntimeClientsSawStatus(observations, runtimeClientsUIPlanPath, http.StatusForbidden, "http://"+strings.TrimPrefix(attacker.URL, "http://")) {
		t.Error("cross-origin browser form did not reach the production Origin defense with 403")
	}
	if !manualRuntimeClientsSawStatus(observations, runtimeClientsUIPlanPath, http.StatusBadRequest, "http://"+appHost) {
		t.Error("same-origin unsupported-client probe did not return a fixed 400 response")
	}
	if !manualRuntimeClientsSawStatus(observations, runtimeClientsUIPlanPath, http.StatusOK, "http://"+appHost) {
		t.Error("headed browser did not successfully submit the reviewed registration plan")
	}
}

func manualRuntimeClientsBrowserAllowedRequest(r *http.Request) bool {
	if r.URL.RawQuery != "" {
		return false
	}
	if r.Method == http.MethodGet {
		return r.URL.Path == "/" || r.URL.Path == runtimeClientsUIStatusPath
	}
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case runtimeClientsUIPlanPath, runtimeClientsUIBackupPath, runtimeClientsUIApplyPath,
		runtimeClientsUIVerifyPath, runtimeClientsUIRestorePath:
		return true
	default:
		return false
	}
}

func manualRuntimeClientsSawStatus(
	observations []manualRuntimeClientsBrowserObservation,
	path string,
	status int,
	origin string,
) bool {
	for _, observation := range observations {
		if observation.method == http.MethodPost && observation.path == path &&
			observation.status == status && observation.origin == origin {
			return true
		}
	}
	return false
}
