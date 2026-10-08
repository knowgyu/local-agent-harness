package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDashboardDiagnosisSafeSummaryProjectsOnlyFixedCodes(t *testing.T) {
	const token = "summary-token-canary-0123456789"
	const credentialRef = "cred:0123456789abcdef0123456789abcdef"
	const privateAddress = "https://private.example.invalid/dashboard"
	result := dashboardDeploymentDiagnosisResult{
		ServiceBundle:   token,
		Environment:     "<img src=x onerror=alert(1)>",
		Target:          privateAddress,
		Namespace:       privateAddress,
		Deployment:      credentialRef,
		OutputTruncated: true,
		Checks: []dashboardDeploymentDiagnosisCheck{
			{Step: "events", Status: "failed", ErrorCode: "access_denied", NextCheck: token},
			{Step: token, Status: privateAddress, ErrorCode: credentialRef, NextCheck: privateAddress},
		},
		Status: &dashboardDeploymentStatusResult{
			Replicas: 2, Available: 1, Unavailable: 1,
			Conditions: []dashboardDeploymentCondition{{Type: "Available", Status: "False", Reason: token}},
		},
		Events: &dashboardDeploymentEventsResult{Events: []dashboardDeploymentEvent{{Name: token, Type: "Warning", Reason: "FailedScheduling", ObjectName: credentialRef}}},
		Pods:   &dashboardDeploymentPodsResult{Pods: []dashboardPodStatus{{Name: token, Status: "Pending", Containers: []dashboardPodContainer{{Name: token, State: "Waiting"}}}}},
	}

	localTime := time.Date(2026, time.October, 4, 9, 30, 0, 0, time.FixedZone("KST", 9*60*60))
	summary := newDashboardDiagnosisSafeSummary(result, localTime)
	if summary.CapturedAt != "2026-10-04T00:30:00Z" || summary.Namespace != "" || summary.Deployment != "" {
		t.Fatalf("summary retained an unsafe scope or wrong timestamp: %+v", summary)
	}
	if summary.NextAction != "review_read_access" || !summary.DetailsShortened {
		t.Fatalf("summary lost fixed next action or truncation state: %+v", summary)
	}
	if len(summary.NotQueried) != 4 || summary.NotQueried[0] != "other_deployment_pods" || summary.NotQueried[2] != "live_resource_usage" {
		t.Fatalf("summary did not state the unqueried scope: %+v", summary.NotQueried)
	}
	if !hasDiagnosisSummaryCode(summary.Findings, "deployment_not_fully_available") ||
		!hasDiagnosisSummaryCode(summary.Findings, "scheduling_constraint") ||
		!hasDiagnosisSummaryCode(summary.Findings, "container_waiting") {
		t.Fatalf("summary omitted fixed warning signals: %+v", summary.Findings)
	}
	if summary.Checks[1].Step != "unknown" || summary.Checks[1].Status != "unknown" || summary.Checks[1].ErrorCode != "" {
		t.Fatalf("summary passed arbitrary check values through: %+v", summary.Checks[1])
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, credentialRef, privateAddress, "<img", "FailedScheduling", "Pending", "Waiting"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Errorf("summary contains unprojected value %q: %s", forbidden, encoded)
		}
	}
}

func TestDashboardDiagnosisSafeSummaryKeepsValidatedMappedScope(t *testing.T) {
	result := dashboardDeploymentDiagnosisResult{
		Namespace:  "apps",
		Deployment: "api.v2",
		Checks:     []dashboardDeploymentDiagnosisCheck{{Step: "status", Status: "succeeded"}},
	}
	summary := newDashboardDiagnosisSafeSummary(result, time.Date(2026, time.October, 4, 0, 0, 0, 0, time.UTC))
	if summary.Namespace != "apps" || summary.Deployment != "api.v2" || summary.Checks[0].Step != "status" || summary.Checks[0].Status != "succeeded" {
		t.Fatalf("summary omitted the registered scope or known check: %+v", summary)
	}
}

func TestDashboardDiagnosisSummaryFragmentCarriesNonceAndSafeCopyFallback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless diagnosis summary fixture")
	}
	const nonce = "summary-fixture-nonce"
	var fragment bytes.Buffer
	if err := page.ExecuteTemplate(&fragment, "lah-ui-fragment-diagnosis-summary", pageData{CSPNonce: nonce}); err != nil {
		t.Fatal(err)
	}
	markup := fragment.String()
	if !strings.Contains(markup, `nonce="`+nonce+`"`) || !strings.Contains(markup, `id="dashboard-diagnosis-safe-summary-fallback" rows="10" readonly hidden`) {
		t.Fatalf("summary partial omitted the per-page nonce or selectable readonly fallback: %s", markup)
	}
	start := strings.Index(markup, `<script nonce="`+nonce+`">`)
	if start < 0 {
		t.Fatal("summary partial script did not carry the CSP nonce")
	}
	start += len(`<script nonce="` + nonce + `">`)
	end := strings.Index(markup[start:], "</script>")
	if end < 0 {
		t.Fatal("summary partial script did not close")
	}
	script := markup[start : start+end]
	fixture := `
class Element {
  constructor(id) { this.id = id; this.hidden = false; this.value = ''; this.textContent = ''; this.listeners = new Map(); this.appended = 0; }
  addEventListener(type, callback) { this.listeners.set(type, callback); }
  append() { this.appended++; }
  focus() { this.focused = true; }
  select() { this.selected = true; }
}
const ids = new Map();
for (const id of ['dashboard-diagnosis-safe-summary','dashboard-diagnosis-safe-summary-heading','dashboard-diagnosis-safe-summary-status','dashboard-diagnosis-safe-summary-text','dashboard-diagnosis-safe-summary-copy','dashboard-diagnosis-safe-summary-fallback-label','dashboard-diagnosis-safe-summary-fallback']) ids.set(id, new Element(id));
let languageObserver;
global.window = global;
global.document = {documentElement: {lang: 'en'}, getElementById(id) { return ids.get(id) || null; }};
global.MutationObserver = class { constructor(callback) { languageObserver = callback; } observe() {} };
global.navigator = {clipboard: {writeText: async () => { throw new Error('synthetic clipboard denial'); }}};
`
	checks := `
(async () => {
  const summary = {
    captured_at: '2026-10-04T00:30:00Z', namespace: 'apps', deployment: 'api.v2',
    checks: [{step: 'events', status: 'failed', error_code: 'access_denied'}],
    findings: ['scheduling_constraint'], next_action: 'review_read_access', details_shortened: true,
    not_queried: ['other_deployment_pods', 'resource_requests_limits', 'live_resource_usage', 'container_logs']
  };
  const container = new Element('result-container');
  if (!LAHDashboardDiagnosisSummary.render(summary, container)) throw new Error('valid safe summary was rejected');
  const root = ids.get('dashboard-diagnosis-safe-summary');
  const text = ids.get('dashboard-diagnosis-safe-summary-text');
  const status = ids.get('dashboard-diagnosis-safe-summary-status');
  const fallback = ids.get('dashboard-diagnosis-safe-summary-fallback');
  if (root.hidden || container.appended !== 1 || !text.textContent.includes('Not queried by this diagnosis') || !text.textContent.includes('Some result details were shortened') || text.textContent.includes('undefined')) throw new Error('safe summary was not rendered as text');
  await ids.get('dashboard-diagnosis-safe-summary-copy').listeners.get('click')();
  if (fallback.hidden || !fallback.focused || !fallback.selected || fallback.value !== text.textContent) throw new Error('clipboard denial did not expose and select the safe readonly fallback');
  if (!status.textContent.includes('selected below') || status.textContent.includes('synthetic clipboard denial')) throw new Error('clipboard failure exposed an exception or missed status');
  document.documentElement.lang = 'ko';
  languageObserver();
  if (!text.textContent.includes('조회하지 않은 범위')) throw new Error('summary did not relocalize when the page language changed');
  if (LAHDashboardDiagnosisSummary.render({...summary, namespace: 'https://private.example.invalid'})) throw new Error('renderer should reject unsafe scope by returning false');
  if (!root.hidden || text.textContent !== '') throw new Error('invalid safe summary left stale content visible');
})().catch(error => { process.stderr.write(String(error)); process.exitCode = 1; });
`
	command := exec.Command(node, "-e", fixture+script+checks)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("summary fragment fixture failed: %v\n%s", err, output)
	}
}

func hasDiagnosisSummaryCode(values []string, code string) bool {
	for _, value := range values {
		if value == code {
			return true
		}
	}
	return false
}

func TestDashboardDiagnosisSummaryFragmentTemplateIsEmbedded(t *testing.T) {
	var rendered bytes.Buffer
	if err := page.ExecuteTemplate(&rendered, "ui.html", pageData{CSPNonce: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), `id="dashboard-diagnosis-safe-summary"`) || !strings.Contains(rendered.String(), `nonce="fixture"`) {
		t.Fatal("embedded Dashboard diagnosis summary fragment was not rendered with the page CSP nonce")
	}
	if _, err := os.Stat("ui_fragments/diagnosis_summary.html"); err != nil {
		t.Fatalf("owned summary fragment is missing: %v", err)
	}
}
