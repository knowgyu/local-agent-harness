package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type harborTestRoundTripper func(*http.Request) (*http.Response, error)

func (f harborTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func harborTestResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestHarborArtifactRequestProjectsAndBoundsRows(t *testing.T) {
	const secret = "harbor_password_canary_0123456789"
	target := harborTarget{
		Name: "staging registry", BaseURL: "https://harbor.example.invalid",
		Username: "robot$staging", Project: "platform", Repository: "team/service/image",
	}
	var requestCount int
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requestCount++
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got, want := r.URL.EscapedPath(), "/api/v2.0/projects/platform/repositories/team%252Fservice%252Fimage/artifacts"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("page_size") != "100" || r.URL.Query().Get("with_tag") != "true" {
			t.Errorf("query = %v", r.URL.Query())
		}
		if user, password, ok := r.BasicAuth(); !ok || user != target.Username || password != secret {
			t.Errorf("Basic auth = (%q, %q, %v)", user, password, ok)
		}
		var rows []map[string]any
		for i := 0; i < 101; i++ {
			rows = append(rows, map[string]any{
				"digest": "sha256:abc", "size": i, "tags": []map[string]any{{"name": "v1", "unprojected": secret}},
				"unprojected": secret,
			})
		}
		body, _ := json.Marshal(rows)
		return harborTestResponse(r, http.StatusOK, string(body)), nil
	})}
	result, err := fetchHarborArtifacts(context.Background(), target, secret, client)
	if err != nil {
		t.Fatal(err)
	}
	if requestCount != 1 || len(result.Artifacts) != harborPageSize || !result.Truncated || !result.MayBePartial {
		t.Fatalf("requests=%d artifacts=%d truncated=%v may_be_partial=%v", requestCount, len(result.Artifacts), result.Truncated, result.MayBePartial)
	}
	if result.Artifacts[0].Digest != "sha256:abc" || result.Artifacts[0].Size != 0 || len(result.Artifacts[0].Tags) != 1 || result.Artifacts[0].Tags[0].Name != "v1" {
		t.Fatalf("unexpected projection: %#v", result.Artifacts[0])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "unprojected") {
		t.Fatalf("MCP projection leaked fixture data: %s", encoded)
	}
}

func TestHarborExactlyFullPageMayBePartial(t *testing.T) {
	rows := make([]harborArtifact, harborPageSize)
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		return harborTestResponse(r, http.StatusOK, string(body)), nil
	})}
	result, err := fetchHarborArtifacts(context.Background(), harborTarget{BaseURL: "https://harbor.example.invalid", Project: "platform", Repository: "app"}, "secret", client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != harborPageSize || result.Truncated || !result.MayBePartial {
		t.Fatalf("artifacts=%d truncated=%v may_be_partial=%v", len(result.Artifacts), result.Truncated, result.MayBePartial)
	}
}

func TestHarborTargetValidationRejectsUnsafeScopeAndURL(t *testing.T) {
	base := harborTarget{
		ID: "harbor:0123456789abcdef0123456789abcdef", Name: "staging",
		BaseURL: "https://harbor.example.invalid/proxy", Username: "robot$staging",
		Project: "platform", Repository: "team/service", SecretRef: "cred:0123456789abcdef0123456789abcdef",
	}
	if err := validateHarborTarget(base); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	for _, change := range []func(*harborTarget){
		func(t *harborTarget) { t.BaseURL = "http://harbor.example.invalid" },
		func(t *harborTarget) { t.BaseURL = "https://user:password@harbor.example.invalid" },
		func(t *harborTarget) { t.BaseURL = "https://harbor.example.invalid?path=/other" },
		func(t *harborTarget) { t.Project = "team/project" },
		func(t *harborTarget) { t.Repository = "../other" },
		func(t *harborTarget) { t.Repository = "team//other" },
		func(t *harborTarget) { t.Repository = "team/%2Fother" },
	} {
		invalid := base
		change(&invalid)
		if err := validateHarborTarget(invalid); err == nil {
			t.Fatalf("unsafe target accepted: %#v", invalid)
		}
	}
}

func TestHarborBoundsResponseAndExpandedOutput(t *testing.T) {
	target := harborTarget{BaseURL: "https://harbor.example.invalid", Project: "platform", Repository: "app"}
	rows := make([]map[string]any, harborPageSize)
	for i := range rows {
		tags := make([]map[string]string, 100)
		for j := range tags {
			tags[j] = map[string]string{"name": strings.Repeat("x", 30)}
		}
		rows[i] = map[string]any{"digest": "d", "size": 1, "tags": tags}
	}
	wideBody, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		secret string
		body   string
	}{
		{name: "response bytes", secret: "long-secret", body: strings.Repeat("x", maxAPIBytes+1)},
		{name: "redaction expansion", secret: "x", body: string(wideBody)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				return harborTestResponse(r, http.StatusOK, test.body), nil
			})}
			if _, err := fetchHarborArtifacts(context.Background(), target, test.secret, client); err == nil {
				t.Fatal("oversized response/output was accepted")
			}
		})
	}
}

func TestHarborRejectsRedirectsAndSanitizesFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "redirect", status: http.StatusFound, body: "redirect-canary"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "credential-canary"},
		{name: "forbidden", status: http.StatusForbidden, body: "credential-canary"},
		{name: "invalid JSON", status: http.StatusOK, body: "response-canary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := harborTestResponse(r, test.status, test.body)
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/leak")
				}
				return resp, nil
			})}
			_, err := fetchHarborArtifacts(context.Background(), harborTarget{BaseURL: "https://harbor.example.invalid", Project: "platform", Repository: "app"}, "secret-canary", client)
			if err == nil || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatalf("error leaked response/credential: %v", err)
			}
		})
	}
}

func TestHarborTargetSaveDisableDeleteLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	secrets := memorySecrets{}
	a := &app{configPath: path, secrets: secrets}
	id, err := a.saveHarborTarget("", "staging", "https://harbor.example.invalid", "robot$staging", "platform", "team/service", "credential-canary")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.HarborTargets) != 1 {
		t.Fatalf("saved config = %#v, err=%v", cfg.HarborTargets, err)
	}
	ref := cfg.HarborTargets[0].SecretRef
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "credential-canary") {
		t.Fatal("Harbor credential was written to settings")
	}
	if _, err := a.saveHarborTarget(id, "staging", "https://harbor.example.invalid", "robot$staging", "platform", "team/service", ""); err != nil {
		t.Fatalf("update without credential = %v", err)
	}
	if _, err := a.saveHarborTarget(id, "staging", "https://harbor.example.invalid", "robot$changed", "platform", "team/service", ""); err == nil {
		t.Fatal("changing Basic-auth username without a new credential succeeded")
	}
	if _, err := a.saveHarborTarget(id, "staging", "https://harbor.example.invalid", "robot$changed", "platform", "team/service", "replacement-canary"); err != nil {
		t.Fatal(err)
	}
	if _, exists := secrets[ref]; exists {
		t.Fatal("replaced credential was not deleted")
	}
	if err := a.setTargetDisabled("harbor", id, true); err != nil {
		t.Fatal(err)
	}
	if _, _, loadErr := a.loadHarborTarget("staging"); loadErr == nil || !strings.Contains(loadErr.Error(), "disabled") {
		t.Fatalf("disabled target load error = %v", loadErr)
	}
	catalog, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Targets) != 0 {
		t.Fatalf("disabled Harbor target appeared in catalog: %#v", catalog)
	}
	if err := a.deleteRegisteredTarget("harbor", id); err != nil {
		t.Fatal(err)
	}
	cfg, err = readConfig(path)
	if err != nil || len(cfg.HarborTargets) != 0 || len(cfg.NamedSecrets) != 1 || len(secrets) != 1 || secrets[cfg.NamedSecrets[0].CredentialRef] == nil {
		t.Fatalf("after delete config=%#v named_secrets=%d secrets=%d err=%v", cfg.HarborTargets, len(cfg.NamedSecrets), len(secrets), err)
	}
}

func TestHarborMCPToolUsesOnlyRegisteredTarget(t *testing.T) {
	const secret = "harbor_mcp_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	ref := "cred:0123456789abcdef0123456789abcdef"
	target := harborTarget{ID: "harbor:0123456789abcdef0123456789abcdef", Name: "staging", BaseURL: "https://harbor.example.invalid", Username: "robot$staging", Project: "platform", Repository: "team/service", SecretRef: ref}
	if err := writeConfig(path, config{Version: configVersion, HarborTargets: []harborTarget{target}}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v2.0/projects/platform/repositories/team%2Fservice/artifacts" {
			t.Errorf("decoded request path = %q", r.URL.Path)
		}
		if user, password, ok := r.BasicAuth(); !ok || user != target.Username || password != secret {
			t.Errorf("Basic auth = (%q, %q, %v)", user, password, ok)
		}
		return harborTestResponse(r, http.StatusOK, `[{"digest":"sha256:abc","size":123,"tags":[{"name":"v1"}],"password":"`+secret+`"}]`), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{ref: []byte(secret)}, client: client}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "harbor-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	catalog, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	if err != nil {
		t.Fatal(err)
	}
	catalogJSON, _ := json.Marshal(catalog)
	if strings.Contains(string(catalogJSON), secret) || strings.Contains(string(catalogJSON), target.BaseURL) || !strings.Contains(string(catalogJSON), "harbor_repository_artifacts") {
		t.Fatalf("unexpected Harbor catalog: %s", catalogJSON)
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "harbor_repository_artifacts", Arguments: map[string]any{"target": target.Name}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "password") || !strings.Contains(string(encoded), `"size":123`) {
		t.Fatalf("unexpected MCP result: %s", encoded)
	}
	extra, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "harbor_repository_artifacts", Arguments: map[string]any{"target": target.Name, "repository": "other"}})
	if err == nil && extra.IsError == false {
		t.Fatalf("MCP accepted an unregistered repository argument: %#v", extra)
	}
	unknown, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "harbor_repository_artifacts", Arguments: map[string]any{"target": "unknown"}})
	if err == nil && !unknown.IsError {
		t.Fatalf("MCP accepted an unregistered target: %#v", unknown)
	}

}

func TestHarborUIIncludesAccessibleTargetFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	target := harborTarget{ID: "harbor:0123456789abcdef0123456789abcdef", Name: "staging", BaseURL: "https://harbor.example.invalid", Username: "robot$staging", Project: "platform", Repository: "team/service", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	if err := writeConfig(path, config{Version: configVersion, HarborTargets: []harborTarget{target}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "/?harbor_error=repository&harbor_id="+target.ID, nil)
	response := httptest.NewRecorder()
	a.handleRoot(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("root status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`id="harbor-selection"`, `action="/save-harbor"`, `action="/test-harbor"`,
		`for="harbor-url"`, `aria-describedby="harbor-url-hint`,
		`id="harbor-repository-error"`, `href="#harbor-repository"`,
		`pattern="[\x2dA-Za-z0-9._~]+"`, `pattern="[\x2dA-Za-z0-9._~]+(/[\x2dA-Za-z0-9._~]+)*"`,
		`name="kind" value="harbor"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("rendered Harbor page missing %q", expected)
		}
	}
}

func TestHarborSaveValidationPreservesSafeDraftOnly(t *testing.T) {
	tests := []struct {
		name          string
		targetID      string
		targetName    string
		baseURL       string
		username      string
		project       string
		repository    string
		secret        string
		errorField    string
		statusMessage string
		existing      []harborTarget
	}{
		{
			name:          "invalid address",
			targetID:      "harbor:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			targetName:    "Draft & <new>",
			baseURL:       "http://harbor.example.invalid",
			username:      "robot$qa",
			project:       "platform",
			repository:    "team/service",
			secret:        "harbor_invalid_url_canary_0123456789",
			errorField:    "url",
			statusMessage: "The Harbor url field is invalid.",
		},
		{
			name:          "duplicate name",
			targetName:    "Existing Harbor",
			baseURL:       "https://draft-harbor.example.invalid",
			username:      "robot$draft",
			project:       "draft-project",
			repository:    "draft/service",
			secret:        "harbor_duplicate_canary_0123456789",
			errorField:    "name",
			statusMessage: "A Harbor target name is already registered.",
			existing: []harborTarget{{
				ID:         "harbor:0123456789abcdef0123456789abcdef",
				Name:       "Existing Harbor",
				BaseURL:    "https://harbor.example.invalid",
				Username:   "robot$existing",
				Project:    "platform",
				Repository: "team/service",
				SecretRef:  "cred:0123456789abcdef0123456789abcdef",
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			var before []byte
			if len(test.existing) > 0 {
				if err := writeConfig(path, config{Version: configVersion, HarborTargets: test.existing}); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}

			secrets := memorySecrets{}
			requests := 0
			a := &app{
				configPath: path,
				secrets:    secrets,
				csrf:       "csrf-token",
				client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
					requests++
					return harborTestResponse(r, http.StatusInternalServerError, "unexpected request"), nil
				})},
			}
			form := url.Values{
				"csrf":              {a.csrf},
				"target_id":         {test.targetID},
				"harbor_name":       {test.targetName},
				"harbor_url":        {test.baseURL},
				"harbor_username":   {test.username},
				"harbor_project":    {test.project},
				"harbor_repository": {test.repository},
				"harbor_secret":     {test.secret},
			}
			request := httptest.NewRequest(http.MethodPost, "/save-harbor", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			a.handleHarborSave(response, request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("save status = %d body=%s", response.Code, response.Body.String())
			}

			body := response.Body.String()
			wantName := template.HTMLEscapeString(test.targetName)
			want := []string{
				`<option value="new" selected>Add new target</option>`,
				`<input type="hidden" name="target_id" value="new">`,
				`id="harbor-name" name="harbor_name" value="` + wantName + `"`,
				`id="harbor-url" name="harbor_url" type="url" value="` + template.HTMLEscapeString(test.baseURL) + `"`,
				`id="harbor-username" name="harbor_username" value="` + template.HTMLEscapeString(test.username) + `"`,
				`id="harbor-project" name="harbor_project" value="` + template.HTMLEscapeString(test.project) + `"`,
				`id="harbor-repository" name="harbor_repository" value="` + template.HTMLEscapeString(test.repository) + `"`,
				`data-message-en="` + test.statusMessage + `"`,
				`id="harbor-` + test.errorField + `-error"`,
				`aria-invalid="true"`,
			}
			for _, expected := range want {
				if !strings.Contains(body, expected) {
					t.Errorf("validation page missing %q", expected)
				}
			}
			assertFirstInvalidFieldFocused(t, body, "harbor-"+test.errorField)
			if strings.Contains(body, test.secret) || strings.Contains(response.Header().Get("Location"), test.secret) || strings.Contains(a.status, test.secret) {
				t.Fatal("Harbor secret appeared in the response, redirect, or shared status")
			}
			if response.Header().Get("Location") != "" {
				t.Fatalf("validation response unexpectedly redirected to %q", response.Header().Get("Location"))
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != localUIReferrerPolicy {
				t.Fatalf("validation response security headers = cache %q, referrer %q", response.Header().Get("Cache-Control"), response.Header().Get("Referrer-Policy"))
			}
			if requests != 0 {
				t.Fatalf("validation made %d service requests", requests)
			}
			if len(secrets) != 0 {
				t.Fatal("validation wrote a credential")
			}
			if len(before) == 0 {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("validation changed settings, stat err=%v", err)
				}
				return
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("duplicate-name validation changed saved settings")
			}
		})
	}
}

func TestHarborEditValidationKeepsSavedCredentialHintAndSafeDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	credentialRef := "cred:0123456789abcdef0123456789abcdef"
	target := harborTarget{
		ID:         "harbor:0123456789abcdef0123456789abcdef",
		Name:       "Existing Harbor",
		BaseURL:    "https://harbor.example.invalid",
		Username:   "robot$existing",
		Project:    "platform",
		Repository: "team/service",
		SecretRef:  credentialRef,
	}
	if err := writeConfig(path, config{Version: configVersion, HarborTargets: []harborTarget{target}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const savedSecret = "harbor_saved_credential_canary_0123456789"
	secrets := memorySecrets{credentialRef: []byte(savedSecret)}
	requests := 0
	a := &app{
		configPath: path,
		secrets:    secrets,
		csrf:       "csrf-token",
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests++
			return nil, errors.New("validation failure must not contact a service")
		})},
	}
	form := url.Values{
		"csrf":              {a.csrf},
		"target_id":         {target.ID},
		"harbor_name":       {"Draft edited Harbor"},
		"harbor_url":        {"https://draft-harbor.example.invalid"},
		"harbor_username":   {"robot$draft"},
		"harbor_project":    {"draft-project"},
		"harbor_repository": {"draft/service"},
		"harbor_secret":     {""},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-harbor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleHarborSave(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save status = %d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`<option value="` + target.ID + `" selected >Existing Harbor</option>`,
		`<input type="hidden" name="target_id" value="` + target.ID + `">`,
		`id="harbor-name" name="harbor_name" value="Draft edited Harbor"`,
		`id="harbor-url" name="harbor_url" type="url" value="https://draft-harbor.example.invalid"`,
		`id="harbor-username" name="harbor_username" value="robot$draft"`,
		`id="harbor-project" name="harbor_project" value="draft-project"`,
		`id="harbor-repository" name="harbor_repository" value="draft/service"`,
		`id="harbor-secret" name="harbor_secret" type="password"`,
		`Saved in Windows Credential Manager. Leave blank to keep it.`,
		`id="harbor-secret-error"`,
		`aria-invalid="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Harbor edit validation page missing %q", want)
		}
	}
	assertFirstInvalidFieldFocused(t, body, "harbor-secret")
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `id="harbor-secret"`) && strings.Contains(line, " value=") {
			t.Fatal("Harbor password field unexpectedly contained a value")
		}
	}
	for _, secret := range []string{savedSecret} {
		if strings.Contains(body, secret) || strings.Contains(response.Header().Get("Location"), secret) || strings.Contains(a.status, secret) {
			t.Fatalf("Harbor credential appeared in response, redirect, or shared status: %q", secret)
		}
	}
	if response.Header().Get("Location") != "" {
		t.Fatalf("validation response unexpectedly redirected to %q", response.Header().Get("Location"))
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != localUIReferrerPolicy {
		t.Fatalf("validation response security headers = cache %q, referrer %q", response.Header().Get("Cache-Control"), response.Header().Get("Referrer-Policy"))
	}
	if requests != 0 {
		t.Fatalf("validation made %d service requests", requests)
	}
	if got := string(secrets[credentialRef]); got != savedSecret {
		t.Fatalf("saved Harbor credential changed to %q", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Harbor edit validation changed saved settings")
	}
}

func TestHarborSettingsPOSTRegistersAndTestsTarget(t *testing.T) {
	const secret = "harbor_form_canary_0123456789"
	a := &app{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		secrets:    memorySecrets{},
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			if user, password, ok := r.BasicAuth(); !ok || user != "robot$qa" || password != secret {
				t.Errorf("Basic auth = (%q, %q, %v)", user, password, ok)
			}
			return harborTestResponse(r, http.StatusOK, `[]`), nil
		})},
		csrf: "csrf-token",
	}
	form := url.Values{
		"csrf":              {a.csrf},
		"target_id":         {"new"},
		"harbor_name":       {"qa"},
		"harbor_url":        {"https://harbor.example.invalid"},
		"harbor_username":   {"robot$qa"},
		"harbor_project":    {"platform"},
		"harbor_repository": {"team/service"},
		"harbor_secret":     {secret},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-harbor", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleHarborSave(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body=%s", response.Code, response.Body.String())
	}
	cfg, err := readConfig(a.configPath)
	if err != nil || len(cfg.HarborTargets) != 1 {
		t.Fatalf("saved Harbor targets=%#v err=%v", cfg.HarborTargets, err)
	}
	target := cfg.HarborTargets[0]
	testForm := url.Values{"csrf": {a.csrf}, "target_id": {target.ID}, "target": {target.Name}}
	request = httptest.NewRequest(http.MethodPost, "/test-harbor", strings.NewReader(testForm.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	a.handleHarborTest(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?harbor_id="+url.QueryEscape(target.ID) {
		t.Fatalf("test response status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	if !strings.Contains(a.status, "Connection test succeeded. Historical status saved.") {
		t.Fatalf("connection status = %q", a.status)
	}
}
