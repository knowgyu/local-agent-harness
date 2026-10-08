package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestReadinessPageRendersLocalizedReadOnlyShell(t *testing.T) {
	tests := []struct {
		name       string
		cookie     *http.Cookie
		wantLang   string
		wantButton string
	}{
		{name: "Korean default", wantLang: `lang="ko"`, wantButton: "클라이언트 등록 상태 확인"},
		{name: "explicit English", cookie: &http.Cookie{Name: "lah_lang", Value: "en"}, wantLang: `lang="en"`, wantButton: "Check CLI registration status"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readiness", nil)
			if test.cookie != nil {
				req.AddCookie(test.cookie)
			}
			recorder := httptest.NewRecorder()
			(&app{}).handleReadiness(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}

			body := recorder.Body.String()
			for _, want := range []string{
				test.wantLang,
				test.wantButton,
				`id="readiness-main"`,
				`id="readiness-connections"`,
				`id="readiness-clients"`,
				`id="readiness-operations"`,
				`id="readiness-features"`,
				`/readiness-status?inspect_clients=1`,
				`/readiness-status`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("rendered page does not contain %q", want)
				}
			}
			if strings.Contains(body, `innerHTML`) {
				t.Error("readiness page must render response data with safe text nodes")
			}

			noncePattern := regexp.MustCompile(`<script nonce="([^"]+)"`)
			match := noncePattern.FindStringSubmatch(body)
			if len(match) != 2 || !strings.Contains(recorder.Header().Get("Content-Security-Policy"), "'nonce-"+match[1]+"'") {
				t.Error("script nonce is missing or does not match the response CSP")
			}
		})
	}
}

func TestReadinessPageRejectsNonGetAndWrongPath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "post", method: http.MethodPost, path: "/readiness", want: http.StatusMethodNotAllowed},
		{name: "wrong path", method: http.MethodGet, path: "/readiness/extra", want: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&app{}).handleReadiness(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
			if got := recorder.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}

func TestReadinessClientInspectionUsesSafeInstalledEnums(t *testing.T) {
	fragment, err := os.ReadFile("ui_fragments/readiness.html")
	if err != nil {
		t.Fatalf("read readiness fragment: %v", err)
	}
	markup := string(fragment)
	for _, required := range []string{
		"client.installed === 'installed'",
		"client.installed === 'installed_cli_not_observed' ? text('installedNotObserved')",
		"client.installed === 'not_checked'",
		"Installed CLI was not observed in this check; verify installation manually.",
		"설치된 CLI를 이 확인에서 확인하지 못했습니다. 설치 여부를 직접 확인하세요.",
		"Verify this CLI installation manually before following its setup guide.",
	} {
		if !strings.Contains(markup, required) {
			t.Errorf("readiness client status does not map contract enum %q", required)
		}
	}
	if strings.Contains(markup, "client.installed === true") || strings.Contains(markup, "client.installed === false") || strings.Contains(markup, "notInstalled") {
		t.Fatal("readiness client state assumes a boolean when the backend status is an enum")
	}
}

func TestReadinessFeatureCardsLinkToExistingReviewFlows(t *testing.T) {
	fragment, err := os.ReadFile("ui_fragments/readiness.html")
	if err != nil {
		t.Fatalf("read readiness fragment: %v", err)
	}
	markup := string(fragment)
	for _, required := range []string{
		"dashboard: [",
		"{href: '/#lah-pane-diagnosis', en: 'Open Dashboard diagnosis'",
		"{href: '/#lah-pane-groups', en: 'Review connection group mapping'",
		"draft: [{href: '/#lah-pane-draft', en: 'Open AI setup draft review'",
		"makeLink(route.href, language === 'ko' ? route.ko : route.en)",
	} {
		if !strings.Contains(markup, required) {
			t.Errorf("readiness feature cards are missing existing workflow link %q", required)
		}
	}
}

func TestReadinessStatusIgnoresOlderResponseAfterNewerInspection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping delayed-response fixture")
	}
	fragment, err := os.ReadFile("ui_fragments/readiness.html")
	if err != nil {
		t.Fatalf("read readiness fragment: %v", err)
	}
	markup := string(fragment)
	start := strings.Index(markup, "    async function loadStatus(inspectClients) {")
	if start < 0 {
		t.Fatal("production readiness status loader is missing")
	}
	endOffset := strings.Index(markup[start:], "    function applyLanguage(next) {")
	if endOffset < 0 {
		t.Fatal("production readiness status loader boundary is missing")
	}
	loadStatus := markup[start : start+endOffset]
	fixture := `
let latestStatusRequest = 0;
let currentData = null;
let lastClientInspection = null;
let lastClientInspectionRevision = '';
let manualRevision = 'revision-old';
let manualMarks = {};
let manualStore = 'memory';
let resetMessagePending = false;
const statusButton = {disabled: false, textContent: 'Refresh', dataset: {}};
const clientsButton = {disabled: false, textContent: 'Inspect', dataset: {}};
const stateNode = {textContent: ''};
const summary = {dataset: {}};
const message = {textContent: ''};
const copyStatus = {textContent: ''};
const renderedRevisions = [];
let releaseOldJSON;
let releaseLatestResponse;
function text(key) { return key; }
function timeLabel(value) { return value ? 'observed' : ''; }
function initializeManualStorage(revision) { manualRevision = revision; manualMarks = {}; }
function render(data) { renderedRevisions.push(data.configuration_revision); }
function fetch(path) {
  if (path === '/readiness-status') {
    return Promise.resolve({ok: true, json: () => new Promise(resolve => { releaseOldJSON = resolve; })});
  }
  if (path === '/readiness-status?inspect_clients=1') {
    return new Promise(resolve => { releaseLatestResponse = resolve; });
  }
  throw new Error('unexpected status path: ' + path);
}
`
	checks := `
(async () => {
  const oldRequest = loadStatus(false);
  await Promise.resolve();
  if (typeof releaseOldJSON !== 'function') throw new Error('old response body was not held');
  const latestRequest = loadStatus(true);
  if (typeof releaseLatestResponse !== 'function') throw new Error('latest response was not held');
  const latestData = {state: 'configured', configuration_revision: 'revision-new', observed_at: '2026-10-06T00:00:00Z', clients: []};
  releaseLatestResponse({ok: true, json: async () => latestData});
  await latestRequest;
  const olderData = {state: 'needs_setup', configuration_revision: 'revision-old', observed_at: '2026-10-05T00:00:00Z', clients: []};
  releaseOldJSON(olderData);
  await oldRequest;
  if (currentData !== latestData) throw new Error('an older snapshot replaced the latest currentData');
  if (lastClientInspection !== latestData || lastClientInspectionRevision !== 'revision-new') throw new Error('an older snapshot replaced the latest explicit client inspection');
  if (manualRevision !== 'revision-new') throw new Error('manual marks were reconciled against a stale revision');
  if (renderedRevisions.join(',') !== 'revision-new') throw new Error('render ran for a stale response: ' + renderedRevisions.join(','));
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(fixture + loadStatus + checks)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("delayed readiness response fixture failed: %v\n%s", err, output)
	}
}
