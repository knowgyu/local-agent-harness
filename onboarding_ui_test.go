package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	onboardingUITestHost = "127.0.0.1:41985"
	onboardingUITestCSRF = "onboarding-ui-test-csrf"
	onboardingUITestID   = "onboarding:0123456789abcdef0123456789abcdef"
)

type onboardingUIControllerSpy struct {
	listCalls     int
	getCalls      int
	saveCalls     int
	deleteCalls   int
	checkCalls    int
	reviewCalls   int
	activateCalls int
	readsCalls    int

	draft                onboardingDraft
	drafts               []onboardingDraft
	saved                onboardingDraftSaveRequest
	deletedID            string
	deletedRevision      uint64
	checked              onboardingCheckRequest
	activated            onboardingActivationRequest
	activationResult     *onboardingActivationRecord
	checkResult          *onboardingCheckResult
	clearCheckCredential bool

	listErr     error
	getErr      error
	saveErr     error
	deleteErr   error
	checkErr    error
	reviewErr   error
	activateErr error
	readsErr    error
}

func (c *onboardingUIControllerSpy) ListDrafts(context.Context) ([]onboardingDraft, error) {
	c.listCalls++
	if c.drafts != nil {
		return append([]onboardingDraft(nil), c.drafts...), c.listErr
	}
	if c.draft.ID != "" {
		return []onboardingDraft{c.draft}, c.listErr
	}
	return []onboardingDraft{}, c.listErr
}

func (c *onboardingUIControllerSpy) GetDraft(_ context.Context, id string) (onboardingDraft, error) {
	c.getCalls++
	if c.getErr != nil {
		return onboardingDraft{}, c.getErr
	}
	if c.draft.ID == id {
		return c.draft, nil
	}
	return onboardingDraft{}, errOnboardingDraftNotFound
}

func (c *onboardingUIControllerSpy) SaveDraft(_ context.Context, request onboardingDraftSaveRequest) (onboardingDraft, error) {
	c.saveCalls++
	c.saved = request
	if c.saveErr != nil {
		return onboardingDraft{}, c.saveErr
	}
	if request.ID == "" {
		request.ID = onboardingUITestID
	}
	c.draft = onboardingDraft{
		ID: request.ID, Revision: request.ExpectedRevision + 1, Service: request.Service,
		Fields: request.Fields, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC(),
	}
	return c.draft, nil
}

func (c *onboardingUIControllerSpy) DeleteDraft(_ context.Context, id string, revision uint64) error {
	c.deleteCalls++
	c.deletedID = id
	c.deletedRevision = revision
	return c.deleteErr
}

func (c *onboardingUIControllerSpy) CheckDraft(_ context.Context, request onboardingCheckRequest) (onboardingCheckResult, error) {
	c.checkCalls++
	c.checked = request
	c.checked.Credential = append([]byte(nil), request.Credential...)
	if c.checkErr != nil {
		return onboardingCheckResult{}, c.checkErr
	}
	if c.clearCheckCredential {
		clear(request.Credential)
	}
	if c.checkResult != nil {
		return *c.checkResult, nil
	}
	return onboardingCheckResult{
		Receipt: onboardingCheckReceipt{
			DraftID: request.DraftID, DraftRevision: request.ExpectedRevision,
			CheckGeneration: c.draft.CheckGeneration + 1,
			CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
			ConfigRevision: "config-revision-safe", Outcome: onboardingCheckSucceeded,
			CheckedAt: time.Unix(3, 0).UTC(),
		},
		Permit: "permit-for-activation-only",
	}, nil
}

func (c *onboardingUIControllerSpy) ReviewDraft(_ context.Context, id string, revision uint64) (onboardingPolicyReview, error) {
	c.reviewCalls++
	if c.reviewErr != nil {
		return onboardingPolicyReview{}, c.reviewErr
	}
	return onboardingPolicyReview{
		DraftID: id, DraftRevision: revision, CheckGeneration: c.draft.CheckGeneration, Service: onboardingServiceGitHub,
		CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
		ConfigRevision: "config-revision-safe", IncludedTools: []string{"github_repository"},
		ExcludedTools: []string{}, MappingRequired: []string{}, PolicyNotes: []string{},
	}, nil
}

func (c *onboardingUIControllerSpy) ActivateDraft(_ context.Context, request onboardingActivationRequest) (onboardingActivationRecord, error) {
	c.activateCalls++
	c.activated = request
	c.activated.Credential = append([]byte(nil), request.Credential...)
	if c.activateErr != nil {
		return onboardingActivationRecord{}, c.activateErr
	}
	if c.activationResult != nil {
		return *c.activationResult, nil
	}
	return onboardingActivationRecord{
		DraftID: request.CAS.DraftID, DraftRevision: request.CAS.ExpectedRevision, CheckGeneration: request.CAS.CheckGeneration,
		TargetID: "github:abcdefabcdefabcdefabcdefabcdefab",
		Service:  onboardingServiceGitHub, CandidateDigest: request.CAS.CandidateDigest,
		PolicyDigest: request.CAS.PolicyDigest, ConfigRevision: request.CAS.ConfigRevision,
		ActivatedAt: time.Unix(4, 0).UTC(),
	}, nil
}

func (c *onboardingUIControllerSpy) RecordFirstRead(context.Context, onboardingFirstReadReceipt) (bool, error) {
	return false, nil
}

func (c *onboardingUIControllerSpy) ListFirstReads(context.Context) ([]onboardingFirstReadReceipt, error) {
	c.readsCalls++
	if c.readsErr != nil {
		return nil, c.readsErr
	}
	return []onboardingFirstReadReceipt{{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub,
		Tool: "github_repository", ObservedAt: time.Unix(5, 0).UTC(),
	}}, nil
}

func newOnboardingUIRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, "http://"+onboardingUITestHost+path, strings.NewReader(body))
	request.Host = onboardingUITestHost
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://"+onboardingUITestHost)
	request.Header.Set("X-CSRF-Token", onboardingUITestCSRF)
	return request
}

func serveOnboardingUI(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func onboardingUIWriteBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOnboardingUIRequestBoundaryPreventsUnauthorizedReadsAndWrites(t *testing.T) {
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)

	tests := []struct {
		name       string
		request    *http.Request
		wantStatus int
		wantAllow  string
	}{
		{
			name: "wrong host read",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/onboarding/drafts", nil)
				req.Host = "attacker.invalid"
				return req
			}(),
			wantStatus: http.StatusNotFound,
		},
		{
			name: "cross origin read",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodGet, onboardingDraftsPath, "")
				req.Header.Set("Origin", "http://attacker.invalid")
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "missing origin write",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Del("Origin")
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "duplicate origin write",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Add("Origin", "http://"+onboardingUITestHost)
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "missing csrf write",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Del("X-CSRF-Token")
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong csrf write",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Set("X-CSRF-Token", "wrong")
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name: "duplicate csrf write",
			request: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Add("X-CSRF-Token", onboardingUITestCSRF)
				return req
			}(),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "query rejected",
			request:    newOnboardingUIRequest(http.MethodGet, onboardingDraftsPath+"?private=1", ""),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "wrong method",
			request:    newOnboardingUIRequest(http.MethodPatch, onboardingDraftsPath, "{}"),
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET, POST",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveOnboardingUI(handler, tt.request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.wantAllow != "" && response.Header().Get("Allow") != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), tt.wantAllow)
			}
		})
	}
	if spy.listCalls != 0 || spy.getCalls != 0 || spy.saveCalls != 0 || spy.deleteCalls != 0 || spy.checkCalls != 0 || spy.reviewCalls != 0 || spy.activateCalls != 0 {
		t.Fatalf("rejected requests reached controller: list=%d get=%d save=%d delete=%d check=%d review=%d activate=%d",
			spy.listCalls, spy.getCalls, spy.saveCalls, spy.deleteCalls, spy.checkCalls, spy.reviewCalls, spy.activateCalls)
	}
}

func TestOnboardingUIStrictJSONDecoderRejectsUnknownDuplicateAndTrailingFields(t *testing.T) {
	spy := &onboardingUIControllerSpy{}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"service":"github","fields":{"name":"Repo"},"secret":"canary"}`},
		{name: "duplicate field", body: `{"service":"github","service":"dashboard","fields":{"name":"Repo"}}`},
		{name: "case folded duplicate field", body: `{"service":"github","Service":"dashboard","fields":{"name":"Repo"}}`},
		{name: "trailing JSON", body: `{"service":"github","fields":{"name":"Repo"}} {}`},
		{name: "top-level null", body: `null`},
		{name: "array body", body: `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, tt.body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error_code":"invalid_request"`) {
				t.Fatalf("strict decode response = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if spy.saveCalls != 0 {
		t.Fatalf("invalid JSON reached SaveDraft %d times", spy.saveCalls)
	}
}

func TestOnboardingUIRejectsOversizedAndNonJSONMutationsBeforeController(t *testing.T) {
	spy := &onboardingUIControllerSpy{}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	tests := []struct {
		name       string
		prepare    func() *http.Request
		wantStatus int
	}{
		{
			name: "oversized body",
			prepare: func() *http.Request {
				return newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"`+strings.Repeat("x", maxOnboardingUIRequestBytes)+`"}}`)
			},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name: "wrong content type",
			prepare: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Set("Content-Type", "text/plain")
				return req
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "duplicate content type",
			prepare: func() *http.Request {
				req := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath, `{"service":"github","fields":{"name":"Repo"}}`)
				req.Header.Add("Content-Type", "application/json")
				return req
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "encoded invalid draft id",
			prepare: func() *http.Request {
				return newOnboardingUIRequest(http.MethodGet, onboardingDraftItemPath+"bad-id", "")
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "wrong method on explicit check",
			prepare: func() *http.Request {
				return newOnboardingUIRequest(http.MethodGet, onboardingDraftItemPath+onboardingUITestID+"/check", "")
			},
			wantStatus: http.StatusMethodNotAllowed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveOnboardingUI(handler, tt.prepare())
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.wantStatus, response.Body.String())
			}
		})
	}
	if spy.saveCalls != 0 || spy.getCalls != 0 || spy.checkCalls != 0 {
		t.Fatalf("rejected requests reached controller: save=%d get=%d check=%d", spy.saveCalls, spy.getCalls, spy.checkCalls)
	}
}

func TestOnboardingUIDraftCRUDUsesExpectedRevisions(t *testing.T) {
	spy := &onboardingUIControllerSpy{}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)

	create := newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath,
		`{"service":"github","fields":{"name":"Engineering","origin":"https://github.example.invalid","repository":"team/repository"}}`)
	created := serveOnboardingUI(handler, create)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"id":"`+onboardingUITestID+`"`) {
		t.Fatalf("create response = %d %s", created.Code, created.Body.String())
	}
	if spy.saveCalls != 1 || spy.saved.ID != "" || spy.saved.ExpectedRevision != 0 || spy.saved.Service != onboardingServiceGitHub {
		t.Fatalf("create request = %#v, calls=%d", spy.saved, spy.saveCalls)
	}

	get := newOnboardingUIRequest(http.MethodGet, onboardingDraftItemPath+onboardingUITestID, "")
	got := serveOnboardingUI(handler, get)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"repository":"team/repository"`) ||
		!strings.Contains(got.Body.String(), `"check_generation":0`) || spy.getCalls != 1 {
		t.Fatalf("get response = %d calls=%d body=%s", got.Code, spy.getCalls, got.Body.String())
	}

	update := newOnboardingUIRequest(http.MethodPut, onboardingDraftItemPath+onboardingUITestID,
		`{"expected_revision":1,"fields":{"name":"Engineering 2","origin":"https://github.example.invalid","repository":"team/repository"}}`)
	updated := serveOnboardingUI(handler, update)
	if updated.Code != http.StatusOK || spy.saved.ID != onboardingUITestID || spy.saved.ExpectedRevision != 1 || spy.saved.Service != onboardingServiceGitHub || spy.saved.Fields.Name != "Engineering 2" {
		t.Fatalf("update response = %d request=%#v body=%s", updated.Code, spy.saved, updated.Body.String())
	}

	delete := newOnboardingUIRequest(http.MethodDelete, onboardingDraftItemPath+onboardingUITestID, `{"expected_revision":2}`)
	deleted := serveOnboardingUI(handler, delete)
	if deleted.Code != http.StatusOK || spy.deletedID != onboardingUITestID || spy.deletedRevision != 2 || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete response = %d id=%q revision=%d body=%s", deleted.Code, spy.deletedID, spy.deletedRevision, deleted.Body.String())
	}
}

func TestOnboardingUIDraftMutationRejectsZeroRevisionBeforeController(t *testing.T) {
	spy := &onboardingUIControllerSpy{}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	tests := []struct {
		name   string
		method string
		body   string
	}{
		{name: "update", method: http.MethodPut, body: `{"expected_revision":0,"fields":{"name":"Repo"}}`},
		{name: "delete", method: http.MethodDelete, body: `{"expected_revision":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveOnboardingUI(handler, newOnboardingUIRequest(tt.method, onboardingDraftItemPath+onboardingUITestID, tt.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("zero revision response = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if spy.saveCalls != 0 || spy.deleteCalls != 0 {
		t.Fatalf("zero revision reached controller: saves=%d deletes=%d", spy.saveCalls, spy.deleteCalls)
	}
}

func TestOnboardingUIPolicyReviewReturnsCurrentFixedPolicy(t *testing.T) {
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	request := newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/policy-review", `{"expected_revision":4}`)
	response := serveOnboardingUI(handler, request)
	if response.Code != http.StatusOK || spy.reviewCalls != 1 ||
		!strings.Contains(response.Body.String(), `"included_tools":["github_repository"]`) ||
		!strings.Contains(response.Body.String(), `"policy_digest":"policy-digest-safe"`) ||
		!strings.Contains(response.Body.String(), `"check_generation":6`) {
		t.Fatalf("policy review response = %d calls=%d body=%s", response.Code, spy.reviewCalls, response.Body.String())
	}
}

func TestOnboardingUISecretIsLimitedToExplicitCheckAndActivation(t *testing.T) {
	const credentialCanary = "temporary-credential-canary-value"
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)

	checkBody := onboardingUIWriteBody(t, onboardingDraftCheckRequestDTO{
		ExpectedRevision: 4, Credential: credentialCanary,
	})
	check := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/check", checkBody))
	if check.Code != http.StatusOK {
		t.Fatalf("check response = %d %s", check.Code, check.Body.String())
	}
	if !strings.Contains(check.Body.String(), `"check_permit":"permit-for-activation-only"`) || strings.Contains(check.Body.String(), credentialCanary) {
		t.Fatalf("check response did not return only its transient permit and safe receipt: %s", check.Body.String())
	}
	if !strings.Contains(check.Body.String(), `"check_generation":7`) {
		t.Fatalf("check response did not expose its nonzero check generation: %s", check.Body.String())
	}
	if spy.checkCalls != 1 || string(spy.checked.Credential) != credentialCanary || spy.checked.DraftID != onboardingUITestID || spy.checked.ExpectedRevision != 4 {
		t.Fatalf("controller did not receive explicit check credential: calls=%d request=%#v", spy.checkCalls, spy.checked)
	}

	activationBody := onboardingUIWriteBody(t, onboardingDraftActivateRequestDTO{
		ExpectedRevision: 4, CheckGeneration: 7, CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
		ConfigRevision: "config-revision-safe", PolicyConfirmed: true, CheckPermit: "permit-for-activation-only",
		Credential: credentialCanary,
	})
	activation := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/activate", activationBody))
	if activation.Code != http.StatusOK || strings.Contains(activation.Body.String(), credentialCanary) || strings.Contains(activation.Body.String(), "permit-for-activation-only") {
		t.Fatalf("activation response = %d %s", activation.Code, activation.Body.String())
	}
	if spy.activateCalls != 1 || string(spy.activated.Credential) != credentialCanary || spy.activated.CheckPermit != "permit-for-activation-only" ||
		spy.activated.CAS.ExpectedRevision != 4 || spy.activated.CAS.CheckGeneration != 7 || spy.activated.CAS.ConfigRevision != "config-revision-safe" {
		t.Fatalf("activation command = %#v calls=%d", spy.activated, spy.activateCalls)
	}

	if strings.Contains(check.Body.String(), `"credential"`) || strings.Contains(activation.Body.String(), `"credential"`) {
		t.Fatal("ephemeral credential appeared in a JSON DTO")
	}
}

func TestOnboardingUICheckReturnsSafeFailureReceiptWithoutPermit(t *testing.T) {
	const credentialCanary = "temporary-credential-failure-canary"
	spy := &onboardingUIControllerSpy{
		draft: onboardingUIDraftForTest(),
		checkResult: &onboardingCheckResult{
			Receipt: onboardingCheckReceipt{
				DraftID: onboardingUITestID, DraftRevision: 4,
				CheckGeneration: 7,
				CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
				ConfigRevision: "config-revision-safe", Outcome: onboardingCheckFailed,
				FailureKind: connectionFailureAuthentication, CheckedAt: time.Unix(3, 0).UTC(),
			},
		},
	}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	body := onboardingUIWriteBody(t, onboardingDraftCheckRequestDTO{ExpectedRevision: 4, Credential: credentialCanary})
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/check", body))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), credentialCanary) || strings.Contains(response.Body.String(), "upstream") {
		t.Fatalf("failed check response = %d %s", response.Code, response.Body.String())
	}
	var decoded onboardingDraftCheckResponseDTO
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode failed check response: %v", err)
	}
	if decoded.Receipt.Outcome != onboardingCheckFailed || decoded.Receipt.FailureKind != connectionFailureAuthentication || decoded.Permit != "" {
		t.Fatalf("failed check result = %#v, want a classified failure receipt with no permit", decoded)
	}
}

func TestOnboardingUICheckResponseGuardKeepsCredentialCanaryAfterControllerClearsInput(t *testing.T) {
	const credentialCanary = "credential-must-not-become-check-permit\nescaped"
	spy := &onboardingUIControllerSpy{
		draft: onboardingUIDraftForTest(), clearCheckCredential: true,
		checkResult: &onboardingCheckResult{
			Receipt: onboardingCheckReceipt{
				DraftID: onboardingUITestID, DraftRevision: 4,
				CheckGeneration: 7,
				CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
				ConfigRevision: "config-revision-safe", Outcome: onboardingCheckSucceeded,
				CheckedAt: time.Unix(3, 0).UTC(),
			},
			Permit: credentialCanary,
		},
	}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	body := onboardingUIWriteBody(t, onboardingDraftCheckRequestDTO{ExpectedRevision: 4, Credential: credentialCanary})
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/check", body))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "credential-must-not-become-check-permit") ||
		!strings.Contains(response.Body.String(), `"error_code":"unavailable"`) {
		t.Fatalf("canary guard response = %d %s", response.Code, response.Body.String())
	}
	if string(spy.checked.Credential) != credentialCanary {
		t.Fatalf("spy did not capture the original credential before controller cleared its input: %q", spy.checked.Credential)
	}
}

func TestOnboardingUICheckRejectsMalformedNamedSecretAndNoCredentialBeforeController(t *testing.T) {
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid named secret id", body: `{"expected_revision":2,"named_secret_id":"secret:bad"}`},
		{name: "both credential sources", body: `{"expected_revision":2,"credential":"token-canary","named_secret_id":"secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{name: "no credential source", body: `{"expected_revision":2}`},
		{name: "zero revision", body: `{"expected_revision":0,"credential":"token-canary"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/check", tt.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid check status = %d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if spy.checkCalls != 0 {
		t.Fatalf("invalid check input reached controller %d times", spy.checkCalls)
	}
}

func TestOnboardingUIActivationRequiresExplicitPolicyConfirmationAndFreshPermit(t *testing.T) {
	spy := &onboardingUIControllerSpy{}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	tests := []struct {
		name string
		body onboardingDraftActivateRequestDTO
	}{
		{
			name: "not explicitly confirmed",
			body: onboardingDraftActivateRequestDTO{
				ExpectedRevision: 1, CheckGeneration: 1, CandidateDigest: "candidate", PolicyDigest: "policy", ConfigRevision: "revision",
				Credential: "credential-canary", CheckPermit: "permit",
			},
		},
		{
			name: "missing permit",
			body: onboardingDraftActivateRequestDTO{
				ExpectedRevision: 1, CheckGeneration: 1, CandidateDigest: "candidate", PolicyDigest: "policy", ConfigRevision: "revision",
				PolicyConfirmed: true, Credential: "credential-canary",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := onboardingUIWriteBody(t, tt.body)
			response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftItemPath+onboardingUITestID+"/activate", body))
			if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "credential-canary") {
				t.Fatalf("activation validation = %d %s", response.Code, response.Body.String())
			}
		})
	}
	if spy.activateCalls != 0 {
		t.Fatalf("invalid activation input reached controller %d times", spy.activateCalls)
	}
}

func TestOnboardingUIActivationRequiresCurrentNonzeroCheckGeneration(t *testing.T) {
	base := `"expected_revision":4,"candidate_digest":"candidate","policy_digest":"policy","config_revision":"config","policy_confirmed":true,"check_permit":"permit","credential":"credential-canary"`
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "missing", body: `{` + base + `}`},
		{name: "zero", body: `{"check_generation":0,` + base + `}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spy := &onboardingUIControllerSpy{}
			handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
			response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
				onboardingDraftItemPath+onboardingUITestID+"/activate", tt.body))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error_code":"invalid_request"`) ||
				strings.Contains(response.Body.String(), "credential-canary") || spy.activateCalls != 0 {
				t.Fatalf("activation generation validation = %d calls=%d body=%s", response.Code, spy.activateCalls, response.Body.String())
			}
		})
	}

	spy := &onboardingUIControllerSpy{activateErr: errOnboardingCheckRequired}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	stale := `{"check_generation":6,` + base + `}`
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/activate", stale))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error_code":"check_required"`) ||
		spy.activateCalls != 1 || spy.activated.CAS.CheckGeneration != 6 {
		t.Fatalf("stale generation activation = %d calls=%d CAS=%#v body=%s", response.Code, spy.activateCalls, spy.activated.CAS, response.Body.String())
	}
}

func TestOnboardingUIActivationRecoveryRequiredHasFixedConflictCode(t *testing.T) {
	const credentialCanary = "credential-must-not-appear-in-error"
	spy := &onboardingUIControllerSpy{activateErr: errOnboardingActivationRecoveryRequired}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	body := onboardingUIWriteBody(t, onboardingDraftActivateRequestDTO{
		ExpectedRevision: 4, CheckGeneration: 7, CandidateDigest: "candidate-digest-safe",
		PolicyDigest: "policy-digest-safe", ConfigRevision: "config-revision-safe",
		PolicyConfirmed: true, CheckPermit: "permit-safe", Credential: credentialCanary,
	})
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/activate", body))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error_code":"activation_recovery_required"`) ||
		strings.Contains(response.Body.String(), credentialCanary) || strings.Contains(response.Body.String(), "activation_pending") || spy.activateCalls != 1 {
		t.Fatalf("recovery-required activation response = %d calls=%d body=%s", response.Code, spy.activateCalls, response.Body.String())
	}
}

func TestOnboardingUIRecoveryRequiredProjectionAndRouteGates(t *testing.T) {
	draft := onboardingUIDraftForTest()
	draft.ActivationRecoveryRequired = true
	spy := &onboardingUIControllerSpy{draft: draft}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)

	list := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodGet, onboardingDraftsPath, ""))
	var listDTO struct {
		Drafts []struct {
			ActivationRecoveryRequired bool `json:"activation_recovery_required"`
		} `json:"drafts"`
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &listDTO) != nil || len(listDTO.Drafts) != 1 ||
		!listDTO.Drafts[0].ActivationRecoveryRequired {
		t.Fatalf("recovery-required list response = %d %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "credential") || strings.Contains(list.Body.String(), "secret_ref") ||
		strings.Contains(list.Body.String(), "fingerprint") || strings.Contains(list.Body.String(), "target_id") {
		t.Fatalf("recovery-required list exposed internal or secret state: %s", list.Body.String())
	}

	detail := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodGet, onboardingDraftItemPath+onboardingUITestID, ""))
	var detailDTO struct {
		Draft struct {
			ActivationRecoveryRequired bool `json:"activation_recovery_required"`
		} `json:"draft"`
	}
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &detailDTO) != nil || !detailDTO.Draft.ActivationRecoveryRequired {
		t.Fatalf("recovery-required detail response = %d %s", detail.Code, detail.Body.String())
	}
	if strings.Contains(detail.Body.String(), "credential") || strings.Contains(detail.Body.String(), "secret_ref") ||
		strings.Contains(detail.Body.String(), "fingerprint") || strings.Contains(detail.Body.String(), "target_id") {
		t.Fatalf("recovery-required detail exposed internal or secret state: %s", detail.Body.String())
	}

	checkBody := onboardingUIWriteBody(t, onboardingDraftCheckRequestDTO{ExpectedRevision: 4, Credential: "temporary-canary"})
	check := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/check", checkBody))
	if check.Code != http.StatusConflict || !strings.Contains(check.Body.String(), `"error_code":"activation_recovery_required"`) || spy.checkCalls != 0 {
		t.Fatalf("recovery-required check response = %d calls=%d body=%s", check.Code, spy.checkCalls, check.Body.String())
	}

	review := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/policy-review", `{"expected_revision":4}`))
	if review.Code != http.StatusConflict || !strings.Contains(review.Body.String(), `"error_code":"activation_recovery_required"`) || spy.reviewCalls != 0 {
		t.Fatalf("recovery-required review response = %d calls=%d body=%s", review.Code, spy.reviewCalls, review.Body.String())
	}
}

func TestOnboardingUIActivationResponseGuardRejectsSecretCanaryFromController(t *testing.T) {
	const canary = "secret-canary-in-controller-target-id"
	spy := &onboardingUIControllerSpy{activationResult: &onboardingActivationRecord{
		DraftID: onboardingUITestID, DraftRevision: 4, CheckGeneration: 7,
		TargetID: canary, Service: onboardingServiceGitHub,
		CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
		ConfigRevision: "config-revision-safe", ActivatedAt: time.Unix(4, 0).UTC(),
	}}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	body := onboardingUIWriteBody(t, onboardingDraftActivateRequestDTO{
		ExpectedRevision: 4, CheckGeneration: 7, CandidateDigest: "candidate-digest-safe",
		PolicyDigest: "policy-digest-safe", ConfigRevision: "config-revision-safe",
		PolicyConfirmed: true, CheckPermit: "permit-safe", Credential: canary,
	})
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/activate", body))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), canary) ||
		!strings.Contains(response.Body.String(), `"error_code":"unavailable"`) {
		t.Fatalf("malicious activation response = %d %s", response.Code, response.Body.String())
	}
}

func TestOnboardingUIRecoveryFlagCannotBeSetByDraftUpdate(t *testing.T) {
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPut,
		onboardingDraftItemPath+onboardingUITestID,
		`{"expected_revision":4,"fields":{"name":"Repo"},"activation_recovery_required":true}`))
	if response.Code != http.StatusBadRequest || spy.saveCalls != 0 || !strings.Contains(response.Body.String(), `"error_code":"invalid_request"`) {
		t.Fatalf("client-supplied recovery state response = %d saves=%d body=%s", response.Code, spy.saveCalls, response.Body.String())
	}
}

func TestOnboardingUICheckRejectsSuccessfulResultWithoutGeneration(t *testing.T) {
	spy := &onboardingUIControllerSpy{
		draft: onboardingUIDraftForTest(),
		checkResult: &onboardingCheckResult{
			Receipt: onboardingCheckReceipt{
				DraftID: onboardingUITestID, DraftRevision: 4,
				CandidateDigest: "candidate-digest-safe", PolicyDigest: "policy-digest-safe",
				ConfigRevision: "config-revision-safe", Outcome: onboardingCheckSucceeded,
				CheckedAt: time.Unix(3, 0).UTC(),
			},
			Permit: "permit-safe",
		},
	}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost,
		onboardingDraftItemPath+onboardingUITestID+"/check", `{"expected_revision":4,"credential":"credential-canary"}`))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"error_code":"unavailable"`) ||
		strings.Contains(response.Body.String(), "credential-canary") {
		t.Fatalf("zero-generation successful check response = %d %s", response.Code, response.Body.String())
	}
}

func TestOnboardingUIErrorMappingNeverExposesControllerErrors(t *testing.T) {
	const internalCanary = "credential response body should never be returned"
	spy := &onboardingUIControllerSpy{saveErr: errors.New(internalCanary)}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	response := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodPost, onboardingDraftsPath,
		`{"service":"github","fields":{"name":"Repo"}}`))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), internalCanary) ||
		!strings.Contains(response.Body.String(), `"error_code":"operation_failed"`) {
		t.Fatalf("error mapping = %d %s", response.Code, response.Body.String())
	}
}

func TestOnboardingUIListsOnlySecretFreeDraftAndFirstReadDTOs(t *testing.T) {
	spy := &onboardingUIControllerSpy{draft: onboardingUIDraftForTest()}
	handler := newOnboardingUIHandler(onboardingUITestHost, onboardingUITestCSRF, spy)
	list := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodGet, onboardingDraftsPath, ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"drafts":[`) ||
		strings.Contains(list.Body.String(), "credential") || strings.Contains(list.Body.String(), "secret_ref") ||
		strings.Contains(list.Body.String(), "repository") || strings.Contains(list.Body.String(), "origin") ||
		strings.Contains(list.Body.String(), "name") || strings.Contains(list.Body.String(), `"check_generation"`) {
		t.Fatalf("draft list response = %d %s", list.Code, list.Body.String())
	}
	reads := serveOnboardingUI(handler, newOnboardingUIRequest(http.MethodGet, onboardingReadReceiptsPath, ""))
	if reads.Code != http.StatusOK || !strings.Contains(reads.Body.String(), `"receipts":[`) || strings.Contains(reads.Body.String(), `"target_id"`) || strings.Contains(reads.Body.String(), `"name"`) {
		t.Fatalf("first read response = %d %s", reads.Code, reads.Body.String())
	}
	if spy.listCalls != 1 || spy.readsCalls != 1 {
		t.Fatalf("read calls: drafts=%d first_reads=%d", spy.listCalls, spy.readsCalls)
	}
}

func onboardingUIDraftForTest() onboardingDraft {
	return onboardingDraft{
		ID: onboardingUITestID, Revision: 4, CheckGeneration: 6, Service: onboardingServiceGitHub,
		Fields:    onboardingDraftFields{Name: "Engineering", Origin: "https://github.example.invalid", Repository: "team/repository"},
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC(),
	}
}
