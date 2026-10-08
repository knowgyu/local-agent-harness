package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectionHistoryConfig() config {
	cfg := serviceBundleConfig()
	when := time.Date(2026, 9, 26, 4, 5, 6, 0, time.UTC)
	cfg.ConnectionTests = map[string]connectionTest{}
	for i, id := range []string{cfg.GitHubTargets[0].ID, cfg.GitHubTargets[1].ID, cfg.JenkinsTargets[0].ID, cfg.HarborTargets[0].ID, cfg.DashboardTargets[0].ID} {
		result := "success"
		if i%2 == 1 {
			result = "failure"
		}
		cfg.ConnectionTests[id] = connectionTest{Result: result, CompletedAt: when.Format(time.RFC3339Nano)}
	}
	return cfg
}

func TestMigrateV3ConnectionHistoryConfigAndStrictSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	want := configV3{Version: 3, GitHubTargets: cfg.GitHubTargets, JenkinsTargets: cfg.JenkinsTargets, HarborTargets: cfg.HarborTargets, DashboardTargets: cfg.DashboardTargets, ServiceBundles: cfg.ServiceBundles}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v3: %v", err)
	}
	got, err := readConfig(path)
	if err != nil || got.Version != configVersion || len(got.PythonTasks) != 0 || len(got.ConnectionTests) != 0 || !reflect.DeepEqual(got.GitHubTargets, want.GitHubTargets) || !reflect.DeepEqual(got.ServiceBundles, want.ServiceBundles) {
		t.Fatalf("v3 migration lost target data or added history: version=%d history=%v err=%v", got.Version, got.ConnectionTests, err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["connection_tests"] = json.RawMessage(`{}`)
	bad, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err == nil {
		t.Fatal("v3 migration accepted an unknown v4 field")
	}
	remaining, err := os.ReadFile(path)
	if err != nil || string(remaining) != string(bad) {
		t.Fatalf("rejected v3 config was rewritten: err=%v", err)
	}
}

func TestConnectionHistoryUIAndCatalogsAreHistoricalAndSecretFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := connectionHistoryConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	var networkCalls atomic.Int32
	a := &app{configPath: path, csrf: "csrf", client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		networkCalls.Add(1)
		return harborTestResponse(r, http.StatusOK, `{}`), nil
	})}}
	for _, tc := range []struct {
		query  string
		result string
	}{{"github_id=" + cfg.GitHubTargets[0].ID, "success"}, {"jenkins_id=" + cfg.JenkinsTargets[0].ID, "success"}, {"harbor_id=" + cfg.HarborTargets[0].ID, "failure"}, {"dashboard_id=" + cfg.DashboardTargets[0].ID, "success"}} {
		response := httptest.NewRecorder()
		a.handleRoot(response, httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil))
		body := response.Body.String()
		history := "Previous connection test (historical; reading this status does not contact the service): " + tc.result + " · 2026-09-26T04:05:06Z (UTC)"
		if response.Code != http.StatusOK || !strings.Contains(body, history) ||
			!strings.Contains(body, `data-history-result="`+tc.result+`"`) {
			t.Fatalf("UI omitted selected target history for %q: status=%d", tc.query, response.Code)
		}
		if strings.Contains(response.Body.String(), "cred:") || strings.Contains(response.Body.String(), "connection_history_secret_canary") {
			t.Fatalf("UI exposed a secret reference or credential for %q", tc.query)
		}
	}

	cfg.ConnectionTests[cfg.GitHubTargets[0].ID] = connectionTest{Result: "unknown", CompletedAt: "not-a-time"}
	delete(cfg.ConnectionTests, cfg.GitHubTargets[1].ID)
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.handleRoot(response, httptest.NewRequest(http.MethodGet, "/?github_id="+cfg.GitHubTargets[0].ID, nil))
	if !strings.Contains(response.Body.String(), "Previous connection test (historical; reading this status does not contact the service): not tested") ||
		!strings.Contains(response.Body.String(), `data-history-result="not tested"`) {
		t.Fatal("missing or invalid test history was not shown as not tested")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "connection-history-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	for _, tool := range []string{"registered_targets", "registered_service_bundles"} {
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: tool})
		if err != nil || result.IsError {
			t.Fatalf("%s failed: result=%+v err=%v", tool, result, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		data := string(encoded)
		for _, value := range []string{`"result":"success"`, `"result":"failure"`, `"result":"not_tested"`, `2026-09-26T04:05:06Z`} {
			if !strings.Contains(data, value) {
				t.Fatalf("%s omitted connection history %q: %s", tool, value, data)
			}
		}
		for _, secret := range []string{"cred:", "connection_history_secret_canary", ".example.invalid"} {
			if strings.Contains(data, secret) {
				t.Fatalf("%s exposed %q", tool, secret)
			}
		}
	}
	if networkCalls.Load() != 0 {
		t.Fatalf("catalog read triggered %d network calls", networkCalls.Load())
	}
	stored, err := readConfig(path)
	if err != nil || jenkinsTriggerApproved(stored.JenkinsTargets[0]) {
		t.Fatalf("catalog status granted Jenkins trigger preapproval: err=%v target=%+v", err, stored.JenkinsTargets[0])
	}
}

func TestConnectionTestRequestsRecordResultsAndDropEditedTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	const canary = "connection_history_secret_canary"
	secrets := memorySecrets{
		cfg.GitHubTargets[0].SecretRef:    []byte(canary),
		cfg.JenkinsTargets[0].SecretRef:   []byte(canary),
		cfg.HarborTargets[0].SecretRef:    []byte(canary),
		cfg.DashboardTargets[0].SecretRef: []byte(canary),
	}
	failRequests, editDuringGitHubRequest := false, false
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if failRequests {
			return harborTestResponse(r, http.StatusServiceUnavailable, `{"error":"`+canary+`"}`), nil
		}
		switch r.URL.Host {
		case "github.example.invalid":
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			fullName := strings.Join(parts[len(parts)-2:], "/")
			body, _ := json.Marshal(repository{Name: parts[len(parts)-1], FullName: fullName, DefaultBranch: "main", HTMLURL: "https://github.example.invalid/" + fullName})
			if editDuringGitHubRequest {
				current, err := readConfig(path)
				if err != nil {
					return nil, err
				}
				current.GitHubTargets[0].Repository = "ops/changed-during-test"
				clearConnectionTest(&current, cfg.GitHubTargets[0].ID)
				if err := writeConfig(path, current); err != nil {
					return nil, err
				}
			}
			return harborTestResponse(r, http.StatusOK, string(body)), nil
		case "jenkins.example.invalid":
			return harborTestResponse(r, http.StatusOK, `{"name":"build","fullName":"folder/build","buildable":true}`), nil
		case "harbor.example.invalid":
			return harborTestResponse(r, http.StatusOK, `[]`), nil
		case "dashboard.example.invalid":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"namespaces":[]}`), nil
		default:
			return nil, context.Canceled
		}
	})}
	a := &app{configPath: path, secrets: secrets, client: client, host: "127.0.0.1:43127", csrf: "csrf"}
	targets := []struct {
		kind, id, name string
		handler        func(http.ResponseWriter, *http.Request)
	}{{"github", cfg.GitHubTargets[0].ID, cfg.GitHubTargets[0].Name, a.handleTest}, {"jenkins", cfg.JenkinsTargets[0].ID, cfg.JenkinsTargets[0].Name, a.handleJenkinsTest}, {"harbor", cfg.HarborTargets[0].ID, cfg.HarborTargets[0].Name, a.handleHarborTest}, {"dashboard", cfg.DashboardTargets[0].ID, cfg.DashboardTargets[0].Name, a.handleDashboardTest}}
	post := func(tc struct {
		kind, id, name string
		handler        func(http.ResponseWriter, *http.Request)
	}) {
		form := url.Values{"csrf": {a.csrf}, "target_id": {tc.id}, "target": {tc.name}}
		request := httptest.NewRequest(http.MethodPost, "/test-"+tc.kind, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		tc.handler(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("%s test handler returned %d", tc.kind, response.Code)
		}
	}
	for _, tc := range targets {
		post(tc)
	}
	stored, err := readConfig(path)
	if err != nil || len(stored.ConnectionTests) != 4 {
		t.Fatalf("successful mock tests were not recorded: history=%v err=%v", stored.ConnectionTests, err)
	}
	for id, test := range stored.ConnectionTests {
		if test.Result != "success" || !validConnectionTest(test) {
			t.Fatalf("invalid success record %s: %+v", id, test)
		}
	}
	failRequests = true
	for _, tc := range targets {
		post(tc)
	}
	stored, err = readConfig(path)
	if err != nil || len(stored.ConnectionTests) != 4 {
		t.Fatalf("failed mock tests were not recorded: history=%v err=%v", stored.ConnectionTests, err)
	}
	for id, test := range stored.ConnectionTests {
		if test.Result != "failure" {
			t.Fatalf("failed request stored non-failure state for %s: %+v", id, test)
		}
	}

	failRequests = false
	editDuringGitHubRequest = true
	post(targets[0])
	stored, err = readConfig(path)
	if err != nil || stored.GitHubTargets[0].Repository != "ops/changed-during-test" {
		t.Fatalf("mock did not edit checked target: config=%+v err=%v", stored.GitHubTargets, err)
	}
	if _, ok := stored.ConnectionTests[cfg.GitHubTargets[0].ID]; ok {
		t.Fatal("stale connection test was attributed to edited target")
	}
	response := httptest.NewRecorder()
	a.handleRoot(response, httptest.NewRequest(http.MethodGet, "/?github_id="+cfg.GitHubTargets[0].ID, nil))
	if !strings.Contains(response.Body.String(), "history could not be saved. Retest the current target") {
		t.Fatal("stale test result did not ask the user to retest")
	}
	wrongTarget := targets[0]
	wrongTarget.id = cfg.GitHubTargets[1].ID
	post(wrongTarget)
	if !strings.Contains(a.status, "Connection test was not run") {
		t.Fatal("target mismatch did not report that no network test ran")
	}
	statusJSON, err := json.Marshal(stored.ConnectionTests)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statusJSON), canary) || strings.Contains(string(statusJSON), ".example.invalid") || strings.Contains(string(statusJSON), "error") || strings.Contains(string(statusJSON), "secret_ref") {
		t.Fatalf("connection history stored details beyond result and time: %s", statusJSON)
	}
}

func TestSavingTargetClearsOnlyItsConnectionHistory(t *testing.T) {
	tests := []struct {
		name     string
		targetID func(config) string
		save     func(*app, config) error
	}{
		{
			name:     "github",
			targetID: func(cfg config) string { return cfg.GitHubTargets[0].ID },
			save: func(a *app, cfg config) error {
				target := cfg.GitHubTargets[0]
				_, err := a.saveTarget(target.ID, target.Name, target.Origin, target.Repository, "")
				return err
			},
		},
		{
			name:     "jenkins",
			targetID: func(cfg config) string { return cfg.JenkinsTargets[0].ID },
			save: func(a *app, cfg config) error {
				target := cfg.JenkinsTargets[0]
				_, err := a.saveJenkinsTarget(target.ID, target.Name, target.BaseURL, target.Username, target.JobPath, target.Environment, "", false)
				return err
			},
		},
		{
			name:     "harbor",
			targetID: func(cfg config) string { return cfg.HarborTargets[0].ID },
			save: func(a *app, cfg config) error {
				target := cfg.HarborTargets[0]
				_, err := a.saveHarborTarget(target.ID, target.Name, target.BaseURL, target.Username, target.Project, target.Repository, "")
				return err
			},
		},
		{
			name:     "dashboard",
			targetID: func(cfg config) string { return cfg.DashboardTargets[0].ID },
			save: func(a *app, cfg config) error {
				target := cfg.DashboardTargets[0]
				_, err := a.saveDashboardTarget(target.ID, target.Name, target.BaseURL, "")
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := connectionHistoryConfig()
			path := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: path, secrets: memorySecrets{}}
			id := tc.targetID(cfg)
			if err := tc.save(a, cfg); err != nil {
				t.Fatalf("save existing target: %v", err)
			}
			got, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got.ConnectionTests[id]; ok || len(got.ConnectionTests) != len(cfg.ConnectionTests)-1 {
				t.Fatalf("save did not clear exactly edited target history: %+v", got.ConnectionTests)
			}
			for otherID := range cfg.ConnectionTests {
				if otherID == id {
					continue
				}
				if _, ok := got.ConnectionTests[otherID]; !ok {
					t.Fatalf("save cleared unrelated target history %s", otherID)
				}
			}
		})
	}
}

func TestDeletingTargetClearsOnlyItsConnectionHistory(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		targetID func(config) string
	}{
		{"github", "github", func(cfg config) string { return cfg.GitHubTargets[0].ID }},
		{"jenkins", "jenkins", func(cfg config) string { return cfg.JenkinsTargets[0].ID }},
		{"harbor", "harbor", func(cfg config) string { return cfg.HarborTargets[0].ID }},
		{"dashboard", "dashboard", func(cfg config) string { return cfg.DashboardTargets[0].ID }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := connectionHistoryConfig()
			cfg.ServiceBundles = nil
			path := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: path, secrets: memorySecrets{}}
			id := tc.targetID(cfg)
			if err := a.deleteRegisteredTarget(tc.kind, id); err != nil {
				t.Fatalf("delete target: %v", err)
			}
			got, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got.ConnectionTests[id]; ok || len(got.ConnectionTests) != len(cfg.ConnectionTests)-1 {
				t.Fatalf("delete did not clear exactly target history: %+v", got.ConnectionTests)
			}
			for otherID := range cfg.ConnectionTests {
				if otherID == id {
					continue
				}
				if _, ok := got.ConnectionTests[otherID]; !ok {
					t.Fatalf("delete cleared unrelated target history %s", otherID)
				}
			}
		})
	}
}

func TestConnectionTestDiagnosticsAcrossAdapters(t *testing.T) {
	const canary = "connection_diagnostic_secret_canary"
	cases := []struct {
		name              string
		status            int
		headers           http.Header
		transportErr      error
		failureKind       connectionFailureKind
		githubRateLimited bool
	}{
		{
			name:         "unreachable address",
			transportErr: errors.New("transport detail contains " + canary),
			failureKind:  connectionFailureAddress,
		},
		{
			name:         "request deadline",
			transportErr: errors.Join(context.DeadlineExceeded, errors.New("transport detail contains "+canary)),
			failureKind:  connectionFailureTimeout,
		},
		{name: "HTTP request timeout", status: http.StatusRequestTimeout, failureKind: connectionFailureTimeout},
		{name: "redirect", status: http.StatusFound, failureKind: connectionFailureEndpoint},
		{name: "authentication rejected", status: http.StatusUnauthorized, failureKind: connectionFailureAuthentication},
		{name: "access denied", status: http.StatusForbidden, failureKind: connectionFailureAccess},
		{
			name:              "GitHub primary rate limit at 403",
			status:            http.StatusForbidden,
			headers:           http.Header{"X-RateLimit-Remaining": {"0"}},
			failureKind:       connectionFailureAccess,
			githubRateLimited: true,
		},
		{
			name:              "GitHub secondary rate limit at 403",
			status:            http.StatusForbidden,
			headers:           http.Header{"Retry-After": {"60"}},
			failureKind:       connectionFailureAccess,
			githubRateLimited: true,
		},
		{
			name:              "GitHub secondary rate limit HTTP date at 403",
			status:            http.StatusForbidden,
			headers:           http.Header{"Retry-After": {"Wed, 21 Oct 2015 07:28:00 GMT"}},
			failureKind:       connectionFailureAccess,
			githubRateLimited: true,
		},
		{
			name:        "403 with remaining requests stays access denied",
			status:      http.StatusForbidden,
			headers:     http.Header{"X-RateLimit-Remaining": {"1"}},
			failureKind: connectionFailureAccess,
		},
		{
			name:        "ambiguous retry-after headers stay access denied",
			status:      http.StatusForbidden,
			headers:     http.Header{"Retry-After": {"60", "120"}},
			failureKind: connectionFailureAccess,
		},
		{
			name:        "blank retry-after stays access denied",
			status:      http.StatusForbidden,
			headers:     http.Header{"Retry-After": {"  "}},
			failureKind: connectionFailureAccess,
		},
		{
			name:        "malformed retry-after stays access denied",
			status:      http.StatusForbidden,
			headers:     http.Header{"Retry-After": {"later"}},
			failureKind: connectionFailureAccess,
		},
		{name: "endpoint or resource missing", status: http.StatusNotFound, failureKind: connectionFailureEndpoint},
		{name: "rate limited", status: http.StatusTooManyRequests, failureKind: connectionFailureRateLimit},
		{name: "unexpected response", status: http.StatusServiceUnavailable, failureKind: connectionFailureOther},
	}
	kinds := []struct {
		name    string
		handler func(*app, http.ResponseWriter, *http.Request)
	}{
		{name: "github", handler: func(a *app, w http.ResponseWriter, r *http.Request) { a.handleTest(w, r) }},
		{name: "jenkins", handler: func(a *app, w http.ResponseWriter, r *http.Request) { a.handleJenkinsTest(w, r) }},
		{name: "harbor", handler: func(a *app, w http.ResponseWriter, r *http.Request) { a.handleHarborTest(w, r) }},
		{name: "dashboard", handler: func(a *app, w http.ResponseWriter, r *http.Request) { a.handleDashboardTest(w, r) }},
	}

	for _, kind := range kinds {
		for _, tc := range cases {
			t.Run(kind.name+"/"+tc.name, func(t *testing.T) {
				cfg := serviceBundleConfig()
				var targetID, targetName string
				switch kind.name {
				case "github":
					targetID, targetName = cfg.GitHubTargets[0].ID, cfg.GitHubTargets[0].Name
				case "jenkins":
					targetID, targetName = cfg.JenkinsTargets[0].ID, cfg.JenkinsTargets[0].Name
				case "harbor":
					targetID, targetName = cfg.HarborTargets[0].ID, cfg.HarborTargets[0].Name
				case "dashboard":
					targetID, targetName = cfg.DashboardTargets[0].ID, cfg.DashboardTargets[0].Name
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := writeConfig(path, cfg); err != nil {
					t.Fatal(err)
				}
				secrets := memorySecrets{
					cfg.GitHubTargets[0].SecretRef:    []byte(canary),
					cfg.JenkinsTargets[0].SecretRef:   []byte(canary),
					cfg.HarborTargets[0].SecretRef:    []byte(canary),
					cfg.DashboardTargets[0].SecretRef: []byte(canary),
				}
				var requests int
				client := &http.Client{
					Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
						requests++
						if tc.transportErr != nil {
							return nil, tc.transportErr
						}
						response := harborTestResponse(r, tc.status, `{"token":"`+canary+`","Authorization":"Bearer `+canary+`","url":"https://private.example.invalid"}`)
						for name, values := range tc.headers {
							for _, value := range values {
								response.Header.Add(name, value)
							}
						}
						if tc.status == http.StatusFound {
							response.Header.Set("Location", "https://login.example.invalid/")
						}
						return response, nil
					}),
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				}
				testApp := &app{configPath: path, secrets: secrets, client: client, csrf: "csrf", host: "127.0.0.1:43127"}
				form := url.Values{"csrf": {"csrf"}, "target_id": {targetID}, "target": {targetName}}
				request := httptest.NewRequest(http.MethodPost, "/test-"+kind.name, strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				kind.handler(testApp, response, request)

				if response.Code != http.StatusSeeOther {
					t.Fatalf("connection test returned status %d", response.Code)
				}
				wantFailureKind := tc.failureKind
				if kind.name == "github" && tc.githubRateLimited {
					wantFailureKind = connectionFailureRateLimit
				}
				wantMessage := connectionFailureMessage(wantFailureKind) + " Historical status saved."
				if testApp.status != wantMessage {
					t.Fatalf("diagnostic = %q, want %q", testApp.status, wantMessage)
				}
				if strings.Contains(testApp.status, canary) || strings.Contains(testApp.status, "example.invalid") || strings.Contains(testApp.status, "Authorization") || strings.Contains(testApp.status, "Bearer") {
					t.Fatalf("diagnostic exposed response or transport details: %q", testApp.status)
				}
				if requests != 1 {
					t.Fatalf("request count = %d, want 1", requests)
				}
				stored, err := readConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				history := stored.ConnectionTests[targetID]
				if history.Result != "failure" || !validConnectionTest(history) {
					t.Fatalf("failure history = %+v", history)
				}
				encoded, err := json.Marshal(stored.ConnectionTests)
				if err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{canary, "example.invalid", "Authorization", "Bearer", "address", "timeout", "authentication", "access"} {
					if strings.Contains(string(encoded), forbidden) {
						t.Fatalf("history leaked %q: %s", forbidden, encoded)
					}
				}
			})
		}
	}
}

func TestConnectionTransportFailureClassifiesNetworkTimeout(t *testing.T) {
	err := &net.DNSError{IsTimeout: true}
	got := connectionFailureFor(connectionTransportFailure(err, "fixed address message"))
	if got != connectionFailureTimeout {
		t.Fatalf("network timeout classified as %q, want %q", got, connectionFailureTimeout)
	}
}

func TestConnectionTestMessagePreservesOutcomeWhenHistoryNotSaved(t *testing.T) {
	tests := []struct {
		name    string
		attempt connectionTestAttempt
		want    string
	}{
		{
			name:    "not attempted",
			attempt: connectionTestAttempt{},
			want:    "Connection test was not run. Check the saved target and credential settings.",
		},
		{
			name:    "success history unavailable",
			attempt: connectionTestAttempt{attempted: true, succeeded: true},
			want:    "Connection test succeeded, but its history could not be saved. Retest the current target.",
		},
		{
			name: "classified failure history unavailable",
			attempt: connectionTestAttempt{
				attempted: true, historySaved: false, failureKind: connectionFailureAuthentication,
			},
			want: connectionFailureMessage(connectionFailureAuthentication) + " Historical status could not be saved. Retest the current target.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			message, failed := connectionTestMessage(tc.attempt)
			if !failed || message != tc.want {
				t.Fatalf("status lost request outcome: message=%q failed=%v", message, failed)
			}
			if strings.Contains(message, ".example.invalid") || strings.Contains(message, "canary") {
				t.Fatalf("status contains sensitive request detail: %q", message)
			}
		})
	}
}
