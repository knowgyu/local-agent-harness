package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type readinessObserverFake struct {
	observation ResidentEndpointObservation
	calls       int
}

func (f *readinessObserverFake) ObserveResidentEndpoint(context.Context) ResidentEndpointObservation {
	f.calls++
	return f.observation
}

type readinessRegistrationFake struct {
	statuses map[string]MCPClientStatus
	errors   map[string]error
	calls    []string
}

func (f *readinessRegistrationFake) InspectClient(_ context.Context, clientID string) (MCPClientStatus, error) {
	f.calls = append(f.calls, clientID)
	if err := f.errors[clientID]; err != nil {
		return MCPClientStatus{}, err
	}
	return f.statuses[clientID], nil
}

func (f *readinessRegistrationFake) PlanClientRegistration(context.Context, MCPRegistrationSpec) (MCPClientRegistrationPlan, error) {
	return MCPClientRegistrationPlan{}, nil
}

func (f *readinessRegistrationFake) BackupClientRegistration(context.Context, string) (MCPClientBackupReceipt, error) {
	return MCPClientBackupReceipt{}, nil
}

func (f *readinessRegistrationFake) ApplyClientRegistration(context.Context, string, string) (MCPClientStatus, error) {
	return MCPClientStatus{}, nil
}

func (f *readinessRegistrationFake) VerifyClientRegistration(context.Context, string) (MCPClientStatus, error) {
	return MCPClientStatus{}, nil
}

func (f *readinessRegistrationFake) RestoreClientRegistration(context.Context, string, string) (MCPClientStatus, error) {
	return MCPClientStatus{}, nil
}

func TestReadinessStatusDefaultGetIsPassiveAndEmptyTargetsNeedSetup(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	before := readReadinessFile(t, configPath)
	observer := &readinessObserverFake{observation: ResidentEndpointObservation{
		State:      ResidentEndpointResponding,
		ObservedAt: time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC),
	}}
	registrations := &readinessRegistrationFake{}
	handler := newReadinessStatusHandler(configPath, registrations, observer)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response readinessStatusDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if response.State != "needs_setup" || len(response.Targets) != 0 {
		t.Fatalf("empty config projection = %q with %d targets; want needs_setup and none", response.State, len(response.Targets))
	}
	if response.ConfigurationRev == "" || response.ResidentEndpoint.State != "responding" || response.ResidentEndpoint.ObservedAt == nil {
		t.Fatalf("missing opaque revision or resident observation: %+v", response)
	}
	if response.UserEnvironment.State != "not_checked" || response.UserEnvironment.Count != nil {
		t.Fatalf("overview read current user environment: %+v", response.UserEnvironment)
	}
	for _, client := range response.Clients {
		if client.Inspection != "not_checked" || client.Installed != "not_checked" || client.Registration != "not_checked" {
			t.Fatalf("default GET inspected client state: %+v", client)
		}
	}
	if len(registrations.calls) != 0 || observer.calls != 1 {
		t.Fatalf("default GET call counts: registrations=%v observer=%d; want none and one", registrations.calls, observer.calls)
	}
	if string(before) != string(readReadinessFile(t, configPath)) {
		t.Fatal("readiness GET changed settings bytes")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
}

func TestReadinessStatusExplicitClientCheckIsFixedAndSanitized(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	registrations := &readinessRegistrationFake{
		statuses: map[string]MCPClientStatus{
			mcpClientIDCodex: {
				ClientID:       mcpClientIDCodex,
				Installed:      mcpClientInstalled,
				Version:        "0.155.1",
				Registration:   mcpClientRegistrationRegistered,
				EvidenceSource: mcpClientEvidenceConfigObserved,
				Backup:         mcpClientBackupNotCreated,
				Connection:     "connected",
				ToolCall:       "verified",
			},
			mcpClientIDClaude: {
				ClientID:       mcpClientIDClaude,
				Installed:      "C:\\private\\client.exe",
				Version:        "password=do-not-echo",
				Registration:   "cred:" + strings.Repeat("a", 32),
				EvidenceSource: "C:\\private\\config.json",
				Backup:         "C:\\private\\backup.json",
			},
		},
		errors: map[string]error{mcpClientIDGemini: errors.New("raw config and token do-not-echo")},
	}
	handler := newReadinessStatusHandler(configPath, registrations, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath+"?inspect_clients=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("explicit check status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response readinessStatusDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if strings.Join(registrations.calls, ",") != "codex,claude,gemini" {
		t.Fatalf("inspected clients = %v, want the fixed supported set", registrations.calls)
	}
	if response.Clients[0].Installed != mcpClientInstalled || response.Clients[0].Registration != mcpClientRegistrationRegistered {
		t.Fatalf("safe Codex state not retained: %+v", response.Clients[0])
	}
	if response.Clients[0].Connection != "unverified" || response.Clients[0].ToolCall != "unverified" {
		t.Fatalf("inspection promoted connection or tool call evidence: %+v", response.Clients[0])
	}
	if response.Clients[1].Inspection != "observed" || response.Clients[1].Installed != "not_checked" || response.Clients[1].Registration != "not_checked" {
		t.Fatalf("unsafe client metadata was not dropped: %+v", response.Clients[1])
	}
	if response.Clients[2].Inspection != "unavailable" {
		t.Fatalf("inspection failure = %+v, want fixed unavailable state", response.Clients[2])
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"do-not-echo", "private\\client.exe", "backup.json", `"tool_call":"verified"`, `"connection":"connected"`, "raw config"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("readiness response leaked %q: %s", forbidden, body)
		}
	}
}

func TestReadinessStatusOnlyExactInspectionQueryRuns(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	registrations := &readinessRegistrationFake{}
	handler := newReadinessStatusHandler(configPath, registrations, nil)
	for _, query := range []string{
		"inspect_clients=true",
		"inspect_clients=1&inspect_clients=1",
		"inspect_clients=1&other=x",
		"client=codex",
		"inspect_clients=%31",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath+"?"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q status = %d", query, recorder.Code)
		}
	}
	if len(registrations.calls) != 0 {
		t.Fatalf("non-exact query triggered inspection: %v", registrations.calls)
	}
}

func TestReadinessStatusReportsOnlySanitizedTargetMetadataAndHistoricalTest(t *testing.T) {
	configPath := writeReadinessConfig(t, readinessConfigFixture())
	observer := &readinessObserverFake{}
	handler := newReadinessStatusHandler(configPath, nil, observer)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response readinessStatusDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if response.State != "configured" || len(response.Targets) != 1 || response.Targets[0].CredentialState != "reference_saved" {
		t.Fatalf("configured target projection = %+v", response)
	}
	if response.Targets[0].ConnectionTest.State != "success" || response.Targets[0].ConnectionTest.Source != "saved_history" || response.Targets[0].ConnectionTest.CompletedAt == "" {
		t.Fatalf("historical connection test missing: %+v", response.Targets[0].ConnectionTest)
	}
	if response.Targets[0].Actions == nil || len(response.Targets[0].Actions) != 3 {
		t.Fatalf("per-target actions = %v, want current GitHub catalog actions", response.Targets[0].Actions)
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"cred:" + strings.Repeat("a", 32), "https://private.example", `C:\private\never-echo`, "secret-name-never-echo", "secret:" + strings.Repeat("b", 32)} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("readiness response leaked config field %q", forbidden)
		}
	}
}

func TestReadinessConfigurationRevisionExcludesConnectionHistory(t *testing.T) {
	cfg := readinessConfigFixture()
	first := readinessConfigurationRevision(cfg)
	cfg.ConnectionTests = map[string]connectionTest{
		cfg.GitHubTargets[0].ID: {Result: "failure", CompletedAt: "2026-10-06T02:03:04Z"},
	}
	if got := readinessConfigurationRevision(cfg); got != first {
		t.Fatalf("connection history changed config revision: %s != %s", got, first)
	}
	cfg.GitHubTargets[0].Origin = "https://different.example"
	if got := readinessConfigurationRevision(cfg); got == first {
		t.Fatal("target configuration change did not change opaque revision")
	}
}

func TestReadinessStatusDisabledTargetDoesNotLookReady(t *testing.T) {
	cfg := readinessConfigFixture()
	cfg.GitHubTargets[0].Disabled = true
	configPath := writeReadinessConfig(t, cfg)
	recorder := httptest.NewRecorder()
	newReadinessStatusHandler(configPath, nil, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
	var response readinessStatusDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode readiness response: %v", err)
	}
	if response.State != "needs_setup" || response.Targets[0].Enabled || len(response.Targets[0].Actions) != 0 {
		t.Fatalf("disabled-only target should not imply readiness: %+v", response)
	}
}

func TestReadinessBundleActionsRequireEnabledDashboardMapping(t *testing.T) {
	cfg := readinessConfigFixture()
	dashboardID := "dashboard:" + strings.Repeat("d", 32)
	cfg.DashboardTargets = []dashboardTarget{{
		ID:        dashboardID,
		Name:      "prod-dashboard",
		BaseURL:   "https://dashboard.example",
		SecretRef: cfg.GitHubTargets[0].SecretRef,
	}}
	cfg.ServiceBundles = []serviceBundle{{
		ID:              "service:" + strings.Repeat("e", 32),
		Name:            "worker-service",
		GitHubTargetIDs: []string{cfg.GitHubTargets[0].ID},
		Environments: []serviceEnvironment{{
			Name:                "production",
			DashboardTargetID:   dashboardID,
			DashboardNamespace:  "default",
			DashboardDeployment: "worker",
		}},
	}}
	assertEligibility := func(t *testing.T, disabled, wantEligible bool) {
		t.Helper()
		cfg.DashboardTargets[0].Disabled = disabled
		path := writeReadinessConfig(t, cfg)
		recorder := httptest.NewRecorder()
		newReadinessStatusHandler(path, nil, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", recorder.Code, recorder.Body.String())
		}
		var response readinessStatusDTO
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode readiness response: %v", err)
		}
		environment := response.ServiceBundles[0].Environments[0]
		if environment.DashboardDiagnosisEligible != wantEligible {
			t.Fatalf("disabled=%t eligibility=%t, want %t", disabled, environment.DashboardDiagnosisEligible, wantEligible)
		}
		if wantEligible && len(environment.DashboardActions) != 7 {
			t.Fatalf("eligible Dashboard actions = %v", environment.DashboardActions)
		}
		if !wantEligible && len(environment.DashboardActions) != 0 {
			t.Fatalf("disabled Dashboard target exposed actions: %v", environment.DashboardActions)
		}
	}
	assertEligibility(t, false, true)
	assertEligibility(t, true, false)
}

func TestReadinessStatusMalformedOrOversizedConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{name: "malformed", data: []byte(`{"token":"do-not-echo"}`)},
		{name: "oversized", data: []byte(strings.Repeat("x", maxConfigSize+1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, testCase.data, 0o600); err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			newReadinessStatusHandler(path, nil, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
			if strings.Contains(recorder.Body.String(), "do-not-echo") || strings.Contains(recorder.Body.String(), `"configuration_revision":"`) {
				t.Fatalf("failure response exposed settings content or revision: %s", recorder.Body.String())
			}
		})
	}
}

func TestReadinessFeatureCatalogMatchesRegisteredToolsAndOmitsUnsupportedPodExec(t *testing.T) {
	mainSource, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	parallelSource, err := os.ReadFile("parallel_contracts.go")
	if err != nil {
		t.Fatal(err)
	}
	setupSource, err := os.ReadFile("setup_draft_mcp.go")
	if err != nil {
		t.Fatal(err)
	}
	registeredSource := string(mainSource) + string(parallelSource) + string(setupSource)
	for _, tool := range readinessFeatureToolNames() {
		if !strings.Contains(registeredSource, tool) {
			t.Errorf("readiness feature %q is absent from MCP registration source", tool)
		}
	}
	for _, feature := range readinessFeatureCatalog() {
		for _, tool := range feature.Tools {
			if tool == "dashboard_pod_exec" {
				t.Fatal("readiness advertised unsupported dashboard_pod_exec")
			}
		}
	}
}

func TestReadinessMethodRejectedWithoutCallingDependencies(t *testing.T) {
	configPath := writeReadinessConfig(t, config{Version: configVersion})
	observer := &readinessObserverFake{}
	registrations := &readinessRegistrationFake{}
	recorder := httptest.NewRecorder()
	newReadinessStatusHandler(configPath, registrations, observer).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, readinessStatusPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed || observer.calls != 0 || len(registrations.calls) != 0 {
		t.Fatalf("POST status=%d observer=%d inspections=%v", recorder.Code, observer.calls, registrations.calls)
	}
}

func readinessConfigFixture() config {
	ref := "cred:" + strings.Repeat("a", 32)
	return config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID:            "secret:" + strings.Repeat("b", 32),
			Name:          "secret-name-never-echo",
			CredentialRef: ref,
		}},
		GitHubTargets: []target{{
			ID:         "github:" + strings.Repeat("c", 32),
			Name:       `C:\private\never-echo`,
			Origin:     "https://private.example",
			Repository: "org/repo",
			SecretRef:  ref,
		}},
		ConnectionTests: map[string]connectionTest{
			"github:" + strings.Repeat("c", 32): {Result: "success", CompletedAt: "2026-10-06T02:03:04Z"},
		},
	}
}

func writeReadinessConfig(t *testing.T, cfg config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode fixture config: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}
	return path
}

func readReadinessFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture config: %v", err)
	}
	return data
}
