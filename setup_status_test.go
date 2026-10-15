package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type setupStatusReadStore struct {
	onboardingController
	receipts []onboardingFirstReadReceipt
	err      error
}

func (s *setupStatusReadStore) ListFirstReads(context.Context) ([]onboardingFirstReadReceipt, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]onboardingFirstReadReceipt(nil), s.receipts...), nil
}

type setupStatusSecretSpy struct{ loads int }

func (s *setupStatusSecretSpy) Save(string, []byte) error { return errors.New("unexpected save") }
func (s *setupStatusSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, errors.New("unexpected load")
}
func (s *setupStatusSecretSpy) Delete(string) error { return errors.New("unexpected delete") }

type setupStatusRoundTripper struct{ calls int }

func (r *setupStatusRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("unexpected network request")
}

func TestFirstReadReadinessByTargetMatchesCurrentDefinitions(t *testing.T) {
	first := firstReadReceiptTestGitHubTarget()
	second := first
	second.ID = "github:1123456789abcdef0123456789abcdef"
	second.Name = "Operations"
	second.Repository = "ops/other"
	cfg := config{GitHubTargets: []target{first, second}}
	observedAt := time.Now().Add(-time.Minute).UTC()
	digest := firstReadDefinitionDigestForCall(cfg, "github", "github_repository", firstReadDefinitionSelector{TargetName: first.Name})
	receipt := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
		ObservedAt: observedAt, DefinitionDigest: digest,
	}
	reads := firstReadReadinessByTarget(cfg, []onboardingFirstReadReceipt{receipt})
	if got := reads[first.ID]; got.State != firstReadStateObserved || got.Tool != "github_repository" {
		t.Fatalf("first target read = %+v", got)
	}
	if got := reads[second.ID]; got.State != "" {
		t.Fatalf("unread target unexpectedly attributed: %+v", got)
	}

	cfg.GitHubTargets[0].Repository = "ops/replaced"
	if got := firstReadReadinessByTarget(cfg, []onboardingFirstReadReceipt{receipt})[first.ID]; got.State != "" {
		t.Fatalf("stale target definition remained attributed: %+v", got)
	}
}

func TestReadinessAtReceiptCapacityMarksMissingProofUnavailable(t *testing.T) {
	cfg := readinessConfigFixture()
	first := cfg.GitHubTargets[0]
	second := first
	second.ID = "github:1123456789abcdef0123456789abcdef"
	second.Name = "Operations"
	second.Repository = "ops/other"
	cfg.GitHubTargets = append(cfg.GitHubTargets, second)
	configPath := writeReadinessConfig(t, cfg)

	firstDigest := firstReadDefinitionDigestForCall(cfg, "github", "github_repository", firstReadDefinitionSelector{TargetName: first.Name})
	secondDigest := firstReadDefinitionDigestForCall(cfg, "github", "github_repository", firstReadDefinitionSelector{TargetName: second.Name})
	observedAt := time.Now().Add(-time.Minute).UTC()
	receipts := make([]onboardingFirstReadReceipt, 0, onboardingStoreMaxFirstReads)
	for index := 1; len(receipts) < onboardingStoreMaxFirstReads; index++ {
		digest := fmt.Sprintf("sha256:%064x", index)
		if digest == firstDigest || digest == secondDigest {
			continue
		}
		receipts = append(receipts, onboardingFirstReadReceipt{
			Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
			ObservedAt: observedAt, DefinitionDigest: digest,
		})
	}

	readStatus := func(receipts []onboardingFirstReadReceipt) readinessStatusDTO {
		handler := &readinessStatusHandler{configPath: configPath, onboarding: &setupStatusReadStore{receipts: receipts}}
		status, ok := handler.snapshot(context.Background(), readinessSnapshotOptions{})
		if !ok {
			t.Fatal("readiness snapshot was unavailable")
		}
		return status
	}
	stateByTargetID := func(status readinessStatusDTO) map[string]string {
		states := make(map[string]string, len(status.Targets))
		for _, target := range status.Targets {
			states[target.ID] = target.FirstRead.State
		}
		return states
	}

	withoutProof := readStatus(receipts)
	if withoutProof.FirstRead.State != firstReadStateUnavailable {
		t.Fatalf("aggregate state without retained active proof = %q; want unavailable", withoutProof.FirstRead.State)
	}
	for targetID, state := range stateByTargetID(withoutProof) {
		if state != firstReadStateUnavailable {
			t.Errorf("target %q state without retained active proof = %q; want unavailable", targetID, state)
		}
	}

	withProofReceipts := append([]onboardingFirstReadReceipt(nil), receipts...)
	withProofReceipts[0].DefinitionDigest = firstDigest
	withProof := readStatus(withProofReceipts)
	states := stateByTargetID(withProof)
	if withProof.FirstRead.State != firstReadStateObserved || states[first.ID] != firstReadStateObserved || states[second.ID] != firstReadStateUnavailable {
		t.Fatalf("capacity states with one retained proof = aggregate %q targets %#v; want observed, observed, unavailable", withProof.FirstRead.State, states)
	}
}

func TestFirstReadReadinessByTargetUsesDashboardMappingDigest(t *testing.T) {
	cfg := firstReadReceiptTestConfig()
	environment := cfg.ServiceBundles[0].Environments[0]
	selector := firstReadDefinitionSelector{ServiceBundleName: cfg.ServiceBundles[0].Name, EnvironmentName: environment.Name}
	digest := firstReadDefinitionDigestForCall(cfg, "dashboard", "dashboard_deployment_status", selector)
	receipt := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceDashboard, Tool: "dashboard_deployment_status",
		ObservedAt: time.Now().Add(-time.Minute).UTC(), DefinitionDigest: digest,
	}
	reads := firstReadReadinessByTarget(cfg, []onboardingFirstReadReceipt{receipt})
	if got := reads[environment.DashboardTargetID]; got.State != firstReadStateObserved || got.Tool != receipt.Tool {
		t.Fatalf("Dashboard target read = %+v", got)
	}

	cfg.ServiceBundles[0].Environments[0].DashboardNamespace = "changed"
	if got := firstReadReadinessByTarget(cfg, []onboardingFirstReadReceipt{receipt})[environment.DashboardTargetID]; got.State != "" {
		t.Fatalf("stale Dashboard mapping remained attributed: %+v", got)
	}
}

func TestSetupStatusDoesNotPromoteConnectionTestToMCPRead(t *testing.T) {
	cfg := readinessConfigFixture()
	reads := &setupStatusReadStore{}
	app := &app{configPath: writeReadinessConfig(t, cfg), onboarding: reads}
	status := app.setupStatus(context.Background(), setupStatusRequest{})
	if len(status.Targets) != 1 || status.Targets[0].ConnectionTest.State != "success" {
		t.Fatalf("fixture connection test missing: %+v", status.Targets)
	}
	if got := status.Targets[0].FirstRead.State; got != firstReadStateNotObserved {
		t.Fatalf("connection test was promoted to MCP read: %q", got)
	}
}

func TestSetupStatusUnavailableReceiptStoreStaysUnknown(t *testing.T) {
	cfg := readinessConfigFixture()
	app := &app{
		configPath: writeReadinessConfig(t, cfg),
		onboarding: &setupStatusReadStore{err: errors.New("private receipt-store detail")},
	}
	status := app.setupStatus(context.Background(), setupStatusRequest{})
	if status.ConfigurationState != "configured" || len(status.Targets) != 1 ||
		status.Targets[0].FirstRead.State != firstReadStateUnavailable {
		t.Fatalf("unavailable read evidence was reported as known: %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private receipt-store detail") {
		t.Fatalf("receipt-store error leaked: %s", encoded)
	}
}

func TestSetupStatusMCPMatchesHTTPAndIsReadOnlyByDefault(t *testing.T) {
	cfg := readinessConfigFixture()
	target := cfg.GitHubTargets[0]
	digest := firstReadDefinitionDigestForCall(cfg, "github", "github_repository", firstReadDefinitionSelector{TargetName: target.Name})
	observedAt := time.Now().Add(-time.Minute).UTC()
	reads := &setupStatusReadStore{receipts: []onboardingFirstReadReceipt{{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub,
		Tool: "github_repository", ObservedAt: observedAt, DefinitionDigest: digest,
	}}}
	configPath := writeReadinessConfig(t, cfg)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	registration := &readinessRegistrationFake{}
	secretSpy := &setupStatusSecretSpy{}
	transport := &setupStatusRoundTripper{}
	a := &app{
		configPath: configPath, onboarding: reads, clientRegistration: registration,
		secrets: secretSpy, client: &http.Client{Transport: transport},
	}

	httpRecorder := httptest.NewRecorder()
	newReadinessStatusHandlerWithOnboarding(configPath, registration, nil, reads).ServeHTTP(
		httpRecorder, httptest.NewRequest(http.MethodGet, readinessStatusPath, nil),
	)
	if httpRecorder.Code != http.StatusOK {
		t.Fatalf("readiness GET status=%d body=%s", httpRecorder.Code, httpRecorder.Body.String())
	}
	var httpStatus readinessStatusDTO
	if err := json.Unmarshal(httpRecorder.Body.Bytes(), &httpStatus); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "setup-status-test", Version: "1"}, nil)
	registerSetupStatusMCPTool(server, a)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect setup status MCP server: %v", err)
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "setup-status-test-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect setup status MCP client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
	})
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "setup_status", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("setup_status call: result=%+v err=%v", result, err)
	}
	mcpStatus, defaultFields := decodeSetupStatusResult(t, result)
	if mcpStatus.SchemaVersion != 1 || mcpStatus.ClientAttribution != "not_observed" {
		t.Fatalf("MCP attribution contract = %+v", mcpStatus)
	}
	if mcpStatus.ConfigurationState != httpStatus.State ||
		!setupStatusTargetsEqual(mcpStatus.Targets, httpStatus.Targets) || !readinessBundlesEqual(mcpStatus.ServiceBundles, httpStatus.ServiceBundles) {
		t.Fatalf("HTTP and MCP readiness fields differ: HTTP=%+v MCP=%+v", httpStatus, mcpStatus)
	}
	draftSnapshot, err := newSetupDraftConfigStore(configPath).Snapshot()
	if err != nil {
		t.Fatalf("read setup-draft config snapshot: %v", err)
	}
	if mcpStatus.ConfigurationRev != draftSnapshot.Revision {
		t.Fatalf("MCP configuration revision = %q, setup-draft snapshot revision = %q", mcpStatus.ConfigurationRev, draftSnapshot.Revision)
	}
	if httpStatus.ConfigurationRev != readinessConfigurationRevision(cfg) {
		t.Fatalf("HTTP readiness revision changed: got %q", httpStatus.ConfigurationRev)
	}
	if mcpStatus.ObservedAt.IsZero() || mcpStatus.ObservedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("MCP observed_at is invalid: %s", mcpStatus.ObservedAt)
	}
	for _, key := range []string{"schema_version", "client_attribution", "configuration_state", "observed_at", "configuration_revision", "targets"} {
		if _, ok := defaultFields[key]; !ok {
			t.Errorf("compact setup_status omitted %q", key)
		}
	}
	for _, key := range []string{"state", "service_bundles", "clients", "resident_endpoint", "mcp_features", "named_secrets", "user_environment", "ssh", "python_tasks", "first_read"} {
		if _, ok := defaultFields[key]; ok {
			t.Errorf("default compact setup_status included %q", key)
		}
	}
	if len(registration.calls) != 0 || transport.calls != 0 || secretSpy.loads != 0 {
		t.Fatalf("default status had side effects: inspections=%v requests=%d secret_loads=%d", registration.calls, transport.calls, secretSpy.loads)
	}
	inspected, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "setup_status", Arguments: map[string]any{"inspect_clients": true}})
	if err != nil || inspected.IsError || len(registration.calls) == 0 {
		t.Fatalf("explicit client inspection was not honored: result=%+v err=%v calls=%v", inspected, err, registration.calls)
	}
	inspectedStatus, inspectedFields := decodeSetupStatusResult(t, inspected)
	if len(inspectedStatus.Clients) == 0 {
		t.Fatalf("explicit client inspection omitted client details: %+v", inspectedStatus)
	}
	if _, ok := inspectedFields["clients"]; !ok {
		t.Fatal("inspect_clients=true omitted the clients field")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("setup_status changed settings bytes")
	}
}

func decodeSetupStatusResult(t *testing.T, result *mcp.CallToolResult) (setupStatusDTO, map[string]json.RawMessage) {
	t.Helper()
	if result == nil || result.StructuredContent == nil {
		t.Fatal("setup_status omitted structured content")
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal setup_status structured content: %v", err)
	}
	var status setupStatusDTO
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatalf("decode setup_status structured content: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode setup_status object fields: %v", err)
	}

	var fallback []byte
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			fallback = []byte(text.Text)
			break
		}
	}
	if len(fallback) == 0 {
		t.Fatal("setup_status omitted its JSON text fallback")
	}
	var fallbackStatus setupStatusDTO
	if err := json.Unmarshal(fallback, &fallbackStatus); err != nil {
		t.Fatalf("decode setup_status text fallback: %v", err)
	}
	if !setupStatusSnapshotsEqual(status, fallbackStatus) {
		t.Fatalf("structured content and text fallback differ: structured=%+v fallback=%+v", status, fallbackStatus)
	}
	return status, fields
}

func setupStatusSnapshotsEqual(a, b setupStatusDTO) bool {
	return a.SchemaVersion == b.SchemaVersion && a.ClientAttribution == b.ClientAttribution &&
		a.ConfigurationState == b.ConfigurationState && a.ObservedAt.Equal(b.ObservedAt) &&
		a.ConfigurationRev == b.ConfigurationRev && setupStatusTargetsEqual(a.Targets, b.Targets) &&
		readinessBundlesEqual(a.ServiceBundles, b.ServiceBundles) && reflect.DeepEqual(a.Clients, b.Clients)
}

func setupStatusTargetsEqual(a, b []readinessTargetDTO) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		left, right := a[index], b[index]
		if left.ID != right.ID || left.Type != right.Type || left.Name != right.Name || left.Enabled != right.Enabled ||
			left.CredentialState != right.CredentialState || !reflect.DeepEqual(left.Actions, right.Actions) ||
			left.ConnectionTest != right.ConnectionTest || !firstReadReadinessEqual(left.FirstRead, right.FirstRead) {
			return false
		}
	}
	return true
}

func readinessBundlesEqual(a, b []readinessBundleDTO) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

func firstReadReadinessEqual(a, b firstReadReadinessDTO) bool {
	if a.State != b.State || a.Source != b.Source || a.Service != b.Service || a.Tool != b.Tool {
		return false
	}
	if a.ObservedAt == nil || b.ObservedAt == nil {
		return a.ObservedAt == nil && b.ObservedAt == nil
	}
	return a.ObservedAt.Equal(*b.ObservedAt)
}
