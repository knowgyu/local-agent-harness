package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalUIHandlerDashboardDiagnosisUsesSameOriginAndSyntheticDashboard(t *testing.T) {
	const (
		token    = "dashboard_ui_handler_canary_0123456789"
		rawError = "RAW_DASHBOARD_ERROR_CANARY"
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	dashboard := cfg.DashboardTargets[0]
	cfg.ConnectionTests = map[string]connectionTest{
		dashboard.ID: {Result: "failure", CompletedAt: "2026-09-26T01:02:03Z"},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	secrets := &dashboardDiagnosisUISecretStore{ref: dashboard.SecretRef, token: []byte(token)}
	var dashboardRequests atomic.Int32
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		dashboardRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "dashboard.example.invalid" {
			t.Errorf("unexpected synthetic Dashboard request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Dashboard request did not use its saved synthetic Bearer")
		}

		var status int
		var body string
		switch r.URL.Path {
		case "/proxy/api/v1/deployment/apps/api.v2":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api.v2","namespace":"apps","annotations":{"private":"` + token + `"}},"statusInfo":{"replicas":2,"updated":2,"available":1,"unavailable":1},"conditions":[]}`
		case "/proxy/api/v1/deployment/apps/api.v2/event":
			status = http.StatusForbidden
			body = `{"error":"` + rawError + `","url":"https://private.example.invalid/response"}`
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api-v2-abc"}}`
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			status = http.StatusOK
			body = `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			status = http.StatusOK
			body = dashboardPodListFixture(1, []map[string]any{
				dashboardPodFixture("api-v2-abc-x1", "apps", "Running", "Running", "node-1", "web", "Running", 0),
			})
		default:
			t.Errorf("unexpected synthetic Dashboard path: %s", r.URL.Path)
			return harborTestResponse(r, http.StatusNotFound, ""), nil
		}
		return harborTestResponse(r, status, body), nil
	})}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		client:     client,
		csrf:       "local-ui-diagnosis-csrf",
	}

	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)
	if !strings.HasPrefix(server.URL, "http://127.0.0.1:") {
		t.Fatalf("test UI server is not loopback HTTP: %q", server.URL)
	}

	pageResponse, err := server.Client().Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(pageResponse.Body)
	pageResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pageResponse.StatusCode != http.StatusOK || !strings.Contains(string(page), `action="/dashboard-diagnosis"`) || !strings.Contains(string(page), `data-service-bundle="Inventory"`) {
		t.Fatalf("root page did not render the saved Dashboard diagnosis form: status=%d", pageResponse.StatusCode)
	}
	csrfMatch := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(page)
	if len(csrfMatch) != 2 || string(csrfMatch[1]) != a.csrf {
		t.Fatal("root page did not render the expected CSRF value")
	}

	form := url.Values{
		"csrf":           {string(csrfMatch[1])},
		"service_bundle": {"Inventory"},
		"environment":    {"qa-blue"},
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/dashboard-diagnosis", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	request.Header.Set("Origin", server.URL)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("same-origin diagnosis POST status=%d body=%s", response.StatusCode, body)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("diagnosis response content type = %q", response.Header.Get("Content-Type"))
	}
	var envelope dashboardDiagnosisUIResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != "" || envelope.Result == nil || envelope.Result.Status == nil || envelope.Result.Events != nil || envelope.Result.Pods == nil {
		t.Fatalf("diagnosis response did not preserve successful steps and the synthetic partial failure: %+v", envelope)
	}
	if len(envelope.Result.Checks) != 3 || envelope.Result.Checks[1].ErrorCode != "access_denied" {
		t.Fatalf("diagnosis response did not project the fixed event access failure: %+v", envelope.Result.Checks)
	}
	if dashboardRequests.Load() == 0 || dashboardRequests.Load() > 5 {
		t.Fatalf("synthetic Dashboard GET count=%d, want 1..5", dashboardRequests.Load())
	}
	if secrets.loads.Load() != 1 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("secret-store operations: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	for _, forbidden := range []string{token, rawError, "dashboard.example.invalid", "private.example.invalid"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("diagnosis response exposed %q: %s", forbidden, body)
		}
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Fatal("diagnosis changed settings or connection-test history")
	}
}

func TestLocalUIHandlerDashboardSaveValidationPreservesLocaleAndSafeDraft(t *testing.T) {
	const token = "dashboard_save_ui_canary_0123456789"
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	secrets := &dashboardDiagnosisUISecretStore{}
	var outboundRequests atomic.Int32
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			outboundRequests.Add(1)
			return harborTestResponse(r, http.StatusInternalServerError, "unexpected outbound request"), nil
		})},
		csrf: "dashboard-save-validation-csrf",
	}
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)

	tests := []struct {
		name         string
		locale       string
		language     string
		localizedErr string
	}{
		{
			name:         "English",
			locale:       "en",
			language:     "en",
			localizedErr: "The Dashboard url field is invalid. A connection test was not run.",
		},
		{
			name:         "Korean",
			locale:       "ko",
			language:     "ko",
			localizedErr: "Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"csrf":            {a.csrf},
				"target_id":       {"new"},
				"dashboard_name":  {"Synthetic Dashboard"},
				"dashboard_url":   {"http://dashboard.example.invalid"},
				"dashboard_token": {token},
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+"/save-dashboard", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", server.URL)
			request.AddCookie(&http.Cookie{Name: "lah_lang", Value: tt.locale})
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			page := string(body)
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("save validation status=%d body=%s", response.StatusCode, page)
			}
			if !strings.Contains(page, `<html lang="`+tt.language+`">`) || !strings.Contains(page, tt.localizedErr) {
				t.Fatalf("validation response did not use %s: %s", tt.language, page)
			}
			for _, safeDraft := range []string{
				`<option value="new" selected>Add new target</option>`,
				`<input type="hidden" name="target_id" value="new">`,
				`id="dashboard-name" name="dashboard_name" value="Synthetic Dashboard"`,
				`id="dashboard-url" name="dashboard_url" type="url" value="http://dashboard.example.invalid"`,
				`id="dashboard-url-error"`,
			} {
				if !strings.Contains(page, safeDraft) {
					t.Errorf("validation page did not preserve safe draft %q", safeDraft)
				}
			}
			urlField := regexp.MustCompile(`<input id="dashboard-url"[^>]*>`).FindString(page)
			if !strings.Contains(urlField, `autofocus aria-invalid="true"`) || !strings.Contains(urlField, `aria-describedby="dashboard-url-hint dashboard-url-error"`) {
				t.Errorf("invalid URL field did not receive focus/error attributes: %s", urlField)
			}
			for _, fieldID := range []string{"dashboard-name", "dashboard-token"} {
				field := regexp.MustCompile(`<input id="` + fieldID + `"[^>]*>`).FindString(page)
				if field == "" || strings.Contains(field, "autofocus") || strings.Contains(field, `aria-invalid="true"`) {
					t.Errorf("non-invalid field %q received validation focus/error: %s", fieldID, field)
				}
				if fieldID == "dashboard-token" && strings.Contains(field, "value=") {
					t.Errorf("submitted token was retained in the password input: %s", field)
				}
			}
			counts, err := countHTMLValidationAttributes(page)
			if err != nil {
				t.Fatalf("could not inspect validation response autofocus attributes: %v", err)
			}
			if counts.ariaInvalidTrueElements != 1 || counts.autofocusElements != 1 {
				t.Errorf("validation response has %d invalid fields and %d autofocus elements, want one each", counts.ariaInvalidTrueElements, counts.autofocusElements)
			}
			if strings.Contains(page, token) || strings.Contains(response.Header.Get("Location"), token) {
				t.Fatal("validation response exposed the submitted token")
			}
			if response.Header.Get("Location") != "" {
				t.Fatalf("validation response unexpectedly redirected to %q", response.Header.Get("Location"))
			}
			if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != localUIReferrerPolicy {
				t.Errorf("security headers: cache=%q referrer=%q", response.Header.Get("Cache-Control"), response.Header.Get("Referrer-Policy"))
			}
			if secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
				t.Errorf("validation touched secret store: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
			}
			if outboundRequests.Load() != 0 {
				t.Errorf("validation made %d outbound requests", outboundRequests.Load())
			}
			configAfter, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(configBefore) != string(configAfter) {
				t.Fatal("validation changed saved settings")
			}
		})
	}
}
