package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const sshUITestHost = "127.0.0.1:39127"
const sshUITestCSRF = "ui-test-csrf-token"

type setupDraftApprovalResultController struct {
	SetupDraftController
	result SetupDraftApprovalResult
}

func (c *setupDraftApprovalResultController) ApproveSetupDraft(context.Context, string, string, string) (SetupDraftApprovalResult, error) {
	return c.result, nil
}

func newSSHUITestHandler(ssh *sshOperationController, drafts SetupDraftController) http.Handler {
	return newSSHSetupUIHandler(sshUITestHost, sshUITestCSRF, ssh, drafts)
}

func sshUIPost(t *testing.T, handler http.Handler, path string, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://"+sshUITestHost+path, strings.NewReader(fields.Encode()))
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://"+sshUITestHost)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sshUIBaseForm() url.Values {
	return url.Values{"csrf": {sshUITestCSRF}}
}

func TestSSHSettingsUIEnforcesLocalRequestBoundary(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, nil, nil)
	handler := newSSHUITestHandler(controller, nil)

	t.Run("wrong host", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://localhost:39127/ssh-settings", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("wrong host status = %d, want 404", response.Code)
		}
	})

	t.Run("bad origin", func(t *testing.T) {
		fields := sshUIBaseForm()
		fields.Set("target_id", "")
		fields.Set("expected_revision", "initial")
		fields.Set("name", "Build host")
		fields.Set("alias", "build-host")
		fields.Set("reviewed_alias", "yes")
		request := httptest.NewRequest(http.MethodPost, "http://"+sshUITestHost+"/save-ssh-target", strings.NewReader(fields.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://attacker.invalid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || store.writes != 0 {
			t.Fatalf("bad origin response=%d writes=%d", response.Code, store.writes)
		}
	})

	t.Run("bad csrf and repeated values", func(t *testing.T) {
		fields := sshUIBaseForm()
		fields.Set("csrf", "wrong")
		fields.Set("target_id", "")
		fields.Set("expected_revision", "initial")
		fields.Set("name", "Build host")
		fields.Set("alias", "build-host")
		fields.Set("reviewed_alias", "yes")
		if response := sshUIPost(t, handler, "/save-ssh-target", fields); response.Code != http.StatusForbidden {
			t.Fatalf("bad CSRF status = %d, want 403", response.Code)
		}
		fields = sshUIBaseForm()
		fields.Add("csrf", sshUITestCSRF)
		fields.Set("target_id", "")
		fields.Set("expected_revision", "initial")
		fields.Set("name", "Build host")
		fields.Set("alias", "build-host")
		fields.Set("reviewed_alias", "yes")
		if response := sshUIPost(t, handler, "/save-ssh-target", fields); response.Code != http.StatusForbidden {
			t.Fatalf("repeated CSRF status = %d, want 403", response.Code)
		}
	})

	if store.writes != 0 {
		t.Fatalf("rejected requests reached the store: writes=%d", store.writes)
	}
}

func TestSSHSettingsUIGetRendersRegisteredCommandAndRootEntry(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, nil, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Build host", Alias: "build-host",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	operation := SSHOperationDefinition{
		ID: "list_pods", Name: "List pods", Summary: "Read registered namespace workload names.",
		Program: "kubectl", FixedArgs: []string{"get", "pods"}, Parameters: []SSHParameterSpec{},
		Risk: SSHRiskReadOnly, Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
	}
	if _, _, err := controller.SaveOperation(context.Background(), target.ID, operation, revision); err != nil {
		t.Fatal(err)
	}
	handler := newSSHUITestHandler(controller, nil)
	request := httptest.NewRequest(http.MethodGet, "http://"+sshUITestHost+"/ssh-settings", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "build-host") || !strings.Contains(response.Body.String(), "kubectl get pods") {
		t.Fatalf("SSH settings did not render the saved alias and command: status=%d", response.Code)
	}
	for _, want := range []string{
		"This local page does not connect to SSH targets.",
		"이 로컬 화면은 SSH 대상에 접속하지 않습니다.",
		"Destructive tasks always require approval on every call.",
		"파괴적 작업은 항상 호출마다 승인이 필요합니다.",
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Errorf("SSH settings omitted its boundary copy %q", want)
		}
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "nonce-") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing page security headers: csp=%q cache=%q", response.Header().Get("Content-Security-Policy"), response.Header().Get("Cache-Control"))
	}

	var root bytes.Buffer
	if err := page.ExecuteTemplate(&root, "ui.html", pageData{Language: "en", CSRF: sshUITestCSRF, CSPNonce: "root-test-nonce"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root.String(), `href="/ssh-settings"`) {
		t.Fatal("the shared page renderer did not include the SSH settings entry fragment")
	}
}

func TestSSHSettingsUITargetUpdatePreservesOperations(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, nil, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Build host", Alias: "build-host",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	operation, revision, err := controller.SaveOperation(context.Background(), target.ID, readOnlySSHOperation(), revision)
	if err != nil {
		t.Fatal(err)
	}
	handler := newSSHUITestHandler(controller, nil)
	fields := sshUIBaseForm()
	fields.Set("target_id", target.ID)
	fields.Set("expected_revision", revision)
	fields.Set("name", "Build cluster")
	fields.Set("alias", "build-cluster")
	fields.Set("reviewed_alias", "yes")
	response := sshUIPost(t, handler, "/save-ssh-target", fields)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/ssh-settings?state=target-saved" {
		t.Fatalf("target save response=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}

	snapshot, err := controller.loadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Targets) != 1 || snapshot.Targets[0].Name != "Build cluster" || snapshot.Targets[0].Alias != "build-cluster" {
		t.Fatalf("updated target not saved: %#v", snapshot.Targets)
	}
	if len(snapshot.Targets[0].Operations) != 1 || snapshot.Targets[0].Operations[0].ID != operation.ID {
		t.Fatalf("target edit discarded saved operations: %#v", snapshot.Targets[0].Operations)
	}
}

func TestSSHSettingsUISavesOnlyExactPreapprovedScopeHashes(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, nil, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Build host", Alias: "build-host",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	handler := newSSHUITestHandler(controller, nil)
	fields := sshUIBaseForm()
	fields.Set("target_id", target.ID)
	fields.Set("operation_id", "")
	fields.Set("expected_revision", revision)
	fields.Set("name", "Apply manifest")
	fields.Set("summary", "Apply a reviewed manifest")
	fields.Set("program", "kubectl")
	fields.Set("fixed_args", "apply\n{{manifest}}")
	fields.Set("parameters_json", `[{"name":"manifest","type":"string","required":true}]`)
	fields.Set("risk", string(SSHRiskStateChanging))
	fields.Set("approval_mode", string(OperationApprovalPreapproved))
	fields.Set("approved_parameter_sets", `[{"manifest":"current"},{"manifest":"next"}]`)
	fields.Set("reviewed_command", "yes")
	fields.Set("confirm_preapproval", "yes")
	response := sshUIPost(t, handler, "/save-ssh-operation", fields)
	if response.Code != http.StatusSeeOther || strings.Contains(response.Header().Get("Location"), "current") {
		t.Fatalf("preapproval save response=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	snapshot, err := controller.loadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Targets[0].Operations) != 1 {
		t.Fatalf("saved operation count = %d, want 1", len(snapshot.Targets[0].Operations))
	}
	saved := snapshot.Targets[0].Operations[0]
	if len(saved.Approval.Scope) != 2 || saved.Approval.Revision != saved.Revision {
		t.Fatalf("exact approval was not bound to the saved revision: %#v", saved.Approval)
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "current") || strings.Contains(string(encoded), "next") {
		t.Fatal("raw preapproval parameter values were persisted")
	}
	current, err := prepareSSHInvocation(target.ID, target.Alias, saved, map[string]json.RawMessage{"manifest": json.RawMessage(`"current"`)})
	if err != nil || !sshApprovalAllows(saved, current.scope) {
		t.Fatalf("saved exact scope did not permit its reviewed values: scope=%q err=%v", current.scope, err)
	}
	other, err := prepareSSHInvocation(target.ID, target.Alias, saved, map[string]json.RawMessage{"manifest": json.RawMessage(`"other"`)})
	if err != nil || sshApprovalAllows(saved, other.scope) {
		t.Fatalf("unreviewed scope was accepted: scope=%q err=%v", other.scope, err)
	}
}

func TestSSHSettingsUIRejectsShellSyntaxAndStaleForms(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, nil, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Build host", Alias: "build-host",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	handler := newSSHUITestHandler(controller, nil)
	fields := sshUIBaseForm()
	fields.Set("target_id", target.ID)
	fields.Set("operation_id", "")
	fields.Set("expected_revision", revision)
	fields.Set("name", "Unsafe command")
	fields.Set("summary", "Invalid shell input")
	fields.Set("program", "kubectl")
	fields.Set("fixed_args", "apply; whoami")
	fields.Set("parameters_json", "[]")
	fields.Set("risk", string(SSHRiskStateChanging))
	fields.Set("approval_mode", string(OperationApprovalPerCall))
	fields.Set("reviewed_command", "yes")
	response := sshUIPost(t, handler, "/save-ssh-operation", fields)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "definition is invalid") {
		t.Fatalf("shell syntax response=%d body=%s", response.Code, response.Body.String())
	}
	if store.writes != 1 {
		t.Fatalf("invalid operation reached the store: writes=%d", store.writes)
	}
	fields.Set("name", "Read-only task")
	fields.Set("summary", "Stale form")
	fields.Set("program", "kubectl")
	fields.Set("fixed_args", "get\npods")
	fields.Set("risk", string(SSHRiskReadOnly))
	fields.Set("approval_mode", string(OperationApprovalReadOnly))
	fields.Set("expected_revision", "old-revision")
	response = sshUIPost(t, handler, "/save-ssh-operation", fields)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "old-revision") {
		t.Fatalf("stale form response=%d body=%s", response.Code, response.Body.String())
	}
	if store.writes != 1 {
		t.Fatalf("stale form reached the store: writes=%d", store.writes)
	}
}

func TestSetupDraftReviewUIIsReadOnlyEscapedAndUsesOneShotApproval(t *testing.T) {
	store := newTestSetupDraftStore()
	drafts := newTestSetupDraftController(store)
	submission := validSetupDraftSubmission("revision-one")
	submission.Connections[0].Name = "<img src=x onerror=alert(1)>"
	review, err := drafts.SubmitSetupDraft(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	handler := newSSHUITestHandler(nil, drafts)
	fields := sshUIBaseForm()
	fields.Set("draft_id", review.ID)
	fields.Set("base_revision", review.BaseRevision)
	fields.Set("digest", review.Digest)
	response := sshUIPost(t, handler, "/review-setup-draft", fields)
	if response.Code != http.StatusOK {
		t.Fatalf("review response=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;") || strings.Contains(body, "<img src=x onerror=alert(1)>") {
		t.Fatal("draft proposal was not safely HTML-escaped")
	}
	if !strings.Contains(body, "GitHub automation") || strings.Contains(body, "secret:"+strings.Repeat("a", 32)) {
		t.Fatal("review did not show the logical credential name safely or exposed a credential reference")
	}
	if store.writes != 0 {
		t.Fatalf("draft review wrote settings: writes=%d", store.writes)
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "nonce-") {
		t.Fatalf("missing local UI security headers: cache=%q csp=%q", response.Header().Get("Cache-Control"), response.Header().Get("Content-Security-Policy"))
	}

	approve := sshUIBaseForm()
	approve.Set("draft_id", review.ID)
	approve.Set("base_revision", review.BaseRevision)
	approve.Set("digest", review.Digest)
	approve.Set("confirm_approve", "yes")
	response = sshUIPost(t, handler, "/approve-setup-draft", approve)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/ssh-settings?state=draft-approved" {
		t.Fatalf("approve response=%d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if store.writes != 1 {
		t.Fatalf("draft approval did not perform exactly one CAS save: writes=%d", store.writes)
	}
	response = sshUIPost(t, handler, "/approve-setup-draft", approve)
	if response.Code != http.StatusConflict || store.writes != 1 {
		t.Fatalf("replayed draft approval response=%d writes=%d", response.Code, store.writes)
	}
}

func TestSetupDraftReviewUIBlocksApprovalWhenRequiredDetailsAreMissing(t *testing.T) {
	store := newTestSetupDraftStore()
	store.snapshot.AvailableCredentials = nil
	drafts := newTestSetupDraftController(store)
	review, err := drafts.SubmitSetupDraft(context.Background(), validSetupDraftSubmission("revision-one"))
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Missing) == 0 {
		t.Fatal("fixture did not produce missing credential mapping")
	}
	handler := newSSHUITestHandler(nil, drafts)
	fields := sshUIBaseForm()
	fields.Set("draft_id", review.ID)
	fields.Set("base_revision", review.BaseRevision)
	fields.Set("digest", review.Digest)
	response := sshUIPost(t, handler, "/review-setup-draft", fields)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Approval is unavailable while required information is missing") {
		t.Fatalf("missing details did not block approval: status=%d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "disabled") {
		t.Fatal("approval control was not disabled for a draft with missing details")
	}
}

func TestSetupDraftUIShowsClaimFailureAndConsumedApprovalWarnings(t *testing.T) {
	tests := []struct {
		name            string
		result          SetupDraftApprovalResult
		state           string
		expectedMessage string
	}{
		{
			name:            "claim outcome uncertain before CAS",
			result:          SetupDraftApprovalResult{NextAction: setupDraftApprovalClaimUncertainNextAction},
			state:           "draft-claim-uncertain",
			expectedMessage: "This request did not change settings. The draft may have been consumed",
		},
		{
			name:            "claimed but stale before CAS",
			result:          SetupDraftApprovalResult{Stale: true, NextAction: setupDraftApprovalClaimedWarningNextAction},
			state:           "draft-stale-after-claim",
			expectedMessage: "Settings changed after the draft was claimed. The draft was not applied",
		},
		{
			name:            "claimed but save not confirmed",
			result:          SetupDraftApprovalResult{NextAction: setupDraftApprovalClaimedWarningNextAction},
			state:           "draft-claimed-not-applied",
			expectedMessage: "The review was consumed, but approval could not be confirmed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &setupDraftApprovalResultController{
				SetupDraftController: newTestSetupDraftController(newTestSetupDraftStore()),
				result:               test.result,
			}
			handler := newSSHUITestHandler(nil, controller)
			fields := sshUIBaseForm()
			fields.Set("draft_id", "setupdraft:"+strings.Repeat("a", 32))
			fields.Set("base_revision", "revision-one")
			fields.Set("digest", "sha256:"+strings.Repeat("b", 64))
			fields.Set("confirm_approve", "yes")
			response := sshUIPost(t, handler, "/approve-setup-draft", fields)
			wantLocation := "/ssh-settings?state=" + test.state
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != wantLocation {
				t.Fatalf("warning redirect=%d location=%q want=%q body=%s", response.Code, response.Header().Get("Location"), wantLocation, response.Body.String())
			}
			request := httptest.NewRequest(http.MethodGet, "http://"+sshUITestHost+wantLocation, nil)
			request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ssh-status warning") ||
				!strings.Contains(response.Body.String(), test.expectedMessage) {
				t.Fatalf("approval warning was not shown truthfully: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
