package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const runtimeClientsUITestHost = "127.0.0.1:43127"

var (
	runtimeClientsUIStaleTestError   = errors.New("synthetic stale settings error")
	runtimeClientsUIFailureTestError = errors.New("synthetic private config error")
)

type runtimeClientsUIControllerFake struct {
	statuses     map[string]MCPClientStatus
	plan         MCPClientRegistrationPlan
	planErr      error
	backup       MCPClientBackupReceipt
	backupErr    error
	applyErr     error
	verifyErr    error
	restoreErr   error
	plannedSpecs []MCPRegistrationSpec
	planCalls    int
	backupCalls  int
	applyCalls   int
	verifyCalls  int
	restoreCalls int
	restoredID   string
	statusCanary string
}

func newRuntimeClientsUIControllerFake() *runtimeClientsUIControllerFake {
	return &runtimeClientsUIControllerFake{
		statuses: map[string]MCPClientStatus{
			"codex":  {ClientID: "codex", Installed: "installed", Version: "0.155.1", Registration: "not_registered", Connection: "unverified", ToolCall: "unverified", Backup: "not_created", EvidenceSource: "config_observed"},
			"claude": {ClientID: "claude", Installed: "installed_cli_not_observed", Registration: "not_observed", Connection: "unverified", ToolCall: "unverified", Backup: "not_created", EvidenceSource: "not_observed"},
			"gemini": {ClientID: "gemini", Installed: "installed", Version: "0.8.3", Registration: "registered", Connection: "unverified", ToolCall: "unverified", Backup: "not_created", EvidenceSource: "config_observed"},
		},
		plan:   MCPClientRegistrationPlan{ID: strings.Repeat("p", 32), ClientID: "codex", BaseRevision: "rev-42", Changes: []string{"registration_added"}},
		backup: MCPClientBackupReceipt{ID: strings.Repeat("b", 32), Status: "created", CreatedAt: time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC)},
	}
}

func (f *runtimeClientsUIControllerFake) InspectClient(_ context.Context, clientID string) (MCPClientStatus, error) {
	status, ok := f.statuses[clientID]
	if !ok {
		return MCPClientStatus{}, errors.New("unknown fixture client")
	}
	return status, nil
}

func (f *runtimeClientsUIControllerFake) PlanClientRegistration(_ context.Context, spec MCPRegistrationSpec) (MCPClientRegistrationPlan, error) {
	f.planCalls++
	f.plannedSpecs = append(f.plannedSpecs, spec)
	return f.plan, f.planErr
}

func (f *runtimeClientsUIControllerFake) BackupClientRegistration(_ context.Context, _ string) (MCPClientBackupReceipt, error) {
	f.backupCalls++
	return f.backup, f.backupErr
}

func (f *runtimeClientsUIControllerFake) ApplyClientRegistration(_ context.Context, _, _ string) (MCPClientStatus, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return MCPClientStatus{}, f.applyErr
	}
	status := f.statuses["codex"]
	status.Registration = "registered"
	status.Backup = "applied"
	status.Connection = "unverified"
	status.ToolCall = "unverified"
	status.EvidenceSource = "config_observed"
	f.statuses["codex"] = status
	return status, nil
}

func (f *runtimeClientsUIControllerFake) VerifyClientRegistration(_ context.Context, clientID string) (MCPClientStatus, error) {
	f.verifyCalls++
	if f.verifyErr != nil {
		return MCPClientStatus{}, f.verifyErr
	}
	return f.statuses[clientID], nil
}

func (f *runtimeClientsUIControllerFake) RestoreClientRegistration(_ context.Context, clientID, receiptID string) (MCPClientStatus, error) {
	f.restoreCalls++
	f.restoredID = receiptID
	if f.restoreErr != nil {
		return MCPClientStatus{}, f.restoreErr
	}
	status := f.statuses[clientID]
	status.Registration = "not_registered"
	status.Backup = "restored"
	status.Connection = "unverified"
	status.ToolCall = "unverified"
	status.EvidenceSource = "config_observed"
	f.statuses[clientID] = status
	return status, nil
}

type runtimeClientsUIRuntimeFake struct {
	status MCPRuntimeStatus
	err    error
}

func (f runtimeClientsUIRuntimeFake) Status(context.Context) (MCPRuntimeStatus, error) {
	return f.status, f.err
}

func (runtimeClientsUIRuntimeFake) Start(context.Context) error { return nil }
func (runtimeClientsUIRuntimeFake) Stop(context.Context) error  { return nil }

func newRuntimeClientsUITestHandler(controller *runtimeClientsUIControllerFake) http.Handler {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return newRuntimeClientsUIHandler(
		runtimeClientsUIRuntimeFake{status: MCPRuntimeStatus{
			State: "running", ChangedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
			UIEndpoint: "http://127.0.0.1:1234/private-ui", MCPEndpoint: "http://127.0.0.1:49321/mcp?private=true", FailureCode: "private-runtime-canary",
		}},
		controller,
		"runtime-clients-csrf",
		runtimeClientsUITestHost,
		executable,
		func(err error) string {
			switch {
			case errors.Is(err, runtimeClientsUIStaleTestError):
				return "stale_settings"
			case errors.Is(err, runtimeClientsUIFailureTestError):
				return "failed"
			default:
				return "failed"
			}
		},
	)
}

func runtimeClientsUIRequest(method, path string, form url.Values) *http.Request {
	var body strings.Reader
	if form != nil {
		body = *strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, "http://"+runtimeClientsUITestHost+path, &body)
	request.Host = runtimeClientsUITestHost
	if method == http.MethodPost {
		request.Header.Set("Origin", "http://"+runtimeClientsUITestHost)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return request
}

func runtimeClientsUIForm(values ...string) url.Values {
	form := url.Values{"csrf": {"runtime-clients-csrf"}}
	for i := 0; i+1 < len(values); i += 2 {
		form.Set(values[i], values[i+1])
	}
	return form
}

func runtimeClientsUIServe(t *testing.T, handler http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, runtimeClientsUIRequest(method, path, form))
	return recorder
}

func decodeRuntimeClientsUIResponse(t *testing.T, recorder *httptest.ResponseRecorder) runtimeClientsUIResponse {
	t.Helper()
	var response runtimeClientsUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response JSON decode failed: %v", err)
	}
	return response
}

func TestRuntimeClientsUIStatusRedactsRuntimeAndUnexpectedClientFields(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	controller.statusCanary = "private-config-path-and-secret-reference"
	status := controller.statuses["codex"]
	status.Version = `C:\private\path\token-canary`
	status.Registration = `registered at C:\private\settings.json ref=credential-canary`
	status.Connection = "connected to https://private.example.invalid/token-canary"
	status.ToolCall = "verified with private backup path"
	status.Backup = `C:\private\backup.json`
	status.EvidenceSource = "config bytes: credential-canary"
	controller.statuses["codex"] = status
	handler := newRuntimeClientsUITestHandler(controller)
	recorder := runtimeClientsUIServe(t, handler, http.MethodGet, runtimeClientsUIStatusPath, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeRuntimeClientsUIResponse(t, recorder)
	if response.Runtime == nil || response.Runtime.State != "running" || len(response.Clients) != 3 {
		t.Fatalf("incomplete status response: %+v", response)
	}
	for _, forbidden := range []string{"private", "credential-canary", "token-canary", "https://", "settings.json", "backup.json"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("status response exposed %q: %s", forbidden, recorder.Body.String())
		}
	}
	if response.Clients[0].Version != "" || response.Clients[0].Registration != "unknown" || response.Clients[0].Connection != "unknown" || response.Clients[0].ToolCall != "unknown" || response.Clients[0].Backup != "unknown" || response.Clients[0].EvidenceSource != "unknown" {
		t.Fatalf("unexpected status values were not reduced to fixed categories: %+v", response.Clients[0])
	}
	if response.Clients[1].Installed != "installed_cli_not_observed" || response.Clients[1].Registration != "not_observed" || response.Clients[1].EvidenceSource != "not_observed" {
		t.Fatalf("absent-client status was not preserved safely: %+v", response.Clients[1])
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unsafe response headers: %v", recorder.Header())
	}
}

func TestRuntimeClientsUIRejectsUninstalledUnsupportedAndUntrustedPlanRequests(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	handler := newRuntimeClientsUITestHandler(controller)
	tests := []struct {
		name      string
		edit      func(*http.Request)
		form      url.Values
		wantCode  int
		wantError string
	}{
		{name: "missing csrf", form: url.Values{"client_id": {"codex"}}, wantCode: http.StatusForbidden, wantError: "request_rejected"},
		{name: "wrong origin", form: runtimeClientsUIForm("client_id", "codex"), edit: func(r *http.Request) { r.Header.Set("Origin", "http://attacker.example") }, wantCode: http.StatusForbidden, wantError: "request_rejected"},
		{name: "unknown field", form: runtimeClientsUIForm("client_id", "codex", "command", "C:\\private\\evil.exe"), wantCode: http.StatusBadRequest, wantError: "invalid_request"},
		{name: "absent client", form: runtimeClientsUIForm("client_id", "claude"), wantCode: http.StatusConflict, wantError: "client_not_installed"},
		{name: "unsupported client", form: runtimeClientsUIForm("client_id", "other"), wantCode: http.StatusBadRequest, wantError: "unsupported_client"},
		{name: "wrong host", form: runtimeClientsUIForm("client_id", "codex"), edit: func(r *http.Request) { r.Host = "attacker.example" }, wantCode: http.StatusNotFound, wantError: "not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := runtimeClientsUIRequest(http.MethodPost, runtimeClientsUIPlanPath, test.form)
			if test.edit != nil {
				test.edit(request)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := decodeRuntimeClientsUIResponse(t, recorder)
			if recorder.Code != test.wantCode || response.ErrorCode != test.wantError {
				t.Fatalf("status=%d code=%q body=%s; want %d/%q", recorder.Code, response.ErrorCode, recorder.Body.String(), test.wantCode, test.wantError)
			}
		})
	}
	if controller.planCalls != 0 {
		t.Fatalf("rejected requests reached PlanClientRegistration %d times", controller.planCalls)
	}
}

func TestRuntimeClientsUIPlanUsesOnlyFixedSpecAndReturnsRedactedReview(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	handler := newRuntimeClientsUITestHandler(controller)
	recorder := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIPlanPath, runtimeClientsUIForm("client_id", "codex"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := decodeRuntimeClientsUIResponse(t, recorder)
	if response.Plan == nil || response.Plan.ClientID != "codex" || response.Plan.DisplayName != "local-agent-harness" || response.Plan.Transport != string(MCPTransportStdio) || response.Plan.BaseRevision != "rev-42" || len(response.Plan.Scope) != 1 || response.Plan.Scope[0] != "user" || len(response.Plan.Changes) != 1 || response.Plan.Changes[0] != "registration_added" {
		t.Fatalf("plan review omitted safe diff, scope, or revision: %+v", response.Plan)
	}
	if len(controller.plannedSpecs) != 1 {
		t.Fatalf("PlanClientRegistration calls=%d", len(controller.plannedSpecs))
	}
	spec := controller.plannedSpecs[0]
	if spec.ClientID != "codex" || spec.DisplayName != "local-agent-harness" || spec.Transport != MCPTransportStdio || spec.Endpoint != "" || !filepath.IsAbs(spec.Command) || len(spec.Args) != 1 || spec.Args[0] != "--mcp" || len(spec.Scope) != 1 || spec.Scope[0] != "user" {
		t.Fatalf("controller received a nonfixed registration spec: %+v", spec)
	}
	if strings.Contains(recorder.Body.String(), spec.Command) || strings.Contains(recorder.Body.String(), strings.Repeat("p", 32)) || strings.Contains(recorder.Body.String(), "endpoint") || strings.Contains(recorder.Body.String(), "command") {
		t.Fatalf("plan response exposed internal registration values: %s", recorder.Body.String())
	}
}

func TestRuntimeClientsUIRequiresExplicitBackupAndSupportsIdempotentApplyAndRestore(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	handler := newRuntimeClientsUITestHandler(controller)
	planRecorder := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIPlanPath, runtimeClientsUIForm("client_id", "codex"))
	plan := decodeRuntimeClientsUIResponse(t, planRecorder).Plan
	applyForm := runtimeClientsUIForm("plan_token", plan.Token)
	withoutBackup := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIApplyPath, applyForm)
	if withoutBackup.Code != http.StatusConflict || decodeRuntimeClientsUIResponse(t, withoutBackup).ErrorCode != "backup_required" || controller.applyCalls != 0 {
		t.Fatalf("apply bypassed explicit backup: status=%d calls=%d body=%s", withoutBackup.Code, controller.applyCalls, withoutBackup.Body.String())
	}
	backup := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIBackupPath, runtimeClientsUIForm("client_id", "codex"))
	if backup.Code != http.StatusOK || !strings.Contains(backup.Body.String(), `"status":"created"`) || strings.Contains(backup.Body.String(), controller.backup.ID) {
		t.Fatalf("backup response leaked receipt or failed: status=%d body=%s", backup.Code, backup.Body.String())
	}
	firstApply := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIApplyPath, applyForm)
	firstStatus := decodeRuntimeClientsUIResponse(t, firstApply)
	if firstApply.Code != http.StatusOK || firstStatus.Client == nil || firstStatus.Client.Registration != "registered" || firstStatus.Client.Connection != "unverified" || firstStatus.Client.ToolCall != "unverified" || firstStatus.Client.EvidenceSource != "config_observed" {
		t.Fatalf("apply conflated config registration with connection/tool-call evidence: status=%d response=%+v", firstApply.Code, firstStatus)
	}
	replay := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIApplyPath, applyForm)
	if replay.Code != http.StatusOK || !decodeRuntimeClientsUIResponse(t, replay).AlreadyApplied || controller.applyCalls != 1 {
		t.Fatalf("duplicate apply was not idempotent: status=%d calls=%d body=%s", replay.Code, controller.applyCalls, replay.Body.String())
	}
	missingConfirmation := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIRestorePath, runtimeClientsUIForm("client_id", "codex"))
	if missingConfirmation.Code != http.StatusBadRequest || decodeRuntimeClientsUIResponse(t, missingConfirmation).ErrorCode != "invalid_request" || controller.restoreCalls != 0 {
		t.Fatalf("restore skipped explicit confirmation: status=%d calls=%d", missingConfirmation.Code, controller.restoreCalls)
	}
	restore := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIRestorePath, runtimeClientsUIForm("client_id", "codex", "confirm", "restore"))
	if restore.Code != http.StatusOK || decodeRuntimeClientsUIResponse(t, restore).Client.Backup != "restored" || controller.restoreCalls != 1 || controller.restoredID != controller.backup.ID {
		t.Fatalf("restore did not use the opaque receipt internally: status=%d calls=%d body=%s", restore.Code, controller.restoreCalls, restore.Body.String())
	}
	for _, body := range []string{backup.Body.String(), firstApply.Body.String(), replay.Body.String(), restore.Body.String()} {
		if strings.Contains(body, controller.backup.ID) || strings.Contains(body, "private config") || strings.Contains(body, "credential_ref") {
			t.Fatalf("operation response exposed backup/config data: %s", body)
		}
	}
}

func TestRuntimeClientsUIStaleApplyIsFixedAndConsumesPlan(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	controller.applyErr = runtimeClientsUIStaleTestError
	handler := newRuntimeClientsUITestHandler(controller)
	plan := decodeRuntimeClientsUIResponse(t, runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIPlanPath, runtimeClientsUIForm("client_id", "codex"))).Plan
	if backup := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIBackupPath, runtimeClientsUIForm("client_id", "codex")); backup.Code != http.StatusOK {
		t.Fatalf("backup status=%d body=%s", backup.Code, backup.Body.String())
	}
	form := runtimeClientsUIForm("plan_token", plan.Token)
	stale := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIApplyPath, form)
	if stale.Code != http.StatusConflict || decodeRuntimeClientsUIResponse(t, stale).ErrorCode != "stale_settings" || strings.Contains(stale.Body.String(), runtimeClientsUIStaleTestError.Error()) {
		t.Fatalf("stale error was not fixed/redacted: status=%d body=%s", stale.Code, stale.Body.String())
	}
	consumed := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIApplyPath, form)
	if consumed.Code != http.StatusConflict || decodeRuntimeClientsUIResponse(t, consumed).ErrorCode != "plan_expired" || controller.applyCalls != 1 {
		t.Fatalf("stale plan could be replayed: status=%d calls=%d body=%s", consumed.Code, controller.applyCalls, consumed.Body.String())
	}
}

func TestRuntimeClientsUIVerifyOnlyReportsConfigObservation(t *testing.T) {
	controller := newRuntimeClientsUIControllerFake()
	handler := newRuntimeClientsUITestHandler(controller)
	recorder := runtimeClientsUIServe(t, handler, http.MethodPost, runtimeClientsUIVerifyPath, runtimeClientsUIForm("client_id", "gemini"))
	response := decodeRuntimeClientsUIResponse(t, recorder)
	if recorder.Code != http.StatusOK || response.Client == nil || response.Client.Registration != "registered" || response.Client.Connection != "unverified" || response.Client.ToolCall != "unverified" || response.Client.EvidenceSource != "config_observed" || controller.verifyCalls != 1 {
		t.Fatalf("verify overstated config observation: status=%d response=%+v calls=%d", recorder.Code, response, controller.verifyCalls)
	}
}

func TestRuntimeClientsUIFragmentIsBilingualAccessibleAndSeparatesEvidence(t *testing.T) {
	fragment, err := os.ReadFile("ui_fragments/runtime_clients.html")
	if err != nil {
		t.Fatalf("read UI fragment: %v", err)
	}
	markup := string(fragment)
	for _, required := range []string{
		"document.documentElement.lang === 'ko'",
		"Windows sign-in startup is managed separately in Advanced settings.",
		"Windows 로그인 자동 시작은 고급 설정에서 별도로 관리합니다.",
		"Registration, endpoint response, client connection, and an actual tool call are separate evidence.",
		"등록, endpoint 응답, 클라이언트 연결, 실제 도구 호출은 서로 다른 근거입니다.",
		"Object.prototype.hasOwnProperty.call(value, 'resident_endpoint')",
		"installed === 'installed_cli_not_observed' ? 'notObserved'",
		"Installed CLI was not observed in this check. Verify installation before registration.",
		"설치된 CLI를 이 확인에서 관찰하지 못했습니다. 등록 전에 설치 상태를 직접 확인하세요.",
		"The fixed local health endpoint responded. This does not prove authentication, an AI app connection, or a tool call.",
		"No response came from the fixed local health endpoint. This does not prove the service is stopped.",
		"Legacy runtime-controller status is unavailable; endpoint response was not observed.",
		"App connection",
		"앱 연결",
		"Check registration",
		"등록 상태 확인",
		"A saved registration does not confirm that the AI app connected or called a tool.",
		"등록을 저장해도 AI 앱 연결이나 도구 호출이 확인되는 것은 아닙니다.",
		"aria-live=\"polite\"",
		"tabindex=\"-1\"",
		"value.supported && installed === 'installed'",
		"applyButton.disabled = !(currentPlan.backup && !currentPlan.applied)",
		"window.confirm(getWords().restoreConfirm)",
		"node.textContent = value",
	} {
		if !strings.Contains(markup, required) {
			t.Errorf("UI fragment does not include required bilingual, accessible, or safe behavior %q", required)
		}
	}
	if strings.Contains(markup, ".innerHTML") || strings.Contains(markup, "insertAdjacentHTML") {
		t.Fatal("UI fragment renders dynamic status through an HTML injection sink")
	}
}
