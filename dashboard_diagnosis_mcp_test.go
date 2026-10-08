package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDashboardDeploymentDiagnosisMCPToolIsRegisteredAndScoped(t *testing.T) {
	const token = "dashboard_diagnosis_mcp_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	baseClient := dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests)
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() == "/proxy/api/v1/log/apps/api-v2-abc-x1/web" {
			requests.Add(1)
			logs := []map[string]any{{"timestamp": "2026-09-27T01:02:03Z", "content": "request completed with token " + token}}
			return harborTestResponse(r, http.StatusOK, dashboardLogFixture("api-v2-abc-x1", "web", false, logs)), nil
		}
		return baseClient.Transport.RoundTrip(r)
	})}
	a := &app{
		configPath: path,
		secrets:    secrets,
		client:     client,
	}
	ctx, clientSession := connectDashboardDiagnosisMCP(t, a)

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "dashboard_deployment_diagnosis",
		Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"},
	})
	if err != nil || result.IsError {
		t.Fatalf("Dashboard diagnosis MCP result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	data := string(encoded)
	if len(encoded) > dashboardDeploymentDiagnosisOutputLimit {
		t.Fatalf("full MCP response exceeded %d bytes: %d", dashboardDeploymentDiagnosisOutputLimit, len(encoded))
	}
	for _, expected := range []string{`"service_bundle":"Inventory"`, `"status":`, `"events":`, `"pods":`, `"checks":`} {
		if !strings.Contains(data, expected) {
			t.Errorf("MCP diagnosis omitted %q: %s", expected, data)
		}
	}
	if strings.Contains(data, token) || strings.Contains(data, "dashboard.example.invalid") || requests.Load() != 5 || secrets.loads.Load() != 1 {
		t.Fatalf("MCP diagnosis crossed its output or binding boundary: requests=%d secret loads=%d result=%s", requests.Load(), secrets.loads.Load(), data)
	}

	resultWithExtraInput, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "dashboard_deployment_diagnosis",
		Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue", "namespace": "other", "url": "https://other.example.invalid"},
	})
	if err == nil && (resultWithExtraInput == nil || !resultWithExtraInput.IsError) {
		t.Fatalf("extra MCP inputs were not rejected: result=%+v", resultWithExtraInput)
	}
	if requests.Load() != 5 || secrets.loads.Load() != 1 {
		t.Fatalf("extra MCP inputs reached the secret store or Dashboard: requests=%d secret loads=%d", requests.Load(), secrets.loads.Load())
	}

	var diagnosis dashboardDeploymentDiagnosisResult
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil || json.Unmarshal(structured, &diagnosis) != nil || diagnosis.Pods == nil || len(diagnosis.Pods.Pods) == 0 || len(diagnosis.Pods.Pods[0].Containers) == 0 {
		t.Fatalf("MCP diagnosis did not return a selectable registered Pod/container: %s err=%v", structured, err)
	}
	pod := diagnosis.Pods.Pods[0]
	container := pod.Containers[0]
	logs, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "dashboard_deployment_pod_logs",
		Arguments: map[string]any{"service_bundle": diagnosis.ServiceBundle, "environment": diagnosis.Environment, "pod": pod.Name, "container": container.Name},
	})
	if err != nil || logs.IsError {
		t.Fatalf("Dashboard logs after diagnosis result=%#v err=%v", logs, err)
	}
	logJSON, err := json.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 9 || secrets.loads.Load() != 2 || strings.Contains(string(logJSON), token) || !strings.Contains(string(logJSON), "[REDACTED]") {
		t.Fatalf("diagnosis-to-log flow escaped its registered Pod/container or secret boundary: requests=%d secret loads=%d result=%s", requests.Load(), secrets.loads.Load(), logJSON)
	}
}

func TestDashboardDeploymentDiagnosisMCPResponseStaysWithinWireLimit(t *testing.T) {
	const token = "dashboard_diagnosis_mcp_large_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	a := &app{
		configPath: path,
		secrets:    secrets,
		client:     dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests, true),
	}
	ctx, clientSession := connectDashboardDiagnosisMCP(t, a)
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "dashboard_deployment_diagnosis",
		Arguments: map[string]any{"service_bundle": "Inventory", "environment": "qa-blue"},
	})
	if err != nil || result.IsError {
		t.Fatalf("large bounded diagnosis result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > dashboardDeploymentDiagnosisOutputLimit || !strings.Contains(string(encoded), `"output_truncated":true`) {
		t.Fatalf("full MCP response is not within the wire limit and marked truncated: bytes=%d result=%s", len(encoded), encoded)
	}
	if requests.Load() != 5 || secrets.loads.Load() != 1 || strings.Contains(string(encoded), token) {
		t.Fatalf("large MCP diagnosis crossed read or secret boundary: requests=%d secret loads=%d", requests.Load(), secrets.loads.Load())
	}
}

func TestDashboardDeploymentLogsMCPResponseStaysWithinWireLimit(t *testing.T) {
	const token = "dashboard_log_mcp_large_canary_0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &dashboardDiagnosisSecretStore{ref: cfg.DashboardTargets[0].SecretRef, token: []byte(token)}
	var requests atomic.Int32
	baseClient := dashboardDiagnosisTestClient(t, token, http.StatusOK, &requests)
	fixtureLogs := make([]map[string]any, dashboardLogLineLimit)
	for i := range fixtureLogs {
		content := strings.Repeat("x", dashboardLogLineBytesLimit)
		if i == 0 {
			content = "TOKEN=" + token + " " + content
		}
		fixtureLogs[i] = map[string]any{
			"timestamp": time.Date(2026, 9, 28, 0, 0, i, 0, time.UTC).Format(time.RFC3339),
			"content":   content,
		}
	}
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() == "/proxy/api/v1/log/apps/api-v2-abc-x1/web" {
			requests.Add(1)
			if r.URL.Host != "dashboard.example.invalid" || r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("Dashboard log request escaped the saved target or credential: host=%q authorization=%q", r.URL.Host, r.Header.Get("Authorization"))
			}
			return harborTestResponse(r, http.StatusOK, dashboardLogFixture("api-v2-abc-x1", "web", false, fixtureLogs)), nil
		}
		return baseClient.Transport.RoundTrip(r)
	})}
	a := &app{configPath: path, secrets: secrets, client: client}
	ctx, clientSession := connectDashboardDiagnosisMCP(t, a)
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "dashboard_deployment_pod_logs",
		Arguments: map[string]any{
			"service_bundle": "Inventory",
			"environment":    "qa-blue",
			"pod":            "api-v2-abc-x1",
			"container":      "web",
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("large Dashboard logs MCP result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > dashboardMCPResponseLimit || !strings.Contains(string(encoded), `"truncated":true`) {
		t.Fatalf("full MCP logs response exceeded %d bytes or lacked truncation marker: bytes=%d result=%s", dashboardMCPResponseLimit, len(encoded), encoded)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var logs dashboardDeploymentLogsResult
	if err := json.Unmarshal(structured, &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Lines) == 0 || len(logs.Lines) >= dashboardLogLineLimit || !logs.Truncated {
		t.Fatalf("oversized MCP logs were not shortened and marked: lines=%d truncated=%v", len(logs.Lines), logs.Truncated)
	}
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "must-not-escape") || strings.Contains(string(encoded), "dashboard.example.invalid") {
		t.Fatalf("bounded MCP logs exposed a secret, unprojected field, or Dashboard URL: %s", encoded)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || requests.Load() == 0 || secrets.loads.Load() != 1 {
		t.Fatalf("MCP logs changed settings or skipped the bounded registered read: requests=%d secret loads=%d", requests.Load(), secrets.loads.Load())
	}
}

func connectDashboardDiagnosisMCP(t *testing.T, a *app) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "dashboard-diagnosis-mcp-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return ctx, clientSession
}
