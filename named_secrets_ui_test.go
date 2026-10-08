package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const namedSecretUICanary = "named-secret-ui-value-canary-6bf197"

var namedSecretUITestID = "secret:" + strings.Repeat("a", 32)

type namedSecretsUIFakeController struct {
	saveCalls   int
	deleteCalls int
	lastWrite   NamedSecretWrite
	valueCopy   []byte
	valueRef    []byte
	lastID      string
	saveErr     error
	deleteErr   error
	view        NamedSecretView
}

type namedSecretCleanupUIFakeController struct {
	*namedSecretsUIFakeController
	pendingIDs   []string
	pendingCalls int
	pendingErr   error
	retryCalls   int
	retryID      string
	retryPending bool
	retryErr     error
}

func (f *namedSecretCleanupUIFakeController) PendingNamedSecretCleanupIDs(context.Context) ([]string, error) {
	f.pendingCalls++
	return append([]string(nil), f.pendingIDs...), f.pendingErr
}

func (f *namedSecretCleanupUIFakeController) RetryNamedSecretCleanup(_ context.Context, id string) (bool, error) {
	f.retryCalls++
	f.retryID = id
	return f.retryPending, f.retryErr
}

func (f *namedSecretsUIFakeController) ListNamedSecrets(_ context.Context) ([]NamedSecretView, error) {
	if f.view.ID == "" {
		return nil, nil
	}
	return []NamedSecretView{f.view}, nil
}

func (f *namedSecretsUIFakeController) SaveNamedSecret(_ context.Context, write NamedSecretWrite) (NamedSecretView, error) {
	f.saveCalls++
	f.lastWrite = write
	f.valueRef = write.Value
	f.valueCopy = append([]byte(nil), write.Value...)
	if f.saveErr != nil {
		if errors.Is(f.saveErr, errNamedSecretCleanup) {
			return f.view, f.saveErr
		}
		return NamedSecretView{}, f.saveErr
	}
	if f.view.ID == "" {
		f.view.ID = namedSecretUITestID
	}
	f.view.Name = write.Name
	f.view.Purpose = write.Purpose
	f.view.Configured = true
	return f.view, nil
}

func (f *namedSecretsUIFakeController) DeleteNamedSecret(_ context.Context, id string) error {
	f.deleteCalls++
	f.lastID = id
	return f.deleteErr
}

func TestNamedSecretUICreateKeepsValueAndReferenceOutOfResponse(t *testing.T) {
	controller := &namedSecretsUIFakeController{view: NamedSecretView{ID: namedSecretUITestID, Configured: true, InUse: true}}
	handler := newNamedSecretUIHandler("127.0.0.1:49401", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49401", uiRouteSaveNamedSecret, url.Values{
		"csrf": {"csrf-ui"}, "name": {"Jenkins deploy"}, "purpose": {"release credential"}, "value": {namedSecretUICanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || controller.saveCalls != 1 || string(controller.valueCopy) != namedSecretUICanary {
		t.Fatalf("create result status=%d calls=%d capturedValue=%q body=%s", response.Code, controller.saveCalls, controller.valueCopy, response.Body.String())
	}
	if controller.lastWrite.Value == nil || len(controller.valueRef) == 0 || !allBytesZero(controller.valueRef) {
		t.Fatalf("handler did not clear its submitted value buffer: %v", controller.valueRef)
	}
	body := response.Body.String()
	var got namedSecretUISuccess
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Secret.ID != namedSecretUITestID || got.Secret.Name != "Jenkins deploy" || !got.Secret.Configured || !got.Secret.InUse {
		t.Fatalf("unexpected safe response: %+v", got)
	}
	if strings.Contains(body, namedSecretUICanary) || strings.Contains(body, "cred:") || strings.Contains(body, "credential_ref") {
		t.Fatalf("response exposed secret material or reference: %s", body)
	}
	if got.Secret.Purpose != "release credential" {
		t.Fatalf("purpose was lost: %+v", got.Secret)
	}
}

func TestNamedSecretUIMetadataEditUsesNilValueAndKeepsCurrentCredential(t *testing.T) {
	controller := &namedSecretsUIFakeController{view: NamedSecretView{ID: namedSecretUITestID, Configured: true}}
	handler := newNamedSecretUIHandler("127.0.0.1:49402", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49402", uiRouteSaveNamedSecret, url.Values{
		"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}, "name": {"Jenkins release"}, "purpose": {"rotated by operations"}, "value": {""},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || controller.saveCalls != 1 || controller.lastWrite.Value != nil {
		t.Fatalf("metadata-only update status=%d calls=%d value=%#v body=%s", response.Code, controller.saveCalls, controller.lastWrite.Value, response.Body.String())
	}
	if controller.lastWrite.ID != namedSecretUITestID || controller.lastWrite.Name != "Jenkins release" {
		t.Fatalf("metadata update changed identity unexpectedly: %+v", controller.lastWrite)
	}
}

func TestNamedSecretUIReportsCommittedRotationWithCleanupWarningAsSaved(t *testing.T) {
	controller := &namedSecretsUIFakeController{
		saveErr: errNamedSecretCleanup,
		view: NamedSecretView{
			ID: namedSecretUITestID, Name: "Jenkins deploy", Purpose: "release credential", Configured: true, InUse: true,
		},
	}
	handler := newNamedSecretUIHandler("127.0.0.1:49411", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49411", uiRouteSaveNamedSecret, url.Values{
		"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}, "name": {"Jenkins deploy"}, "purpose": {"release credential"}, "value": {namedSecretUICanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	var got namedSecretUISuccess
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || controller.saveCalls != 1 || !got.OK || got.Warning != "cleanup_pending" || got.Secret.ID != namedSecretUITestID {
		t.Fatalf("committed rotation was not reported as saved with a cleanup warning: status=%d result=%+v body=%s", response.Code, got, response.Body.String())
	}
	if strings.Contains(response.Body.String(), namedSecretUICanary) || strings.Contains(response.Body.String(), "cred:") || strings.Contains(response.Body.String(), "credential_ref") {
		t.Fatalf("cleanup warning exposed credential material: %s", response.Body.String())
	}
}

func TestNamedSecretUIReturnsSavedViewWhenCleanupCommitMarkerFails(t *testing.T) {
	const (
		oldCanary = "cleanup-marker-old-canary"
		newCanary = "cleanup-marker-new-canary"
	)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		Name:  "Marker failure",
		Value: []byte(oldCanary),
	})
	if err != nil {
		t.Fatalf("create named secret: %v", err)
	}
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("f", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	queue := controller.cleanup
	controller.cleanup = failingNamedSecretCleanupQueue{
		namedSecretCleanupQueue: queue,
		failMark:                true,
	}

	host := "127.0.0.1:49425"
	handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
	request := namedSecretUIRequest(t, host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {"csrf-ui"},
		"id":      {created.ID},
		"name":    {created.Name},
		"purpose": {created.Purpose},
		"value":   {newCanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	var got namedSecretUISuccess
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode save response: %v; body=%s", err, response.Body.String())
	}
	if response.Code != http.StatusOK || !got.OK || got.Warning != "cleanup_pending" ||
		got.Secret.ID != created.ID || !got.Secret.Configured {
		t.Fatalf("committed rotation status=%d result=%+v body=%s", response.Code, got, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{oldRef, newRef, oldCanary, newCanary, "credential_ref"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("save response exposed credential data %q: %s", forbidden, body)
		}
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != newRef {
		t.Fatalf("committed settings ref=%q, err=%v", stored.NamedSecrets[0].CredentialRef, err)
	}
	if string(store.values[oldRef]) != oldCanary || string(store.values[newRef]) != newCanary {
		t.Fatal("cleanup marker failure did not preserve both credentials for retry")
	}
	intents, err := queue.Load()
	if err != nil || len(intents) != 1 || intents[0].Committed || intents[0].OldRef != oldRef || intents[0].NewRef != newRef {
		t.Fatalf("pending cleanup intent=%+v, err=%v", intents, err)
	}
}

func TestNamedSecretUIDeleteInUseReturnsFixedConflict(t *testing.T) {
	controller := &namedSecretsUIFakeController{deleteErr: errNamedSecretInUse}
	handler := newNamedSecretUIHandler("127.0.0.1:49403", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49403", uiRouteDeleteNamedSecret, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || controller.deleteCalls != 1 || controller.lastID != namedSecretUITestID {
		t.Fatalf("in-use delete status=%d calls=%d id=%q body=%s", response.Code, controller.deleteCalls, controller.lastID, response.Body.String())
	}
	if got := response.Body.String(); !strings.Contains(got, `"error":"in_use"`) || strings.Contains(got, "named secret is still in use") {
		t.Fatalf("unexpected in-use error response: %s", got)
	}
}

func TestNamedSecretUIDeleteSuccessReturnsNoCredentialDetails(t *testing.T) {
	host := "127.0.0.1:49406"
	controller := &namedSecretsUIFakeController{}
	handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
	request := namedSecretUIRequest(t, host, uiRouteDeleteNamedSecret, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || controller.deleteCalls != 1 || controller.lastID != namedSecretUITestID || response.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("delete result status=%d calls=%d id=%q body=%s", response.Code, controller.deleteCalls, controller.lastID, response.Body.String())
	}
}

func TestNamedSecretUICleanupRetryUsesOnlyPendingLogicalIDAndReturnsSafeState(t *testing.T) {
	host := "127.0.0.1:49421"
	controller := &namedSecretCleanupUIFakeController{
		namedSecretsUIFakeController: &namedSecretsUIFakeController{view: NamedSecretView{ID: namedSecretUITestID, Configured: true}},
		pendingIDs:                   []string{namedSecretUITestID},
	}
	handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
	request := namedSecretUIRequest(t, host, uiRouteRetryNamedSecretCleanup, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var got namedSecretCleanupUISuccess
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || controller.retryCalls != 1 || controller.retryID != namedSecretUITestID || !got.OK || got.CleanupPending {
		t.Fatalf("retry result status=%d calls=%d id=%q result=%+v body=%s", response.Code, controller.retryCalls, controller.retryID, got, response.Body.String())
	}
	if strings.Contains(response.Body.String(), namedSecretUICanary) || strings.Contains(response.Body.String(), "cred:") || strings.Contains(response.Body.String(), "credential_ref") {
		t.Fatalf("cleanup retry exposed credential material: %s", response.Body.String())
	}
}

func TestNamedSecretUICleanupRetryKeepsPendingAndRejectsUnlistedID(t *testing.T) {
	host := "127.0.0.1:49422"
	controller := &namedSecretCleanupUIFakeController{
		namedSecretsUIFakeController: &namedSecretsUIFakeController{},
		pendingIDs:                   []string{namedSecretUITestID},
		retryPending:                 true,
	}
	handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
	request := namedSecretUIRequest(t, host, uiRouteRetryNamedSecretCleanup, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var got namedSecretCleanupUISuccess
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || controller.retryCalls != 1 || !got.OK || !got.CleanupPending {
		t.Fatalf("pending retry result status=%d calls=%d result=%+v body=%s", response.Code, controller.retryCalls, got, response.Body.String())
	}
	request = namedSecretUIRequest(t, host, uiRouteRetryNamedSecretCleanup, url.Values{"csrf": {"csrf-ui"}, "id": {"secret:" + strings.Repeat("b", 32)}})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || controller.retryCalls != 1 || !strings.Contains(response.Body.String(), `"error":"not_found"`) {
		t.Fatalf("unlisted retry status=%d calls=%d body=%s", response.Code, controller.retryCalls, response.Body.String())
	}
}

func TestNamedSecretUICleanupRetryEnforcesHostOriginAndCSRF(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "host", mutate: func(r *http.Request) { r.Host = "other.invalid" }, want: http.StatusNotFound},
		{name: "origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "http://other.invalid") }, want: http.StatusForbidden},
		{name: "csrf", mutate: func(r *http.Request) {
			replaceNamedSecretUIRequestBody(r, "csrf=wrong&id="+url.QueryEscape(namedSecretUITestID))
		}, want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := "127.0.0.1:49423"
			controller := &namedSecretCleanupUIFakeController{namedSecretsUIFakeController: &namedSecretsUIFakeController{}, pendingIDs: []string{namedSecretUITestID}}
			handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
			request := namedSecretUIRequest(t, host, uiRouteRetryNamedSecretCleanup, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || controller.retryCalls != 0 || controller.pendingCalls != 0 {
				t.Fatalf("status=%d pending calls=%d retry calls=%d body=%s", response.Code, controller.pendingCalls, controller.retryCalls, response.Body.String())
			}
		})
	}
}

func TestNamedSecretUICleanupRetryFailsClosedOnBrokenQueue(t *testing.T) {
	host := "127.0.0.1:49424"
	controller := &namedSecretCleanupUIFakeController{
		namedSecretsUIFakeController: &namedSecretsUIFakeController{},
		pendingErr:                   errors.New(namedSecretUICanary),
	}
	handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
	request := namedSecretUIRequest(t, host, uiRouteRetryNamedSecretCleanup, url.Values{"csrf": {"csrf-ui"}, "id": {namedSecretUITestID}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || controller.pendingCalls != 1 || controller.retryCalls != 0 {
		t.Fatalf("broken queue status=%d pending calls=%d retry calls=%d body=%s", response.Code, controller.pendingCalls, controller.retryCalls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), namedSecretUICanary) || strings.Contains(response.Body.String(), "credential_ref") {
		t.Fatalf("broken queue details escaped: %s", response.Body.String())
	}
}

func TestNamedSecretCleanupUIStateFailsClosedOnInvalidBackendData(t *testing.T) {
	controller := &namedSecretCleanupUIFakeController{
		namedSecretsUIFakeController: &namedSecretsUIFakeController{},
		pendingIDs:                   []string{namedSecretUITestID},
	}
	views := []NamedSecretView{{ID: namedSecretUITestID, Configured: true}}
	state := currentNamedSecretCleanupUIState(context.Background(), controller, views)
	if state.Status != "available" || len(state.PendingIDs) != 1 || state.PendingIDs[0] != namedSecretUITestID {
		t.Fatalf("valid cleanup state = %+v", state)
	}
	controller.pendingIDs = []string{"cred:raw-reference"}
	state = currentNamedSecretCleanupUIState(context.Background(), controller, views)
	if state.Status != "unavailable" || len(state.PendingIDs) != 0 {
		t.Fatalf("invalid cleanup data was not hidden: %+v", state)
	}
}

func TestNamedSecretUIMapsUnknownErrorsWithoutEchoingCanary(t *testing.T) {
	controller := &namedSecretsUIFakeController{saveErr: errors.New(namedSecretUICanary)}
	handler := newNamedSecretUIHandler("127.0.0.1:49404", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49404", uiRouteSaveNamedSecret, url.Values{
		"csrf": {"csrf-ui"}, "name": {"Jenkins deploy"}, "purpose": {"release credential"}, "value": {namedSecretUICanary},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), namedSecretUICanary) {
		t.Fatalf("raw controller error escaped: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"operation_failed"`) {
		t.Fatalf("unexpected fixed error: %s", response.Body.String())
	}
}

func TestNamedSecretUIRejectsOriginHostCSRFAndDuplicateFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "host", mutate: func(r *http.Request) { r.Host = "other.invalid" }, want: http.StatusNotFound},
		{name: "origin null", mutate: func(r *http.Request) { r.Header.Set("Origin", "null") }, want: http.StatusForbidden},
		{name: "csrf", mutate: func(r *http.Request) { replaceNamedSecretUIRequestBody(r, "csrf=wrong&name=ok&value=x") }, want: http.StatusForbidden},
		{name: "duplicate name", mutate: func(r *http.Request) { replaceNamedSecretUIRequestBody(r, "csrf=csrf-ui&name=one&name=two&value=x") }, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &namedSecretsUIFakeController{}
			host := "127.0.0.1:49410"
			handler := newNamedSecretUIHandler(host, "csrf-ui", controller)
			request := namedSecretUIRequest(t, host, uiRouteSaveNamedSecret, url.Values{"csrf": {"csrf-ui"}, "name": {"ok"}, "purpose": {""}, "value": {"x"}})
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || controller.saveCalls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, controller.saveCalls, response.Body.String())
			}
		})
	}
}

func TestNamedSecretUIRequiresValueForCreateAndHasNoReadRoute(t *testing.T) {
	controller := &namedSecretsUIFakeController{}
	handler := newNamedSecretUIHandler("127.0.0.1:49405", "csrf-ui", controller)
	request := namedSecretUIRequest(t, "127.0.0.1:49405", uiRouteSaveNamedSecret, url.Values{"csrf": {"csrf-ui"}, "name": {"empty"}, "purpose": {""}, "value": {""}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || controller.saveCalls != 0 {
		t.Fatalf("empty create status=%d calls=%d", response.Code, controller.saveCalls)
	}
	read := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49405/list-named-secrets", nil)
	read.Host = "127.0.0.1:49405"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, read)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unexpected feature read route: status=%d", response.Code)
	}
}

func TestSecretsEnvironmentPartialRendersWithNonceAndCSRF(t *testing.T) {
	parsed, err := parseUIPage(uiFiles)
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{Language: "ko", CSRF: "<csrf-canary>", CSPNonce: "nonce-canary"}
	var output strings.Builder
	if err := parsed.ExecuteTemplate(&output, "ui.html", data); err != nil {
		t.Fatal(err)
	}
	page := output.String()
	for _, want := range []string{
		`id="lah-secrets-environment"`, `action="/save-named-secret"`, `'/delete-named-secret'`, `'/retry-named-secret-cleanup'`, `data-named-secret-cleanup=`,
		`action="/save-user-environment"`, `'/delete-user-environment'`, `name="csrf" value="&lt;csrf-canary&gt;"`,
		`nonce="nonce-canary"`, `type="password"`, `LAHSecretsEnvironment`, `createSecretPicker`, `attachSecretPicker`, `named_secret_id`,
		"secretSavedCleanupWarning", "use Retry cleanup below", "정리 재시도",
		`id="lah-secrets-environment-heading">비밀값과 Windows 사용자 환경변수</h2>`,
		`id="lah-user-environment-heading">Windows 사용자 환경변수</h3>`,
		`JavaScript가 꺼져 있어 저장된 목록과 실시간 상태를 불러오지 못합니다.`,
		`저장된 목록을 보려면 JavaScript를 켜세요.`,
		`id="lah-user-environment-save" type="submit">환경변수 저장</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("rendered page omitted %q", want)
		}
	}
	if strings.Contains(page, `id="lah-secrets-environment" class="panel" hidden`) {
		t.Fatal("secrets and environment controls are hidden before JavaScript runs")
	}
	if strings.Contains(page, `type="password" value=`) || strings.Contains(page, "innerHTML") || strings.Contains(page, "credential_ref") {
		t.Fatalf("partial contains an unsafe value-rendering path")
	}
}

func namedSecretUIRequest(t *testing.T, host, path string, values url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://"+host+path, strings.NewReader(values.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Origin", "http://"+host)
	return req
}

func replaceNamedSecretUIRequestBody(request *http.Request, body string) {
	request.Body = io.NopCloser(strings.NewReader(body))
	request.ContentLength = int64(len(body))
}

func allBytesZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
