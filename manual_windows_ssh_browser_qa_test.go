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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	q01BrowserAcceptanceEnv  = "LAH_MANUAL_WINDOWS_SSH_BROWSER_QA"
	q01BrowserTargetName     = "Q01 Synthetic SSH Target"
	q01BrowserTargetAlias    = "q01-synthetic-alias"
	q01BrowserReboundAlias   = "q01-synthetic-rebound-alias"
	q01BrowserReadOperation  = "Q01 Synthetic Read Status"
	q01BrowserPreOperation   = "Q01 Synthetic Write Preapproved"
	q01BrowserCallOperation  = "Q01 Synthetic Write Per Call"
	q01BrowserParameter      = "namespace"
	q01BrowserParameterValue = "q01-synthetic-namespace"
	q01BrowserCanary         = "Q01_FAKE_CREDENTIAL_CANARY_ONLY"
	q01BrowserCredentialRef  = "cred:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// TestManualWindowsSSHBrowserAcceptance is intentionally opt-in. It serves the
// production local UI mux on loopback and waits while a headed browser submits
// a synthetic target and three fixed operations. No real SSH process,
// Credential Manager, environment variable, client config, or service is used.
func TestManualWindowsSSHBrowserAcceptance(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv(q01BrowserAcceptanceEnv) != "1" {
		t.Skip("set LAH_MANUAL_WINDOWS_SSH_BROWSER_QA=1 on Windows to run the headed browser acceptance")
	}

	configPath := filepath.Join(t.TempDir(), "settings.json")
	cfg := config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID:            "secret:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Name:          "Q01 Synthetic Credential",
			Purpose:       "synthetic output leak check",
			CredentialRef: q01BrowserCredentialRef,
		}},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatalf("write synthetic version %d settings: %v", configVersion, err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host := listener.Addr().String()
	secrets := &q01BrowserFakeSecretStore{values: map[string][]byte{
		q01BrowserCredentialRef: []byte(q01BrowserCanary),
	}}
	runner := &q01BrowserRecordingSSHRunner{}
	approver := &q01BrowserSSHApprover{approved: true}
	ssh := newSSHOperationController(newSSHConfigStore(configPath), runner, approver)
	drafts := &q01BrowserUnusedDraftController{}
	a := &app{
		configPath:  configPath,
		secrets:     secrets,
		ssh:         ssh,
		setupDrafts: drafts,
		host:        host,
		csrf:        "q01-synthetic-browser-csrf",
	}
	audit := &q01BrowserUIAudit{next: newLocalUIHandler(a)}
	server := &http.Server{Handler: audit}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	continuePath, err := filepath.Abs(filepath.Join("..", "browser-flow-complete.signal"))
	if err != nil {
		t.Fatalf("resolve browser completion signal path: %v", err)
	}
	if _, err := os.Lstat(continuePath); err == nil {
		t.Fatalf("browser completion signal already exists; preserving it")
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect browser completion signal: %v", err)
	}
	fmt.Fprintf(os.Stdout, "Q01_BROWSER_URL=http://%s/\n", host)
	fmt.Fprintf(os.Stdout, "After saving the synthetic target and all three fixed operations, create this empty file to continue: %s\n", continuePath)
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for the headed browser flow")
		case <-ticker.C:
			if _, err := os.Stat(continuePath); err == nil {
				if err := os.Remove(continuePath); err != nil {
					t.Fatalf("remove the owned browser completion signal: %v", err)
				}
				goto browserDone
			} else if !os.IsNotExist(err) {
				t.Fatalf("inspect browser completion signal: %v", err)
			}
		}
	}

browserDone:

	saved, err := readConfig(configPath)
	if err != nil {
		t.Fatalf("read temporary settings after browser flow: %v", err)
	}
	if saved.Version != configVersion || len(saved.SSHTargets) != 1 {
		t.Fatalf("browser did not save one target into version %d settings", configVersion)
	}
	target := saved.SSHTargets[0]
	if target.Name != q01BrowserTargetName || target.Alias != q01BrowserTargetAlias || len(target.Operations) != 3 {
		t.Fatalf("browser target or fixed operations did not match the synthetic flow")
	}
	operations := make(map[string]SSHOperationDefinition, len(target.Operations))
	for _, operation := range target.Operations {
		operations[operation.Name] = operation
	}
	readOperation, hasRead := operations[q01BrowserReadOperation]
	preapprovedOperation, hasPreapproved := operations[q01BrowserPreOperation]
	perCallOperation, hasPerCall := operations[q01BrowserCallOperation]
	if !hasRead || !hasPreapproved || !hasPerCall {
		t.Fatalf("browser did not save all three named synthetic operations")
	}
	if readOperation.Risk != SSHRiskReadOnly || readOperation.Approval.Mode != OperationApprovalReadOnly || len(readOperation.Approval.Scope) != 0 {
		t.Fatalf("saved browser operation is not fixed read-only with the read-only approval policy")
	}
	if preapprovedOperation.Risk != SSHRiskStateChanging || preapprovedOperation.Approval.Mode != OperationApprovalPreapproved ||
		len(preapprovedOperation.Approval.Scope) != 1 || !q01BrowserHasNamespaceParameter(preapprovedOperation) {
		t.Fatalf("saved browser preapproval does not match the state-changing exact-scope operation")
	}
	if perCallOperation.Risk != SSHRiskStateChanging || perCallOperation.Approval.Mode != OperationApprovalPerCall ||
		len(perCallOperation.Approval.Scope) != 0 || !q01BrowserHasNamespaceParameter(perCallOperation) {
		t.Fatalf("saved browser per-call operation does not match the state-changing confirmation policy")
	}
	parameters := map[string]json.RawMessage{q01BrowserParameter: json.RawMessage(`"q01-synthetic-namespace"`)}
	expectedScope := sshApprovalScopeFingerprint(target.ID, target.Alias, preapprovedOperation.ID, map[string]string{
		q01BrowserParameter: q01BrowserParameterValue,
	})
	if preapprovedOperation.Approval.Scope[0] != expectedScope {
		t.Fatalf("saved preapproval scope was not bound to the exact alias, operation, and parameter set")
	}

	result, err := ssh.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: readOperation.ID,
	})
	if err != nil || result.Status != "succeeded" || result.RemoteOutcome != SSHRemoteOutcomeConfirmed {
		t.Fatalf("fixed read-only controller call did not succeed against the fake runner")
	}
	if strings.Contains(result.Output, q01BrowserCredentialRef) || strings.Contains(result.Output, q01BrowserCanary) {
		t.Fatalf("fake read-only result exposed a synthetic credential reference or canary")
	}
	if got := runner.snapshot(); got.starts != 1 || got.alias != q01BrowserTargetAlias || len(got.commandHash) != sha256.Size*2 {
		t.Fatalf("read-only call did not use exactly one fake command for the registered alias")
	}
	if approver.calls != 0 {
		t.Fatalf("read-only operation unexpectedly requested per-call approval")
	}

	wrongScopeParameters := map[string]json.RawMessage{q01BrowserParameter: json.RawMessage(`"q01-other-namespace"`)}
	result, err = ssh.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: preapprovedOperation.ID, Parameters: wrongScopeParameters,
	})
	if err != nil || result.Status != "rejected" || runner.snapshot().starts != 1 || approver.calls != 0 {
		t.Fatalf("preapproval did not reject a parameter set outside its exact scope")
	}
	result, err = ssh.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: preapprovedOperation.ID, Parameters: parameters,
	})
	if err != nil || result.Status != "succeeded" || approver.calls != 0 || runner.snapshot().starts != 2 {
		t.Fatalf("exactly preapproved state-changing operation did not run without a per-call prompt")
	}
	result, err = ssh.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: perCallOperation.ID, Parameters: parameters,
	})
	if err != nil || result.Status != "succeeded" || approver.calls != 1 || runner.snapshot().starts != 3 {
		t.Fatalf("state-changing per-call operation did not require and pass fake local approval")
	}
	if approver.request.TargetName != q01BrowserTargetName || approver.request.Operation != q01BrowserCallOperation || len(approver.request.Parameters) != 1 {
		t.Fatalf("per-call approval request did not describe the registered synthetic operation")
	}

	current, err := ssh.loadSnapshot(context.Background())
	if err != nil || len(current.Targets) != 1 {
		t.Fatalf("load synthetic SSH target before alias binding check")
	}
	rebound := cloneSSHTarget(current.Targets[0])
	rebound.Alias = q01BrowserReboundAlias
	if _, _, err := ssh.SaveTarget(context.Background(), rebound, current.Revision); err != nil {
		t.Fatalf("rebind the temporary synthetic alias for the scope check")
	}
	result, err = ssh.RunSSHOperation(context.Background(), SSHOperationInput{
		TargetID: target.ID, OperationID: preapprovedOperation.ID, Parameters: parameters,
	})
	if err != nil || result.Status != "rejected" || runner.snapshot().starts != 3 || approver.calls != 1 {
		t.Fatalf("preapproval was not blocked after its registered target alias changed")
	}

	current, err = ssh.loadSnapshot(context.Background())
	if err != nil || len(current.Targets) != 1 {
		t.Fatalf("load settings before the CSRF and Origin rejection checks")
	}
	attackForm := url.Values{
		"csrf": {a.csrf}, "expected_revision": {current.Revision}, "name": {"Q01 Must Not Save"},
		"alias": {"q01-blocked-alias"}, "reviewed_alias": {"yes"},
	}
	if status := q01BrowserPostTarget(t, host, attackForm, "http://attacker.invalid", ""); status != http.StatusForbidden {
		t.Fatalf("wrong-origin browser route request was not rejected")
	}
	attackForm.Set("csrf", "q01-invalid-csrf")
	if status := q01BrowserPostTarget(t, host, attackForm, "http://"+host, ""); status != http.StatusForbidden {
		t.Fatalf("invalid-CSRF browser route request was not rejected")
	}
	attackForm.Set("csrf", a.csrf)
	if status := q01BrowserPostTarget(t, host, attackForm, "http://"+host, "attacker.invalid"); status != http.StatusNotFound {
		t.Fatalf("wrong-Host browser route request was not rejected")
	}
	afterRejectedPosts, err := readConfig(configPath)
	if err != nil || afterRejectedPosts.Version != configVersion || len(afterRejectedPosts.SSHTargets) != 1 ||
		afterRejectedPosts.SSHTargets[0].Name != q01BrowserTargetName || afterRejectedPosts.SSHTargets[0].Alias != q01BrowserReboundAlias {
		t.Fatalf("rejected Origin, CSRF, or Host requests changed temporary settings")
	}
	if secrets.counts() != (q01BrowserSecretCounts{}) {
		t.Fatalf("SSH browser flow touched the fake credential store")
	}
	if drafts.calls != 0 {
		t.Fatalf("SSH browser flow entered the separate setup-draft approval flow")
	}

	records := audit.snapshot()
	var rootHTML, settingsHTML string
	var targetPosts, operationPosts int
	var wrongOriginRejected, wrongCSRFRejected, wrongHostRejected int
	for _, record := range records {
		if strings.Contains(record.body, q01BrowserCredentialRef) || strings.Contains(record.body, q01BrowserCanary) {
			t.Fatalf("a local UI response exposed a synthetic credential reference or canary")
		}
		if record.method == http.MethodGet && record.path == "/" {
			rootHTML = record.body
		}
		if record.method == http.MethodGet && record.path == "/ssh-settings" {
			settingsHTML = record.body
		}
		if record.method == http.MethodPost && record.path == "/save-ssh-target" && record.status == http.StatusSeeOther {
			targetPosts++
			if record.origin != "http://"+host || record.host != host {
				t.Fatalf("browser target form did not reach the exact-origin production route")
			}
		}
		if record.method == http.MethodPost && record.path == "/save-ssh-target" &&
			record.status == http.StatusForbidden && record.origin == "http://attacker.invalid" {
			wrongOriginRejected++
		}
		if record.method == http.MethodPost && record.path == "/save-ssh-target" &&
			record.status == http.StatusForbidden && record.origin == "http://"+host {
			wrongCSRFRejected++
		}
		if record.method == http.MethodPost && record.path == "/save-ssh-target" &&
			record.status == http.StatusNotFound && record.host == "attacker.invalid" {
			wrongHostRejected++
		}
		if record.method == http.MethodPost && record.path == "/save-ssh-operation" && record.status == http.StatusSeeOther {
			operationPosts++
			if record.origin != "http://"+host || record.host != host {
				t.Fatalf("browser operation form did not reach the exact-origin production route")
			}
		}
	}
	if rootHTML == "" || strings.Count(rootHTML, `id="ssh-settings-entry"`) != 1 || strings.Count(rootHTML, `href="/ssh-settings"`) != 1 {
		t.Fatalf("production root page did not render exactly one SSH settings entry")
	}
	if settingsHTML == "" || strings.Count(settingsHTML, `id="ssh-settings-heading"`) != 1 ||
		targetPosts != 1 || operationPosts != 3 || wrongOriginRejected != 1 || wrongCSRFRejected != 1 || wrongHostRejected != 1 {
		t.Fatalf("production SSH page or its expected save/security routes were not exercised")
	}
	if got := secrets.counts(); got != (q01BrowserSecretCounts{}) {
		t.Fatalf("fake credential store was unexpectedly accessed during response rendering")
	}
	t.Log("native Windows headed browser: production target and three operation form saves passed; fake-controller read/write/approval/scope and POST-boundary checks passed")
}

func q01BrowserHasNamespaceParameter(operation SSHOperationDefinition) bool {
	return len(operation.Parameters) == 1 && operation.Parameters[0].Name == q01BrowserParameter &&
		operation.Parameters[0].Type == "string" && operation.Parameters[0].Required
}

func q01BrowserPostTarget(t *testing.T, host string, form url.Values, origin, overrideHost string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+host+uiRouteSaveSSHTarget, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("create production route request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	if overrideHost != "" {
		request.Host = overrideHost
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send production route request: %v", err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

type q01BrowserSecretCounts struct {
	loads   int
	saves   int
	deletes int
}

type q01BrowserFakeSecretStore struct {
	mu     sync.Mutex
	values map[string][]byte
	count  q01BrowserSecretCounts
}

func (s *q01BrowserFakeSecretStore) Save(ref string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count.saves++
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *q01BrowserFakeSecretStore) Load(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count.loads++
	value, ok := s.values[ref]
	if !ok {
		return nil, errors.New("synthetic credential unavailable")
	}
	return append([]byte(nil), value...), nil
}

func (s *q01BrowserFakeSecretStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count.deletes++
	delete(s.values, ref)
	return nil
}

func (s *q01BrowserFakeSecretStore) counts() q01BrowserSecretCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

type q01BrowserRecordingSSHRunner struct {
	mu          sync.Mutex
	starts      int
	alias       string
	commandHash string
}

func (r *q01BrowserRecordingSSHRunner) Start(_ context.Context, alias, command string) (sshRunningProcess, error) {
	hash := sha256.Sum256([]byte(command))
	r.mu.Lock()
	r.starts++
	r.alias = alias
	r.commandHash = hex.EncodeToString(hash[:])
	r.mu.Unlock()
	return &q01BrowserRunningProcess{result: sshProcessResult{
		Stdout: []byte("q01 synthetic read-only result\n"), ExitKnown: true,
	}}, nil
}

func (r *q01BrowserRecordingSSHRunner) snapshot() (got struct {
	starts      int
	alias       string
	commandHash string
}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	got.starts, got.alias, got.commandHash = r.starts, r.alias, r.commandHash
	return got
}

type q01BrowserRunningProcess struct{ result sshProcessResult }

func (p *q01BrowserRunningProcess) Wait() sshProcessResult { return p.result }

type q01BrowserSSHApprover struct {
	approved bool
	calls    int
	request  sshApprovalRequest
}

func (a *q01BrowserSSHApprover) Confirm(_ context.Context, request sshApprovalRequest) (bool, error) {
	a.calls++
	a.request = request
	return a.approved, nil
}

type q01BrowserUnusedDraftController struct{ calls int }

func (d *q01BrowserUnusedDraftController) SubmitSetupDraft(context.Context, SetupDraftSubmission) (SetupDraftReview, error) {
	d.calls++
	return SetupDraftReview{}, errors.New("setup draft controller is disabled for this SSH-only acceptance")
}

func (d *q01BrowserUnusedDraftController) ReviewSetupDraft(context.Context, string, string, string) (SetupDraftReview, error) {
	d.calls++
	return SetupDraftReview{}, errors.New("setup draft controller is disabled for this SSH-only acceptance")
}

func (d *q01BrowserUnusedDraftController) ApproveSetupDraft(context.Context, string, string, string) (SetupDraftApprovalResult, error) {
	d.calls++
	return SetupDraftApprovalResult{}, errors.New("setup draft controller is disabled for this SSH-only acceptance")
}

func (d *q01BrowserUnusedDraftController) RejectSetupDraft(context.Context, string, string, string) error {
	d.calls++
	return errors.New("setup draft controller is disabled for this SSH-only acceptance")
}

type q01BrowserUIAudit struct {
	next    http.Handler
	mu      sync.Mutex
	records []q01BrowserUIResponse
}

type q01BrowserUIResponse struct {
	method string
	path   string
	host   string
	origin string
	status int
	body   string
}

type q01BrowserCaptureWriter struct {
	http.ResponseWriter
	status int
	body   strings.Builder
}

func (w *q01BrowserCaptureWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *q01BrowserCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (a *q01BrowserUIAudit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	capture := &q01BrowserCaptureWriter{ResponseWriter: w}
	a.next.ServeHTTP(capture, r)
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	a.mu.Lock()
	a.records = append(a.records, q01BrowserUIResponse{
		method: r.Method, path: r.URL.Path, host: r.Host,
		origin: r.Header.Get("Origin"), status: status, body: capture.body.String(),
	})
	a.mu.Unlock()
}

func (a *q01BrowserUIAudit) snapshot() []q01BrowserUIResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]q01BrowserUIResponse(nil), a.records...)
}
