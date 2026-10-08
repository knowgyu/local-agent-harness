package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const dashboardDiagnosisLogsUITestToken = "dashboard_logs_ui_canary_0123456789"

type dashboardDiagnosisLogsUIFixture struct {
	app         *app
	secrets     *dashboardDiagnosisUISecretStore
	pods        string
	logs        string
	logStatus   int
	logError    error
	requests    int
	logRequests int
}

func newDashboardDiagnosisLogsUIFixture(t *testing.T, cfg config) *dashboardDiagnosisLogsUIFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &dashboardDiagnosisLogsUIFixture{
		secrets: &dashboardDiagnosisUISecretStore{
			ref:   cfg.DashboardTargets[0].SecretRef,
			token: []byte(dashboardDiagnosisLogsUITestToken),
		},
		pods: dashboardPodListFixture(1, []map[string]any{dashboardDiagnosisLogsUIPod("api-v2-abc-x1", "apps", "web")}),
		logs: dashboardLogFixture("api-v2-abc-x1", "web", false, []map[string]any{
			{"timestamp": "2026-09-27T01:02:03Z", "content": "ready " + dashboardDiagnosisLogsUITestToken},
		}),
		logStatus: http.StatusOK,
	}
	f.app = &app{
		configPath: path,
		secrets:    f.secrets,
		host:       "127.0.0.1:1234",
		csrf:       "dashboard-logs-ui-csrf",
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			f.requests++
			if r.Method != http.MethodGet || r.URL.Host != "dashboard.example.invalid" {
				t.Errorf("unexpected synthetic request method or host: %s %s", r.Method, r.URL.Host)
			}
			if r.Header.Get("Authorization") != "Bearer "+dashboardDiagnosisLogsUITestToken {
				t.Error("synthetic Dashboard request did not use the saved credential")
			}
			var body string
			wantQuery := ""
			switch r.URL.EscapedPath() {
			case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
				body = `{"objectMeta":{"name":"api-v2-abc"}}`
			case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
				wantQuery = "itemsPerPage=16&page=1"
				body = `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`
			case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
				wantQuery = "itemsPerPage=100&page=1"
				body = f.pods
			case "/proxy/api/v1/log/apps/api-v2-abc-x1/web":
				f.logRequests++
				want := url.Values{
					"referenceTimestamp": {"newest"},
					"referenceLineNum":   {"0"},
					"offsetFrom":         {"-99"},
					"offsetTo":           {"1"},
					"logFilePosition":    {"end"},
				}
				if r.URL.RawQuery != want.Encode() {
					t.Errorf("log request escaped the bounded query: %q", r.URL.RawQuery)
				}
				if f.logError != nil {
					return nil, f.logError
				}
				return harborTestResponse(r, f.logStatus, f.logs), nil
			default:
				t.Errorf("request escaped saved Deployment routes: %s", r.URL.EscapedPath())
				return harborTestResponse(r, http.StatusNotFound, "unexpected synthetic route"), nil
			}
			if r.URL.RawQuery != wantQuery {
				t.Errorf("inventory request query=%q, want %q", r.URL.RawQuery, wantQuery)
			}
			return harborTestResponse(r, http.StatusOK, body), nil
		})},
	}
	t.Cleanup(func() {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("UI log read changed saved settings")
		}
		if f.secrets.writes.Load() != 0 || f.secrets.deletes.Load() != 0 {
			t.Error("UI log read changed the secret store")
		}
	})
	return f
}

func dashboardDiagnosisLogsUIPod(pod, namespace, container string) map[string]any {
	return dashboardPodFixture(
		pod,
		namespace,
		"Running",
		"Running",
		"node-1",
		container,
		"Running",
		0,
	)
}

func (f *dashboardDiagnosisLogsUIFixture) form() url.Values {
	return url.Values{
		"csrf":           {f.app.csrf},
		"service_bundle": {"Inventory"},
		"environment":    {"qa-blue"},
		"pod":            {"api-v2-abc-x1"},
		"container":      {"web"},
	}
}

func (f *dashboardDiagnosisLogsUIFixture) request(form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://"+f.app.host+"/dashboard-diagnosis-logs", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+f.app.host)
	return r
}

func (f *dashboardDiagnosisLogsUIFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	f.app.handleDashboardDiagnosisLogs(recorder, r)
	return recorder
}

func requireDashboardDiagnosisLogsUIError(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status=%d, want %d; body=%s", recorder.Code, status, recorder.Body.String())
	}
	want := "{\"error\":\"Dashboard logs could not be read.\"}\n"
	if recorder.Body.String() != want {
		t.Fatalf("response was not the fixed generic error: %s", recorder.Body.String())
	}
}

func TestDashboardDiagnosisLogsUIUsesSavedScopeAndMasksOutput(t *testing.T) {
	f := newDashboardDiagnosisLogsUIFixture(t, serviceBundleConfig())
	recorder := f.serve(f.request(f.form()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("log status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dashboardDiagnosisLogsUIResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := response.Result
	if result == nil || response.Error != "" {
		t.Fatalf("missing bounded log result: %+v", response)
	}
	if result.ServiceBundle != "Inventory" || result.Environment != "qa-blue" {
		t.Fatalf("log result lost saved scope: %+v", result)
	}
	if result.Pod != "api-v2-abc-x1" || result.Container != "web" || len(result.Lines) != 1 {
		t.Fatalf("log result lost the verified Pod and container: %+v", result)
	}
	if !strings.Contains(result.Lines[0].Content, "[REDACTED]") {
		t.Fatalf("credential was not masked in log content: %q", result.Lines[0].Content)
	}
	for _, forbidden := range []string{dashboardDiagnosisLogsUITestToken, "must-not-escape", "unprojected", "dashboard.example.invalid"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Errorf("log response exposed forbidden fixture data %q", forbidden)
		}
	}
	if f.requests != 4 || f.logRequests != 1 || f.secrets.loads.Load() != 1 {
		t.Fatalf("unexpected synthetic reads: HTTP=%d logs=%d credentials=%d", f.requests, f.logRequests, f.secrets.loads.Load())
	}
	if recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Error("log response did not use JSON")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("log response omitted no-store or nosniff")
	}
}

func TestDashboardDiagnosisLogsUIRejectsUntrustedInputsBeforeReads(t *testing.T) {
	tests := []struct {
		name        string
		editForm    func(url.Values)
		editRequest func(*http.Request)
		status      int
	}{
		{name: "method", editRequest: func(r *http.Request) { r.Method = http.MethodGet }, status: http.StatusMethodNotAllowed},
		{name: "missing origin", editRequest: func(r *http.Request) { r.Header.Del("Origin") }, status: http.StatusForbidden},
		{name: "foreign origin", editRequest: func(r *http.Request) { r.Header.Set("Origin", "https://other.example.invalid") }, status: http.StatusForbidden},
		{name: "duplicate origin", editRequest: func(r *http.Request) { r.Header.Add("Origin", r.Header.Get("Origin")) }, status: http.StatusForbidden},
		{name: "foreign host", editRequest: func(r *http.Request) { r.Host = "other.example.invalid" }, status: http.StatusForbidden},
		{name: "bad csrf", editForm: func(v url.Values) { v.Set("csrf", "wrong") }, status: http.StatusForbidden},
		{name: "missing csrf", editForm: func(v url.Values) { v.Del("csrf") }, status: http.StatusForbidden},
		{name: "duplicate csrf", editForm: func(v url.Values) { v.Add("csrf", v.Get("csrf")) }, status: http.StatusBadRequest},
		{name: "duplicate pod", editForm: func(v url.Values) { v.Add("pod", "another-pod") }, status: http.StatusBadRequest},
		{name: "missing container", editForm: func(v url.Values) { v.Del("container") }, status: http.StatusBadRequest},
		{name: "query", editRequest: func(r *http.Request) { r.URL.RawQuery = "endpoint=https://other.example.invalid" }, status: http.StatusBadRequest},
		{name: "json content type", editRequest: func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, status: http.StatusBadRequest},
		{name: "multipart content type", editRequest: func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=fixture") }, status: http.StatusBadRequest},
		{name: "duplicate content type", editRequest: func(r *http.Request) { r.Header.Add("Content-Type", r.Header.Get("Content-Type")) }, status: http.StatusBadRequest},
		{name: "oversized body", editForm: func(v url.Values) { v.Set("pod", strings.Repeat("x", maxDashboardDiagnosisLogsUIRequestBytes)) }, status: http.StatusForbidden},
		{name: "invalid bundle", editForm: func(v url.Values) { v.Set("service_bundle", " Inventory") }, status: http.StatusBadRequest},
		{name: "invalid environment", editForm: func(v url.Values) { v.Set("environment", "qa\nblue") }, status: http.StatusBadRequest},
		{name: "pod traversal", editForm: func(v url.Values) { v.Set("pod", "../other") }, status: http.StatusBadRequest},
		{name: "pod URL", editForm: func(v url.Values) { v.Set("pod", "https://other.example.invalid") }, status: http.StatusBadRequest},
		{name: "container traversal", editForm: func(v url.Values) { v.Set("container", "../other") }, status: http.StatusBadRequest},
		{name: "container query", editForm: func(v url.Values) { v.Set("container", "web?previous=true") }, status: http.StatusBadRequest},
		{name: "container too long", editForm: func(v url.Values) { v.Set("container", strings.Repeat("a", 64)) }, status: http.StatusBadRequest},
	}
	for _, field := range []string{"endpoint", "url", "target", "namespace", "deployment", "token", "credential", "tail", "previous"} {
		tests = append(tests, struct {
			name        string
			editForm    func(url.Values)
			editRequest func(*http.Request)
			status      int
		}{
			name:     "arbitrary " + field,
			editForm: func(v url.Values) { v.Set(field, "caller-supplied") },
			status:   http.StatusBadRequest,
		})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newDashboardDiagnosisLogsUIFixture(t, serviceBundleConfig())
			form := f.form()
			if test.editForm != nil {
				test.editForm(form)
			}
			r := f.request(form)
			if test.editRequest != nil {
				test.editRequest(r)
			}
			recorder := f.serve(r)
			requireDashboardDiagnosisLogsUIError(t, recorder, test.status)
			if test.status == http.StatusMethodNotAllowed && recorder.Header().Get("Allow") != http.MethodPost {
				t.Error("method rejection omitted Allow: POST")
			}
			if f.requests != 0 || f.secrets.loads.Load() != 0 {
				t.Fatalf("invalid request reached HTTP or credentials: %d/%d", f.requests, f.secrets.loads.Load())
			}
		})
	}
}

func TestDashboardDiagnosisLogsUIRejectsUnsavedScopeBeforeReads(t *testing.T) {
	tests := []struct {
		name       string
		editConfig func(*config)
		editForm   func(url.Values)
	}{
		{name: "unknown bundle", editForm: func(v url.Values) { v.Set("service_bundle", "Unregistered") }},
		{name: "unknown environment", editForm: func(v url.Values) { v.Set("environment", "Unregistered") }},
		{name: "disabled target", editConfig: func(c *config) { c.DashboardTargets[0].Disabled = true }},
		{name: "missing deployment mapping", editConfig: func(c *config) {
			c.ServiceBundles[0].Environments[0].DashboardNamespace = ""
			c.ServiceBundles[0].Environments[0].DashboardDeployment = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := serviceBundleConfig()
			if test.editConfig != nil {
				test.editConfig(&cfg)
			}
			f := newDashboardDiagnosisLogsUIFixture(t, cfg)
			form := f.form()
			if test.editForm != nil {
				test.editForm(form)
			}
			requireDashboardDiagnosisLogsUIError(t, f.serve(f.request(form)), http.StatusInternalServerError)
			if f.requests != 0 || f.secrets.loads.Load() != 0 {
				t.Fatalf("unregistered scope reached HTTP or credentials: %d/%d", f.requests, f.secrets.loads.Load())
			}
		})
	}
}

func TestDashboardDiagnosisLogsUIRevalidatesPodAndContainerOnEveryRead(t *testing.T) {
	tests := []struct {
		name      string
		pod       string
		namespace string
		container string
	}{
		{name: "Pod disappeared", pod: "replacement-pod", namespace: "apps", container: "web"},
		{name: "container disappeared", pod: "api-v2-abc-x1", namespace: "apps", container: "replacement"},
		{name: "wrong namespace", pod: "api-v2-abc-x1", namespace: "other", container: "web"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newDashboardDiagnosisLogsUIFixture(t, serviceBundleConfig())
			first := f.serve(f.request(f.form()))
			if first.Code != http.StatusOK {
				t.Fatalf("first log request failed: %s", first.Body.String())
			}
			pod := dashboardDiagnosisLogsUIPod(test.pod, test.namespace, test.container)
			f.pods = dashboardPodListFixture(1, []map[string]any{pod})
			requireDashboardDiagnosisLogsUIError(t, f.serve(f.request(f.form())), http.StatusInternalServerError)
			if f.requests != 7 || f.logRequests != 1 || f.secrets.loads.Load() != 2 {
				t.Fatalf("stale selection bypassed inventory validation: HTTP=%d logs=%d credentials=%d", f.requests, f.logRequests, f.secrets.loads.Load())
			}
		})
	}
}

func TestDashboardDiagnosisLogsUIBoundsLinesAndJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentSize int
	}{
		{name: "line count", contentSize: 1},
		{name: "line and JSON bytes", contentSize: dashboardLogLineBytesLimit + 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newDashboardDiagnosisLogsUIFixture(t, serviceBundleConfig())
			lines := make([]map[string]any, dashboardLogLineLimit+5)
			for i := range lines {
				lines[i] = map[string]any{
					"timestamp": "2026-09-27T01:02:03Z",
					"content":   dashboardDiagnosisLogsUITestToken + " " + strconv.Itoa(i) + strings.Repeat("x", test.contentSize),
				}
			}
			f.logs = dashboardLogFixture("api-v2-abc-x1", "web", false, lines)
			recorder := f.serve(f.request(f.form()))
			if recorder.Code != http.StatusOK || recorder.Body.Len() > maxDashboardDiagnosisLogsUIResponseBytes {
				t.Fatalf("unbounded UI result: status=%d bytes=%d", recorder.Code, recorder.Body.Len())
			}
			var response dashboardDiagnosisLogsUIResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Result == nil || !response.Result.Truncated {
				t.Fatal("bounded log result did not report truncation")
			}
			if len(response.Result.Lines) == 0 || len(response.Result.Lines) > dashboardLogLineLimit {
				t.Fatalf("unexpected bounded line count: %d", len(response.Result.Lines))
			}
			if test.contentSize == 1 && len(response.Result.Lines) != dashboardLogLineLimit {
				t.Fatalf("short log output retained %d lines, want %d", len(response.Result.Lines), dashboardLogLineLimit)
			}
			for _, line := range response.Result.Lines {
				if len(line.Content) > dashboardLogLineBytesLimit || strings.Contains(line.Content, dashboardDiagnosisLogsUITestToken) {
					t.Fatal("line exceeded the byte limit or exposed the saved token")
				}
			}
		})
	}
}

func TestDashboardDiagnosisLogsUIHidesUpstreamFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{name: "HTTP failure", status: http.StatusForbidden, body: "private https://private.example.invalid/ " + dashboardDiagnosisLogsUITestToken},
		{name: "invalid JSON", status: http.StatusOK, body: "invalid " + dashboardDiagnosisLogsUITestToken},
		{name: "oversized upstream", status: http.StatusOK, body: strings.Repeat("x", maxAPIBytes+1)},
		{name: "transport failure", err: errors.New("private https://private.example.invalid/ " + dashboardDiagnosisLogsUITestToken)},
		{name: "wrong Pod", status: http.StatusOK, body: dashboardLogFixture("other-pod", "web", false, []map[string]any{})},
		{name: "wrong container", status: http.StatusOK, body: dashboardLogFixture("api-v2-abc-x1", "other", false, []map[string]any{})},
		{name: "invalid line", status: http.StatusOK, body: dashboardLogFixture("api-v2-abc-x1", "web", false, []map[string]any{{"content": dashboardDiagnosisLogsUITestToken}})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newDashboardDiagnosisLogsUIFixture(t, serviceBundleConfig())
			f.logs, f.logStatus, f.logError = test.body, test.status, test.err
			requireDashboardDiagnosisLogsUIError(t, f.serve(f.request(f.form())), http.StatusInternalServerError)
			if f.requests != 4 || f.logRequests != 1 {
				t.Fatalf("unexpected failure reads: HTTP=%d logs=%d", f.requests, f.logRequests)
			}
		})
	}
}

func TestDashboardDiagnosisLogsUIBoundsEnvelopeAndReturnsFixedOverflowError(t *testing.T) {
	t.Run("envelope adds bytes", func(t *testing.T) {
		result := dashboardDeploymentLogsResult{Lines: []dashboardLogLine{{Content: ""}}}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		result.Lines[0].Content = strings.Repeat("x", maxDashboardDiagnosisLogsUIResponseBytes-len(data))
		recorder := httptest.NewRecorder()
		writeDashboardDiagnosisLogsUIJSON(recorder, http.StatusOK, &result)
		var response dashboardDiagnosisLogsUIResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || recorder.Body.Len() > maxDashboardDiagnosisLogsUIResponseBytes {
			t.Fatalf("envelope exceeded response limit: status=%d bytes=%d", recorder.Code, recorder.Body.Len())
		}
		if response.Result == nil || !response.Result.Truncated || len(response.Result.Lines) != 0 {
			t.Fatal("envelope did not trim the final line and mark truncation")
		}
	})
	t.Run("fixed overflow error", func(t *testing.T) {
		result := dashboardDeploymentLogsResult{Pod: strings.Repeat("x", maxDashboardDiagnosisLogsUIResponseBytes)}
		recorder := httptest.NewRecorder()
		writeDashboardDiagnosisLogsUIJSON(recorder, http.StatusOK, &result)
		requireDashboardDiagnosisLogsUIError(t, recorder, http.StatusRequestEntityTooLarge)
	})
}
