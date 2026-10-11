package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFirstReadServiceForTool(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		wantService string
		wantOK      bool
	}{
		{name: "GitHub repository read", tool: "github_repository", wantService: "github", wantOK: true},
		{name: "GitHub pull request read", tool: "github_pull_request", wantService: "github", wantOK: true},
		{name: "Jenkins build log read", tool: "jenkins_registered_build_log", wantService: "jenkins", wantOK: true},
		{name: "Harbor quota read", tool: "harbor_project_quota", wantService: "harbor", wantOK: true},
		{name: "Dashboard Pod logs read", tool: "dashboard_deployment_pod_logs", wantService: "dashboard", wantOK: true},
		{name: "local target catalog", tool: "registered_targets"},
		{name: "connection test", tool: "registered_target_connection_test"},
		{name: "Jenkins write", tool: "jenkins_run_registered_job"},
		{name: "SSH operation", tool: mcpToolRunSSHOperation},
		{name: "Python task", tool: "python_run_registered_task"},
		{name: "unknown tool", tool: "external_read"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotService, gotOK := firstReadServiceForTool(test.tool)
			if gotService != test.wantService || gotOK != test.wantOK {
				t.Fatalf("firstReadServiceForTool(%q) = (%q, %v), want (%q, %v)", test.tool, gotService, gotOK, test.wantService, test.wantOK)
			}
		})
	}
}

func TestFirstReadCallObserverCapturesOnlySupportedLoadedDefinition(t *testing.T) {
	registeredTarget := firstReadReceiptTestGitHubTarget()
	wantDigest := digestFirstReadDefinition(registeredTarget)
	disabledTarget := registeredTarget
	disabledTarget.Disabled = true

	tests := []struct {
		name       string
		service    string
		tool       string
		definition any
		wantDigest string
	}{
		{name: "matching loaded GitHub target", service: "github", tool: "github_repository", definition: registeredTarget, wantDigest: wantDigest},
		{name: "non-read tool", service: "github", tool: "registered_target_connection_test", definition: registeredTarget},
		{name: "wrong service tool pair", service: "github", tool: "jenkins_registered_job", definition: registeredTarget},
		{name: "unexpected definition type", service: "github", tool: "github_repository", definition: "secret or response text"},
		{name: "disabled target", service: "github", tool: "github_repository", definition: disabledTarget},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, observer := withFirstReadCallObserver(context.Background(), test.service, test.tool)
			observeFirstReadDefinition(ctx, test.definition)
			if got := observer.Digest(); got != test.wantDigest {
				t.Fatalf("captured digest = %q, want %q", got, test.wantDigest)
			}
			if got := observer.Digest(); got != "" && (!validFirstReadDefinitionDigest(got) || strings.Contains(got, registeredTarget.Origin) || strings.Contains(got, registeredTarget.SecretRef)) {
				t.Fatalf("captured digest is not an opaque canonical digest: %q", got)
			}
		})
	}

	observer := &firstReadCallObserver{service: "github", tool: "github_repository"}
	observeFirstReadDefinition(context.Background(), registeredTarget)
	if got := observer.Digest(); got != "" {
		t.Fatalf("definition was captured without an observer in context: %q", got)
	}
}

func TestFirstReadDashboardBindingDigestMatchesCurrentMapping(t *testing.T) {
	cfg := firstReadReceiptTestConfig()
	selector := firstReadDefinitionSelector{ServiceBundleName: "Inventory", EnvironmentName: "production"}
	definition := dashboardFirstReadDefinition(cfg, selector)
	wantDigest := digestFirstReadDefinition(definition)
	if wantDigest == "" {
		t.Fatal("dashboard binding digest is empty")
	}

	binding := dashboardServiceBinding{
		ServiceBundle: "Inventory", Environment: "production", Target: cfg.DashboardTargets[0],
		Namespace: "apps", Deployment: "api",
	}
	ctx, observer := withFirstReadCallObserver(context.Background(), "dashboard", "dashboard_deployment_status")
	observeFirstReadDashboardBinding(ctx, binding)
	if got := observer.Digest(); got != wantDigest {
		t.Fatalf("captured Dashboard binding digest = %q, want %q", got, wantDigest)
	}
	if !firstReadDefinitionDigestIsActive(cfg, "dashboard", "dashboard_deployment_status", wantDigest) {
		t.Fatal("current Dashboard mapping did not match its captured digest")
	}

	cfg.ServiceBundles[0].Environments[0].DashboardDeployment = "api-next"
	if firstReadDefinitionDigestIsActive(cfg, "dashboard", "dashboard_deployment_status", wantDigest) {
		t.Fatal("stale Dashboard mapping was accepted after the active definition changed")
	}
}

func TestFirstReadDefinitionDigestMustMatchEnabledCurrentTarget(t *testing.T) {
	cfg := firstReadReceiptTestConfig()
	digest := firstReadDefinitionDigestForCall(cfg, "github", "github_repository", firstReadDefinitionSelector{TargetName: "Engineering"})
	if !validFirstReadDefinitionDigest(digest) {
		t.Fatalf("invalid target digest %q", digest)
	}
	if !firstReadDefinitionDigestIsActive(cfg, "github", "github_repository", digest) {
		t.Fatal("current GitHub target did not match its definition digest")
	}
	if firstReadDefinitionDigestIsActive(cfg, "github", "registered_target_connection_test", digest) {
		t.Fatal("connection-test tool was accepted as a first external read")
	}

	cfg.GitHubTargets[0].Repository = "ops/changed"
	if firstReadDefinitionDigestIsActive(cfg, "github", "github_repository", digest) {
		t.Fatal("old receipt digest remained current after the registered repository changed")
	}
	cfg.GitHubTargets[0].Disabled = true
	if firstReadDefinitionDigestIsActive(cfg, "github", "github_repository", digest) {
		t.Fatal("disabled target was treated as a current verified read")
	}
}

func TestFirstReadReadinessExposesOnlyCurrentMCPObservedEvidence(t *testing.T) {
	cfg := firstReadReceiptTestConfig()
	registeredTarget := cfg.GitHubTargets[0]
	digest := digestFirstReadDefinition(registeredTarget)
	observedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	tests := []struct {
		name     string
		receipt  onboardingFirstReadReceipt
		wantSeen bool
	}{
		{
			name: "current successful MCP read",
			receipt: onboardingFirstReadReceipt{
				Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
				ObservedAt: observedAt, DefinitionDigest: digest,
			},
			wantSeen: true,
		},
		{
			name: "user reported evidence is separate",
			receipt: onboardingFirstReadReceipt{
				Source: "user_reported", Service: onboardingServiceGitHub, Tool: "github_repository",
				ObservedAt: observedAt, DefinitionDigest: digest,
			},
		},
		{
			name: "connection test is not a read receipt",
			receipt: onboardingFirstReadReceipt{
				Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "registered_target_connection_test",
				ObservedAt: observedAt, DefinitionDigest: digest,
			},
		},
		{
			name: "digest mismatch is not current evidence",
			receipt: onboardingFirstReadReceipt{
				Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
				ObservedAt: observedAt, DefinitionDigest: digestFirstReadDefinition("old configuration"),
			},
		},
		{
			name: "invalid time is ignored",
			receipt: onboardingFirstReadReceipt{
				Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
				DefinitionDigest: digest,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := firstReadReadinessFromConfig(cfg, []onboardingFirstReadReceipt{test.receipt})
			if (got.State == firstReadStateObserved) != test.wantSeen {
				t.Fatalf("readiness state = %q, want observed=%v", got.State, test.wantSeen)
			}
			if !test.wantSeen {
				return
			}
			if got.Source != onboardingFirstReadSource || got.Service != "github" || got.Tool != "github_repository" ||
				got.ObservedAt == nil || !got.ObservedAt.Equal(observedAt) {
				t.Fatalf("readiness evidence = %#v", got)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal readiness evidence: %v", err)
			}
			for _, forbidden := range []string{digest, registeredTarget.Origin, registeredTarget.Name, registeredTarget.Repository, registeredTarget.SecretRef} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("readiness exposed private definition value %q in %s", forbidden, encoded)
				}
			}
		})
	}

	changed := cfg
	changed.GitHubTargets = append([]target(nil), cfg.GitHubTargets...)
	changed.GitHubTargets[0].Repository = "ops/replaced"
	stale := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
		ObservedAt: observedAt, DefinitionDigest: digest,
	}
	if got := firstReadReadinessFromConfig(changed, []onboardingFirstReadReceipt{stale}); got.State != firstReadStateNotObserved {
		t.Fatalf("stale definition produced readiness %#v", got)
	}
}

func firstReadReceiptTestConfig() config {
	github := firstReadReceiptTestGitHubTarget()
	dashboard := dashboardTarget{
		ID: "dashboard:0123456789abcdef0123456789abcdef", Name: "Cluster UI",
		BaseURL: "https://dashboard.example.invalid/proxy", SecretRef: "cred:4123456789abcdef0123456789abcdef",
	}
	return config{
		GitHubTargets:    []target{github},
		DashboardTargets: []dashboardTarget{dashboard},
		ServiceBundles: []serviceBundle{{
			ID: "service:0123456789abcdef0123456789abcdef", Name: "Inventory",
			GitHubTargetIDs: []string{github.ID},
			Environments: []serviceEnvironment{{
				Name: "production", DashboardTargetID: dashboard.ID,
				DashboardNamespace: "apps", DashboardDeployment: "api",
			}},
		}},
	}
}

func firstReadReceiptTestGitHubTarget() target {
	return target{
		ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering",
		Origin: "https://github.example.invalid", Repository: "ops/agent",
		SecretRef: "cred:0123456789abcdef0123456789abcdef",
	}
}

func TestFirstReadReceiptPersistsThroughAppController(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := firstReadReceiptTestConfig()
	cfg.Version = configVersion
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	app := &app{configPath: path}
	controller, err := app.ensureOnboardingController()
	if err != nil {
		t.Fatalf("ensure onboarding controller: %v", err)
	}
	receipt := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub,
		Tool: "github_repository", ObservedAt: time.Now().UTC(),
		DefinitionDigest: digestFirstReadDefinition(cfg.GitHubTargets[0]),
	}
	if added, err := controller.RecordFirstRead(context.Background(), receipt); err != nil || !added {
		t.Fatalf("record first read: added=%v err=%v", added, err)
	}
	got, err := controller.ListFirstReads(context.Background())
	if err != nil {
		t.Fatalf("list first reads: %v", err)
	}
	if len(got) != 1 || got[0].DefinitionDigest != receipt.DefinitionDigest {
		t.Fatalf("list first reads = %#v", got)
	}
}

func TestDashboardDiagnosisMCPFirstReadReceiptRequiresSuccessfulSubread(t *testing.T) {
	tests := []struct {
		name                 string
		statusReadSucceeds   bool
		wantFirstReadReceipt bool
	}{
		{name: "all subreads failed"},
		{name: "partial success", statusReadSucceeds: true, wantFirstReadReceipt: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const token = "dashboard_first_read_canary_0123456789"
			configPath := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if err := writeConfig(configPath, cfg); err != nil {
				t.Fatal(err)
			}

			secretStore := &dashboardDiagnosisSecretStore{
				ref:   cfg.DashboardTargets[0].SecretRef,
				token: []byte(token),
			}
			var requests atomic.Int32
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.URL.Host != "dashboard.example.invalid" ||
					r.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("synthetic diagnosis request escaped its saved GET target: method=%q host=%q authorization=%q", r.Method, r.URL.Host, r.Header.Get("Authorization"))
				}

				status := http.StatusServiceUnavailable
				body := `{"message":"synthetic Dashboard read failure"}`
				if test.statusReadSucceeds && r.URL.EscapedPath() == "/proxy/api/v1/deployment/apps/api.v2" {
					status = http.StatusOK
					body = `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":2,"updated":2,"available":1,"unavailable":1},"conditions":[]}`
				}
				return harborTestResponse(r, status, body), nil
			})}
			a := &app{configPath: configPath, secrets: secretStore, client: client}

			ctx, clientSession := connectDashboardDiagnosisMCP(t, a)
			result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
				Name:      "dashboard_deployment_diagnosis",
				Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"},
			})
			if err != nil || result == nil || result.IsError {
				t.Fatalf("Dashboard diagnosis MCP result=%#v err=%v", result, err)
			}
			structured, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatalf("marshal diagnosis result: %v", err)
			}
			var diagnosis dashboardDeploymentDiagnosisResult
			if err := json.Unmarshal(structured, &diagnosis); err != nil {
				t.Fatalf("decode diagnosis result: %v; result=%s", err, structured)
			}
			if len(diagnosis.Checks) != 3 {
				t.Fatalf("diagnosis checks = %#v, want status, events, and pods", diagnosis.Checks)
			}
			checkStatuses := make(map[string]string, len(diagnosis.Checks))
			for _, check := range diagnosis.Checks {
				checkStatuses[check.Step] = check.Status
			}
			wantStatuses := map[string]string{"status": "failed", "events": "failed", "pods": "failed"}
			if test.statusReadSucceeds {
				wantStatuses["status"] = "succeeded"
			}
			for step, want := range wantStatuses {
				if got := checkStatuses[step]; got != want {
					t.Errorf("diagnosis %s status = %q, want %q (all statuses: %#v)", step, got, want, checkStatuses)
				}
			}
			if requests.Load() == 0 || secretStore.loads.Load() != 1 {
				t.Fatalf("diagnosis did not use the registered synthetic binding: requests=%d secret loads=%d", requests.Load(), secretStore.loads.Load())
			}

			controller, err := a.ensureOnboardingController()
			if err != nil {
				t.Fatalf("ensure onboarding controller: %v", err)
			}
			durableStore, err := newOnboardingDraftStoreForConfigPath(configPath)
			if err != nil {
				t.Fatalf("open durable onboarding store: %v", err)
			}
			persistedReceipts, err := durableStore.ListFirstReads(ctx)
			if err != nil {
				t.Fatalf("list persisted first-read receipts: %v", err)
			}
			if !test.wantFirstReadReceipt && len(persistedReceipts) != 0 {
				t.Fatalf("all-failed diagnosis persisted a first-read receipt: %#v", persistedReceipts)
			}
			if test.wantFirstReadReceipt {
				wantDigest := digestFirstReadDefinition(dashboardFirstReadDefinition(cfg, firstReadDefinitionSelector{
					ServiceBundleName: "Inventory",
					EnvironmentName:   "qa-blue",
				}))
				if len(persistedReceipts) != 1 {
					t.Fatalf("partial-success durable receipts = %#v, want exactly one", persistedReceipts)
				}
				got := persistedReceipts[0]
				if got.Source != onboardingFirstReadSource || got.Service != onboardingServiceDashboard ||
					got.Tool != "dashboard_deployment_diagnosis" || got.DefinitionDigest != wantDigest ||
					!validFirstReadReceiptTime(got.ObservedAt) {
					t.Fatalf("persisted receipt does not match the exact current Dashboard binding: got=%#v want digest=%q", got, wantDigest)
				}
			}

			receipts, err := controller.ListFirstReads(ctx)
			if err != nil {
				t.Fatalf("list current first-read receipts: %v", err)
			}
			if !test.wantFirstReadReceipt {
				if len(receipts) != 0 {
					t.Fatalf("all-failed diagnosis persisted a first-read receipt: %#v", receipts)
				}
			} else {
				wantDigest := digestFirstReadDefinition(dashboardFirstReadDefinition(cfg, firstReadDefinitionSelector{
					ServiceBundleName: "Inventory",
					EnvironmentName:   "qa-blue",
				}))
				if len(receipts) != 1 {
					t.Fatalf("partial-success diagnosis receipts = %#v, want exactly one", receipts)
				}
				got := receipts[0]
				if got.Source != onboardingFirstReadSource || got.Service != onboardingServiceDashboard ||
					got.Tool != "dashboard_deployment_diagnosis" || got.DefinitionDigest != wantDigest ||
					!validFirstReadReceiptTime(got.ObservedAt) {
					t.Fatalf("partial-success receipt does not match the exact current Dashboard binding: got=%#v want digest=%q", got, wantDigest)
				}
			}

			readinessHandler := newReadinessStatusHandlerWithOnboarding(configPath, nil, nil, controller)
			readinessResponse := httptest.NewRecorder()
			readinessHandler.ServeHTTP(readinessResponse, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil))
			if readinessResponse.Code != http.StatusOK {
				t.Fatalf("readiness status = %d body=%s", readinessResponse.Code, readinessResponse.Body.String())
			}
			var readiness readinessStatusDTO
			if err := json.Unmarshal(readinessResponse.Body.Bytes(), &readiness); err != nil {
				t.Fatalf("decode readiness status: %v", err)
			}
			if !test.wantFirstReadReceipt {
				if readiness.FirstRead.State != firstReadStateNotObserved {
					t.Fatalf("all-failed diagnosis readiness = %#v, want not observed", readiness.FirstRead)
				}
			} else if readiness.FirstRead.State != firstReadStateObserved ||
				readiness.FirstRead.Source != onboardingFirstReadSource || readiness.FirstRead.Service != "dashboard" ||
				readiness.FirstRead.Tool != "dashboard_deployment_diagnosis" || readiness.FirstRead.ObservedAt == nil ||
				!readiness.FirstRead.ObservedAt.Equal(receipts[0].ObservedAt) {
				t.Fatalf("partial-success readiness did not expose the matching current receipt: %#v", readiness.FirstRead)
			}
		})
	}
}
