package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func dashboardTestTarget() dashboardTarget {
	return dashboardTarget{
		ID:        "dashboard:0123456789abcdef0123456789abcdef",
		Name:      "QA cluster",
		BaseURL:   "https://dashboard.example.invalid/proxy/context",
		SecretRef: "cred:0123456789abcdef0123456789abcdef",
	}
}

type dashboardDiagnosisSecretStore struct {
	ref   string
	token []byte
	loads atomic.Int32
}

func (s *dashboardDiagnosisSecretStore) Save(string, []byte) error { return nil }
func (s *dashboardDiagnosisSecretStore) Load(ref string) ([]byte, error) {
	s.loads.Add(1)
	if ref != s.ref {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), s.token...), nil
}
func (s *dashboardDiagnosisSecretStore) Delete(string) error { return nil }

func dashboardDiagnosisTestClient(t *testing.T, token string, eventsStatus int, requests *atomic.Int32, largePods ...bool) *http.Client {
	t.Helper()
	podList := dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", "apps", "Running", "Running", "node-1", "web", "Running", 0)})
	if len(largePods) != 0 && largePods[0] {
		podList = dashboardDiagnosisLargePodList()
	}
	return &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.URL.Host != "dashboard.example.invalid" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("diagnostic request escaped registered target or Bearer scope: host=%q authorization=%q", r.URL.Host, r.Header.Get("Authorization"))
		}
		var status int
		var body string
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api.v2","namespace":"apps","annotations":{"private":"` + token + `"}},"statusInfo":{"replicas":2,"updated":2,"available":1,"unavailable":1},"conditions":[]}`
		case "/proxy/api/v1/deployment/apps/api.v2/event":
			status = eventsStatus
			if status == http.StatusOK {
				body = dashboardDeploymentEventList(1, []map[string]any{dashboardDeploymentEventFixture(0, token)}, nil)
			} else {
				body = `{"error":"` + token + `","url":"https://private.example.invalid/response"}`
			}
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api-v2-abc"}}`
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			status = http.StatusOK
			body = `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			status = http.StatusOK
			body = podList
		default:
			t.Errorf("unexpected Dashboard diagnostic route %s", r.URL.EscapedPath())
			return harborTestResponse(r, http.StatusNotFound, token), nil
		}
		return harborTestResponse(r, status, body), nil
	})}
}

func dashboardDiagnosisLargePodList() string {
	containers := make([]map[string]any, 32)
	for i := range containers {
		containers[i] = map[string]any{
			"name":  "container-" + strings.Repeat("x", 45) + strconv.Itoa(i),
			"state": "Running",
			"ready": true,
		}
	}
	pods := make([]map[string]any, dashboardDeploymentPodLimit)
	for i := range pods {
		pods[i] = map[string]any{
			"objectMeta":        map[string]any{"name": "api-v2-abc-" + strconv.Itoa(i), "namespace": "apps"},
			"status":            "CrashLoopBackOff",
			"restartCount":      3,
			"nodeName":          "node-" + strings.Repeat("n", 180),
			"containerStatuses": containers,
		}
	}
	return dashboardPodListFixture(len(pods), pods)
}

func TestDashboardDeploymentDiagnosisKeepsProjectedDataAndLoadsBindingOnce(t *testing.T) {
	const token = "dashboard_diagnosis_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{configPath: path, secrets: secrets, client: dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests)}

	result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if secrets.loads.Load() != 1 || requests.Load() != 5 {
		t.Fatalf("secret loads=%d requests=%d, want one secret load and five bounded Dashboard reads", secrets.loads.Load(), requests.Load())
	}
	if result.ServiceBundle != "Inventory" || result.Environment != "qa-blue" || result.Target != "Cluster UI" || result.Namespace != "apps" || result.Deployment != "api.v2" || result.Status == nil || result.Events == nil || result.Pods == nil {
		t.Fatalf("diagnostic omitted registered mapping or a successful read: %+v", result)
	}
	if len(result.Checks) != 3 {
		t.Fatalf("diagnostic checks = %+v", result.Checks)
	}
	for _, check := range result.Checks {
		if check.Status != "succeeded" || check.ErrorCode != "" || check.NextCheck != "" {
			t.Fatalf("unexpected successful diagnostic check: %+v", check)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, cfg.DashboardTargets[0].BaseURL, "annotations", "private.example.invalid", "cumulativeMetrics", "sourceHost"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("diagnostic exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestDashboardDeploymentDiagnosisLimitsReplicaSetPodReads(t *testing.T) {
	tests := []struct {
		name              string
		currentReplicaSet string
		oldReplicaSets    int
		emptyPods         bool
		wantPod           string
		wantTruncated     bool
	}{
		{
			name: "current and multiple old ReplicaSets", currentReplicaSet: "api-v2-abc", oldReplicaSets: 2,
			wantPod: "api-v2-abc-pod", wantTruncated: true,
		},
		{
			name: "no current ReplicaSet", oldReplicaSets: 2,
			wantPod: "api-v2-old-0-pod", wantTruncated: true,
		},
		{
			name: "empty first Pod list", currentReplicaSet: "api-v2-abc", oldReplicaSets: 2,
			emptyPods: true, wantTruncated: true,
		},
		{
			name: "maximum old ReplicaSet discovery", currentReplicaSet: "api-v2-abc", oldReplicaSets: 16,
			wantPod: "api-v2-abc-pod", wantTruncated: true,
		},
		{
			name: "single current ReplicaSet", currentReplicaSet: "api-v2-abc",
			wantPod: "api-v2-abc-pod",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const token = "dashboard_diagnosis_budget_canary_0123456789"
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			oldReplicaSets := make([]map[string]any, test.oldReplicaSets)
			for i := range oldReplicaSets {
				oldReplicaSets[i] = map[string]any{
					"objectMeta": map[string]string{"name": "api-v2-old-" + strconv.Itoa(i)},
				}
			}
			oldBody, err := json.Marshal(map[string]any{
				"listMeta":    map[string]int{"totalItems": len(oldReplicaSets)},
				"replicaSets": oldReplicaSets,
				"errors":      []string{},
			})
			if err != nil {
				t.Fatal(err)
			}
			secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
			var requests atomic.Int32
			var podReads atomic.Int32
			baseClient := dashboardDiagnosisTestClient(
				t,
				token,
				http.StatusOK,
				&requests,
			)
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				validTarget := r.URL.Host == "dashboard.example.invalid"
				validBearer := r.Header.Get("Authorization") == "Bearer "+token
				validRead := r.Method == http.MethodGet && validTarget && validBearer
				if !validRead {
					t.Error("diagnosis request escaped its registered read-only scope")
				}
				route := r.URL.EscapedPath()
				switch route {
				case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
					requests.Add(1)
					body := `{"objectMeta":{"name":"` + test.currentReplicaSet + `"}}`
					return harborTestResponse(r, http.StatusOK, body), nil
				case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
					requests.Add(1)
					return harborTestResponse(r, http.StatusOK, string(oldBody)), nil
				}
				const podRoutePrefix = "/proxy/api/v1/replicaset/apps/"
				if strings.HasPrefix(route, podRoutePrefix) && strings.HasSuffix(route, "/pod") {
					requests.Add(1)
					podReads.Add(1)
					replicaSet := strings.TrimSuffix(strings.TrimPrefix(route, podRoutePrefix), "/pod")
					pods := make([]map[string]any, 0, 1)
					if !test.emptyPods {
						pods = append(pods, dashboardPodFixture(
							replicaSet+"-pod",
							"apps",
							"Running",
							"Running",
							"node-1",
							"web",
							"Running",
							0,
						))
					}
					return harborTestResponse(r, http.StatusOK, dashboardPodListFixture(len(pods), pods)), nil
				}
				return baseClient.Transport.RoundTrip(r)
			})}
			a := &app{configPath: path, secrets: secrets, client: client}

			result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 5 || podReads.Load() != 1 {
				t.Fatalf(
					"diagnosis sent %d requests and %d Pod reads; want five total and one Pod read",
					requests.Load(), podReads.Load(),
				)
			}
			if secrets.loads.Load() != 1 {
				t.Fatalf("credential loaded %d times; want once", secrets.loads.Load())
			}
			hasAllResults := result.Status != nil && result.Events != nil && result.Pods != nil
			if !hasAllResults || len(result.Checks) != 3 {
				t.Fatalf("bounded diagnosis lost successful steps: %+v", result)
			}
			for _, check := range result.Checks {
				if check.Status != "succeeded" || check.ErrorCode != "" {
					t.Fatalf("bounded successful read became a failure: %+v", check)
				}
			}
			if result.Pods.Truncated != test.wantTruncated {
				t.Fatalf("pods_truncated=%v, want %v", result.Pods.Truncated, test.wantTruncated)
			}
			if test.emptyPods {
				if len(result.Pods.Pods) != 0 {
					t.Fatalf("empty first Pod list returned unexpected Pods: %+v", result.Pods.Pods)
				}
			} else if len(result.Pods.Pods) != 1 || result.Pods.Pods[0].Name != test.wantPod {
				t.Fatalf("first permitted Pod result was lost: %+v", result.Pods.Pods)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), cfg.DashboardTargets[0].BaseURL) {
				t.Fatal("bounded diagnosis exposed its credential or registered address")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("bounded diagnosis changed saved settings")
			}
		})
	}
}

func TestDashboardDeploymentDiagnosisRetainsSuccessfulStepsAndHidesFailureDetails(t *testing.T) {
	const token = "dashboard_diagnosis_error_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{configPath: path, secrets: secrets, client: dashboardDiagnosisTestClient(t, token, http.StatusForbidden, &requests)}

	result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if secrets.loads.Load() != 1 || result.Status == nil || result.Pods == nil || result.Events != nil {
		t.Fatalf("partial diagnosis discarded successful data or loaded unexpected secrets: loads=%d result=%+v", secrets.loads.Load(), result)
	}
	if len(result.Checks) != 3 || result.Checks[0].Status != "succeeded" || result.Checks[1].Status != "failed" || result.Checks[1].ErrorCode != "access_denied" || result.Checks[1].NextCheck == "" || result.Checks[2].Status != "succeeded" {
		t.Fatalf("unexpected per-step diagnosis: %+v", result.Checks)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, cfg.DashboardTargets[0].BaseURL, "private.example.invalid", "response\"", "https://"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("failed diagnosis exposed upstream data %q: %s", forbidden, encoded)
		}
	}
}

func TestDashboardDeploymentDiagnosisReports404WithoutAssumingTheCause(t *testing.T) {
	const token = "dashboard_diagnosis_404_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{configPath: path, secrets: secrets, client: dashboardDiagnosisTestClient(t, token, http.StatusNotFound, &requests)}

	result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == nil || result.Pods == nil || result.Events != nil || len(result.Checks) != 3 {
		t.Fatalf("404 diagnosis discarded successful steps: %+v", result)
	}
	check := result.Checks[1]
	if check.Status != "failed" || check.ErrorCode != "resource_or_path_not_found" || !strings.Contains(check.NextCheck, "A 404 alone cannot identify the cause") || !strings.Contains(check.NextCheck, "mapping") || !strings.Contains(check.NextCheck, "REST path") {
		t.Fatalf("404 diagnosis assumed a cause or omitted checks: %+v", check)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "private.example.invalid") {
		t.Fatalf("404 diagnostic exposed upstream details: %s", encoded)
	}
}

func TestDashboardDeploymentDiagnosisClassifiesUpstream5xxWithoutLeakingDetails(t *testing.T) {
	const token = "dashboard_diagnosis_5xx_canary_0123456789"
	const upstreamErrorGuidance = "Dashboard returned a server error. Check its availability and retry the read-only diagnostic later."
	tests := []struct {
		name      string
		status    int
		errorCode string
		nextCheck string
	}{
		{name: "500 internal server error", status: http.StatusInternalServerError, errorCode: "upstream_error", nextCheck: upstreamErrorGuidance},
		{name: "502 bad gateway", status: http.StatusBadGateway, errorCode: "upstream_error", nextCheck: upstreamErrorGuidance},
		{name: "503 service unavailable", status: http.StatusServiceUnavailable, errorCode: "upstream_error", nextCheck: upstreamErrorGuidance},
		{name: "599 upper 5xx boundary", status: 599, errorCode: "upstream_error", nextCheck: upstreamErrorGuidance},
		{name: "499 below 5xx range", status: 499, errorCode: "request_failed", nextCheck: "Review the registered Dashboard REST path and read permissions, then retry the diagnostic."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
			var requests atomic.Int32
			baseClient := dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests)
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.EscapedPath() != "/proxy/api/v1/deployment/apps/api.v2/event" {
					return baseClient.Transport.RoundTrip(r)
				}
				requests.Add(1)
				response := harborTestResponse(r, test.status, token+" https://private.example.invalid/response")
				response.Header.Set("X-Diagnostic-Canary", "private.example.invalid")
				return response, nil
			})}
			a := &app{configPath: path, secrets: secrets, client: client}

			result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 5 || secrets.loads.Load() != 1 || result.Status == nil || result.Events != nil || result.Pods == nil {
				t.Fatalf("HTTP error diagnosis did not preserve bounded partial results: requests=%d loads=%d result=%+v", requests.Load(), secrets.loads.Load(), result)
			}
			if len(result.Checks) != 3 || result.Checks[0].Status != "succeeded" || result.Checks[1].Status != "failed" || result.Checks[1].ErrorCode != test.errorCode || result.Checks[1].NextCheck != test.nextCheck || result.Checks[2].Status != "succeeded" {
				t.Fatalf("unexpected status guidance or partial checks for HTTP %d: %+v", test.status, result.Checks)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{token, "private.example.invalid", "https://", "X-Diagnostic-Canary"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("5xx diagnosis leaked %q: %s", forbidden, encoded)
				}
			}
		})
	}
}
func TestDashboardDeploymentDiagnosisReturnsPartialResultsOnDeadline(t *testing.T) {
	const token = "dashboard_diagnosis_timeout_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	var eventStarted atomic.Bool
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":1,"updated":1,"available":1,"unavailable":0},"conditions":[]}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/event":
			eventStarted.Store(true)
			<-r.Context().Done()
			return nil, r.Context().Err()
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api-v2-abc"}}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`), nil
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			return harborTestResponse(r, http.StatusOK, dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", "apps", "Running", "Running", "node-1", "web", "Running", 0)})), nil
		default:
			t.Errorf("unexpected Dashboard diagnosis route %s", r.URL.EscapedPath())
			return harborTestResponse(r, http.StatusNotFound, token), nil
		}
	})}
	a := &app{configPath: path, secrets: secrets, client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := a.registeredDashboardDeploymentDiagnosis(ctx, "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("diagnosis exceeded its caller deadline: %v", elapsed)
	}
	if !eventStarted.Load() || requests.Load() != 5 || secrets.loads.Load() != 1 {
		t.Fatalf("deadline flow did not complete the bounded requests: event=%t requests=%d secret loads=%d", eventStarted.Load(), requests.Load(), secrets.loads.Load())
	}
	if result.Status == nil || result.Pods == nil || result.Events != nil || len(result.Checks) != 3 {
		t.Fatalf("deadline discarded successful steps or included incomplete events: %+v", result)
	}
	if result.Checks[0].Status != "succeeded" || result.Checks[1].Status != "failed" || result.Checks[1].ErrorCode != "timeout" || result.Checks[1].NextCheck == "" || result.Checks[2].Status != "succeeded" {
		t.Fatalf("unexpected partial timeout checks: %+v", result.Checks)
	}
}

func TestDashboardDeploymentDiagnosisCapsCombinedJSONOutput(t *testing.T) {
	const token = "dashboard_diagnosis_large_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	largeBody := dashboardDiagnosisLargePodList()
	if len(largeBody) >= maxAPIBytes {
		t.Fatalf("large fixture must remain inside the existing per-response limit: %d bytes", len(largeBody))
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{configPath: path, secrets: secrets, client: dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests, true)}

	result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if dashboardDeploymentDiagnosisSize(result) > dashboardDeploymentDiagnosisOutputLimit || !result.OutputTruncated || result.Pods == nil || !result.Pods.Truncated || len(result.Checks) != 3 {
		t.Fatalf("diagnostic MCP response was not bounded and marked: bytes=%d structure=%d truncated=%v result=%+v", dashboardDeploymentDiagnosisSize(result), len(encoded), result.OutputTruncated, result)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), cfg.DashboardTargets[0].BaseURL) {
		t.Fatalf("bounded diagnostic exposed secret or registered address: %s", encoded)
	}
}

func TestDashboardDeploymentDiagnosisRejectsInvalidScopeBeforeSecretOrHTTPAccess(t *testing.T) {
	tests := []struct {
		name        string
		bundle      string
		environment string
		wantCode    string
		mutate      func(*config)
	}{
		{name: "missing names", wantCode: "invalid_input"},
		{name: "unknown bundle", bundle: "unregistered", environment: "qa-blue", wantCode: "service_bundle_not_found"},
		{name: "unknown environment", bundle: "Inventory", environment: "unregistered", wantCode: "environment_not_found"},
		{name: "disabled mapped target", bundle: "Inventory", environment: "qa-blue", wantCode: "target_disabled", mutate: func(cfg *config) { cfg.DashboardTargets[0].Disabled = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			secrets := &serviceBundleSecrets{}
			var requests atomic.Int32
			a := &app{configPath: path, secrets: secrets, client: dashboardDiagnosisTestClient(t, "unused-token", http.StatusOK, &requests)}

			result, err := a.registeredDashboardDeploymentDiagnosis(context.Background(), test.bundle, test.environment)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Checks) != 1 || result.Checks[0].Step != "binding" || result.Checks[0].Status != "failed" || result.Checks[0].ErrorCode != test.wantCode || result.Checks[0].NextCheck == "" {
				t.Fatalf("invalid scope check = %+v, want code %q", result.Checks, test.wantCode)
			}
			if secrets.loads != 0 || requests.Load() != 0 {
				t.Fatalf("invalid scope accessed secret or Dashboard: secret loads=%d requests=%d", secrets.loads, requests.Load())
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, forbidden := range []string{"unused-token", "dashboard.example.invalid", "https://"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Errorf("invalid scope response exposed %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestDashboardNamespaceRequestProjectsAndBoundsResponse(t *testing.T) {
	const token = "dashboard_api_canary_0123456789"
	target := dashboardTestTarget()
	namespaces := make([]map[string]any, 101)
	for i := range namespaces {
		namespaces[i] = map[string]any{"objectMeta": map[string]string{"name": "namespace-" + strconv.Itoa(i)}, "token": token}
	}
	body, err := json.Marshal(map[string]any{
		"listMeta":    map[string]int{"totalItems": len(namespaces)},
		"namespaces":  namespaces,
		"unprojected": map[string]string{"password": token},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/proxy/context/api/v1/namespace" || r.URL.RawQuery != "itemsPerPage=100" {
			t.Errorf("Dashboard request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q", got)
		}
		return harborTestResponse(r, http.StatusOK, string(body)), nil
	})}
	result, err := fetchDashboardNamespaces(context.Background(), target, token, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Namespaces) != dashboardNamespaceLimit || !result.Truncated {
		t.Fatalf("namespaces=%d truncated=%v", len(result.Namespaces), result.Truncated)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "unprojected") || strings.Contains(string(encoded), "namespace-100") {
		t.Fatalf("Dashboard projection leaked or exceeded the limit: %s", encoded)
	}
}

func TestDashboardNamespaceRejectsRedirectsAndHTTPFailures(t *testing.T) {
	const token = "dashboard_error_canary_0123456789"
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "redirect", status: http.StatusFound, body: token},
		{name: "unauthorized", status: http.StatusUnauthorized, body: token},
		{name: "forbidden", status: http.StatusForbidden, body: token},
		{name: "server error", status: http.StatusInternalServerError, body: token},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := harborTestResponse(r, test.status, test.body)
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/collect")
				}
				return resp, nil
			})}
			_, err := fetchDashboardNamespaces(context.Background(), dashboardTestTarget(), token, client)
			if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "collect") || requests != 1 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}
}

func TestDashboardNamespaceRejectsMalformedAndOversizedBodies(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"namespaces":`},
		{name: "missing list", body: `{}`},
		{name: "null list", body: `{"namespaces":null}`},
		{name: "missing metadata", body: `{"namespaces":[]}`},
		{name: "missing item count", body: `{"listMeta":{},"namespaces":[]}`},
		{name: "inconsistent count", body: `{"listMeta":{"totalItems":0},"namespaces":[{"objectMeta":{"name":"default"}}]}`},
		{name: "invalid name", body: `{"namespaces":[{"objectMeta":{"name":"../secret"}}]}`},
		{name: "oversized", body: strings.Repeat(" ", maxAPIBytes+1) + "response-canary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				return harborTestResponse(r, http.StatusOK, test.body), nil
			})}
			if _, err := fetchDashboardNamespaces(context.Background(), dashboardTestTarget(), "known-api-bearer", client); err == nil {
				t.Fatal("invalid Dashboard response accepted")
			}
		})
	}
}

func TestDashboardNamespaceUsesTotalItemsForPartialPage(t *testing.T) {
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":5},"namespaces":[{"objectMeta":{"name":"default"}}]}`), nil
	})}
	result, err := fetchDashboardNamespaces(context.Background(), dashboardTestTarget(), "known-api-bearer", client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Namespaces) != 1 || !result.Truncated {
		t.Fatalf("namespaces=%v truncated=%v; totalItems 5 must report partial", result.Namespaces, result.Truncated)
	}
}

func TestDashboardTargetValidationRejectsUnsafeAddresses(t *testing.T) {
	base := dashboardTestTarget()
	if err := validateDashboardTarget(base); err != nil {
		t.Fatalf("valid Dashboard target rejected: %v", err)
	}
	for _, address := range []string{
		"http://dashboard.example.invalid",
		"https://user:password@dashboard.example.invalid",
		"https://dashboard.example.invalid?path=/other",
		"https://dashboard.example.invalid/#fragment",
		"https://dashboard.example.invalid/../other",
	} {
		target := base
		target.BaseURL = address
		if err := validateDashboardTarget(target); err == nil {
			t.Errorf("unsafe Dashboard address accepted: %q", address)
		}
	}
	target := base
	target.ID = "harbor:0123456789abcdef0123456789abcdef"
	if err := validateDashboardTarget(target); err == nil {
		t.Fatal("target with wrong adapter ID accepted")
	}
}

func TestDashboardTargetSaveEditDisableDeleteLifecycle(t *testing.T) {
	const token = "dashboard_saved_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	secrets := memorySecrets{}
	a := &app{configPath: path, secrets: secrets}
	id, err := a.saveDashboardTarget("", "QA cluster", "https://dashboard.example.invalid/proxy", token)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.DashboardTargets) != 1 {
		t.Fatalf("saved Dashboard targets=%#v err=%v", cfg.DashboardTargets, err)
	}
	ref := cfg.DashboardTargets[0].SecretRef
	contents, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(contents), token) {
		t.Fatalf("credential reached settings file: err=%v", err)
	}
	if string(secrets[ref]) != token {
		t.Fatal("Dashboard token was not saved through the secret store")
	}
	if _, err := a.saveDashboardTarget(id, "QA cluster", "https://dashboard.example.invalid/proxy", ""); err != nil {
		t.Fatalf("edit without rotating token: %v", err)
	}
	if _, err := a.saveDashboardTarget(id, "QA cluster", "https://new-dashboard.example.invalid", ""); err == nil {
		t.Fatal("changing origin without a replacement token succeeded")
	}
	if err := a.setTargetDisabled("dashboard", id, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.loadDashboardTarget("QA cluster"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled target load error = %v", err)
	}
	if err := a.setTargetDisabled("dashboard", id, false); err != nil {
		t.Fatal(err)
	}
	if err := a.deleteRegisteredTarget("dashboard", id); err != nil {
		t.Fatal(err)
	}
	deletedConfig, err := readConfig(path)
	if err != nil || len(deletedConfig.DashboardTargets) != 0 || len(deletedConfig.NamedSecrets) != 1 || deletedConfig.NamedSecrets[0].CredentialRef != ref || string(secrets[ref]) != token {
		t.Fatalf("deleted Dashboard target did not retain its reusable named secret: cfg=%#v secretPresent=%v err=%v", deletedConfig.NamedSecrets, secrets[ref] != nil, err)
	}
}

func TestDashboardMCPUsesOnlyRegisteredTargetAndCatalogHidesSecrets(t *testing.T) {
	const token = "dashboard_mcp_canary_0123456789"
	target := dashboardTestTarget()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, DashboardTargets: []dashboardTarget{target}}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "dashboard.example.invalid" || r.URL.EscapedPath() != "/proxy/context/api/v1/namespace" {
			t.Errorf("MCP request escaped registered target: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("MCP sent incorrect API Bearer: %q", r.Header.Get("Authorization"))
		}
		return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":1},"namespaces":[{"objectMeta":{"name":"default"},"token":"`+token+`"}]}`), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{target.SecretRef: []byte(token)}, client: client}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dashboard-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	catalog, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	if err != nil {
		t.Fatal(err)
	}
	catalogJSON, _ := json.Marshal(catalog)
	if strings.Contains(string(catalogJSON), token) || strings.Contains(string(catalogJSON), target.BaseURL) || !strings.Contains(string(catalogJSON), "dashboard_namespaces") {
		t.Fatalf("unexpected Dashboard target catalog: %s", catalogJSON)
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_namespaces", Arguments: map[string]any{"target": target.Name}})
	if err != nil || result.IsError {
		t.Fatalf("Dashboard MCP tool result=%#v err=%v", result, err)
	}
	resultJSON, _ := json.Marshal(result)
	if strings.Contains(string(resultJSON), token) || !strings.Contains(string(resultJSON), "default") {
		t.Fatalf("unexpected Dashboard MCP output: %s", resultJSON)
	}
	extra, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_namespaces", Arguments: map[string]any{"target": target.Name, "url": "https://other.example.invalid"}})
	if err == nil && extra != nil && !extra.IsError {
		t.Fatalf("MCP accepted caller-supplied URL: %#v", extra)
	}
	unknown, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_namespaces", Arguments: map[string]any{"target": "unregistered"}})
	if err == nil && unknown != nil && !unknown.IsError {
		t.Fatalf("MCP accepted an unregistered Dashboard target: %#v", unknown)
	}
	if requests != 1 {
		t.Fatalf("unregistered or invalid MCP inputs made additional requests: %d", requests)
	}
}

func TestDashboardDisabledTargetDoesNotLoadTokenOrCallRemote(t *testing.T) {
	target := dashboardTestTarget()
	target.Disabled = true
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, DashboardTargets: []dashboardTarget{target}}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	a := &app{
		configPath: path,
		secrets:    memorySecrets{},
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests++
			return harborTestResponse(r, http.StatusOK, `{"namespaces":[]}`), nil
		})},
	}
	if _, err := a.registeredDashboardNamespaces(context.Background(), target.Name); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled target error = %v", err)
	}
	if requests != 0 {
		t.Fatalf("disabled target made %d HTTP requests", requests)
	}
}

func TestDashboardUIFieldsAndPOSTRegistrationTest(t *testing.T) {
	const token = "dashboard_ui_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	a := &app{
		configPath: path,
		secrets:    memorySecrets{},
		client: &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.EscapedPath() != "/proxy/api/v1/namespace" || r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("connection test request = %s %s, authorization %q", r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"))
			}
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":1},"namespaces":[{"objectMeta":{"name":"default"}}]}`), nil
		})},
		csrf: "csrf-token",
	}
	form := url.Values{
		"csrf":            {a.csrf},
		"target_id":       {"new"},
		"dashboard_name":  {"QA cluster"},
		"dashboard_url":   {"https://dashboard.example.invalid/proxy"},
		"dashboard_token": {token},
	}
	req := httptest.NewRequest(http.MethodPost, "/save-dashboard", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	a.handleDashboardSave(recorder, req)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("save status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.DashboardTargets) != 1 {
		t.Fatalf("saved Dashboard targets=%#v err=%v", cfg.DashboardTargets, err)
	}
	if strings.Contains(a.status, token) {
		t.Fatal("save status leaked Dashboard API Bearer")
	}
	root := httptest.NewRecorder()
	a.handleRoot(root, httptest.NewRequest(http.MethodGet, "/?dashboard_id="+url.QueryEscape(cfg.DashboardTargets[0].ID), nil))
	for _, want := range []string{
		`id="dashboard-selection"`, `action="/save-dashboard"`, `action="/test-dashboard"`,
		`for="dashboard-token"`, `aria-describedby="dashboard-token-hint`,
		`Dashboard's API Bearer from`, `raw Kubernetes/service-account tokens are unsupported`, `kind" value="dashboard"`,
	} {
		if !strings.Contains(root.Body.String(), want) {
			t.Errorf("rendered Dashboard UI missing %q", want)
		}
	}
	errorPage := httptest.NewRecorder()
	errorRequest := httptest.NewRequest(http.MethodGet, "/?dashboard_id="+url.QueryEscape(cfg.DashboardTargets[0].ID)+"&dashboard_error=token", nil)
	a.handleRoot(errorPage, errorRequest)
	for _, want := range []string{`aria-invalid="true"`, `id="dashboard-token-error"`, `href="#dashboard-token"`} {
		if !strings.Contains(errorPage.Body.String(), want) {
			t.Errorf("Dashboard field error missing %q", want)
		}
	}
	assertFirstInvalidFieldFocused(t, errorPage.Body.String(), "dashboard-token")
	testForm := url.Values{"csrf": {a.csrf}, "target_id": {cfg.DashboardTargets[0].ID}, "target": {cfg.DashboardTargets[0].Name}}
	testReq := httptest.NewRequest(http.MethodPost, "/test-dashboard", strings.NewReader(testForm.Encode()))
	testReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	testRecorder := httptest.NewRecorder()
	a.handleDashboardTest(testRecorder, testReq)
	if testRecorder.Code != http.StatusSeeOther || !strings.Contains(a.status, "Connection test succeeded. Historical status saved.") || strings.Contains(a.status, token) {
		t.Fatalf("Dashboard test status=%d result=%q", testRecorder.Code, a.status)
	}
	toggleForm := url.Values{"csrf": {a.csrf}, "kind": {"dashboard"}, "target_id": {cfg.DashboardTargets[0].ID}, "disabled": {"yes"}}
	toggleReq := httptest.NewRequest(http.MethodPost, "/toggle-target", strings.NewReader(toggleForm.Encode()))
	toggleReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	toggleRecorder := httptest.NewRecorder()
	a.handleToggleTarget(toggleRecorder, toggleReq)
	if toggleRecorder.Code != http.StatusSeeOther || toggleRecorder.Header().Get("Location") != "/?dashboard_id="+url.QueryEscape(cfg.DashboardTargets[0].ID) {
		t.Fatalf("Dashboard disable redirect = %d %q", toggleRecorder.Code, toggleRecorder.Header().Get("Location"))
	}
	cfg, err = readConfig(path)
	if err != nil || !cfg.DashboardTargets[0].Disabled {
		t.Fatalf("Dashboard target disabled=%v err=%v", len(cfg.DashboardTargets) == 1 && cfg.DashboardTargets[0].Disabled, err)
	}
	deleteForm := url.Values{"csrf": {a.csrf}, "kind": {"dashboard"}, "target_id": {cfg.DashboardTargets[0].ID}, "confirm_delete": {"yes"}}
	deleteReq := httptest.NewRequest(http.MethodPost, "/delete-target", strings.NewReader(deleteForm.Encode()))
	deleteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteRecorder := httptest.NewRecorder()
	a.handleDeleteTarget(deleteRecorder, deleteReq)
	if deleteRecorder.Code != http.StatusSeeOther || deleteRecorder.Header().Get("Location") != "/?dashboard_id=new" {
		t.Fatalf("Dashboard delete redirect = %d %q", deleteRecorder.Code, deleteRecorder.Header().Get("Location"))
	}
	deletedConfig, err := readConfig(path)
	if err != nil || len(deletedConfig.DashboardTargets) != 0 || len(deletedConfig.NamedSecrets) != 1 {
		t.Fatalf("deleted Dashboard target did not retain its reusable named-secret metadata: cfg=%#v err=%v", deletedConfig, err)
	}
	if got := string(a.secrets.(memorySecrets)[deletedConfig.NamedSecrets[0].CredentialRef]); got != token {
		t.Fatal("deleted Dashboard target's reusable credential was not retained")
	}
}

func TestDashboardInvalidAddressDoesNotRunConnectionTest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	secrets := memorySecrets{}
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
		"csrf":            {a.csrf},
		"target_id":       {"dashboard:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"dashboard_name":  {"Invalid address"},
		"dashboard_url":   {"http://dashboard.example.invalid"},
		"dashboard_token": {"dashboard_POST_canary_0123456789"},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-dashboard", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	response := httptest.NewRecorder()
	a.handleDashboardSave(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save status = %d", response.Code)
	}
	body := response.Body.String()
	for _, want := range []string{
		`<option value="new" selected>Add new target</option>`,
		`<input type="hidden" name="target_id" value="new">`,
		`id="dashboard-name" name="dashboard_name" value="Invalid address"`,
		`id="dashboard-url" name="dashboard_url" type="url" value="http://dashboard.example.invalid"`,
		`id="dashboard-url-error"`,
		`data-message-en="The Dashboard url field is invalid. A connection test was not run."`,
		`Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("validation page missing %q", want)
		}
	}
	assertFirstInvalidFieldFocused(t, body, "dashboard-url")
	if strings.Contains(body, "dashboard_POST_canary_0123456789") || strings.Contains(response.Header().Get("Location"), "dashboard_POST_canary_0123456789") {
		t.Fatal("Dashboard token was included in the validation response or redirect")
	}
	if response.Header().Get("Location") != "" {
		t.Fatalf("validation response unexpectedly redirected to %q", response.Header().Get("Location"))
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != localUIReferrerPolicy {
		t.Fatalf("validation response security headers = cache %q, referrer %q", response.Header().Get("Cache-Control"), response.Header().Get("Referrer-Policy"))
	}
	if a.status != "" {
		t.Fatalf("request-local validation changed shared status to %q", a.status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid address should not save settings, stat err = %v", err)
	}
	if len(secrets) != 0 {
		t.Fatal("invalid address should not save the credential")
	}
	if requests != 0 {
		t.Fatalf("invalid address made %d service requests", requests)
	}
}

func TestDashboardDuplicateNameValidationPreservesOnlySafeDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	credentialRef := "cred:0123456789abcdef0123456789abcdef"
	cfg := config{
		Version: configVersion,
		DashboardTargets: []dashboardTarget{{
			ID:        "dashboard:0123456789abcdef0123456789abcdef",
			Name:      "Existing cluster",
			BaseURL:   "https://dashboard.example.invalid/proxy",
			SecretRef: credentialRef,
		}},
	}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := memorySecrets{}
	a := &app{configPath: path, secrets: secrets, csrf: "csrf-token"}
	form := url.Values{
		"csrf":            {a.csrf},
		"target_id":       {"new"},
		"dashboard_name":  {"Existing cluster"},
		"dashboard_url":   {"https://draft-dashboard.example.invalid/proxy"},
		"dashboard_token": {"dashboard_duplicate_canary_0123456789"},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-dashboard", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleDashboardSave(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save status = %d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`id="dashboard-name" name="dashboard_name" value="Existing cluster"`,
		`id="dashboard-url" name="dashboard_url" type="url" value="https://draft-dashboard.example.invalid/proxy"`,
		`id="dashboard-name-error"`,
		`aria-invalid="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("duplicate-name validation page missing %q", want)
		}
	}
	assertFirstInvalidFieldFocused(t, body, "dashboard-name")
	if strings.Contains(body, "dashboard_duplicate_canary_0123456789") || strings.Contains(response.Header().Get("Location"), "dashboard_duplicate_canary_0123456789") {
		t.Fatal("Dashboard token was included in the validation response or redirect")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("duplicate-name validation changed saved settings")
	}
	if len(secrets) != 0 {
		t.Fatal("duplicate-name validation wrote a credential")
	}
}

func TestDashboardWhitespaceBearerValidationHasNoSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	secrets := memorySecrets{}
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
	const token = " dashboard_whitespace_canary_0123456789 "
	form := url.Values{
		"csrf":            {a.csrf},
		"target_id":       {"new"},
		"dashboard_name":  {"Whitespace token"},
		"dashboard_url":   {"https://dashboard.example.invalid/proxy"},
		"dashboard_token": {token},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-dashboard", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleDashboardSave(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save status = %d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`id="dashboard-token-error"`,
		`id="dashboard-token"`,
		`aria-invalid="true"`,
		`The Dashboard token field is invalid. A connection test was not run.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("whitespace-token validation page missing %q", want)
		}
	}
	assertFirstInvalidFieldFocused(t, body, "dashboard-token")
	if strings.Contains(body, strings.TrimSpace(token)) || strings.Contains(response.Header().Get("Location"), strings.TrimSpace(token)) {
		t.Fatal("Dashboard Bearer appeared in the validation response or redirect")
	}
	if response.Header().Get("Location") != "" {
		t.Fatalf("validation response unexpectedly redirected to %q", response.Header().Get("Location"))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid token should not save settings, stat err = %v", err)
	}
	if len(secrets) != 0 || requests != 0 || a.status != "" {
		t.Fatalf("validation side effects: secrets=%d requests=%d shared status=%q", len(secrets), requests, a.status)
	}
}

func TestDashboardEditValidationKeepsSavedCredentialHintAndDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	target := dashboardTarget{
		ID:        "dashboard:0123456789abcdef0123456789abcdef",
		Name:      "Existing cluster",
		BaseURL:   "https://dashboard.example.invalid/proxy",
		SecretRef: "cred:0123456789abcdef0123456789abcdef",
	}
	if err := writeConfig(path, config{Version: configVersion, DashboardTargets: []dashboardTarget{target}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := memorySecrets{}
	a := &app{configPath: path, secrets: secrets, csrf: "csrf-token"}
	form := url.Values{
		"csrf":            {a.csrf},
		"target_id":       {target.ID},
		"dashboard_name":  {"Draft edited cluster"},
		"dashboard_url":   {"https://draft-dashboard.example.invalid/proxy"},
		"dashboard_token": {""},
	}
	request := httptest.NewRequest(http.MethodPost, "/save-dashboard", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleDashboardSave(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("save status = %d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `id="dashboard-name" name="dashboard_name" value="Draft edited cluster"`) {
		t.Fatal("Dashboard edit draft was not preserved")
	}
	if !strings.Contains(body, `Saved in Windows Credential Manager. Leave blank to keep it.`) {
		t.Fatal("existing Dashboard credential hint was lost while rendering the draft")
	}
	if !strings.Contains(body, `id="dashboard-token-error"`) || !strings.Contains(body, `id="dashboard-token"`) || !strings.Contains(body, `aria-invalid="true"`) {
		t.Fatal("missing replacement-token error was not attached to the Dashboard token field")
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `id="dashboard-token"`) && strings.Contains(line, " required") {
			t.Fatal("saved Dashboard credential input was incorrectly marked required")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("validation changed the saved Dashboard target")
	}
	if len(secrets) != 0 {
		t.Fatal("validation wrote a Dashboard credential")
	}
}

func dashboardStatusTestBinding() dashboardServiceBinding {
	cfg := serviceBundleConfig()
	target := cfg.DashboardTargets[0]
	environment := cfg.ServiceBundles[0].Environments[0]
	return dashboardServiceBinding{
		ServiceBundle: cfg.ServiceBundles[0].Name,
		Environment:   environment.Name,
		Target:        target,
		Namespace:     environment.DashboardNamespace,
		Deployment:    environment.DashboardDeployment,
	}
}

func TestDashboardDeploymentStatusUsesRegisteredBundleMapping(t *testing.T) {
	const token = "dashboard_status_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.Host != "dashboard.example.invalid" || r.URL.EscapedPath() != "/proxy/api/v1/deployment/apps/api.v2" || r.URL.RawQuery != "" {
			t.Errorf("Dashboard request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Dashboard authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Dashboard accept = %q", got)
		}
		body := `{"objectMeta":{"name":"api.v2","namespace":"apps","annotations":{"password":"` + token + `"}},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[{"type":"Available","status":"False","lastProbeTime":"2026-09-26T01:02:03Z","lastTransitionTime":"2026-09-26T01:02:03Z","reason":"MinimumReplicasUnavailable","message":"` + token + `"}],"strategy":"RollingUpdate","errors":[]}`
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{cfg.DashboardTargets[0].SecretRef: []byte(token)}, client: client}
	result, err := a.registeredDashboardDeploymentStatus(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.ServiceBundle != "Inventory" || result.Environment != "qa-blue" || result.Target != "Cluster UI" || result.Namespace != "apps" || result.Deployment != "api.v2" {
		t.Fatalf("unexpected Dashboard binding/result: requests=%d result=%+v", requests, result)
	}
	if result.Replicas != 4 || result.Updated != 3 || result.Available != 2 || result.Unavailable != 2 || len(result.Conditions) != 1 || result.Conditions[0].Type != "Available" || result.Conditions[0].Status != "False" || result.Conditions[0].Reason != "MinimumReplicasUnavailable" {
		t.Fatalf("unexpected Dashboard status projection: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "annotations") || strings.Contains(string(encoded), "message") || len(encoded) > maxAPIBytes {
		t.Fatalf("Dashboard status leaked unprojected fields or exceeded output limit: %s", encoded)
	}
}

func TestDashboardDeploymentStatusMCPOnlyAcceptsBundleAndEnvironmentScope(t *testing.T) {
	const token = "dashboard_mcp_status_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.EscapedPath() != "/proxy/api/v1/deployment/apps/api.v2" {
			t.Errorf("MCP request escaped saved service mapping: %s", r.URL)
		}
		return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":1,"updated":1,"available":1,"unavailable":0},"conditions":[]}`), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{cfg.DashboardTargets[0].SecretRef: []byte(token)}, client: client}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dashboard-status-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_status", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"}})
	if err != nil || result.IsError {
		t.Fatalf("Dashboard status MCP result=%#v err=%v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), token) || !strings.Contains(string(encoded), `"available_replicas":1`) || requests != 1 {
		t.Fatalf("unexpected Dashboard status MCP result: requests=%d result=%s", requests, encoded)
	}
	extra, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_status", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue", "namespace": "other", "deployment": "other"}})
	if err == nil && extra != nil && !extra.IsError && requests != 2 {
		t.Fatalf("extra MCP resource arguments changed or bypassed the saved scope: requests=%d", requests)
	}
}

func TestDashboardDeploymentStatusRejectsUnmappedOrDisabledScopeBeforeSecretLoad(t *testing.T) {
	tests := []struct {
		name        string
		bundle      string
		environment string
		mutate      func(*config)
	}{
		{name: "unknown bundle", bundle: "missing", environment: "qa-blue"},
		{name: "unknown environment", bundle: "Inventory", environment: "missing"},
		{name: "disabled Dashboard target", bundle: "Inventory", environment: "qa-blue", mutate: func(cfg *config) { cfg.DashboardTargets[0].Disabled = true }},
		{name: "missing resource mapping", bundle: "Inventory", environment: "qa-blue", mutate: func(cfg *config) {
			cfg.ServiceBundles[0].Environments[0].DashboardTargetID = ""
			cfg.ServiceBundles[0].Environments[0].DashboardNamespace = ""
			cfg.ServiceBundles[0].Environments[0].DashboardDeployment = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			secrets := &serviceBundleSecrets{}
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				return harborTestResponse(r, http.StatusOK, `{}`), nil
			})}
			a := &app{configPath: path, secrets: secrets, client: client}
			_, err := a.registeredDashboardDeploymentStatus(context.Background(), test.bundle, test.environment)
			if err == nil || secrets.loads != 0 || requests != 0 {
				t.Fatalf("invalid mapping error=%v secret loads=%d requests=%d", err, secrets.loads, requests)
			}
		})
	}
}

func TestDashboardDeploymentStatusRejectsRedirectsHTTPFailuresAndTimeouts(t *testing.T) {
	const token = "dashboard_status_error_canary_0123456789"
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "redirect", status: http.StatusFound},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "server error", status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := harborTestResponse(r, test.status, token)
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/collect")
				}
				return resp, nil
			})}
			_, err := fetchDashboardDeploymentStatus(context.Background(), dashboardStatusTestBinding(), token, client)
			if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "other.example.invalid") || requests != 1 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	_, err := fetchDashboardDeploymentStatus(ctx, dashboardStatusTestBinding(), token, client)
	if err == nil || !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), token) {
		t.Fatalf("timeout result error=%v", err)
	}
}

func TestDashboardDeploymentStatusStrictlyValidatesAndBoundsProjection(t *testing.T) {
	binding := dashboardStatusTestBinding()
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"statusInfo":`},
		{name: "missing identity", body: `{"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[]}`},
		{name: "wrong deployment", body: `{"objectMeta":{"name":"other","namespace":"apps"},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[]}`},
		{name: "wrong namespace", body: `{"objectMeta":{"name":"api.v2","namespace":"other"},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[]}`},
		{name: "missing replica field", body: `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":4,"available":2,"unavailable":2},"conditions":[]}`},
		{name: "negative replicas", body: `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":-1,"updated":0,"available":0,"unavailable":0},"conditions":[]}`},
		{name: "missing conditions", body: `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2}}`},
		{name: "invalid condition status", body: `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[{"type":"Available","status":"Ready","lastTransitionTime":"2026-09-26T01:02:03Z","reason":"Ready"}]}`},
		{name: "invalid condition time", body: `{"objectMeta":{"name":"api.v2","namespace":"apps"},"statusInfo":{"replicas":4,"updated":3,"available":2,"unavailable":2},"conditions":[{"type":"Available","status":"True","lastTransitionTime":"never","reason":"Ready"}]}`},
		{name: "oversized", body: strings.Repeat("x", maxAPIBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				return harborTestResponse(r, http.StatusOK, test.body), nil
			})}
			_, err := fetchDashboardDeploymentStatus(context.Background(), binding, "dashboard_decode_canary", client)
			if err == nil || strings.Contains(err.Error(), "dashboard_decode_canary") || strings.Contains(err.Error(), "never") {
				t.Fatalf("invalid status accepted or leaked input: %v", err)
			}
		})
	}

	conditions := make([]map[string]string, dashboardDeploymentConditionLimit+1)
	for i := range conditions {
		conditions[i] = map[string]string{"type": "Available", "status": "True", "lastTransitionTime": "2026-09-26T01:02:03Z", "reason": "MinimumReplicasAvailable", "message": "not projected"}
	}
	body, err := json.Marshal(map[string]any{
		"objectMeta": map[string]string{"name": binding.Deployment, "namespace": binding.Namespace},
		"statusInfo": map[string]int{"replicas": 1, "updated": 1, "available": 1, "unavailable": 0},
		"conditions": conditions,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		return harborTestResponse(r, http.StatusOK, string(body)), nil
	})}
	result, err := fetchDashboardDeploymentStatus(context.Background(), binding, "known-bearer", client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conditions) != dashboardDeploymentConditionLimit || !result.Truncated {
		t.Fatalf("conditions=%d truncated=%v", len(result.Conditions), result.Truncated)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "not projected") || len(encoded) > maxAPIBytes {
		t.Fatalf("unbounded or unprojected output: err=%v body=%s", err, encoded)
	}
	if _, err := dashboardDeploymentStatusURL(binding.Target, "../other", "api.v2"); err == nil {
		t.Fatal("unsafe namespace accepted by Dashboard URL builder")
	}
}

func dashboardDeploymentEventFixture(index int, canary string) map[string]any {
	return map[string]any{
		"objectMeta": map[string]any{"name": "api-v2-event-" + strconv.Itoa(index), "namespace": "apps", "annotations": map[string]string{"token": canary}},
		"type":       "Warning", "reason": "FailedScheduling", "count": 3,
		"firstSeen": "2026-09-26T01:02:03Z", "lastSeen": "2026-09-26T01:05:06Z",
		"objectName": "api.v2", "message": canary, "sourceComponent": canary, "sourceHost": canary,
		"object": "pod/api-v2", "objectKind": "Pod", "objectNamespace": "apps",
	}
}

func dashboardDeploymentEventList(total int, events []map[string]any, errorsValue any) string {
	body, _ := json.Marshal(map[string]any{
		"listMeta": map[string]int{"totalItems": total},
		"events":   events,
		"errors":   errorsValue,
	})
	return string(body)
}

func dashboardHasAction(actions []string, name string) bool {
	for _, action := range actions {
		if action == name {
			return true
		}
	}
	return false
}

func TestDashboardDeploymentEventsProjectsAndBoundsResponse(t *testing.T) {
	const token = "dashboard_events_canary_0123456789"
	binding := dashboardStatusTestBinding()
	events := make([]map[string]any, dashboardDeploymentEventLimit+1)
	for i := range events {
		events[i] = dashboardDeploymentEventFixture(i, token)
	}
	body := dashboardDeploymentEventList(len(events), events, nil)
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.Host != "dashboard.example.invalid" || r.URL.EscapedPath() != "/proxy/api/v1/deployment/apps/api.v2/event" || r.URL.RawQuery != "itemsPerPage=100&page=1" {
			t.Errorf("Dashboard events request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Dashboard events authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Dashboard events accept = %q", got)
		}
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	result, err := fetchDashboardDeploymentEvents(context.Background(), binding, token, client)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(result.Events) != dashboardDeploymentEventLimit || !result.Truncated {
		t.Fatalf("events=%d truncated=%v requests=%d", len(result.Events), result.Truncated, requests)
	}
	first := result.Events[0]
	if first.Name != "api-v2-event-0" || first.Type != "Warning" || first.Reason != "FailedScheduling" || first.Count != 3 || first.FirstSeen != "2026-09-26T01:02:03Z" || first.LastSeen != "2026-09-26T01:05:06Z" || first.ObjectName != "api.v2" {
		t.Fatalf("unexpected event projection: %+v", first)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, "message", "sourceComponent", "sourceHost", "annotations", "objectKind", "objectNamespace"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("event result contains unprojected field %q: %s", forbidden, encoded)
		}
	}
	if len(encoded) > maxAPIBytes {
		t.Fatalf("event result exceeds output limit: %d", len(encoded))
	}
}

func TestDashboardDeploymentEventsRejectMalformedAndInconsistentResponses(t *testing.T) {
	const canary = "dashboard_event_error_canary_0123456789"
	valid := dashboardDeploymentEventFixture(0, canary)
	missingCount := dashboardDeploymentEventFixture(0, canary)
	delete(missingCount, "count")
	invalidTimestamp := dashboardDeploymentEventFixture(0, canary)
	invalidTimestamp["firstSeen"] = "not-a-time"
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"events":`},
		{name: "missing list metadata", body: `{"events":[]}`},
		{name: "missing events", body: `{"listMeta":{"totalItems":0}}`},
		{name: "negative total", body: dashboardDeploymentEventList(-1, []map[string]any{}, nil)},
		{name: "total smaller than returned count", body: dashboardDeploymentEventList(0, []map[string]any{valid}, nil)},
		{name: "missing count", body: dashboardDeploymentEventList(1, []map[string]any{missingCount}, nil)},
		{name: "negative count", body: dashboardDeploymentEventList(1, []map[string]any{map[string]any{"objectMeta": map[string]any{"name": "event-0"}, "type": "Warning", "reason": "FailedScheduling", "count": -1, "firstSeen": "2026-09-26T01:02:03Z", "lastSeen": "2026-09-26T01:05:06Z"}}, nil)},
		{name: "invalid timestamp", body: dashboardDeploymentEventList(1, []map[string]any{invalidTimestamp}, nil)},
		{name: "upstream partial error", body: dashboardDeploymentEventList(1, []map[string]any{valid}, []string{canary})},
		{name: "oversized body", body: strings.Repeat("x", maxAPIBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				return harborTestResponse(r, http.StatusOK, test.body), nil
			})}
			_, err := fetchDashboardDeploymentEvents(context.Background(), dashboardStatusTestBinding(), canary, client)
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("invalid event response accepted or leaked input: %v", err)
			}
		})
	}
}

func TestDashboardDeploymentEventsRejectRedirectsFailuresAndTimeouts(t *testing.T) {
	const token = "dashboard_events_failure_canary_0123456789"
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "redirect", status: http.StatusFound},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "server error", status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := harborTestResponse(r, test.status, token)
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/collect")
				}
				return resp, nil
			})}
			_, err := fetchDashboardDeploymentEvents(context.Background(), dashboardStatusTestBinding(), token, client)
			if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "other.example.invalid") || requests != 1 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	_, err := fetchDashboardDeploymentEvents(ctx, dashboardStatusTestBinding(), token, client)
	if err == nil || !strings.Contains(err.Error(), "timed out") || strings.Contains(err.Error(), token) {
		t.Fatalf("timeout result error=%v", err)
	}
}

func TestDashboardDeploymentEventsMCPAndCatalogAreBundleScoped(t *testing.T) {
	const token = "dashboard_events_mcp_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() != "/proxy/api/v1/deployment/apps/api.v2/event" || r.URL.RawQuery != "itemsPerPage=100&page=1" {
			t.Errorf("MCP request escaped the saved Dashboard mapping: %s", r.URL)
		}
		body := dashboardDeploymentEventList(1, []map[string]any{dashboardDeploymentEventFixture(0, token)}, nil)
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{cfg.DashboardTargets[0].SecretRef: []byte(token)}, client: client}
	catalog, err := a.registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	dashboard := catalog.Bundles[0].Environments[0].Dashboard
	if dashboard == nil || !dashboardHasAction(dashboard.Actions, "dashboard_deployment_events") {
		t.Fatalf("active mapped environment did not advertise event lookup: %+v", dashboard)
	}
	catalogJSON, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(catalogJSON), "apps") || strings.Contains(string(catalogJSON), "api.v2") {
		t.Fatalf("service catalog exposed Dashboard resource names: %s", catalogJSON)
	}
	targets, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets.Targets) == 0 || targets.Targets[0].Type != "dashboard" || len(targets.Targets[0].Actions) != 2 || targets.Targets[0].Actions[0] != "dashboard_namespaces" || targets.Targets[0].Actions[1] != "registered_target_connection_test" {
		t.Fatalf("registered_targets Dashboard actions changed: %+v", targets.Targets)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dashboard-events-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_events", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"}})
	if err != nil || result.IsError {
		t.Fatalf("Dashboard events MCP result=%#v err=%v", result, err)
	}
	encoded, _ := json.Marshal(result)
	for _, forbidden := range []string{token, "sourceHost", "sourceComponent", "annotations"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("MCP event result leaked %q: %s", forbidden, encoded)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*config)
	}{
		{name: "disabled target", mutate: func(cfg *config) { cfg.DashboardTargets[0].Disabled = true }},
		{name: "incomplete mapping", mutate: func(cfg *config) {
			cfg.ServiceBundles[0].Environments[0].DashboardNamespace = ""
			cfg.ServiceBundles[0].Environments[0].DashboardDeployment = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPath := filepath.Join(t.TempDir(), "config.json")
			testConfig := serviceBundleConfig()
			test.mutate(&testConfig)
			if err := writeConfig(testPath, testConfig); err != nil {
				t.Fatal(err)
			}
			view, err := (&app{configPath: testPath}).registeredServiceBundles()
			if err != nil {
				t.Fatal(err)
			}
			mapped := view.Bundles[0].Environments[0].Dashboard
			if mapped == nil || dashboardHasAction(mapped.Actions, "dashboard_deployment_events") {
				t.Fatalf("invalid scope advertised events: %+v", mapped)
			}
		})
	}
}

func TestDashboardDeploymentEventsRejectsUnmappedOrDisabledScopeBeforeSecretLoad(t *testing.T) {
	tests := []struct {
		name        string
		bundle      string
		environment string
		mutate      func(*config)
	}{
		{name: "unknown bundle", bundle: "missing", environment: "qa-blue"},
		{name: "unknown environment", bundle: "Inventory", environment: "missing"},
		{name: "disabled Dashboard target", bundle: "Inventory", environment: "qa-blue", mutate: func(cfg *config) { cfg.DashboardTargets[0].Disabled = true }},
		{name: "missing resource mapping", bundle: "Inventory", environment: "qa-blue", mutate: func(cfg *config) {
			mapping := &cfg.ServiceBundles[0].Environments[0]
			mapping.DashboardTargetID = ""
			mapping.DashboardNamespace = ""
			mapping.DashboardDeployment = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := serviceBundleConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			if err := writeConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			secrets := &serviceBundleSecrets{}
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"events":[]}`), nil
			})}
			a := &app{configPath: path, secrets: secrets, client: client}
			_, eventErr := a.registeredDashboardDeploymentEvents(context.Background(), test.bundle, test.environment)
			_, podErr := a.registeredDashboardDeploymentPods(context.Background(), test.bundle, test.environment)
			if eventErr == nil || podErr == nil || secrets.loads != 0 || requests != 0 {
				t.Fatalf("invalid Dashboard mapping event_error=%v pod_error=%v secret loads=%d requests=%d", eventErr, podErr, secrets.loads, requests)
			}
		})
	}
}

func dashboardPodFixture(name, namespace, status, phase, node, containerName, containerState string, restarts int32) map[string]any {
	return map[string]any{
		"objectMeta": map[string]any{"name": name, "namespace": namespace, "annotations": map[string]string{"private": "must-not-escape"}},
		"status":     status, "restartCount": restarts, "nodeName": node,
		"containerStatuses": []map[string]any{{"name": containerName, "state": containerState, "ready": phase == "Running"}},
		"warnings":          []string{"must-not-escape"},
	}
}

func dashboardPodListFixture(total int, pods []map[string]any) string {
	body, _ := json.Marshal(map[string]any{
		"listMeta":          map[string]int{"totalItems": total},
		"pods":              pods,
		"errors":            []string{},
		"cumulativeMetrics": []string{"must-not-escape"},
	})
	return string(body)
}

func dashboardLogFixture(pod, container string, truncated bool, logs []map[string]any) string {
	body, _ := json.Marshal(map[string]any{
		"info":      map[string]any{"podName": pod, "containerName": container, "truncated": truncated, "fromDate": "unprojected"},
		"logs":      logs,
		"selection": map[string]any{"referencePoint": map[string]any{"timestamp": "unprojected", "lineNum": 1}},
		"secret":    "must-not-escape",
	})
	return string(body)
}

func TestDashboardDeploymentPodsUseMappedReplicaSetsAndProjectBoundedStatus(t *testing.T) {
	const token = "dashboard_pods_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.Host != "dashboard.example.invalid" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Dashboard request = %s %s authorization=%q", r.Method, r.URL, r.Header.Get("Authorization"))
		}
		var body string
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			if r.URL.RawQuery != "" {
				t.Errorf("new ReplicaSet query = %q", r.URL.RawQuery)
			}
			body = `{"objectMeta":{"name":"api-v2-abc"}}`
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			if r.URL.RawQuery != "itemsPerPage=16&page=1" {
				t.Errorf("old ReplicaSet query = %q", r.URL.RawQuery)
			}
			body = `{"listMeta":{"totalItems":1},"replicaSets":[{"objectMeta":{"name":"api-v2-old"}}],"errors":[]}`
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			if r.URL.RawQuery != "itemsPerPage=100&page=1" {
				t.Errorf("current ReplicaSet Pod query = %q", r.URL.RawQuery)
			}
			body = dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", "apps", "CrashLoopBackOff", "Running", "node-1", "web", "Waiting", 4)})
		case "/proxy/api/v1/replicaset/apps/api-v2-old/pod":
			if r.URL.RawQuery != "itemsPerPage=100&page=1" {
				t.Errorf("old ReplicaSet Pod query = %q", r.URL.RawQuery)
			}
			body = dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-old-y1", "apps", "Running", "Running", "node-2", "web", "Running", 0)})
		default:
			t.Errorf("unexpected Dashboard route %s", r.URL)
			return harborTestResponse(r, http.StatusNotFound, token), nil
		}
		return harborTestResponse(r, http.StatusOK, body), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{cfg.DashboardTargets[0].SecretRef: []byte(token)}, client: client}
	result, err := a.registeredDashboardDeploymentPods(context.Background(), "Inventory", "qa-blue")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(result.Pods) != 2 || result.Truncated {
		t.Fatalf("requests=%d pods=%+v truncated=%v", requests, result.Pods, result.Truncated)
	}
	if result.Pods[0].Name != "api-v2-abc-x1" || result.Pods[0].Status != "CrashLoopBackOff" || result.Pods[0].Node != "node-1" || result.Pods[0].RestartCount != 4 || len(result.Pods[0].Containers) != 1 || result.Pods[0].Containers[0].State != "Waiting" {
		t.Fatalf("unexpected projected current Pod status: %+v", result.Pods[0])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "must-not-escape") || strings.Contains(string(encoded), "annotations") || strings.Contains(string(encoded), "warnings") || strings.Contains(string(encoded), "cumulativeMetrics") || len(encoded) > maxAPIBytes {
		t.Fatalf("Pod result leaked unprojected fields or exceeded output cap: %s", encoded)
	}
}

func TestDashboardDeploymentPodsCapsPodInventoryAndRejectsWrongNamespace(t *testing.T) {
	binding := dashboardStatusTestBinding()
	pods := make([]map[string]any, dashboardDeploymentPodLimit+1)
	for i := range pods {
		pods[i] = dashboardPodFixture("api-v2-abc-"+strconv.Itoa(i), binding.Namespace, "Running", "Running", "node-1", "web", "Running", 0)
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api-v2-abc"}}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`), nil
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			return harborTestResponse(r, http.StatusOK, dashboardPodListFixture(len(pods), pods)), nil
		default:
			t.Errorf("unexpected Dashboard route %s", r.URL)
			return harborTestResponse(r, http.StatusNotFound, ""), nil
		}
	})}
	result, err := fetchDashboardDeploymentPods(context.Background(), binding, "", client)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pods) != dashboardDeploymentPodLimit || !result.Truncated {
		t.Fatalf("pods=%d truncated=%v, want cap=%d and truncated", len(result.Pods), result.Truncated, dashboardDeploymentPodLimit)
	}

	wrongNamespace := dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", "other", "Running", "Running", "node-1", "web", "Running", 0)})
	client = &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api-v2-abc"}}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`), nil
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			return harborTestResponse(r, http.StatusOK, wrongNamespace), nil
		default:
			return harborTestResponse(r, http.StatusNotFound, ""), nil
		}
	})}
	if _, err := fetchDashboardDeploymentPods(context.Background(), binding, "", client); err == nil {
		t.Fatal("Pod from a different namespace accepted")
	}
}

func TestDashboardDeploymentPodsMCPAndCatalogAreBundleScoped(t *testing.T) {
	const token = "dashboard_pods_mcp_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api-v2-abc"}}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`), nil
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			return harborTestResponse(r, http.StatusOK, dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", "apps", "Running", "Running", "node-1", "web", "Running", 0)})), nil
		case "/proxy/api/v1/log/apps/api-v2-abc-x1/web":
			if r.URL.RawQuery != "logFilePosition=end&offsetFrom=-99&offsetTo=1&referenceLineNum=0&referenceTimestamp=newest" {
				t.Errorf("Dashboard log query = %q", r.URL.RawQuery)
			}
			logs := []map[string]any{
				{"timestamp": "2026-09-26T01:02:03Z", "content": "request completed with token " + token},
				{"timestamp": "2026-09-26T01:02:03Z", "content": "request completed with token " + token},
				{"timestamp": "2026-09-26T01:02:04Z", "content": "Authorization: Bearer " + token},
			}
			return harborTestResponse(r, http.StatusOK, dashboardLogFixture("api-v2-abc-x1", "web", false, logs)), nil
		default:
			t.Errorf("MCP request escaped saved Dashboard mapping: %s", r.URL)
			return harborTestResponse(r, http.StatusNotFound, token), nil
		}
	})}
	a := &app{configPath: path, secrets: memorySecrets{cfg.DashboardTargets[0].SecretRef: []byte(token)}, client: client}
	catalog, err := a.registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	dashboard := catalog.Bundles[0].Environments[0].Dashboard
	if dashboard == nil || !dashboardHasAction(dashboard.Actions, "dashboard_deployment_pods") {
		t.Fatalf("mapped environment did not advertise Pod status: %+v", dashboard)
	}
	if !dashboardHasAction(dashboard.Actions, "dashboard_deployment_pod_logs") {
		t.Fatalf("mapped environment did not advertise bounded Pod logs: %+v", dashboard)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dashboard-pods-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_pods", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"}})
	if err != nil || result.IsError {
		t.Fatalf("Dashboard Pod MCP result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "annotations") || strings.Contains(string(encoded), "apps/api.v2") {
		t.Fatalf("MCP Pod status leaked credentials or mapping details: %s", encoded)
	}
	if requests != 3 {
		t.Fatalf("valid scoped call made %d Dashboard requests, want 3", requests)
	}
	extra, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_pods", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue", "namespace": "other"}})
	if err != nil || !extra.IsError || requests != 3 {
		t.Fatalf("unmapped MCP fields result=%#v err=%v requests=%d", extra, err, requests)
	}
	logs, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_pod_logs", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue", "pod": "api-v2-abc-x1", "container": "web"}})
	if err != nil || logs.IsError {
		t.Fatalf("Dashboard bounded Pod logs MCP result=%#v err=%v", logs, err)
	}
	logJSON, err := json.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 7 || strings.Contains(string(logJSON), token) || strings.Contains(string(logJSON), "must-not-escape") || strings.Contains(string(logJSON), "selection") || strings.Contains(string(logJSON), "fromDate") || strings.Count(string(logJSON), "request completed") != 2 || !strings.Contains(string(logJSON), "[REDACTED]") {
		t.Fatalf("MCP Pod log result leaked, failed deduplication, or escaped inventory: requests=%d result=%s", requests, logJSON)
	}
	extraLogs, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "dashboard_deployment_pod_logs", Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue", "pod": "api-v2-abc-x1", "container": "web", "offset_from": -99999}})
	if err != nil || !extraLogs.IsError || requests != 7 {
		t.Fatalf("unbounded log offset result=%#v err=%v requests=%d", extraLogs, err, requests)
	}
}

func TestDashboardGetJSONRejectsRedirectsFailuresAndOversizedBodiesWithoutLeaking(t *testing.T) {
	const token = "dashboard_pod_error_canary_0123456789"
	endpoint, err := dashboardAPIURL(dashboardTestTarget(), "replicaset/apps/api-v2/pod")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "redirect", status: http.StatusFound, body: token},
		{name: "unauthorized", status: http.StatusUnauthorized, body: token},
		{name: "forbidden", status: http.StatusForbidden, body: token},
		{name: "server error", status: http.StatusInternalServerError, body: token},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", maxAPIBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := harborTestResponse(r, test.status, test.body)
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/collect")
				}
				return resp, nil
			})}
			_, err := dashboardGetJSON(context.Background(), client, endpoint, token, "ReplicaSet Pods")
			if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), test.body) || requests != 1 {
				t.Fatalf("error=%v requests=%d; expected one bounded non-leaking request", err, requests)
			}
		})
	}
}

func TestDashboardDeploymentLogsCapBytesAndRequireAnInventoryContainer(t *testing.T) {
	binding := dashboardStatusTestBinding()
	const token = "dashboard_log_limit_canary_0123456789"
	logs := make([]map[string]any, dashboardLogLineLimit)
	for i := range logs {
		logs[i] = map[string]any{"timestamp": "2026-09-26T01:02:03Z", "content": strings.Repeat("x", dashboardLogLineBytesLimit) + strconv.Itoa(i)}
	}
	logBody := dashboardLogFixture("api-v2-abc-x1", "web", false, logs)
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		switch r.URL.EscapedPath() {
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"objectMeta":{"name":"api-v2-abc"}}`), nil
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			return harborTestResponse(r, http.StatusOK, `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`), nil
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			return harborTestResponse(r, http.StatusOK, dashboardPodListFixture(1, []map[string]any{dashboardPodFixture("api-v2-abc-x1", binding.Namespace, "Running", "Running", "node-1", "web", "Running", 0)})), nil
		case "/proxy/api/v1/log/apps/api-v2-abc-x1/web":
			return harborTestResponse(r, http.StatusOK, logBody), nil
		default:
			t.Errorf("unexpected Dashboard route %s", r.URL)
			return harborTestResponse(r, http.StatusNotFound, token), nil
		}
	})}
	result, err := fetchDashboardDeploymentLogs(context.Background(), binding, token, "api-v2-abc-x1", "web", client)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > dashboardMCPResponseLimit || !result.Truncated || len(result.Lines) >= dashboardLogLineLimit || strings.Contains(string(encoded), token) {
		t.Fatalf("log output cap/truncation failed: bytes=%d lines=%d truncated=%v", len(encoded), len(result.Lines), result.Truncated)
	}

	requests = 0
	if _, err := fetchDashboardDeploymentLogs(context.Background(), binding, token, "api-v2-abc-x1", "not-registered", client); err == nil {
		t.Fatal("unregistered container was allowed to select a log route")
	}
	if requests != 3 {
		t.Fatalf("unregistered container made %d requests, want only bounded inventory discovery", requests)
	}
}
