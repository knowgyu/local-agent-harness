package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type dashboardDiagnosisUISecretStore struct {
	ref     string
	token   []byte
	loads   atomic.Int32
	writes  atomic.Int32
	deletes atomic.Int32
}

func (s *dashboardDiagnosisUISecretStore) Save(string, []byte) error {
	s.writes.Add(1)
	return nil
}

func (s *dashboardDiagnosisUISecretStore) Load(ref string) ([]byte, error) {
	s.loads.Add(1)
	if ref != s.ref {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), s.token...), nil
}

func (s *dashboardDiagnosisUISecretStore) Delete(string) error {
	s.deletes.Add(1)
	return nil
}

func TestDashboardDiagnosisUIUsesSavedScopeAndOnlyBoundedReads(t *testing.T) {
	const token = "dashboard_diagnosis_ui_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisUISecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{
		configPath: path,
		secrets:    secrets,
		client:     dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests),
		host:       "127.0.0.1:1234",
		csrf:       "dashboard-ui-csrf",
	}

	form := url.Values{
		"csrf":           {a.csrf},
		"service_bundle": {"Inventory"},
		"environment":    {"qa-blue"},
	}
	recorder := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleDashboardDiagnosis)).ServeHTTP(recorder, dashboardDiagnosisUIRequest(a, http.MethodPost, "/dashboard-diagnosis", form.Encode(), true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("diagnosis status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected diagnostic response headers: %v", recorder.Header())
	}
	var response dashboardDiagnosisUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.Result == nil || response.Result.ServiceBundle != "Inventory" || response.Result.Environment != "qa-blue" || response.Result.Status == nil || response.Result.Events == nil || response.Result.Pods == nil || response.Summary == nil || response.Summary.Namespace != "apps" || response.Summary.Deployment != "api.v2" {
		t.Fatalf("diagnosis response omitted saved scope or partial read results: %+v", response)
	}
	if requests.Load() != 5 || secrets.loads.Load() != 1 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("unexpected reads or secret side effects: HTTP=%d secret loads=%d writes=%d deletes=%d", requests.Load(), secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only diagnosis changed local settings")
	}
	if strings.Contains(recorder.Body.String(), token) || strings.Contains(recorder.Body.String(), "dashboard.example.invalid") {
		t.Fatalf("UI response exposed a credential or Dashboard address: %s", recorder.Body.String())
	}
}

func TestDashboardDiagnosisUIShowsPartialReadFailuresWithoutUpstreamDetails(t *testing.T) {
	const token = "dashboard_diagnosis_ui_partial_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisUISecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{
		configPath: path,
		secrets:    secrets,
		client:     dashboardDiagnosisTestClient(t, token, http.StatusForbidden, &requests),
		host:       "127.0.0.1:1234",
		csrf:       "dashboard-ui-csrf",
	}
	form := url.Values{"csrf": {a.csrf}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}
	recorder := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleDashboardDiagnosis)).ServeHTTP(recorder, dashboardDiagnosisUIRequest(a, http.MethodPost, "/dashboard-diagnosis", form.Encode(), true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("partial diagnosis status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dashboardDiagnosisUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Result
	if result == nil || result.Status == nil || result.Events != nil || result.Pods == nil || len(result.Checks) != 3 || result.Checks[1].ErrorCode != "access_denied" {
		t.Fatalf("UI did not preserve partial result and fixed access guidance: %+v", response)
	}
	if requests.Load() != 5 || secrets.loads.Load() != 1 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("unexpected partial read side effects: HTTP=%d secret loads=%d writes=%d deletes=%d", requests.Load(), secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	if strings.Contains(recorder.Body.String(), token) || strings.Contains(recorder.Body.String(), "private.example.invalid") || strings.Contains(recorder.Body.String(), "https://") {
		t.Fatalf("partial UI response exposed upstream details: %s", recorder.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("partial read diagnosis changed local settings")
	}
}

func TestDashboardDiagnosisUIInvalidSavedScopeStopsBeforeSecretOrDashboardAccess(t *testing.T) {
	const token = "dashboard_diagnosis_ui_scope_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisUISecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{
		configPath: path,
		secrets:    secrets,
		client:     dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests),
		host:       "127.0.0.1:1234",
		csrf:       "dashboard-ui-csrf",
	}
	form := url.Values{"csrf": {a.csrf}, "service_bundle": {"Unregistered"}, "environment": {"qa-blue"}}
	recorder := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleDashboardDiagnosis)).ServeHTTP(recorder, dashboardDiagnosisUIRequest(a, http.MethodPost, "/dashboard-diagnosis", form.Encode(), true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("binding result status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dashboardDiagnosisUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result == nil || len(response.Result.Checks) != 1 || response.Result.Checks[0].Step != "binding" || response.Result.Checks[0].ErrorCode != "service_bundle_not_found" {
		t.Fatalf("invalid saved scope did not return a fixed binding diagnosis: %+v", response)
	}
	if requests.Load() != 0 || secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("invalid saved scope reached Dashboard or secret store: HTTP=%d loads=%d writes=%d deletes=%d", requests.Load(), secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
}

func TestDashboardDiagnosisUIRejectsUntrustedOrAmbiguousInputBeforeReads(t *testing.T) {
	const token = "dashboard_diagnosis_ui_reject_canary_0123456789"
	tests := []struct {
		name                 string
		method               string
		path                 string
		form                 url.Values
		contentType          string
		duplicateContentType bool
		origin               bool
		untrustedOrigin      bool
		wantStatus           int
		wantAllow            string
	}{
		{name: "method", method: http.MethodGet, path: "/dashboard-diagnosis", wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		{name: "csrf", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"wrong"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}, origin: true, wantStatus: http.StatusForbidden},
		{name: "extra field", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}, "url": {"https://other.example.invalid"}}, origin: true, wantStatus: http.StatusBadRequest},
		{name: "duplicate field", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory", "Other"}, "environment": {"qa-blue"}}, origin: true, wantStatus: http.StatusBadRequest},
		{name: "query field", method: http.MethodPost, path: "/dashboard-diagnosis?service_bundle=Inventory", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}, origin: true, wantStatus: http.StatusBadRequest},
		{name: "unsupported content type", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}, contentType: "application/json", origin: true, wantStatus: http.StatusForbidden},
		{name: "duplicate content type", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}, duplicateContentType: true, origin: true, wantStatus: http.StatusBadRequest},
		{name: "body exceeds limit", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {strings.Repeat("x", maxDashboardDiagnosisUIRequestBytes)}, "environment": {"qa-blue"}}, origin: true, wantStatus: http.StatusForbidden},
		{name: "untrusted origin", method: http.MethodPost, path: "/dashboard-diagnosis", form: url.Values{"csrf": {"dashboard-ui-csrf"}, "service_bundle": {"Inventory"}, "environment": {"qa-blue"}}, untrustedOrigin: true, wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			secrets := &dashboardDiagnosisUISecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
			var requests atomic.Int32
			a := &app{
				configPath: path,
				secrets:    secrets,
				client:     dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests),
				host:       "127.0.0.1:1234",
				csrf:       "dashboard-ui-csrf",
			}
			body := ""
			if test.form != nil {
				body = test.form.Encode()
			}
			recorder := httptest.NewRecorder()
			request := dashboardDiagnosisUIRequest(a, test.method, test.path, body, test.origin)
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.duplicateContentType {
				request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
			}
			if test.untrustedOrigin {
				request.Header.Set("Origin", "http://untrusted.example.invalid")
			}
			a.securityHeaders(http.HandlerFunc(a.handleDashboardDiagnosis)).ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || recorder.Header().Get("Allow") != test.wantAllow {
				t.Fatalf("status=%d allow=%q body=%s, want status=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"), recorder.Body.String(), test.wantStatus, test.wantAllow)
			}
			if requests.Load() != 0 || secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
				t.Fatalf("rejected input reached Dashboard or secret store: HTTP=%d secret loads=%d writes=%d deletes=%d", requests.Load(), secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
			}
		})
	}
}

func TestDashboardDiagnosisUIResponseLimitReturnsFixedError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeDashboardDiagnosisUIJSON(recorder, http.StatusOK, dashboardDiagnosisUIResponse{Error: strings.Repeat("x", dashboardDeploymentDiagnosisOutputLimit)})
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized UI response status=%d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
	if got, want := recorder.Body.String(), "{\"error\":\"Dashboard diagnosis details exceeded the response limit.\"}\n"; got != want {
		t.Fatalf("oversized UI response body=%q, want fixed error %q", got, want)
	}
}

func TestDashboardDiagnosisUIRendersOnlySavedDashboardMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, host: "127.0.0.1:1234", csrf: "dashboard-ui-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/?bundle_id=new", nil)
	recorder := httptest.NewRecorder()
	a.renderRootPage(recorder, request, nil, http.StatusOK)
	page := recorder.Body.String()
	for _, expected := range []string{
		`id="dashboard-diagnosis-form"`,
		`action="/dashboard-diagnosis"`,
		`data-service-bundle="Inventory"`,
		`data-environment="qa-blue"`,
		`Dashboard deployment diagnosis`,
		`Run read-only diagnosis`,
		`id="dashboard-diagnosis-result" aria-live="polite"`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("Dashboard diagnosis UI omitted %q", expected)
		}
	}
	if strings.Index(page, `id="dashboard-diagnosis-form"`) > strings.Index(page, `action="/save-bundle"`) {
		t.Fatal("diagnosis form should be outside the editable service bundle form")
	}
	if strings.Contains(page, `data-environment="draft-environment"`) {
		t.Fatal("diagnosis UI exposed a non-saved environment mapping")
	}
	formStart := strings.Index(page, `<form id="dashboard-diagnosis-form"`)
	if formStart < 0 {
		t.Fatal("diagnosis UI omitted the form for an enabled Dashboard target")
	}
	formEnd := strings.Index(page[formStart:], `</form>`)
	if formEnd < 0 {
		t.Fatal("diagnosis form was not closed")
	}
	form := page[formStart : formStart+formEnd+len(`</form>`)]
	if !strings.Contains(form, `Run read-only diagnosis</button>`) || strings.Contains(form, `type="submit" disabled`) {
		t.Fatal("diagnosis UI disabled the action for an enabled Dashboard target")
	}
}

func TestDashboardDiagnosisUIOnlyListsEnabledMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	activeEnvironment := cfg.ServiceBundles[0].Environments[0]
	disabledTarget := cfg.DashboardTargets[0]
	disabledTarget.ID = "dashboard:5123456789abcdef0123456789abcdef"
	disabledTarget.Name = "Paused cluster"
	disabledTarget.SecretRef = "cred:5123456789abcdef0123456789abcdef"
	disabledTarget.Disabled = true
	cfg.DashboardTargets = append(cfg.DashboardTargets, disabledTarget)
	disabledEnvironment := activeEnvironment
	disabledEnvironment.Name = "qa-disabled"
	disabledEnvironment.DashboardTargetID = disabledTarget.ID
	cfg.ServiceBundles[0].Environments = []serviceEnvironment{disabledEnvironment, activeEnvironment}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	a := &app{configPath: path, host: "127.0.0.1:1234", csrf: "dashboard-ui-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	recorder := httptest.NewRecorder()
	a.renderRootPage(recorder, request, nil, http.StatusOK)
	page := recorder.Body.String()
	formStart := strings.Index(page, `<form id="dashboard-diagnosis-form"`)
	if formStart < 0 {
		t.Fatal("diagnosis form was not rendered for saved mappings")
	}
	formEndOffset := strings.Index(page[formStart:], `</form>`)
	if formEndOffset < 0 {
		t.Fatal("diagnosis form was not closed")
	}
	form := page[formStart : formStart+formEndOffset]
	if !strings.Contains(form, `data-environment="qa-blue"`) {
		t.Fatal("enabled Dashboard mapping was missing from the diagnosis selector")
	}
	if strings.Contains(form, `data-environment="qa-disabled"`) {
		t.Fatal("disabled Dashboard mapping was included in the diagnosis selector")
	}
	if !strings.Contains(page, `id="dashboard-diagnosis-disabled-mappings" role="status">Disabled Dashboard targets are omitted from the diagnosis selector.`) {
		t.Fatal("mixed enabled and disabled mappings lacked an explanation")
	}
	if strings.Contains(form, `type="submit" disabled`) {
		t.Fatal("diagnosis action should remain available when an enabled mapping exists")
	}
}

func TestDashboardDiagnosisUIExplainsWhenNoEnabledCompleteMappingExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	cfg.DashboardTargets[0].Disabled = true
	enabledIncompleteTarget := cfg.DashboardTargets[0]
	enabledIncompleteTarget.ID = "dashboard:6123456789abcdef0123456789abcdef"
	enabledIncompleteTarget.Name = "Incomplete mapping target"
	enabledIncompleteTarget.SecretRef = "cred:6123456789abcdef0123456789abcdef"
	enabledIncompleteTarget.Disabled = false
	cfg.DashboardTargets = append(cfg.DashboardTargets, enabledIncompleteTarget)
	incompleteEnvironment := cfg.ServiceBundles[0].Environments[0]
	incompleteEnvironment.Name = "qa-incomplete"
	incompleteEnvironment.DashboardTargetID = enabledIncompleteTarget.ID
	incompleteEnvironment.DashboardNamespace = ""
	incompleteEnvironment.DashboardDeployment = ""
	cfg.ServiceBundles[0].Environments = append(cfg.ServiceBundles[0].Environments, incompleteEnvironment)
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	a := &app{configPath: path, host: "127.0.0.1:1234", csrf: "dashboard-ui-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	recorder := httptest.NewRecorder()
	a.renderRootPage(recorder, request, nil, http.StatusOK)
	page := recorder.Body.String()
	if strings.Contains(page, `id="dashboard-diagnosis-form"`) || strings.Contains(page, `id="dashboard-diagnosis-selection"`) {
		t.Fatal("missing enabled complete mapping should not render an empty selector or diagnosis form")
	}
	if !strings.Contains(page, `id="dashboard-diagnosis-unavailable" role="status">No enabled Dashboard mapping with both namespace and Deployment is available.`) {
		t.Fatal("missing enabled complete mapping lacked an explanation")
	}
	if !strings.Contains(page, `namespace와 Deployment가 모두 연결된 활성 Dashboard 진단 매핑이 없습니다.`) {
		t.Fatal("missing enabled complete mapping lacked a Korean translation")
	}
}

func TestDashboardDiagnosisUIRendersDynamicResultAsTextAndRelocalizes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless diagnosis UI fixture")
	}
	html, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	const marker = `<script nonce="{{.CSPNonce}}">`
	start := strings.Index(string(html), marker)
	if start < 0 {
		t.Fatal("inline UI script not found")
	}
	start += len(marker)
	end := strings.Index(string(html[start:]), "</script>")
	if end < 0 {
		t.Fatal("inline UI script end not found")
	}
	script := string(html[start : start+end])
	fixture := `
class Element {
  constructor(tag, connected = false) {
    this.tagName = tag;
    this.children = [];
    this.listeners = new Map();
    this.attributes = new Map();
    this.dataset = {};
    this.style = {};
    this._classes = new Set();
    this.classList = {
      add: name => this._classes.add(name),
      remove: name => this._classes.delete(name),
      contains: name => this._classes.has(name)
    };
    this.isConnected = connected;
    this._text = '';
    this.value = '';
    this.checked = false;
    this.files = [];
  }
  addEventListener(type, listener) { this.listeners.set(type, listener); }
  append(...nodes) {
    for (const node of nodes) {
      this.children.push(node);
      node.parent = this;
      node.setConnected(this.isConnected);
    }
  }
  setConnected(value) { this.isConnected = value; for (const child of this.children) child.setConnected(value); }
  contains(node) { return this === node || this.children.some(child => child.contains(node)); }
  replaceChildren(...nodes) { for (const child of this.children) child.setConnected(false); this.children = []; this._text = ''; this.append(...nodes); }
  set textContent(value) { for (const child of this.children) child.setConnected(false); this.children = []; this._text = String(value); }
  get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
  set innerHTML(value) { throw new Error('diagnosis fixture must not render HTML: ' + value); }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  removeAttribute(name) { this.attributes.delete(name); }
  get childNodes() { return this.children; }
  querySelector(selector) {
    if (selector === 'button[type="submit"]') return this.submitButton || null;
    if (selector === 'input[name="csrf"]') return this.csrfInput || null;
    return null;
  }
  querySelectorAll() { return []; }
  focus() { this.focused = true; }
}
const ids = new Map();
const createdElements = [];
const element = (id, connected = false) => { const value = new Element(id, connected); ids.set(id, value); return value; };
const languageControl = element('language-select', true);
languageControl.value = 'en';
const environments = element('service-environments', true);
const addEnvironment = element('add-environment', true);
const noopForm = id => { const form = element(id, true); form.action = '/unused'; form.submitButton = element('button', true); return form; };
for (const id of ['git-import-form','runbook-import-form','ssh-import-form','settings-import-form','json-remote-import-form']) noopForm(id);
for (const id of ['git-config-file','git-import-status','git-import-preview','git-import-heading','runbook-file','runbook-import-status','runbook-import-preview','runbook-import-heading','ssh-config-file','ssh-import-status','ssh-import-preview','ssh-import-heading','settings-json-file','settings-import-status','settings-import-preview','settings-import-heading','json-remote-file','json-remote-import-status','json-remote-import-preview']) element(id, true);
const diagnosisForm = element('dashboard-diagnosis-form', true);
diagnosisForm.action = '/dashboard-diagnosis';
diagnosisForm.submitButton = element('button', true);
diagnosisForm.csrfInput = {value: 'fixture-csrf'};
const diagnosisSelection = element('dashboard-diagnosis-selection', true);
diagnosisSelection.value = '0';
diagnosisSelection.selectedOptions = [{dataset: {serviceBundle: 'Environments', environment: 'qa-blue'}}];
const diagnosisStatus = element('dashboard-diagnosis-status', true);
diagnosisStatus.hidden = true;
const diagnosisResult = element('dashboard-diagnosis-result', true);
global.NodeFilter = {SHOW_TEXT: 4};
global.__localStorageWrites = [];
global.localStorage = {getItem() {return null;}, setItem(key, value) {global.__localStorageWrites.push([key, value]);}};
global.document = {
  cookie: 'lah_lang=en',
  documentElement: new Element('html', true),
  createTreeWalker() {return {nextNode() {return null;}};},
  createElement(tag) {const value = new Element(tag); createdElements.push(value); return value;},
  getElementById(id) {
    if (id === 'language-select') return languageControl;
    if (id === 'service-environments') return environments;
    if (id === 'add-environment') return addEnvironment;
    if (id === 'environment-template') return {content: {cloneNode() {return new Element('template');}}};
    if (ids.has(id)) return ids.get(id);
    throw new Error('unexpected element: ' + id);
  }
};
for (const client of ['codex', 'claude', 'gemini']) {
  element('client-' + client + '-server-listed', true);
  element('client-' + client + '-tools-visible', true);
  element('client-' + client + '-catalog-response', true);
  element('client-' + client + '-progress-status', true);
  element('client-' + client + '-progress-reset', true);
}
global.__request = null;
const logCanary = '<img src=x onerror=alert(2)> Environments';
const logTimestamp = '2026-09-27T00:00:00Z';
global.fetch = async (url, options) => {
  global.__request = {url, options};
  if (url === '/dashboard-diagnosis-logs') {
    return new Promise(resolve => {
      global.__releaseLogResponse = () => resolve({ok: true, json: async () => ({result: {
        pod: 'api-v2-abc-x1', container: 'web', truncated: true,
        lines: [{timestamp: logTimestamp, content: logCanary}]
      }})});
    });
  }
  if (url !== '/dashboard-diagnosis') throw new Error('unexpected request endpoint: ' + url);
  return {ok: true, json: async () => ({result: {
    service_bundle: 'Environments', environment: 'qa-blue', target: 'Dashboard', namespace: 'apps', deployment: 'api',
    output_truncated: true,
    checks: [{step: 'events', status: 'failed', error_code: 'access_denied'}, {step: 'status', status: 'failed', error_code: 'timeout'}, {step: 'output', status: 'truncated', error_code: 'output_limit'}, {step: 'status', status: 'failed', error_code: 'upstream_error'}],
    events: {events_truncated: true, events: [{reason: '<img src=x onerror=alert(1)>', object_name: 'Environments'}]},
    pods: {pods: [{name: 'api-v2-abc-x1', containers: [{name: 'web'}]}]}
  }})};
};
`
	checks := `
(async () => {
  const codexListed = ids.get('client-codex-server-listed');
  const codexTools = ids.get('client-codex-tools-visible');
  const codexCatalog = ids.get('client-codex-catalog-response');
  const codexStatus = ids.get('client-codex-progress-status');
  const codexReset = ids.get('client-codex-progress-reset');
  const claudeTools = ids.get('client-claude-tools-visible');
  const claudeStatus = ids.get('client-claude-progress-status');
  const geminiListed = ids.get('client-gemini-server-listed');
  const geminiTools = ids.get('client-gemini-tools-visible');
  const geminiCatalog = ids.get('client-gemini-catalog-response');
  const geminiStatus = ids.get('client-gemini-progress-status');
  if (!codexStatus.textContent.includes('No steps reported by you') || !claudeStatus.textContent.includes('No steps reported by you')) throw new Error('client progress should start unreported');
  codexListed.checked = true;
  codexListed.listeners.get('change')();
  if (!codexStatus.textContent.includes('the server in the saved CLI list') || !codexStatus.textContent.includes('has not verified')) throw new Error('saved server-list report did not preserve its user-reported boundary');
  if (codexStatus.textContent.includes('Connected')) throw new Error('server-list report implied an app-verified connection');
  codexTools.checked = true;
  codexTools.listeners.get('change')();
  if (!codexStatus.textContent.includes('tools in an active session') || !codexStatus.textContent.includes('has not verified')) throw new Error('combined report did not preserve the unverified boundary');
  codexCatalog.checked = true;
  codexCatalog.listeners.get('change')();
  if (!codexStatus.textContent.includes('registered_targets or registered_service_bundles')) throw new Error('catalog response report was not included');
  claudeTools.checked = true;
  claudeTools.listeners.get('change')();
  if (!claudeStatus.textContent.includes('tools in an active session') || claudeStatus.textContent.includes('saved CLI list')) throw new Error('CLI tool-list check did not remain independent');
  codexReset.listeners.get('click')();
  if (!codexStatus.textContent.includes('No steps reported by you') || codexListed.checked || codexTools.checked || codexCatalog.checked || !claudeStatus.textContent.includes('tools in an active session')) throw new Error('reset did not clear only the selected CLI report');
  geminiCatalog.checked = true;
  geminiCatalog.listeners.get('change')();
  if (!geminiStatus.textContent.includes('registered_targets or registered_service_bundles') || geminiStatus.textContent.includes('saved CLI list') || geminiStatus.textContent.includes('tools in an active session')) throw new Error('catalog-only report included unreported client steps');
  geminiListed.checked = true;
  geminiTools.checked = true;
  geminiListed.listeners.get('change')();
  geminiTools.listeners.get('change')();
  if (!geminiStatus.textContent.includes('saved CLI list') || !geminiStatus.textContent.includes('tools in an active session') || !geminiStatus.textContent.includes('registered_targets or registered_service_bundles')) throw new Error('Gemini CLI progress controls were not wired');
  if (global.__request !== null || global.__localStorageWrites.length !== 0) throw new Error('client progress touched the network or browser storage');
  diagnosisSelection.listeners.get('change')();
  if (!diagnosisStatus.hidden || diagnosisStatus.textContent !== '') throw new Error('untouched scope change displayed a diagnosis status');
  let prevented = false;
  await diagnosisForm.listeners.get('submit')({preventDefault() {prevented = true;}});
  if (!prevented || global.__request.url !== '/dashboard-diagnosis') throw new Error('diagnosis did not submit through its same-origin endpoint');
  const sent = new URLSearchParams(global.__request.options.body);
  if (sent.get('csrf') !== 'fixture-csrf' || sent.get('service_bundle') !== 'Environments' || sent.get('environment') !== 'qa-blue') throw new Error('diagnosis did not submit the selected saved mapping');
  if (!diagnosisResult.textContent.includes('Deployment status') && !diagnosisResult.textContent.includes('Event read')) throw new Error('diagnosis result did not render check labels');
  if (!diagnosisResult.textContent.includes('Some Dashboard result details were shortened')) throw new Error('output truncation was not identified');
  if (!diagnosisResult.textContent.includes('One or more Dashboard sections were shortened')) throw new Error('per-section truncation was not identified');
  if (!diagnosisResult.textContent.includes('Some details were omitted')) throw new Error('truncated output guidance was not rendered');
  if (!diagnosisResult.textContent.includes('Dashboard returned a server error. Check its availability and retry the read-only diagnostic later.')) throw new Error('English upstream error guidance was not rendered');
  const dataRegion = createdElements.find(item => item.tagName === 'pre');
  const dataHeading = createdElements.find(item => item.id === 'dashboard-diagnosis-data-heading');
  if (!dataRegion || !dataHeading || dataRegion.attributes.get('aria-labelledby') !== dataHeading.id) throw new Error('projected JSON region lacks its translated accessible name');
  if (!diagnosisResult.textContent.includes('<img src=x onerror=alert(1)>')) throw new Error('projected text was omitted from the text-only response view');
  const logButton = createdElements.find(item => item.tagName === 'button' && item.textContent === 'Read recent logs: api-v2-abc-x1 / web');
  const logHeading = createdElements.find(item => item.id === 'dashboard-logs-heading');
  const logOutput = createdElements.find(item => item.tagName === 'pre' && item.attributes.get('aria-labelledby') === 'dashboard-logs-heading');
  if (!logButton || !logHeading || !logOutput || !logOutput.hidden) throw new Error('listed Pod/container did not expose a named, initially hidden log region');
  const pendingLogRequest = logButton.listeners.get('click')();
  if (logButton.disabled || logButton.attributes.get('aria-disabled') !== 'true') throw new Error('log read removed its focused control from the keyboard tab order');
  global.__releaseLogResponse();
  await pendingLogRequest;
  if (global.__request.url !== '/dashboard-diagnosis-logs' || global.__request.options.method !== 'POST' || global.__request.options.credentials !== 'same-origin') throw new Error('logs did not use the same-origin POST endpoint');
  const logSent = new URLSearchParams(global.__request.options.body);
  if (Array.from(logSent.keys()).length !== 5 || logSent.get('csrf') !== 'fixture-csrf' || logSent.get('service_bundle') !== 'Environments' || logSent.get('environment') !== 'qa-blue' || logSent.get('pod') !== 'api-v2-abc-x1' || logSent.get('container') !== 'web') throw new Error('logs did not submit the exact saved scope and listed Pod/container');
  if (global.__request.options.headers.Accept !== 'application/json' || !global.__request.options.headers['Content-Type'].startsWith('application/x-www-form-urlencoded')) throw new Error('logs omitted the expected response or form content type');
  if (logOutput.hidden || logOutput.textContent !== logTimestamp + ' ' + logCanary || logOutput.children.length !== 0) throw new Error('log canary was omitted or interpreted as markup');
  if (!diagnosisResult.textContent.includes('1 log line(s) returned. Some log output was shortened to stay within the configured limit.')) throw new Error('log count and truncation were not reported');
  if (logButton.disabled || logButton.attributes.has('aria-disabled')) throw new Error('log action remained unavailable after the response');
  languageControl.value = 'ko';
  languageControl.listeners.get('change')();
  if (!claudeStatus.textContent.includes('실행 중인 세션에서 도구 확인')) throw new Error('client progress did not relocalize to Korean');
  if (!geminiStatus.textContent.includes('registered_targets 또는 registered_service_bundles 응답 확인')) throw new Error('catalog report did not relocalize to Korean');
  codexListed.checked = true;
  codexListed.listeners.get('change')();
  if (!codexStatus.textContent.includes('저장된 CLI 목록에서 서버 확인')) throw new Error('single-step Korean report did not identify the user-provided state');
  codexReset.listeners.get('click')();
  if (!codexStatus.textContent.includes('보고한 단계가 없습니다')) throw new Error('reset status did not relocalize to Korean');
  if (global.__localStorageWrites.length !== 1 || global.__localStorageWrites[0][0] !== 'lah-lang' || global.__localStorageWrites[0][1] !== 'ko') throw new Error('language choice was not stored in local storage');
  if (document.cookie !== 'lah_lang=ko; Path=/; SameSite=Strict') throw new Error('language choice was not stored in the strict locale cookie');
  if (document.documentElement.lang !== 'ko' || !diagnosisResult.textContent.includes('이벤트 조회: 실패.')) throw new Error('dynamic diagnosis labels did not switch to Korean');
  if (!diagnosisResult.textContent.includes('Environments') || !diagnosisResult.textContent.includes('<img src=x onerror=alert(1)>')) throw new Error('dynamic data was translated or interpreted as markup');
  if (!diagnosisResult.textContent.includes('등록된 Bearer에 이 Deployment')) throw new Error('fixed per-step guidance did not switch to Korean');
  if (!diagnosisResult.textContent.includes('12초 진단 제한')) throw new Error('timeout guidance did not switch to Korean');
  if (!diagnosisResult.textContent.includes('응답 크기 제한을 지키기 위해 Dashboard 결과 일부를 줄였습니다.')) throw new Error('output truncation notice did not switch to Korean');
  if (!diagnosisResult.textContent.includes('항목 수 제한으로 Dashboard 결과 일부 항목을 생략했습니다.')) throw new Error('per-section truncation notice did not switch to Korean');
  if (!diagnosisResult.textContent.includes('일부 세부 정보를 생략했습니다.')) throw new Error('output limit check guidance did not switch to Korean');
  if (!diagnosisResult.textContent.includes('Dashboard 서버 오류가 반환됐습니다. 가용 상태를 확인하고 나중에 읽기 전용 진단을 다시 실행하세요.')) throw new Error('Korean upstream error guidance did not switch to Korean');
  if (logHeading.textContent !== '목록에 표시된 Pod의 최근 로그 읽기' || logButton.textContent !== '최근 로그 읽기: api-v2-abc-x1 / web') throw new Error('log controls did not relocalize to Korean');
  if (!diagnosisResult.textContent.includes('로그 1줄을 받았습니다. 응답 제한에 맞추기 위해 로그 일부를 줄였습니다.')) throw new Error('log count and truncation did not relocalize to Korean');
  if (logOutput.textContent !== logTimestamp + ' ' + logCanary || logOutput.children.length !== 0) throw new Error('language change translated log data or interpreted it as markup');
  if (diagnosisForm.submitButton.disabled || diagnosisForm.submitButton.attributes.has('aria-disabled') || diagnosisForm.attributes.has('aria-busy')) throw new Error('diagnosis form remained busy after the response');
  let failedDiagnosisRequests = 0;
  global.fetch = async () => {
    failedDiagnosisRequests++;
    throw new Error('synthetic_canary_secret');
  };
  await diagnosisForm.listeners.get('submit')({preventDefault() {}});
  if (!diagnosisStatus.classList.contains('error') || !diagnosisStatus.textContent.includes('Dashboard 진단 요청에 실패했습니다.')) throw new Error('failed diagnosis did not show its fixed Korean error state');
  if (diagnosisStatus.textContent.includes('synthetic_canary_secret') || diagnosisResult.textContent !== '') throw new Error('failed diagnosis exposed an error detail or stale result');
  const callsBeforeSelectionChange = failedDiagnosisRequests;
  diagnosisSelection.selectedOptions = [{dataset: {serviceBundle: 'Environments', environment: 'qa-green'}}];
  diagnosisSelection.listeners.get('change')();
  if (diagnosisStatus.classList.contains('error') || !diagnosisStatus.textContent.includes('저장된 조회 범위 선택이 바뀌어 이전 결과를 지웠습니다.')) throw new Error('scope change did not clear and replace the previous Korean failure');
  if (diagnosisResult.textContent !== '' || failedDiagnosisRequests !== callsBeforeSelectionChange) throw new Error('scope change retained stale diagnosis content or issued a request');
  global.__abortedSignal = null;
  global.__releaseStaleResponse = null;
  global.__pendingFetches = 0;
  global.fetch = async (url, options) => {
    global.__pendingFetches++;
    return new Promise(resolve => {
      global.__abortedSignal = options.signal;
      global.__releaseStaleResponse = () => resolve({ok: true, json: async () => ({result: {service_bundle: 'Environments', environment: 'qa-blue', checks: []}})});
    });
  };
  const staleRequest = diagnosisForm.listeners.get('submit')({preventDefault() {}});
  if (diagnosisForm.submitButton.disabled || diagnosisForm.submitButton.attributes.get('aria-disabled') !== 'true' || !diagnosisForm.attributes.has('aria-busy')) throw new Error('diagnosis did not expose an accessible in-flight state while keeping its button focusable');
  await diagnosisForm.listeners.get('submit')({preventDefault() {}});
  if (global.__pendingFetches !== 1) throw new Error('another diagnosis request started while one was pending');
  diagnosisSelection.selectedOptions = [{dataset: {serviceBundle: 'Environments', environment: 'qa-blue'}}];
  diagnosisSelection.listeners.get('change')();
  if (!global.__abortedSignal.aborted || diagnosisForm.submitButton.disabled || diagnosisForm.submitButton.attributes.has('aria-disabled') || diagnosisForm.attributes.has('aria-busy')) throw new Error('scope change did not abort and clear the in-flight state');
  if (diagnosisResult.textContent !== '') throw new Error('scope change did not clear the previous diagnosis result');
  global.__releaseStaleResponse();
  await staleRequest;
  if (diagnosisResult.textContent !== '' || !diagnosisStatus.textContent.includes('저장된 조회 범위 선택이 바뀌어 이전 결과를 지웠습니다.')) throw new Error('stale response replaced the cleared scope result');
  if (diagnosisForm.submitButton.disabled || diagnosisForm.submitButton.attributes.has('aria-disabled') || diagnosisForm.attributes.has('aria-busy')) throw new Error('diagnosis form remained busy after scope change');
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture + script + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Dashboard diagnosis UI fixture failed: %v\n%s", err, output)
	}
}

func dashboardDiagnosisUIRequest(a *app, method, path, body string, sameOrigin bool) *http.Request {
	request := httptest.NewRequest(method, "http://"+a.host+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sameOrigin {
		request.Header.Set("Origin", "http://"+a.host)
	}
	return request
}
