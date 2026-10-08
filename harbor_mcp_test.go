package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHarborTargetManagementRedirectsKeepHarborSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	a := &app{configPath: path, secrets: memorySecrets{}, csrf: "csrf-token"}
	var ids []string
	for _, name := range []string{"staging", "production", "development"} {
		id, err := a.saveHarborTarget("", name, "https://harbor.example.invalid", "robot$"+name, "platform", "team/service", "token-"+name)
		if err != nil {
			t.Fatalf("create Harbor target %q: %v", name, err)
		}
		ids = append(ids, id)
	}

	for _, id := range ids {
		form := url.Values{"csrf": {a.csrf}, "kind": {"harbor"}, "target_id": {id}, "disabled": {"yes"}}
		request := httptest.NewRequest(http.MethodPost, "/toggle-target", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		a.handleToggleTarget(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("disable %q status = %d, want %d", id, response.Code, http.StatusSeeOther)
		}
		location, err := url.Parse(response.Header().Get("Location"))
		if err != nil || location.Query().Get("harbor_id") != id || location.Query().Get("github_id") != "" {
			t.Fatalf("disable %q redirect = %q, err=%v", id, response.Header().Get("Location"), err)
		}
		cfg, err := readConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		foundDisabled := false
		for _, target := range cfg.HarborTargets {
			if target.ID == id {
				foundDisabled = target.Disabled
				break
			}
		}
		if !foundDisabled {
			t.Fatalf("target %q was not disabled", id)
		}
	}

	for _, id := range ids {
		form := url.Values{"csrf": {a.csrf}, "kind": {"harbor"}, "target_id": {id}, "confirm_delete": {"yes"}}
		request := httptest.NewRequest(http.MethodPost, "/delete-target", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		a.handleDeleteTarget(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("delete %q status = %d, want %d", id, response.Code, http.StatusSeeOther)
		}
		location, err := url.Parse(response.Header().Get("Location"))
		if err != nil || location.Query().Get("harbor_id") != "new" || location.Query().Get("github_id") != "" {
			t.Fatalf("delete %q redirect = %q, err=%v", id, response.Header().Get("Location"), err)
		}
	}

	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HarborTargets) != 0 {
		t.Fatalf("Harbor targets remain after deleting all selections: %#v", cfg.HarborTargets)
	}
}

func TestHarborProjectQuotaMCPUsesOnlyRegisteredTarget(t *testing.T) {
	const secret = "harbor_quota_mcp_canary_0123456789"
	ref := "cred:0123456789abcdef0123456789abcdef"
	target := harborTarget{
		ID: "harbor:0123456789abcdef0123456789abcdef", Name: "staging",
		BaseURL: "https://harbor.example.invalid/proxy", Username: "robot$staging",
		Project: "platform", Repository: "team/service", SecretRef: ref,
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, HarborTargets: []harborTarget{target}}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/proxy/api/v2.0/projects/platform/summary" || r.URL.RawQuery != "" {
			t.Errorf("quota request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		if user, password, ok := r.BasicAuth(); !ok || user != target.Username || password != secret {
			t.Errorf("Basic auth = (%q, %q, %v)", user, password, ok)
		}
		return harborTestResponse(r, http.StatusOK, `{"project_id":7,"repo_count":2,"quota":{"hard":{"storage":1000,"count":20},"used":{"storage":37,"count":3}},"debug":"`+secret+`"}`), nil
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
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "harbor-quota-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	catalog, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	if err != nil {
		t.Fatal(err)
	}
	catalogJSON, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalogJSON), "harbor_project_quota") {
		t.Fatalf("Harbor catalog did not advertise quota action: %s", catalogJSON)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "harbor_project_quota", Arguments: map[string]any{"target": target.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"target":"staging"`, `"project":"platform"`, `"hard":{"count":20,"storage":1000}`, `"used":{"count":3,"storage":37}`} {
		if !strings.Contains(string(encoded), expected) {
			t.Errorf("quota result missing %s: %s", expected, encoded)
		}
	}
	if result.IsError || strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), `"debug"`) {
		t.Fatalf("unexpected or unprojected quota result: %s", encoded)
	}

	wrongTarget, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "harbor_project_quota", Arguments: map[string]any{"target": "Staging"},
	})
	if err == nil && !wrongTarget.IsError {
		t.Fatalf("MCP accepted non-exact Harbor target: %#v", wrongTarget)
	}
	withProject, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "harbor_project_quota", Arguments: map[string]any{"target": target.Name, "project": "other"},
	})
	if err == nil && !withProject.IsError {
		t.Fatalf("MCP accepted caller-supplied project: %#v", withProject)
	}
	if requests != 1 {
		t.Fatalf("quota HTTP requests = %d, want 1", requests)
	}
}
