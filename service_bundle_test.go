package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func serviceBundleTargets() (target, target, jenkinsTarget, harborTarget, dashboardTarget) {
	githubOne := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	githubTwo := target{ID: "github:1123456789abcdef0123456789abcdef", Name: "Platform", Origin: "https://github.example.invalid", Repository: "platform/agent", SecretRef: "cred:1123456789abcdef0123456789abcdef"}
	jenkins := jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "CI", BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user", JobPath: "folder/build", Environment: "qa", SecretRef: "cred:2123456789abcdef0123456789abcdef"}
	harbor := harborTarget{ID: "harbor:0123456789abcdef0123456789abcdef", Name: "Container images", BaseURL: "https://harbor.example.invalid", Username: "robot-ci", Project: "platform", Repository: "service/api", SecretRef: "cred:3123456789abcdef0123456789abcdef"}
	dashboard := dashboardTarget{ID: "dashboard:0123456789abcdef0123456789abcdef", Name: "Cluster UI", BaseURL: "https://dashboard.example.invalid/proxy", SecretRef: "cred:4123456789abcdef0123456789abcdef"}
	return githubOne, githubTwo, jenkins, harbor, dashboard
}

func serviceBundleConfig() config {
	githubOne, githubTwo, jenkins, harbor, dashboard := serviceBundleTargets()
	return config{
		Version:       configVersion,
		GitHubTargets: []target{githubOne, githubTwo}, JenkinsTargets: []jenkinsTarget{jenkins}, HarborTargets: []harborTarget{harbor}, DashboardTargets: []dashboardTarget{dashboard},
		ServiceBundles: []serviceBundle{{
			ID: "service:0123456789abcdef0123456789abcdef", Name: "Inventory", GitHubTargetIDs: []string{githubOne.ID, githubTwo.ID},
			Environments: []serviceEnvironment{{Name: "qa-blue", JenkinsTargetID: jenkins.ID, HarborTargetID: harbor.ID, DashboardTargetID: dashboard.ID, DashboardNamespace: "apps", DashboardDeployment: "api.v2"}},
		}},
	}
}

type serviceBundleSecrets struct{ writes, loads, deletes int }

func (s *serviceBundleSecrets) Save(string, []byte) error   { s.writes++; return nil }
func (s *serviceBundleSecrets) Load(string) ([]byte, error) { s.loads++; return nil, http.ErrNoCookie }
func (s *serviceBundleSecrets) Delete(string) error         { s.deletes++; return nil }

func TestMigrateV2ConfigPreservesTargetsAndSecretReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := serviceBundleConfig()
	old.Version = 2
	old.ServiceBundles = nil
	old.JenkinsTargets[0].NonProductionPreapproved = true
	old.JenkinsTargets[0].NonProductionApprovalScope = "preserve-existing-scope"
	want := configV2{Version: 2, GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets, HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v2 settings: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != configVersion || len(got.ServiceBundles) != 0 || !reflect.DeepEqual(got.GitHubTargets, old.GitHubTargets) || !reflect.DeepEqual(got.JenkinsTargets, old.JenkinsTargets) || !reflect.DeepEqual(got.HarborTargets, old.HarborTargets) || !reflect.DeepEqual(got.DashboardTargets, old.DashboardTargets) {
		t.Fatalf("migrated config lost fields or references: %#v", got)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat migration changed settings bytes")
	}
}

func TestMigrateV2RejectsUnknownAndTrailingJSONWithoutWriting(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown field":  `{"version":2,"future":true}`,
		"trailing value": `{"version":2} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if err := migrateConfig(path); err == nil {
				t.Fatal("invalid v2 settings were accepted")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("failed migration modified settings")
			}
		})
	}
}

func TestServiceBundleValidationRejectsBadReferencesAndNames(t *testing.T) {
	valid := serviceBundleConfig()
	tests := map[string]func(*config){
		"stale GitHub reference": func(cfg *config) {
			cfg.ServiceBundles[0].GitHubTargetIDs[0] = "github:ffffffffffffffffffffffffffffffff"
		},
		"wrong reference type": func(cfg *config) { cfg.ServiceBundles[0].Environments[0].JenkinsTargetID = cfg.HarborTargets[0].ID },
		"duplicate repository": func(cfg *config) { cfg.ServiceBundles[0].GitHubTargetIDs[1] = cfg.ServiceBundles[0].GitHubTargetIDs[0] },
		"duplicate environment": func(cfg *config) {
			cfg.ServiceBundles[0].Environments = append(cfg.ServiceBundles[0].Environments, cfg.ServiceBundles[0].Environments[0])
		},
		"unpaired dashboard resource":             func(cfg *config) { cfg.ServiceBundles[0].Environments[0].DashboardDeployment = "" },
		"namespace cannot be dotted":              func(cfg *config) { cfg.ServiceBundles[0].Environments[0].DashboardNamespace = "apps.team" },
		"deployment accepts dotted DNS subdomain": func(cfg *config) { cfg.ServiceBundles[0].Environments[0].DashboardDeployment = "api.v2" },
		"invalid deployment name":                 func(cfg *config) { cfg.ServiceBundles[0].Environments[0].DashboardDeployment = "api..v2" },
		"resource requires Dashboard target":      func(cfg *config) { cfg.ServiceBundles[0].Environments[0].DashboardTargetID = "" },
		"duplicate bundle name": func(cfg *config) {
			cfg.ServiceBundles = append(cfg.ServiceBundles, serviceBundle{ID: "service:1123456789abcdef0123456789abcdef", Name: cfg.ServiceBundles[0].Name, GitHubTargetIDs: []string{cfg.GitHubTargets[0].ID}})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			cfg.ServiceBundles = append([]serviceBundle(nil), valid.ServiceBundles...)
			cfg.ServiceBundles[0].GitHubTargetIDs = append([]string(nil), valid.ServiceBundles[0].GitHubTargetIDs...)
			cfg.ServiceBundles[0].Environments = append([]serviceEnvironment(nil), valid.ServiceBundles[0].Environments...)
			mutate(&cfg)
			err := validateConfig(cfg)
			if name == "deployment accepts dotted DNS subdomain" {
				if err != nil {
					t.Fatalf("valid dotted Deployment rejected: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid service bundle was accepted")
			}
		})
	}
}

func TestServiceBundleHTTPCreateEditDeleteAndTargetDeletionGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfigWithoutBundles()); err != nil {
		t.Fatal(err)
	}
	secrets := &serviceBundleSecrets{}
	a := &app{configPath: path, secrets: secrets, csrf: "csrf-token"}
	form := url.Values{
		"csrf": {a.csrf}, "bundle_id": {"new"}, "bundle_name": {"My service"},
		"github_target_ids": {"github:0123456789abcdef0123456789abcdef", "github:1123456789abcdef0123456789abcdef"},
		"environment_name":  {"qa-blue"}, "environment_jenkins_id": {"jenkins:0123456789abcdef0123456789abcdef"},
		"environment_harbor_id": {"harbor:0123456789abcdef0123456789abcdef"}, "environment_dashboard_id": {"dashboard:0123456789abcdef0123456789abcdef"},
		"environment_dashboard_namespace": {"apps"}, "environment_dashboard_deployment": {"api.v2"},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-bundle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleServiceBundleSave(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, body=%s", response.Code, response.Body)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	id := location.Query().Get("bundle_id")
	if !serviceBundleIDPattern.MatchString(id) {
		t.Fatalf("created bundle redirect ID = %q", id)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.ServiceBundles) != 1 || cfg.ServiceBundles[0].Name != "My service" || cfg.ServiceBundles[0].Environments[0].DashboardDeployment != "api.v2" {
		t.Fatalf("saved service bundle = %#v, err=%v", cfg.ServiceBundles, err)
	}
	if secrets.writes != 0 || secrets.loads != 0 {
		t.Fatalf("bundle save accessed credentials: writes=%d loads=%d", secrets.writes, secrets.loads)
	}
	if err := a.deleteRegisteredTarget("github", cfg.GitHubTargets[0].ID); err == nil {
		t.Fatal("referenced GitHub target deletion succeeded")
	}
	if err := a.deleteRegisteredTarget("jenkins", cfg.JenkinsTargets[0].ID); err == nil {
		t.Fatal("referenced Jenkins target deletion succeeded")
	}
	if err := a.deleteRegisteredTarget("harbor", cfg.HarborTargets[0].ID); err == nil {
		t.Fatal("referenced Harbor target deletion succeeded")
	}
	if err := a.deleteRegisteredTarget("dashboard", cfg.DashboardTargets[0].ID); err == nil {
		t.Fatal("referenced Dashboard target deletion succeeded")
	}
	if secrets.deletes != 0 {
		t.Fatal("blocked target deletion accessed credentials")
	}
	if _, err := a.saveServiceBundle(id, "Renamed service", cfg.ServiceBundles[0].GitHubTargetIDs, cfg.ServiceBundles[0].Environments); err != nil {
		t.Fatalf("edit service bundle: %v", err)
	}
	form = url.Values{"csrf": {a.csrf}, "bundle_id": {id}, "confirm_delete": {"yes"}}
	request = httptest.NewRequest(http.MethodPost, "/delete-bundle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	a.handleServiceBundleDelete(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d", response.Code)
	}
	cfg, err = readConfig(path)
	if err != nil || len(cfg.ServiceBundles) != 0 {
		t.Fatalf("deleted service bundle remains: %#v, err=%v", cfg.ServiceBundles, err)
	}
}

func serviceBundleConfigWithoutBundles() config {
	cfg := serviceBundleConfig()
	cfg.ServiceBundles = nil
	return cfg
}

func TestServiceBundleCatalogIsMCPExposedAndSecretFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	cfg.GitHubTargets[1].Disabled = true
	cfg.JenkinsTargets[0].Disabled = true
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "service-bundle-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_service_bundles"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("registered_service_bundles returned an MCP error: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	data := string(encoded)
	serviceCatalog, err := a.registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	if len(serviceCatalog.Bundles) != 1 || len(serviceCatalog.Bundles[0].Repositories[1].Actions) != 0 || serviceCatalog.Bundles[0].Environments[0].Jenkins == nil || len(serviceCatalog.Bundles[0].Environments[0].Jenkins.Actions) != 0 {
		t.Fatalf("disabled targets advertised actions: %+v", serviceCatalog)
	}
	for _, expected := range []string{"Inventory", "qa-blue", "Engineering", "Platform", "CI", "Container images", "Cluster UI", "github_repository", "github_pull_request", "registered_target_connection_test", "harbor_repository_artifacts", "dashboard_namespaces", "dashboard_deployment_status", "dashboard_deployment_diagnosis"} {
		if !strings.Contains(data, expected) {
			t.Errorf("catalog missing %q: %s", expected, data)
		}
	}
	for _, forbidden := range []string{
		"github:0123456789abcdef0123456789abcdef", "jenkins:0123456789abcdef0123456789abcdef", "https://github.example.invalid", "https://jenkins.example.invalid", "folder/build", "platform", "service/api", "cred:", "apps", "api.v2",
	} {
		if strings.Contains(data, forbidden) {
			t.Errorf("catalog leaked %q: %s", forbidden, data)
		}
	}
}

func TestServiceBundleDashboardDeploymentActionRequiresEnabledMappedTarget(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config)
		invalid bool
	}{
		{
			name: "disabled Dashboard target",
			mutate: func(cfg *config) {
				cfg.DashboardTargets[0].Disabled = true
			},
		},
		{
			name: "target with no resource mapping",
			mutate: func(cfg *config) {
				environment := &cfg.ServiceBundles[0].Environments[0]
				environment.DashboardNamespace = ""
				environment.DashboardDeployment = ""
			},
		},
		{
			name: "incomplete resource mapping",
			mutate: func(cfg *config) {
				cfg.ServiceBundles[0].Environments[0].DashboardDeployment = ""
			},
			invalid: true,
		},
		{
			name: "invalid namespace mapping",
			mutate: func(cfg *config) {
				cfg.ServiceBundles[0].Environments[0].DashboardNamespace = "apps.team"
			},
			invalid: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			test.mutate(&cfg)
			if test.invalid {
				data, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}

			catalog, err := (&app{configPath: path}).registeredServiceBundles()
			if test.invalid {
				if err == nil {
					t.Fatal("catalog accepted an invalid Dashboard mapping")
				}
				if len(catalog.Bundles) != 0 {
					t.Fatalf("catalog returned bundles for invalid Dashboard mapping: %+v", catalog)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			dashboard := catalog.Bundles[0].Environments[0].Dashboard
			if dashboard == nil {
				t.Fatal("catalog omitted the registered Dashboard target")
			}
			for _, action := range dashboard.Actions {
				if action == "dashboard_deployment_status" || action == "dashboard_deployment_events" || action == "dashboard_deployment_pods" || action == "dashboard_deployment_pod_logs" || action == "dashboard_deployment_diagnosis" {
					t.Fatalf("catalog advertised %s without an enabled target and valid mapping", action)
				}
			}
		})
	}
}

func TestServiceBundleDashboardDeploymentActionsListedForEnabledCompleteMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	catalog, err := (&app{configPath: path}).registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	dashboard := catalog.Bundles[0].Environments[0].Dashboard
	if dashboard == nil {
		t.Fatal("catalog omitted enabled Dashboard target with complete mapping")
	}
	hasStatus, hasEvents, hasDiagnosis := false, false, false
	for _, action := range dashboard.Actions {
		hasStatus = hasStatus || action == "dashboard_deployment_status"
		hasEvents = hasEvents || action == "dashboard_deployment_events"
		hasDiagnosis = hasDiagnosis || action == "dashboard_deployment_diagnosis"
	}
	if !hasStatus || !hasEvents || !hasDiagnosis {
		t.Fatalf("catalog actions = %v, want Dashboard status, events, and diagnosis", dashboard.Actions)
	}
}

func TestServiceBundleRootUIHasAccessibleManagementControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, csrf: "csrf-token", host: "127.0.0.1:1234"}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/?bundle_id=service:0123456789abcdef0123456789abcdef", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	for _, expected := range []string{
		"Connection groups", `for="service-bundle-name"`, `name="github_target_ids"`, `name="environment_name"`,
		`name="environment_jenkins_id"`, `name="environment_harbor_id"`, `name="environment_dashboard_id"`,
		`name="environment_dashboard_namespace"`, `name="environment_dashboard_deployment"`,
		`aria-label="Remove environment qa-blue"`, `id="add-environment"`, `value="csrf-token"`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("service bundle UI missing %q", expected)
		}
	}
	if got := response.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'nonce-") {
		t.Fatalf("inline environment controls are missing their CSP nonce: %q", got)
	}
	wrappedLabels := strings.Contains(response.Body.String(), "<label>Environment name<input")
	if !wrappedLabels {
		t.Fatal("environment inputs do not have programmatic labels")
	}
}

func TestRootCSPNonceIsFreshAndSeparateFromCSRF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	const host = "127.0.0.1:1234"
	a := &app{configPath: path, csrf: "stable-csrf-token", host: host}
	render := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
		return response
	}
	nonce := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		const marker = "script-src 'nonce-"
		csp := response.Header().Get("Content-Security-Policy")
		start := strings.Index(csp, marker)
		if start < 0 {
			t.Fatalf("response CSP has no nonce source: %q", csp)
		}
		rest := csp[start+len(marker):]
		end := strings.IndexByte(rest, '\'')
		if end <= 0 {
			t.Fatalf("response CSP has an invalid nonce source: %q", csp)
		}
		value := rest[:end]
		if !strings.Contains(response.Body.String(), `nonce="`+value+`"`) {
			t.Fatalf("response CSP nonce does not match its script element: %q", csp)
		}
		return value
	}

	first, second := render(), render()
	firstNonce, secondNonce := nonce(first), nonce(second)
	if firstNonce == secondNonce {
		t.Fatal("separate HTML responses reused their CSP nonce")
	}
	csrfField := `name="csrf" value="stable-csrf-token"`
	if !strings.Contains(first.Body.String(), csrfField) || !strings.Contains(second.Body.String(), csrfField) {
		t.Fatal("CSRF form token changed or was omitted across HTML responses")
	}

	nonHTML := httptest.NewRecorder()
	plain := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	})
	a.securityHeaders(plain).ServeHTTP(nonHTML, httptest.NewRequest(http.MethodGet, "http://"+host+"/save", nil))
	if csp := nonHTML.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") || strings.Contains(csp, "nonce-") {
		t.Fatalf("non-HTML response allows inline scripts: %q", csp)
	}
}

func TestRemovingServiceBundleEnvironmentKeepsKeyboardFocus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless focus fixture")
	}

	html, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	const marker = `<script nonce="{{.CSPNonce}}">`
	start := strings.Index(string(html), marker)
	if start < 0 {
		t.Fatal("inline environment script not found")
	}
	start += len(marker)
	end := strings.Index(string(html[start:]), "</script>")
	if end < 0 {
		t.Fatal("inline environment script end not found")
	}
	script := string(html[start : start+end])

	fixture := `
const focusable = () => ({ focused: false, focus() { this.focused = true; } });
const addButton = focusable();
addButton.addEventListener = () => {};
let clickHandler;
let currentRow;
const listElement = { addEventListener(_type, handler) { clickHandler = handler; } };
const mockLanguageSelect = { value: 'en', addEventListener() {} };
const mockSubmitButton = { disabled: false, addEventListener() {} };
const mockFileInput = { addEventListener() {} };
const mockImportForm = { addEventListener() {}, querySelector() { return mockSubmitButton; } };
global.NodeFilter = { SHOW_TEXT: 4 };
global.localStorage = { getItem() { return null; }, setItem() {} };
const inertImporterElements = new Map([
  ['git-import-form', mockImportForm],
  ['git-config-file', mockFileInput],
  ['git-import-status', {}],
  ['git-import-preview', {}],
  ['git-import-heading', {}],
  ['runbook-import-form', mockImportForm],
  ['runbook-file', mockFileInput],
  ['runbook-import-status', {}],
  ['runbook-import-preview', {}],
  ['runbook-import-heading', {}],
  ['ssh-import-form', mockImportForm],
  ['ssh-config-file', mockFileInput],
  ['ssh-import-status', {}],
  ['ssh-import-preview', {}],
  ['ssh-import-heading', {}],
  ['settings-import-form', mockImportForm],
  ['settings-json-file', mockFileInput],
  ['settings-import-status', {}],
  ['settings-import-preview', {}],
  ['settings-import-heading', {}],
  ['json-remote-import-form', mockImportForm],
  ['json-remote-file', mockFileInput],
  ['json-remote-import-status', {}],
  ['json-remote-import-preview', {}],
]);
const removeButton = {
  closest(selector) { return selector === '.environment-row' ? currentRow : null; }
};
global.document = {
  cookie: '',
  documentElement: {},
  createTreeWalker() { return { nextNode() { return null; } }; },
  getElementById(id) {
    if (id === 'language-select') return mockLanguageSelect;
    if (id === 'service-environments') return listElement;
    if (id === 'add-environment') return addButton;
    if (id === 'dashboard-diagnosis-form') return null;
    if (/^client-(codex|claude|gemini)-(server-listed|tools-visible|catalog-response)$/.test(id)) return {checked: false, addEventListener() {}};
    if (/^client-(codex|claude|gemini)-progress-reset$/.test(id)) return {addEventListener() {}};
    if (/^client-(codex|claude|gemini)-progress-status$/.test(id)) return {textContent: ''};
    if (inertImporterElements.has(id)) return inertImporterElements.get(id);
    throw new Error('unexpected element: ' + id);
  }
};
function peerRow(input) {
  return {
    tabIndex: -1,
    focus() { if (this.tabIndex >= 0) this.focused = true; },
    querySelector(selector) {
      return selector === 'input[name="environment_name"]' ? input : null;
    }
  };
}
function verify(next, previous, expected, label) {
  expected.focused = false;
  currentRow = {
    nextElementSibling: next,
    previousElementSibling: previous,
    removed: false,
    remove() { this.removed = true; }
  };
  clickHandler({target: {closest(selector) {
    return selector === '.remove-environment' ? removeButton : null;
  }}});
  if (!currentRow.removed || !expected.focused) throw new Error(label + ': focus was lost');
}
`
	checks := `
const nextInput = focusable();
const previousInput = focusable();
verify(peerRow(nextInput), null, nextInput, 'next environment');
verify(null, peerRow(previousInput), previousInput, 'previous environment');
verify(null, null, addButton, 'last environment');
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture + script + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environment removal focus fixture failed: %v\n%s", err, output)
	}
}

func TestServiceBundleConfigRejectsStaleReferenceOnRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	cfg.ServiceBundles[0].GitHubTargetIDs[0] = "github:ffffffffffffffffffffffffffffffff"
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&app{configPath: path}).registeredServiceBundles(); err == nil {
		t.Fatal("catalog accepted stale reference")
	}
}

func TestMCPRegisteredTargetsRemainsCompatibleWithServiceBundles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	result, err := (&app{configPath: path}).registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 5 || result.Targets[0].Type != "dashboard" || result.Targets[0].Name != "Cluster UI" {
		t.Fatalf("registered target catalog changed: %+v", result)
	}
	if !reflect.DeepEqual(result.Targets[0].Actions, []string{"dashboard_namespaces", "registered_target_connection_test"}) {
		t.Fatalf("target-level Dashboard actions = %v; bundle-scoped status action must stay on the service-bundle catalog", result.Targets[0].Actions)
	}
}

func TestServiceBundleValidationRejectsDuplicateBundleID(t *testing.T) {
	cfg := serviceBundleConfig()
	cfg.ServiceBundles = append(cfg.ServiceBundles, serviceBundle{
		ID:              cfg.ServiceBundles[0].ID,
		Name:            "Another service",
		GitHubTargetIDs: []string{cfg.GitHubTargets[0].ID},
	})
	if err := validateConfig(cfg); err == nil {
		t.Fatal("duplicate service bundle ID was accepted")
	}
}
