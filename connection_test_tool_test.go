package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRegisteredTargetConnectionTestMCPUsesSavedTargets(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{
		cfg.GitHubTargets[0].SecretRef:    "github-connection-canary",
		cfg.JenkinsTargets[0].SecretRef:   "jenkins-connection-canary",
		cfg.HarborTargets[0].SecretRef:    "harbor-connection-canary",
		cfg.DashboardTargets[0].SecretRef: "dashboard-connection-canary",
	}
	secrets := &connectionTestToolSecrets{values: tokens}
	requests := make(map[string]int)
	var requestCount int
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("connection test used %s, want GET", r.Method)
		}
		requests[r.URL.Host]++
		requestCount++
		var body string
		switch r.URL.Host {
		case "github.example.invalid":
			if got := r.Header.Get("Authorization"); got != "Bearer "+tokens[cfg.GitHubTargets[0].SecretRef] {
				t.Errorf("GitHub authorization = %q", got)
			}
			body = `{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main","html_url":"https://github.example.invalid/ops/agent"}`
		case "jenkins.example.invalid":
			username, password, ok := r.BasicAuth()
			if !ok || username != cfg.JenkinsTargets[0].Username || password != tokens[cfg.JenkinsTargets[0].SecretRef] {
				t.Errorf("Jenkins basic auth did not use the saved target credential")
			}
			body = `{"name":"build","fullName":"folder/build","buildable":true}`
		case "harbor.example.invalid":
			username, password, ok := r.BasicAuth()
			if !ok || username != cfg.HarborTargets[0].Username || password != tokens[cfg.HarborTargets[0].SecretRef] {
				t.Errorf("Harbor basic auth did not use the saved target credential")
			}
			body = `[]`
		case "dashboard.example.invalid":
			if got := r.Header.Get("Authorization"); got != "Bearer "+tokens[cfg.DashboardTargets[0].SecretRef] {
				t.Errorf("Dashboard authorization = %q", got)
			}
			body = `{"listMeta":{"totalItems":0},"namespaces":[]}`
		default:
			t.Errorf("connection test contacted unexpected host %q", r.URL.Host)
			return nil, fmt.Errorf("unexpected mock host")
		}
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	a := &app{configPath: path, secrets: secrets, client: client}
	targets, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := a.registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ adapter, name string }{
		{adapter: "github", name: cfg.GitHubTargets[0].Name},
		{adapter: "jenkins", name: cfg.JenkinsTargets[0].Name},
		{adapter: "harbor", name: cfg.HarborTargets[0].Name},
		{adapter: "dashboard", name: cfg.DashboardTargets[0].Name},
	} {
		targetAdvertised := false
		for _, registered := range targets.Targets {
			if registered.Type == tc.adapter && registered.Name == tc.name && slices.Contains(registered.Actions, "registered_target_connection_test") {
				targetAdvertised = true
			}
		}
		if !targetAdvertised {
			t.Errorf("registered_targets omitted connection test for %s target %q", tc.adapter, tc.name)
		}
	}
	for _, environment := range bundles.Bundles[0].Environments {
		if environment.Jenkins != nil && !slices.Contains(environment.Jenkins.Actions, "registered_target_connection_test") {
			t.Error("service bundle omitted the Jenkins connection test action")
		}
		if environment.Harbor != nil && !slices.Contains(environment.Harbor.Actions, "registered_target_connection_test") {
			t.Error("service bundle omitted the Harbor connection test action")
		}
		if environment.Dashboard != nil && !slices.Contains(environment.Dashboard.Actions, "registered_target_connection_test") {
			t.Error("service bundle omitted the Dashboard connection test action")
		}
	}
	if !slices.Contains(bundles.Bundles[0].Repositories[0].Actions, "registered_target_connection_test") {
		t.Error("service bundle omitted the GitHub connection test action")
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "connection-test-tool-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	cases := []struct {
		name    string
		adapter string
		target  string
		host    string
		id      string
	}{
		{name: "github", adapter: "github", target: cfg.GitHubTargets[0].Name, host: "github.example.invalid", id: cfg.GitHubTargets[0].ID},
		{name: "jenkins", adapter: "jenkins", target: cfg.JenkinsTargets[0].Name, host: "jenkins.example.invalid", id: cfg.JenkinsTargets[0].ID},
		{name: "harbor", adapter: "harbor", target: cfg.HarborTargets[0].Name, host: "harbor.example.invalid", id: cfg.HarborTargets[0].ID},
		{name: "dashboard", adapter: "dashboard", target: cfg.DashboardTargets[0].Name, host: "dashboard.example.invalid", id: cfg.DashboardTargets[0].ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callRegisteredTargetConnectionTest(t, ctx, clientSession, map[string]any{
				"adapter": tc.adapter,
				"target":  tc.target,
			})
			if result.Adapter != tc.adapter || result.Target != tc.target || result.Result != "success" ||
				result.CompletedAt == "" || result.Message != "Connection test succeeded. Historical status saved." {
				t.Fatalf("connection test result = %+v", result)
			}
			completed, err := time.Parse(time.RFC3339Nano, result.CompletedAt)
			if err != nil || completed.Location() != time.UTC {
				t.Fatalf("completion time %q is not valid UTC: %v", result.CompletedAt, err)
			}
			if requests[tc.host] != 1 {
				t.Fatalf("requests to %s = %d, want exactly one", tc.host, requests[tc.host])
			}
			stored, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if history := stored.ConnectionTests[tc.id]; history.Result != "success" || history.CompletedAt != result.CompletedAt {
				t.Fatalf("saved history = %+v, want the successful tool result time", history)
			}
		})
	}

	if secrets.loads != 4 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("credential store calls = %+v, want four reads and no writes", secrets)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stored.ConnectionTests)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{
		"cred:", "github-connection-canary", "jenkins-connection-canary",
		"harbor-connection-canary", "dashboard-connection-canary", ".example.invalid",
	} {
		if strings.Contains(string(encoded), privateValue) {
			t.Errorf("saved connection history exposed %q: %s", privateValue, encoded)
		}
	}

	beforeRequests := requestCount
	beforeLoads := secrets.loads
	rejected := []struct {
		name string
		args map[string]any
	}{
		{name: "unsupported adapter", args: map[string]any{"adapter": "other", "target": "Engineering"}},
		{name: "unregistered target", args: map[string]any{"adapter": "github", "target": "unknown-canary"}},
		{name: "padded target name", args: map[string]any{"adapter": "github", "target": " Engineering "}},
		{name: "arbitrary endpoint argument", args: map[string]any{"adapter": "github", "target": "Engineering", "url": "https://endpoint-canary.invalid"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			response, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_target_connection_test", Arguments: tc.args})
			if err == nil && (response == nil || !response.IsError) {
				t.Fatalf("invalid connection-test input was accepted: %+v", response)
			}
			encoded, _ := json.Marshal(response)
			if strings.Contains(string(encoded), "endpoint-canary.invalid") || strings.Contains(string(encoded), "unknown-canary") {
				t.Fatalf("MCP error echoed an untrusted value: %s", encoded)
			}
		})
	}
	if requestCount != beforeRequests || secrets.loads != beforeLoads {
		t.Fatalf("rejected inputs caused side effects: requests=%v secret loads=%d", requests, secrets.loads)
	}

	cfg.HarborTargets[0].Disabled = true
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	response, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "registered_target_connection_test",
		Arguments: map[string]any{"adapter": "harbor", "target": cfg.HarborTargets[0].Name},
	})
	if err == nil && (response == nil || !response.IsError) {
		t.Fatalf("disabled target was accepted: %+v", response)
	}
	if requestCount != beforeRequests || secrets.loads != beforeLoads {
		t.Fatalf("disabled target caused side effects: requests=%v secret loads=%d", requests, secrets.loads)
	}
}

func TestRegisteredTargetConnectionTestFailureIsFixedAndSecretFree(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	const token = "connection-test-mcp-token-canary"
	secrets := &connectionTestToolSecrets{values: map[string]string{cfg.GitHubTargets[0].SecretRef: token}}
	var calls int
	a := &app{
		configPath: path,
		secrets:    secrets,
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return harborTestResponse(r, http.StatusUnauthorized, "401-body-canary "+token), nil
		})},
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "missing-credential-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result := callRegisteredTargetConnectionTest(t, ctx, clientSession, map[string]any{
		"adapter": "github",
		"target":  cfg.GitHubTargets[0].Name,
	})
	if result.Result != "failure" || result.CompletedAt == "" || !strings.Contains(result.Message, "401") ||
		!strings.Contains(result.Message, "Historical status saved.") {
		t.Fatalf("connection failure result = %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{token, "401-body-canary", ".example.invalid", "cred:"} {
		if strings.Contains(string(encoded), privateValue) {
			t.Errorf("failure result exposed %q: %s", privateValue, encoded)
		}
	}
	if calls != 1 || secrets.loads != 1 {
		t.Fatalf("failure path made %d requests and %d secret reads", calls, secrets.loads)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if history := stored.ConnectionTests[cfg.GitHubTargets[0].ID]; history.Result != "failure" || history.CompletedAt != result.CompletedAt {
		t.Fatalf("saved failure history = %+v", history)
	}
}

func TestRegisteredTargetConnectionTestGitHub403RateLimitIsFixedAndSecretFree(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	const token = "github-rate-limit-token-canary"
	const responseCanary = "github-rate-limit-response-canary"
	const headerCanary = "github-rate-limit-header-canary"
	secrets := &connectionTestToolSecrets{values: map[string]string{cfg.GitHubTargets[0].SecretRef: token}}
	var calls int
	a := &app{
		configPath: path,
		secrets:    secrets,
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			response := harborTestResponse(r, http.StatusForbidden, responseCanary+" "+token)
			response.Header.Set("X-RateLimit-Remaining", "0")
			response.Header.Set("X-GitHub-Request-Id", headerCanary)
			return response, nil
		})},
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "github-rate-limit-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result := callRegisteredTargetConnectionTest(t, ctx, clientSession, map[string]any{
		"adapter": "github",
		"target":  cfg.GitHubTargets[0].Name,
	})
	wantMessage := connectionFailureMessage(connectionFailureRateLimit) + " Historical status saved."
	if result.Result != "failure" || result.CompletedAt == "" || result.Message != wantMessage {
		t.Fatalf("GitHub 403 rate-limit result = %+v, want fixed rate-limit guidance", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{
		token,
		responseCanary,
		headerCanary,
		cfg.GitHubTargets[0].Origin,
		cfg.GitHubTargets[0].SecretRef,
		"X-RateLimit-Remaining",
	} {
		if strings.Contains(string(encoded), privateValue) {
			t.Errorf("GitHub 403 rate-limit result exposed %q: %s", privateValue, encoded)
		}
	}
	if calls != 1 || secrets.loads != 1 {
		t.Fatalf("GitHub 403 rate-limit path made %d requests and %d secret reads", calls, secrets.loads)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	history := stored.ConnectionTests[cfg.GitHubTargets[0].ID]
	if history.Result != "failure" || history.CompletedAt != result.CompletedAt || !validConnectionTest(history) {
		t.Fatalf("GitHub 403 rate-limit history = %+v", history)
	}
}

func TestRegisteredTargetConnectionTestTimeoutIsFixedAndSecretFree(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	const token = "connection-test-timeout-token-canary"
	const responseBodyCanary = "connection-test-timeout-body-canary"
	secrets := &connectionTestToolSecrets{values: map[string]string{cfg.GitHubTargets[0].SecretRef: token}}
	var calls int
	a := &app{
		configPath: path,
		secrets:    secrets,
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, fmt.Errorf(
				"transport timeout to %s with Authorization %q and response body %q: %w",
				r.URL.String(),
				r.Header.Get("Authorization"),
				responseBodyCanary,
				context.DeadlineExceeded,
			)
		})},
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "timeout-connection-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result := callRegisteredTargetConnectionTest(t, ctx, clientSession, map[string]any{
		"adapter": "github",
		"target":  cfg.GitHubTargets[0].Name,
	})
	const wantMessage = "Connection test failed: the request timed out. Check network or VPN access and retry. Historical status saved."
	if result.Result != "failure" || result.CompletedAt == "" || result.Message != wantMessage {
		t.Fatalf("connection timeout result = %+v, want failure with saved history and fixed message", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{
		token,
		responseBodyCanary,
		cfg.GitHubTargets[0].Origin,
		cfg.GitHubTargets[0].SecretRef,
		"Authorization",
		"Bearer",
		"connection-test-timeout",
	} {
		if strings.Contains(string(encoded), privateValue) {
			t.Errorf("connection timeout result exposed %q: %s", privateValue, encoded)
		}
	}
	if calls != 1 || secrets.loads != 1 {
		t.Fatalf("timeout path made %d requests and %d secret reads", calls, secrets.loads)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	history := stored.ConnectionTests[cfg.GitHubTargets[0].ID]
	if history.Result != "failure" || history.CompletedAt != result.CompletedAt || !validConnectionTest(history) {
		t.Fatalf("saved timeout history = %+v", history)
	}
}

func TestRegisteredTargetConnectionTestMissingCredentialDoesNotAttemptOrWriteHistory(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &connectionTestToolSecrets{values: map[string]string{}}
	var requests int
	a := &app{
		configPath: path,
		secrets:    secrets,
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests++
			return harborTestResponse(r, http.StatusOK, "{}"), nil
		})},
	}
	result, err := a.testRegisteredTargetConnection(context.Background(), registeredTargetConnectionTestInput{
		Adapter: "github",
		Target:  cfg.GitHubTargets[0].Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "not_tested" || result.CompletedAt != "" || result.Message != "Connection test was not run. Check the saved target and credential settings." {
		t.Fatalf("missing-credential result = %+v", result)
	}
	if requests != 0 || secrets.loads != 1 || secrets.saves != 0 {
		t.Fatalf("missing credential caused requests=%d and secret-store calls=%+v", requests, secrets)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ConnectionTests) != 0 {
		t.Fatalf("missing credential wrote connection history: %+v", stored.ConnectionTests)
	}
}

func TestRegisteredTargetConnectionTestDropsStaleHistory(t *testing.T) {
	cfg := serviceBundleConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	const token = "connection-test-stale-token-canary"
	secrets := &connectionTestToolSecrets{values: map[string]string{cfg.GitHubTargets[0].SecretRef: token}}
	a := &app{configPath: path, secrets: secrets, host: "127.0.0.1:43127"}
	a.client = &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		current, err := readConfig(path)
		if err != nil {
			return nil, err
		}
		current.GitHubTargets[0].Repository = "ops/changed-during-test"
		if err := writeConfig(path, current); err != nil {
			return nil, err
		}
		body := `{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main","html_url":"https://github.example.invalid/ops/agent"}`
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	result, err := a.testRegisteredTargetConnection(context.Background(), registeredTargetConnectionTestInput{
		Adapter: "github",
		Target:  cfg.GitHubTargets[0].Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "success" || result.CompletedAt != "" || !strings.Contains(result.Message, "history could not be saved") {
		t.Fatalf("stale test result = %+v", result)
	}
	stored, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := stored.ConnectionTests[cfg.GitHubTargets[0].ID]; exists {
		t.Fatal("test history was attributed to an edited target")
	}
}

func callRegisteredTargetConnectionTest(t *testing.T, ctx context.Context, session *mcp.ClientSession, args map[string]any) registeredTargetConnectionTestResult {
	t.Helper()
	response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_target_connection_test", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if response.IsError {
		t.Fatalf("connection test tool failed: %+v", response)
	}
	encoded, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result registeredTargetConnectionTestResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	fullResponse, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullResponse) > 4096 {
		t.Fatalf("connection test response is %d bytes, above its small result contract", len(fullResponse))
	}
	for _, privateValue := range []string{"cred:", ".example.invalid", "connection-canary"} {
		if strings.Contains(string(fullResponse), privateValue) {
			t.Fatalf("connection test response exposed %q: %s", privateValue, fullResponse)
		}
	}
	return result
}

type connectionTestToolSecrets struct {
	values       map[string]string
	loads, saves int
	deletes      int
}

func (s *connectionTestToolSecrets) Save(string, []byte) error {
	s.saves++
	return nil
}

func (s *connectionTestToolSecrets) Load(ref string) ([]byte, error) {
	s.loads++
	value, ok := s.values[ref]
	if !ok {
		return nil, http.ErrNoCookie
	}
	return []byte(value), nil
}

func (s *connectionTestToolSecrets) Delete(string) error {
	s.deletes++
	return nil
}

var _ secretStore = (*connectionTestToolSecrets)(nil)
