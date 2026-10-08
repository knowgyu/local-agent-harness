package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsImportPreviewReturnsCountsOnlyWithoutSideEffects(t *testing.T) {
	cfg := settingsImportFixture()
	selected, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "active-settings.json")
	activeSettings := []byte("active-settings-canary; do not inspect or overwrite")
	if err := os.WriteFile(configPath, activeSettings, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := &settingsImportSecretSpy{}
	requests := &settingsImportRequestSpy{}
	a := &app{configPath: configPath, secrets: secrets, client: &http.Client{Transport: requests}, host: "127.0.0.1:43291", csrf: "settings-preview-csrf"}
	response := serveSettingsImportPreview(t, a, selected, "backup-settings.json", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Body.Len() > maxSettingsImportResponseBytes {
		t.Fatalf("preview response headers/size invalid: headers=%v bytes=%d", response.Header(), response.Body.Len())
	}
	var preview settingsImportPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	want := settingsImportPreview{
		Source:              settingsImportSource,
		Version:             configVersion,
		Targets:             settingsImportTargetCount{GitHub: 1, Jenkins: 1, Harbor: 1, Dashboard: 1},
		ServiceBundles:      1,
		Environments:        1,
		DisabledTargets:     2,
		PythonTasks:         2,
		DisabledPythonTasks: 1,
	}
	if preview != want {
		t.Fatalf("preview = %#v, want %#v", preview, want)
	}
	for _, privateValue := range []string{
		"github-id-canary", "jenkins-id-canary", "harbor-id-canary", "dashboard-id-canary", "service-id-canary",
		"github-name-canary", "jenkins-name-canary", "harbor-name-canary", "dashboard-name-canary",
		"https://github-canary.example.invalid", "https://jenkins-canary.example.invalid", "https://harbor-canary.example.invalid", "https://dashboard-canary.example.invalid",
		"user-canary", "cred:", "approval-scope-canary", "bundle-name-canary", "environment-name-canary", "namespace-canary", "deployment-canary",
		"python-task-name-canary", "interpreter-path-canary", "script-path-canary",
		"active-settings-canary", "backup-settings.json", "settings-preview-csrf",
	} {
		if strings.Contains(response.Body.String(), privateValue) {
			t.Errorf("preview exposed %q: %s", privateValue, response.Body)
		}
	}
	for _, privateValue := range []string{
		cfg.GitHubTargets[0].ID, cfg.JenkinsTargets[0].ID, cfg.HarborTargets[0].ID, cfg.DashboardTargets[0].ID,
		cfg.ServiceBundles[0].ID, cfg.GitHubTargets[0].SecretRef, cfg.JenkinsTargets[0].SecretRef,
		cfg.HarborTargets[0].SecretRef, cfg.DashboardTargets[0].SecretRef,
	} {
		if strings.Contains(response.Body.String(), privateValue) {
			t.Errorf("preview exposed identifier or credential reference %q", privateValue)
		}
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("preview accessed credential store: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("preview made %d outbound requests", requests.count)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(after, activeSettings) {
		t.Fatalf("active settings changed: err=%v contents=%q", err, after)
	}
}

func TestParseSelectedSettingsJSONRejectsAmbiguousOrUnsupportedInput(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "duplicate root key", data: []byte(`{"version":4,"version":4}`)},
		{name: "duplicate nested key", data: []byte(`{"version":4,"github_targets":[{"id":"github:11111111111111111111111111111111","id":"github:22222222222222222222222222222222"}]}`)},
		{name: "case variant root key", data: []byte(`{"Version":4}`)},
		{name: "case variant target key", data: []byte(`{"version":4,"github_targets":[{"ID":"github:11111111111111111111111111111111"}]}`)},
		{name: "unknown field", data: []byte(`{"version":4,"api_token":"never-return-this"}`)},
		{name: "unknown nested field", data: []byte(`{"version":4,"github_targets":[{"unexpected":"canary"}]}`)},
		{name: "unknown Python task field", data: []byte(`{"version":5,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","args":["unsafe"]}]}`)},
		{name: "secret fields in v5 task", data: []byte(`{"version":5,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","secret_ref":"cred:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","secret_env_name":"BUILD_TOKEN"}]}`)},
		{name: "fingerprints in v6 task", data: []byte(`{"version":6,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","secret_ref":"cred:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","secret_env_name":"BUILD_TOKEN","interpreter_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)},
		{name: "malformed v7 fingerprint", data: []byte(`{"version":7,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","interpreter_sha256":"not-a-digest","interpreter_size":1,"script_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","script_size":1}]}`)},
		{name: "partial v7 fingerprint", data: []byte(`{"version":7,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","interpreter_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","interpreter_size":1}]}`)},
		{name: "oversize v7 fingerprint", data: []byte(`{"version":7,"python_tasks":[{"id":"python:11111111111111111111111111111111","name":"task","interpreter_path":"C:\\Python\\python.exe","script_path":"C:\\task.py","interpreter_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","interpreter_size":268435457,"script_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","script_size":1}]}`)},
		{name: "named secrets not allowed in v7", data: []byte(`{"version":7,"named_secrets":[]}`)},
		{name: "Python task key in v4", data: []byte(`{"version":4,"python_tasks":[]}`)},
		{name: "null field", data: []byte(`{"version":4,"github_targets":null}`)},
		{name: "unsupported version", data: []byte(`{"version":3}`)},
		{name: "trailing value", data: []byte(`{"version":4} {"version":4}`)},
		{name: "invalid UTF-8", data: []byte{0xff, 0xfe}},
		{name: "invalid syntax", data: []byte(`{"version":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSelectedSettingsJSON(test.data); err == nil {
				t.Fatal("unsupported settings JSON was accepted")
			} else if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "token") {
				t.Fatalf("parser error echoed source data: %v", err)
			}
		})
	}
}

func TestSettingsImportPreviewSupportsLegacyV4AndOmitsTaskPaths(t *testing.T) {
	cfg := settingsImportFixture()
	cfg.Version = 4
	cfg.NamedSecrets = nil
	cfg.PythonTasks = nil
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, sourceVersion, err := parseSelectedSettingsJSONWithVersion(data)
	if err != nil || sourceVersion != 4 || parsed.Version != configVersion || len(parsed.PythonTasks) != 0 {
		t.Fatalf("v4 preview parse cfg=%#v sourceVersion=%d err=%v", parsed, sourceVersion, err)
	}

	a := newSettingsImportTestApp(t)
	response := serveSettingsImportPreview(t, a, data, "legacy-settings.json", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy v4 preview status = %d body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"version":4`) || !strings.Contains(response.Body.String(), `"python_tasks":0`) || strings.Contains(response.Body.String(), "interpreter_path") {
		t.Fatalf("legacy preview version or count projection is wrong: %s", response.Body)
	}
}

func TestSettingsImportPreviewV5V6AndV7TaskSecretsRemainCountOnly(t *testing.T) {
	legacy := configV5{
		Version: 5,
		PythonTasks: []pythonTaskV5{{
			ID: "python:" + strings.Repeat("1", 32), Name: "legacy task",
			InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\task.py`,
		}},
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	parsed, sourceVersion, err := parseSelectedSettingsJSONWithVersion(legacyJSON)
	if err != nil || sourceVersion != 5 || len(parsed.PythonTasks) != 1 {
		t.Fatalf("v5 settings parse cfg=%#v version=%d err=%v", parsed, sourceVersion, err)
	}
	legacyV6 := configV6{
		Version: 6,
		PythonTasks: []pythonTaskV6{{
			ID: "python:" + strings.Repeat("2", 32), Name: "legacy v6 task",
			InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\task.py`,
			SecretEnvName: "V6_SECRET", SecretRef: "cred:" + strings.Repeat("3", 32),
		}},
	}
	legacyV6JSON, err := json.Marshal(legacyV6)
	if err != nil {
		t.Fatal(err)
	}
	parsedV6, sourceV6, err := parseSelectedSettingsJSONWithVersion(legacyV6JSON)
	if err != nil || sourceV6 != 6 || len(parsedV6.PythonTasks) != 1 ||
		parsedV6.PythonTasks[0].SecretEnvName != "V6_SECRET" || parsedV6.PythonTasks[0].SecretRef != legacyV6.PythonTasks[0].SecretRef ||
		pythonTaskIsPinned(parsedV6.PythonTasks[0]) {
		t.Fatalf("v6 preview parse lost secret mapping or inferred pins: cfg=%#v version=%d err=%v", parsedV6, sourceV6, err)
	}

	cfg := settingsImportFixture()
	const secretRef = "cred:" + "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	cfg.PythonTasks[0].SecretEnvName = "BUILD_TOKEN"
	cfg.PythonTasks[0].SecretRef = secretRef
	cfg.PythonTasks[0].InterpreterSHA256 = strings.Repeat("a", 64)
	cfg.PythonTasks[0].InterpreterSize = 1024
	cfg.PythonTasks[0].ScriptSHA256 = strings.Repeat("b", 64)
	cfg.PythonTasks[0].ScriptSize = 512
	cfg.PythonTasks[1].InterpreterSHA256 = strings.Repeat("c", 64)
	cfg.PythonTasks[1].InterpreterSize = 1024
	cfg.PythonTasks[1].ScriptSHA256 = strings.Repeat("d", 64)
	cfg.PythonTasks[1].ScriptSize = 512
	legacyV7 := configV7{
		Version: 7, ConnectionTests: cfg.ConnectionTests,
		GitHubTargets: cfg.GitHubTargets, JenkinsTargets: cfg.JenkinsTargets,
		HarborTargets: cfg.HarborTargets, DashboardTargets: cfg.DashboardTargets,
		ServiceBundles: cfg.ServiceBundles, PythonTasks: cfg.PythonTasks,
	}
	data, err := json.Marshal(legacyV7)
	if err != nil {
		t.Fatal(err)
	}
	a := newSettingsImportTestApp(t)
	v6Response := serveSettingsImportPreview(t, a, legacyV6JSON, "legacy-v6.json", a.csrf, false, false)
	if v6Response.Code != http.StatusOK || !strings.Contains(v6Response.Body.String(), `"version":6`) ||
		!strings.Contains(v6Response.Body.String(), `"python_tasks":1`) || strings.Contains(v6Response.Body.String(), "V6_SECRET") ||
		strings.Contains(v6Response.Body.String(), legacyV6.PythonTasks[0].SecretRef) {
		t.Fatalf("v6 task preview is not count-only: status=%d body=%s", v6Response.Code, v6Response.Body)
	}
	response := serveSettingsImportPreview(t, a, data, "settings.json", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy v7 settings preview status=%d body=%s", response.Code, response.Body)
	}
	for _, forbidden := range []string{"secret_env_name", "secret_ref", secretRef, "BUILD_TOKEN", "interpreter_path", "python-task-name-canary", "interpreter_sha256", "script_sha256", strings.Repeat("a", 64), strings.Repeat("d", 64)} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("settings preview exposed %q: %s", forbidden, response.Body)
		}
	}
	if !strings.Contains(response.Body.String(), `"version":7`) || !strings.Contains(response.Body.String(), `"python_tasks":2`) {
		t.Fatalf("settings preview omitted version/count: %s", response.Body)
	}
}

func TestSettingsImportPreviewRejectsBadFileAndMultipartWithoutEcho(t *testing.T) {
	valid, err := json.Marshal(settingsImportFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		filename   string
		data       []byte
		csrf       string
		extraText  bool
		extraFile  bool
		wantStatus int
	}{
		{name: "wrong extension", filename: "config.txt", data: valid, wantStatus: http.StatusBadRequest},
		{name: "empty file", filename: "config.json", data: nil, wantStatus: http.StatusBadRequest},
		{name: "oversized file", filename: "config.json", data: bytes.Repeat([]byte(" "), maxSettingsImportFileBytes+1), wantStatus: http.StatusBadRequest},
		{name: "unsupported schema", filename: "config.json", data: []byte(`{"version":3}`), wantStatus: http.StatusBadRequest},
		{name: "secret-like unknown field", filename: "config.json", data: []byte(`{"version":4,"api_token":"secret-canary"}`), wantStatus: http.StatusBadRequest},
		{name: "wrong CSRF", filename: "config.json", data: valid, csrf: "wrong-csrf-canary", wantStatus: http.StatusForbidden},
		{name: "extra text field", filename: "config.json", data: valid, extraText: true, wantStatus: http.StatusBadRequest},
		{name: "extra file field", filename: "config.json", data: valid, extraFile: true, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newSettingsImportTestApp(t)
			csrf := test.csrf
			if csrf == "" {
				csrf = a.csrf
			}
			response := serveSettingsImportPreview(t, a, test.data, test.filename, csrf, test.extraText, test.extraFile)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body)
			}
			for _, privateValue := range []string{"wrong-csrf-canary", "extra-field-canary", "extra-file-canary", "github-canary.example.invalid", "secret-canary", "api_token", "cred:"} {
				if strings.Contains(response.Body.String(), privateValue) {
					t.Fatalf("error response exposed %q: %s", privateValue, response.Body)
				}
			}
			if secrets := a.secrets.(*settingsImportSecretSpy); secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
				t.Fatalf("rejected preview accessed credential store: %#v", secrets)
			}
			if requests := a.client.Transport.(*settingsImportRequestSpy); requests.count != 0 {
				t.Fatalf("rejected preview made %d outbound requests", requests.count)
			}
		})
	}
}

func TestSettingsImportPreviewRejectsDuplicateDispositionAndOversize(t *testing.T) {
	t.Run("duplicate disposition", func(t *testing.T) {
		a := newSettingsImportTestApp(t)
		boundary := "settings-preview-boundary"
		body := "--" + boundary + "\r\n" +
			"Content-Disposition: form-data; name=\"csrf\"\r\n\r\n" + a.csrf + "\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Disposition: form-data; name=\"settings_json\"; filename=\"config.json\"\r\n" +
			"Content-Disposition: form-data; name=\"settings_json\"; filename=\"other.json\"\r\n" +
			"Content-Type: application/json\r\n\r\n{\"version\":4}\r\n" +
			"--" + boundary + "--\r\n"
		request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-settings-json", strings.NewReader(body))
		request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
		request.Header.Set("Origin", "http://"+a.host)
		response := httptest.NewRecorder()
		a.securityHeaders(http.HandlerFunc(a.handleSettingsImportPreview)).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "other.json") {
			t.Fatalf("duplicate disposition status/body = %d %s", response.Code, response.Body)
		}
	})

	t.Run("body over limit", func(t *testing.T) {
		a := newSettingsImportTestApp(t)
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("csrf", a.csrf); err != nil {
			t.Fatal(err)
		}
		padding, err := writer.CreateFormField("padding")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := padding.Write(bytes.Repeat([]byte("p"), maxSettingsImportRequestBytes)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-settings-json", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Origin", "http://"+a.host)
		response := httptest.NewRecorder()
		a.securityHeaders(http.HandlerFunc(a.handleSettingsImportPreview)).ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || response.Body.Len() > 1024 || strings.Contains(response.Body.String(), "padding") {
			t.Fatalf("oversized body response = %d %s", response.Code, response.Body)
		}
	})
}

func TestSettingsImportPreviewUIIsBilingualAndSummaryOnly(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43292", csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`id="settings-import-form"`,
		`action="/preview-settings-json"`,
		`name="settings_json"`,
		`accept=".json,application/json"`,
		`aria-describedby="settings-import-hint"`,
		`Versions 4 through 9 are supported for preview; versions 5 through 9 include Python task counts, and version 9 includes SSH target counts.`,
		`schema version and record counts only`,
		`credential references`,
		`does not import or save settings, read or write credentials, or contact a service`,
		`'Preview saved settings JSON': '저장된 설정 JSON 미리보기'`,
		`'Preview settings counts': '설정 항목 개수 미리보기'`,
		`settingsPythonTasks: ['Python tasks', 'Python 작업']`,
		`setDynamic(term, labelKey)`,
		`description.textContent = String(value)`,
		`settingsImportPreview.append(item)`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("settings preview UI missing %q", expected)
		}
	}
	formIndex, saveIndex := strings.Index(body, `id="settings-import-form"`), strings.Index(body, `action="/save-bundle"`)
	if formIndex < 0 || saveIndex < 0 || formIndex > saveIndex {
		t.Fatal("settings preview form should be separate from settings save forms")
	}
	if strings.Contains(body[strings.Index(body, "const settingsImportForm"):], "innerHTML") {
		t.Fatal("settings preview script renders selected values through innerHTML")
	}
	koreanRequest := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	koreanRequest.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	koreanResponse := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(koreanResponse, koreanRequest)
	if !strings.Contains(koreanResponse.Body.String(), `<html lang="ko">`) {
		t.Fatal("Korean settings preview page lost its language attribute")
	}
}

func TestSettingsImportPreviewParsesV8AndV9WithCountOnlySSHProjection(t *testing.T) {
	base := settingsImportFixture()
	if err := ensureNamedSecretMetadata(&base); err != nil {
		t.Fatal(err)
	}
	v8 := configV8{
		Version: 8, ConnectionTests: base.ConnectionTests, NamedSecrets: base.NamedSecrets,
		GitHubTargets: base.GitHubTargets, JenkinsTargets: base.JenkinsTargets,
		HarborTargets: base.HarborTargets, DashboardTargets: base.DashboardTargets,
		ServiceBundles: base.ServiceBundles, PythonTasks: base.PythonTasks,
	}
	v8JSON, err := json.Marshal(v8)
	if err != nil {
		t.Fatal(err)
	}
	parsed, sourceVersion, err := parseSelectedSettingsJSONWithVersion(v8JSON)
	if err != nil || sourceVersion != 8 || parsed.Version != configVersion || len(parsed.NamedSecrets) == 0 {
		t.Fatalf("v8 settings import failed: version=%d parsed=%#v err=%v", sourceVersion, parsed, err)
	}

	v9JSON := []byte(`{"version":9,"ssh_targets":[]}`)
	parsed, sourceVersion, err = parseSelectedSettingsJSONWithVersion(v9JSON)
	if err != nil || sourceVersion != 9 || len(parsed.SSHTargets) != 0 {
		t.Fatalf("v9 SSH settings import failed: version=%d parsed=%#v err=%v", sourceVersion, parsed, err)
	}
	if _, err := parseSelectedSettingsJSON([]byte(`{"version":9,"ssh_targets":[{"id":"target","operations":[{"program":"ssh","token":"must-not-be-supported"}]}]}`)); err == nil {
		t.Fatal("v9 SSH import accepted an unknown sensitive field")
	}
}

func settingsImportFixture() config {
	githubID := "github:" + strings.Repeat("1", 32)
	jenkinsID := "jenkins:" + strings.Repeat("2", 32)
	harborID := "harbor:" + strings.Repeat("3", 32)
	dashboardID := "dashboard:" + strings.Repeat("4", 32)
	cfg := config{
		Version:          configVersion,
		GitHubTargets:    []target{{ID: githubID, Name: "github-name-canary", Origin: "https://github-canary.example.invalid", Repository: "owner-canary/repo-canary", SecretRef: "cred:" + strings.Repeat("a", 32)}},
		JenkinsTargets:   []jenkinsTarget{{ID: jenkinsID, Name: "jenkins-name-canary", BaseURL: "https://jenkins-canary.example.invalid", Username: "user-canary", JobPath: "folder-canary/job-canary", Environment: "production-canary", SecretRef: "cred:" + strings.Repeat("b", 32), NonProductionPreapproved: true, NonProductionApprovalScope: "approval-scope-canary"}},
		HarborTargets:    []harborTarget{{ID: harborID, Name: "harbor-name-canary", BaseURL: "https://harbor-canary.example.invalid", Username: "harbor-user-canary", Project: "project-canary", Repository: "repository-canary", SecretRef: "cred:" + strings.Repeat("c", 32), Disabled: true}},
		DashboardTargets: []dashboardTarget{{ID: dashboardID, Name: "dashboard-name-canary", BaseURL: "https://dashboard-canary.example.invalid", SecretRef: "cred:" + strings.Repeat("d", 32), Disabled: true}},
		ServiceBundles:   []serviceBundle{{ID: "service:" + strings.Repeat("5", 32), Name: "bundle-name-canary", GitHubTargetIDs: []string{githubID}, Environments: []serviceEnvironment{{Name: "environment-name-canary", JenkinsTargetID: jenkinsID, HarborTargetID: harborID, DashboardTargetID: dashboardID, DashboardNamespace: "namespace-canary", DashboardDeployment: "deployment-canary"}}}},
		ConnectionTests:  map[string]connectionTest{githubID: {Result: "success", CompletedAt: "2026-09-27T00:00:00Z"}},
		PythonTasks: []pythonTask{
			{ID: "python:" + strings.Repeat("6", 32), Name: "python-task-name-canary", InterpreterPath: filepath.Join(os.TempDir(), "interpreter-path-canary.exe"), ScriptPath: filepath.Join(os.TempDir(), "script-path-canary.py"), SecretEnvName: "PYTHON_TASK_TOKEN", SecretRef: "cred:" + strings.Repeat("e", 32)},
			{ID: "python:" + strings.Repeat("7", 32), Name: "disabled-python-task-canary", InterpreterPath: filepath.Join(os.TempDir(), "disabled-interpreter-canary.exe"), ScriptPath: filepath.Join(os.TempDir(), "disabled-script-canary.py"), Disabled: true},
		},
	}
	if err := ensureNamedSecretMetadata(&cfg); err != nil {
		panic("invalid settings import fixture: " + err.Error())
	}
	return cfg
}

func newSettingsImportTestApp(t *testing.T) *app {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "active-config.json")
	if err := os.WriteFile(configPath, []byte("active-config-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &app{configPath: configPath, secrets: &settingsImportSecretSpy{}, client: &http.Client{Transport: &settingsImportRequestSpy{}}, host: "127.0.0.1:43293", csrf: "settings-csrf-token"}
}

func serveSettingsImportPreview(t *testing.T, a *app, data []byte, filename, csrf string, extraText, extraFile bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("settings_json", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if extraText {
		if err := writer.WriteField("unexpected", "extra-field-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if extraFile {
		other, err := writer.CreateFormFile("another_file", "notes.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(other, "extra-file-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-settings-json", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleSettingsImportPreview)).ServeHTTP(response, request)
	return response
}

type settingsImportSecretSpy struct {
	loads, saves, deletes int
}

func (s *settingsImportSecretSpy) Save(string, []byte) error {
	s.saves++
	return nil
}

func (s *settingsImportSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, nil
}

func (s *settingsImportSecretSpy) Delete(string) error {
	s.deletes++
	return nil
}

type settingsImportRequestSpy struct {
	count int
}

func (s *settingsImportRequestSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.count++
	return nil, errors.New("unexpected outbound request")
}
