package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestUILocaleForRequest(t *testing.T) {
	cases := []struct {
		name           string
		cookie         string
		acceptLanguage string
		want           uiLocale
	}{
		{name: "missing defaults to Korean", want: uiLocaleKorean},
		{name: "explicit English", cookie: "lah_lang=en", want: uiLocaleEnglish},
		{name: "explicit Korean", cookie: "lah_lang=ko", want: uiLocaleKorean},
		{name: "unsupported defaults to Korean", cookie: "lah_lang=fr", want: uiLocaleKorean},
		{name: "case-sensitive cookie defaults to Korean", cookie: "lah_lang=KO", want: uiLocaleKorean},
		{name: "malformed cookie defaults to Korean", cookie: "lah_lang=%zz", want: uiLocaleKorean},
		{name: "Accept-Language is not inferred", acceptLanguage: "en-US,en;q=0.9", want: uiLocaleKorean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != "" {
				r.Header.Set("Cookie", tc.cookie)
			}
			if tc.acceptLanguage != "" {
				r.Header.Set("Accept-Language", tc.acceptLanguage)
			}
			if got := uiLocaleForRequest(r); got != tc.want {
				t.Fatalf("locale = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientLanguagePreservesChoiceAndDefaultsToKorean(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless language preference fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	start := strings.Index(pageText, "      let cookieLanguage;")
	if start < 0 {
		t.Fatal("language preference initialization is missing")
	}
	end := strings.Index(pageText[start:], "\n      function translated(")
	if end < 0 {
		t.Fatal("language preference initialization boundary is missing")
	}
	initialization := pageText[start : start+end]
	fixture := `
const initializeLanguage = new Function('document', 'localStorage', ` + strconv.Quote(initialization+"\nreturn language;") + `);
const cases = [
  {name: 'Korean cookie wins over English storage', cookie: 'lah_lang=ko', stored: 'en', want: 'ko'},
  {name: 'English cookie wins over Korean storage', cookie: 'lah_lang=en', stored: 'ko', want: 'en'},
  {name: 'explicit English storage survives without cookie', cookie: '', stored: 'en', want: 'en'},
  {name: 'Korean storage survives without cookie', cookie: '', stored: 'ko', want: 'ko'},
  {name: 'unsupported cookie falls back to valid English storage', cookie: 'lah_lang=fr', stored: 'en', want: 'en'},
  {name: 'malformed cookie and storage default to Korean', cookie: 'lah_lang=%zz', stored: 'fr', want: 'ko'},
  {name: 'unrelated cookie does not select a language', cookie: 'other=lah_lang=en', stored: null, want: 'ko'},
  {name: 'unsupported storage defaults to Korean', cookie: '', stored: 'en-US', want: 'ko'},
  {name: 'missing storage defaults to Korean', cookie: '', stored: null, want: 'ko'}
];
for (const test of cases) {
  const got = initializeLanguage({cookie: test.cookie}, {getItem() { return test.stored; }});
  if (got !== test.want) throw new Error(test.name + ': language = ' + got + ', want ' + test.want);
}
const fallback = initializeLanguage({cookie: ''}, {getItem() { throw new Error('storage blocked'); }});
if (fallback !== 'ko') throw new Error('storage failure language = ' + fallback);
const cookieBlocked = {};
Object.defineProperty(cookieBlocked, 'cookie', {get() { throw new Error('cookie blocked'); }});
const cookieStorageFallback = initializeLanguage(cookieBlocked, {getItem() { return 'en'; }});
if (cookieStorageFallback !== 'en') throw new Error('blocked cookie language = ' + cookieStorageFallback);
const bothStoresBlocked = initializeLanguage(cookieBlocked, {getItem() { throw new Error('storage blocked'); }});
if (bothStoresBlocked !== 'ko') throw new Error('blocked cookie and storage language = ' + bothStoresBlocked);
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("language preference fixture failed: %v\n%s", err, output)
	}
}

func TestClientLanguageSelectionPersistsCookieAndStorage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless language selection fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	start := strings.Index(pageText, "      languageSelect.addEventListener('change', () => {")
	if start < 0 {
		t.Fatal("language selection handler is missing")
	}
	end := strings.Index(pageText[start:], "\n      for (const client of clientProgressClients)")
	if end < 0 {
		t.Fatal("language selection handler boundary is missing")
	}
	handler := pageText[start : start+end]
	fixture := `
let language = 'ko';
const writes = [];
const localStorage = {setItem(key, value) { writes.push([key, value]); }};
const document = {documentElement: {lang: 'ko'}, cookie: ''};
const languageSelect = {value: 'ko', change: null, addEventListener(name, callback) { this.change = callback; }};
function applyLanguage() {
  document.documentElement.lang = language;
  document.cookie = 'lah_lang=' + language + '; Path=/; SameSite=Strict';
}
` + handler + `
languageSelect.value = 'en';
languageSelect.change();
if (language !== 'en' || document.documentElement.lang !== 'en' || document.cookie !== 'lah_lang=en; Path=/; SameSite=Strict') {
  throw new Error('explicit English choice was not applied to the server cookie');
}
if (writes.length !== 1 || writes[0][0] !== 'lah-lang' || writes[0][1] !== 'en') {
  throw new Error('explicit English choice was not written to browser storage');
}
languageSelect.value = 'ko';
languageSelect.change();
if (language !== 'ko' || document.documentElement.lang !== 'ko' || document.cookie !== 'lah_lang=ko; Path=/; SameSite=Strict') {
  throw new Error('explicit Korean choice was not applied to the server cookie');
}
if (writes.length !== 2 || writes[1][0] !== 'lah-lang' || writes[1][1] !== 'ko') {
  throw new Error('explicit Korean choice was not written to browser storage');
}
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("language selection persistence fixture failed: %v\n%s", err, output)
	}
}

func TestDefaultRootHTMLUsesKoreanBeforeClientTranslation(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43254", csrf: "test-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	page := response.Body.String()
	for _, want := range []string{
		`<html lang="ko">`,
		`<body>`,
		`<label for="language-select">언어</label>`,
		`<select id="language-select" aria-label="언어">`,
		`<option value="ko" selected>한국어</option>`,
		`html.locale-pending body { visibility: hidden; }`,
		`<script data-locale-guard nonce=`,
		`document.documentElement.classList?.add`,
		`globalThis.setTimeout?.(()=>document.documentElement.classList.remove('locale-pending'),2000)`,
		`      applyLanguage();`,
		`      document.documentElement.classList?.remove('locale-pending');`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("default Korean server page lacks %q", want)
		}
	}
	if strings.Contains(page, `<html lang="en"`) || strings.Contains(page, `<label for="language-select">Language</label>`) || strings.Contains(page, `<body data-locale-pending=`) {
		t.Fatal("the no-cookie server markup exposed an English initial language")
	}
}

func TestEnglishCookieKeepsServerLanguageAndPostRejection(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43254", csrf: "expected"}
	get := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	get.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	page := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(page, get)
	if page.Code != http.StatusOK {
		t.Fatalf("GET status = %d", page.Code)
	}
	for _, want := range []string{
		`<html lang="en">`,
		`<label for="language-select">Language</label>`,
		`<option value="en" selected>English</option>`,
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("explicit English page lacks %q", want)
		}
	}
	if strings.Contains(page.Body.String(), `class.add('locale-pending')`) {
		t.Fatal("explicit English page was marked pending Korean localization")
	}

	post := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/", strings.NewReader("csrf=wrong"))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	response := httptest.NewRecorder()
	if a.checkPost(response, post) {
		t.Fatal("invalid CSRF passed validation")
	}
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Request rejected") {
		t.Fatalf("English POST rejection status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestInitialLanguagePassIncludesFooter(t *testing.T) {
	content, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(content)
	footer := strings.Index(page, `<p class="mcp">`)
	script := strings.Index(page, "      const koText = {")
	if footer < 0 || script < 0 || footer > script {
		t.Fatal("the MCP footer must be parsed before the initial language pass")
	}
}

func TestMCPClientSetupGuidanceIsManualAndHonest(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43254", csrf: "test-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	page := response.Body.String()
	for _, expected := range []string{
		`id="mcp-client-setup-heading"`,
		`<html lang="en">`,
		`https://developers.openai.com/codex/mcp`,
		`https://code.claude.com/docs/en/mcp`,
		`https://geminicli.com/docs/tools/mcp-server/`,
		`codex mcp add local-agent-harness --`,
		`claude mcp add --scope user --transport stdio local-agent-harness --`,
		`gemini mcp add --scope user local-agent-harness`,
		`The client-registration panel checks supported CLI settings when this settings page opens and when you refresh. You must explicitly request review, backup, or registration changes. It cannot verify an AI app connection or tool call.`,
		`Optional check-ins below record only what you report seeing. They stay in this page until it reloads, are not saved to app settings, and do not mean this app verified a client connection.`,
		`id="client-codex-server-listed"`,
		`id="client-codex-tools-visible"`,
		`id="client-codex-catalog-response"`,
		`id="client-claude-server-listed"`,
		`id="client-claude-tools-visible"`,
		`id="client-claude-catalog-response"`,
		`id="client-gemini-server-listed"`,
		`id="client-gemini-tools-visible"`,
		`id="client-gemini-catalog-response"`,
		`I ran the registration command and saw this server in the saved MCP list for this CLI.`,
		`I saw this server’s MCP tools in an active session for this CLI.`,
		`I called registered_targets or registered_service_bundles and saw a result in this CLI session.`,
		`clientProgressSummary`,
		`This app has not verified this CLI connection.`,
		`이 앱은 이 CLI 연결을 확인하지 않았습니다.`,
		`'Optional check-ins below record only what you report seeing. They stay in this page until it reloads, are not saved to app settings, and do not mean this app verified a client connection.': '아래 선택 항목은 사용자가 확인한 내용만 기록합니다. 이 페이지를 새로 고칠 때까지 유지되고 앱 설정에는 저장되지 않으며, 앱이 클라이언트 연결을 검증했다는 뜻이 아닙니다.'`,
		`'Optional progress check-in': '선택 진행 상태 확인'`,
		`'To check a client tool call without contacting a saved service, start with': '저장된 서비스에 요청하지 않고 클라이언트 도구 호출을 확인하려면'`,
		`'or': '또는'`,
		`'. These catalogs list local registrations only.': '로 시작하세요. 이 catalog에는 로컬 등록 정보만 표시됩니다.'`,
		`'I ran the registration command and saw this server in the saved MCP list for this CLI.': '등록 명령을 실행하고 이 CLI의 저장된 MCP 목록에서 이 서버를 확인했습니다.'`,
		`'I saw this server’s MCP tools in an active session for this CLI.': '실행 중인 이 CLI 세션의 MCP 도구 목록에서 이 서버의 도구를 확인했습니다.'`,
		`'I called registered_targets or registered_service_bundles and saw a result in this CLI session.': 'registered_targets 또는 registered_service_bundles를 호출해 이 CLI 세션에서 결과를 확인했습니다.'`,
		`'Clear my report for this CLI': '이 CLI의 보고 상태 지우기'`,
		`If the server is missing from an already-open session, start a new Codex CLI session. A saved-list entry alone does not confirm an active connection.`,
		`'If the server is missing from an already-open session, start a new Codex CLI session. A saved-list entry alone does not confirm an active connection.': '이미 열린 세션에서 서버가 보이지 않으면 Codex CLI 세션을 새로 시작하세요. 저장 목록에 항목이 있다는 사실만으로 현재 연결이 확인되지는 않습니다.'`,
		`'If the server is disconnected, check its status in /mcp and run claude mcp get local-agent-harness. If it still fails, start a new Claude Code CLI session. A saved-list entry alone does not confirm an active connection.': '서버 연결이 끊겼으면 /mcp에서 상태를 확인하고 claude mcp get local-agent-harness를 실행하세요. 그래도 연결되지 않으면 Claude Code CLI 세션을 새로 시작하세요. 저장 목록에 항목이 있어도 현재 연결을 확인한 것은 아닙니다.'`,
		`<fieldset aria-describedby="client-codex-progress-status">`,
		`id="client-codex-progress-status" class="status client-progress-status" role="status" aria-live="polite"`,
		`<input id="client-codex-server-listed" type="checkbox" data-client-progress-step="server-listed">`,
		`<input id="client-codex-tools-visible" type="checkbox" data-client-progress-step="tools-visible">`,
		`<input id="client-codex-catalog-response" type="checkbox" data-client-progress-step="catalog-response">`,
		`<button class="secondary" type="button" id="client-codex-progress-reset"`,
		`data-client-progress-reset="codex"`,
		`data-client-progress-reset="claude"`,
		`data-client-progress-reset="gemini"`,
		`This procedure does not configure Claude Desktop.`,
		`Stdio servers are reported as Connected only when the current folder is trusted. If Disconnected, review the folder before using`,
		`'official MCP guide': '공식 MCP 안내'`,
		`'Official Codex CLI installation guide (opens in a new tab)': 'Codex CLI 공식 설치 안내(새 탭에서 열림)'`,
		`'Official Codex MCP guide (opens in a new tab)': 'Codex MCP 공식 안내(새 탭에서 열림)'`,
		`'Official Claude Code installation guide (opens in a new tab)': 'Claude Code 공식 설치 안내(새 탭에서 열림)'`,
		`'Official Claude MCP guide (opens in a new tab)': 'Claude MCP 공식 안내(새 탭에서 열림)'`,
		`'Official Gemini CLI installation guide (opens in a new tab)': 'Gemini CLI 공식 설치 안내(새 탭에서 열림)'`,
		`'Official Gemini MCP guide (opens in a new tab)': 'Gemini MCP 공식 안내(새 탭에서 열림)'`,
		`', then open a new PowerShell window and check the installed version with': '새 PowerShell 창을 연 뒤 다음 명령으로 설치 버전을 확인하세요:'`,
		`'to reload servers. Stdio servers are reported as Connected only when the current folder is trusted. If Disconnected, review the folder before using': '로 서버를 다시 불러오세요. stdio 서버는 현재 폴더가 신뢰된 경우에만 Connected로 표시됩니다. Disconnected이면 폴더를 검토한 뒤 다음 명령을 사용하세요:'`,
		`'. See the': '. 자세한 내용은'`,
		`'Gemini CLI registration and verification commands': 'Gemini CLI 등록 및 확인 명령'`,
		`'Connect an MCP client (CLI guide)': 'MCP 클라이언트 연결 안내(CLI)'`,
		`These examples use CLI commands to register this executable as a local stdio process. Replace the example path with the absolute path to your built executable. Codex CLI, the ChatGPT desktop app, and the Codex IDE extension share MCP configuration on the same host. This guide covers CLI setup, not separate desktop or IDE setup steps. The client-registration panel checks supported CLI settings when this settings page opens and when you refresh. You must explicitly request review, backup, or registration changes. It cannot verify an AI app connection or tool call.': '아래 예시는 CLI 명령으로 이 실행 파일을 로컬 stdio 프로세스로 등록합니다. 예시 경로를 빌드한 실행 파일의 절대 경로로 바꾸세요. Codex CLI, ChatGPT 데스크톱 앱, Codex IDE 확장은 같은 호스트에서 MCP 설정을 공유합니다. 이 안내는 CLI 설정 절차를 다루며 데스크톱이나 IDE의 별도 설정 절차는 제공하지 않습니다. 이 설정 페이지를 열거나 새로 고칠 때 클라이언트 등록 패널이 지원 CLI 설정을 확인합니다. 검토, 백업, 등록 변경은 사용자가 직접 요청해야 합니다. AI 앱 연결이나 도구 호출은 검증하지 않습니다.`,
		`The ChatGPT desktop app and Codex IDE extension share this same-host configuration. If the server still does not appear, use the Restart control if that client provides one. This app does not detect whether the client reloaded or connected.': 'ChatGPT 데스크톱 앱과 Codex IDE 확장은`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("client guidance is missing %q", expected)
		}
	}
}

func TestMCPClientRecoveryGuidanceIsRenderedInMatchingCards(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43254", csrf: "test-csrf"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	page := response.Body.String()
	checks := []struct {
		name                 string
		summary              string
		want                 string
		localizationInput    string
		koreanMessage        string
		koreanScriptMappings []string
	}{
		{
			name:    "Codex new session fallback",
			summary: `<summary>Codex CLI</summary>`,
			want:    `<p class="hint">If the server is missing from an already-open session, start a new Codex CLI session. A saved-list entry alone does not confirm an active connection.</p>`,
		},
		{
			name:              "Claude status check and CLI details fallback",
			summary:           `<summary>Claude Code CLI</summary>`,
			want:              `<p class="hint">If the server is disconnected, check its status in /mcp and run claude mcp get local-agent-harness. If it still fails, start a new Claude Code CLI session. A saved-list entry alone does not confirm an active connection.</p>`,
			localizationInput: "If the server is disconnected, check its status in /mcp and run claude mcp get local-agent-harness. If it still fails, start a new Claude Code CLI session. A saved-list entry alone does not confirm an active connection.",
			koreanMessage:     "서버 연결이 끊겼으면 /mcp에서 상태를 확인하고 claude mcp get local-agent-harness를 실행하세요. 그래도 연결되지 않으면 Claude Code CLI 세션을 새로 시작하세요. 저장 목록에 항목이 있어도 현재 연결을 확인한 것은 아닙니다.",
			koreanScriptMappings: []string{
				`'If the server is disconnected, check its status in /mcp and run claude mcp get local-agent-harness. If it still fails, start a new Claude Code CLI session. A saved-list entry alone does not confirm an active connection.': '서버 연결이 끊겼으면 /mcp에서 상태를 확인하고 claude mcp get local-agent-harness를 실행하세요. 그래도 연결되지 않으면 Claude Code CLI 세션을 새로 시작하세요. 저장 목록에 항목이 있어도 현재 연결을 확인한 것은 아닙니다.'`,
			},
		},
		{
			name:    "Gemini reload and folder trust guidance",
			summary: `<summary>Gemini CLI</summary>`,
			want:    `<p class="hint">Use <code>/mcp list</code> to inspect a running session or <code>/mcp reload</code> to reload servers. Stdio servers are reported as Connected only when the current folder is trusted. If Disconnected, review the folder before using <code>gemini trust</code>. See the`,
			koreanScriptMappings: []string{
				`'to inspect a running session or': '로 실행 중인 세션을 확인하고,'`,
				`'to reload servers. Stdio servers are reported as Connected only when the current folder is trusted. If Disconnected, review the folder before using': '로 서버를 다시 불러오세요. stdio 서버는 현재 폴더가 신뢰된 경우에만 Connected로 표시됩니다. Disconnected이면 폴더를 검토한 뒤 다음 명령을 사용하세요:'`,
			},
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			summary := strings.Index(page, check.summary)
			if summary < 0 {
				t.Fatalf("rendered %s card heading is missing", check.name)
			}
			cardStart := strings.LastIndex(page[:summary], "<details>")
			cardEndOffset := strings.Index(page[summary:], "</details>")
			if cardStart < 0 || cardEndOffset < 0 {
				t.Fatalf("rendered %s card could not be located", check.name)
			}
			cardEnd := summary + cardEndOffset + len("</details>")
			if !strings.Contains(page[cardStart:cardEnd], check.want) {
				t.Fatalf("rendered CLI guidance is missing %q", check.want)
			}
			if check.koreanMessage != "" {
				if got := localizeUIMessage(uiLocaleKorean, check.localizationInput); got != check.koreanMessage {
					t.Fatalf("localized CLI guidance = %q, want %q", got, check.koreanMessage)
				}
			}
			for _, mapping := range check.koreanScriptMappings {
				if !strings.Contains(page, mapping) {
					t.Errorf("Korean CLI guidance mapping is missing %q", mapping)
				}
			}
		})
	}
}

func TestLocalizeUIMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "connection auth with saved history",
			message: connectionFailureMessage(connectionFailureAuthentication) + " Historical status saved.",
			want:    "연결 테스트 실패: 서비스가 저장된 자격 증명을 거부했습니다(401). 올바른 자격 증명으로 바꾼 뒤 다시 테스트하세요. 이력에 결과를 저장했습니다.",
		},
		{
			name:    "connection address without saved history",
			message: connectionFailureMessage(connectionFailureAddress) + " Historical status could not be saved. Retest the current target.",
			want:    "연결 테스트 실패: 서비스에 연결할 수 없습니다. 저장된 HTTPS 주소, 네트워크 또는 VPN 접근, 프록시 경로와 TLS 인증서를 확인하세요. 이력에 결과를 저장하지 못했습니다. 현재 대상을 다시 테스트하세요.",
		},
		{
			name:    "Harbor field",
			message: "The Harbor secret field is invalid.",
			want:    "Harbor 자격 증명 항목이 올바르지 않습니다.",
		},
		{
			name:    "Dashboard field",
			message: "The Dashboard url field is invalid. A connection test was not run.",
			want:    "Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
		},
		{
			name:    "service bundle error",
			message: "The service bundle is invalid or references unavailable targets. Settings were not changed.",
			want:    "서비스 묶음이 올바르지 않거나 사용할 수 없는 대상을 참조합니다. 설정은 변경되지 않았습니다.",
		},
		{
			name:    "unknown text remains unchanged",
			message: "Unrecognized status: user-provided name",
			want:    "Unrecognized status: user-provided name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localizeUIMessage(uiLocaleKorean, tc.message); got != tc.want {
				t.Fatalf("Korean message = %q, want %q", got, tc.want)
			}
			if got := localizeUIMessage(uiLocaleEnglish, tc.message); got != tc.message {
				t.Fatalf("English message changed: %q", got)
			}
		})
	}
}

func TestConnectionFailureMessagesAreLocalized(t *testing.T) {
	kinds := []connectionFailureKind{
		connectionFailureAddress,
		connectionFailureTimeout,
		connectionFailureAuthentication,
		connectionFailureAccess,
		connectionFailureEndpoint,
		connectionFailureRateLimit,
		connectionFailureOther,
	}
	for _, kind := range kinds {
		for _, historySaved := range []bool{false, true} {
			attempt := connectionTestAttempt{
				attempted:    true,
				failureKind:  kind,
				historySaved: historySaved,
			}
			message, failed := connectionTestMessage(attempt)
			if !failed {
				t.Fatalf("failure kind %q was reported as success", kind)
			}
			translated := localizeUIMessage(uiLocaleKorean, message)
			if translated == message || strings.Contains(translated, "Connection test failed:") {
				t.Errorf("failure kind %q, saved=%t was not localized: %q", kind, historySaved, translated)
			}
		}
	}
}

func TestRateLimitGuidanceDoesNotAssumeHTTPStatus(t *testing.T) {
	const english = "Connection test failed: the service is rate limiting requests. Wait before retrying."
	const englishWithHistory = english + " Historical status saved."
	const koreanWithHistory = "연결 테스트 실패: 서비스가 요청을 제한하고 있습니다. 잠시 기다린 뒤 다시 시도하세요. 이력에 결과를 저장했습니다."
	if got := connectionFailureMessage(connectionFailureRateLimit); got != english {
		t.Fatalf("English rate-limit message = %q, want %q", got, english)
	}
	if got := localizeUIMessage(uiLocaleKorean, englishWithHistory); got != koreanWithHistory {
		t.Fatalf("Korean rate-limit message = %q, want %q", got, koreanWithHistory)
	}
}

func TestConnectionTimeoutMessageHasFixedEnglishAndKoreanCopy(t *testing.T) {
	const english = "Connection test failed: the request timed out. Check network or VPN access and retry."
	const korean = "연결 테스트 실패: 요청 시간이 초과되었습니다. 네트워크 또는 VPN 연결을 확인하고 다시 시도하세요."
	if got := connectionFailureMessage(connectionFailureTimeout); got != english {
		t.Fatalf("English timeout message = %q, want %q", got, english)
	}
	messageWithHistory := english + " Historical status saved."
	wantKorean := korean + " 이력에 결과를 저장했습니다."
	if got := localizeUIMessage(uiLocaleKorean, messageWithHistory); got != wantKorean {
		t.Fatalf("Korean timeout message = %q, want %q", got, wantKorean)
	}
}

func TestHandleRootLocalizesStatusAndFieldError(t *testing.T) {
	const marker = "secret-canary-should-not-appear"
	app := &app{configPath: filepath.Join(t.TempDir(), "missing.json"), csrf: "test-csrf"}
	app.setStatus(connectionFailureMessage(connectionFailureAuthentication)+" Historical status saved.", true)
	r := httptest.NewRequest(http.MethodGet, "/?harbor_error=secret&canary="+marker, nil)
	r.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	w := httptest.NewRecorder()
	app.handleRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	page := w.Body.String()
	for _, want := range []string{
		`<html lang="ko">`,
		"연결 테스트 실패: 서비스가 저장된 자격 증명을 거부했습니다(401).",
		"저장하기 전에 강조된 Harbor 항목을 수정하세요.",
		`data-message-en="Connection test failed:`,
		`data-message-ko="연결 테스트 실패:`,
		`data-message-en="Correct the highlighted Harbor field before saving."`,
		`data-message-ko="저장하기 전에 강조된 Harbor 항목을 수정하세요."`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered page lacks %q", want)
		}
	}
	if strings.Contains(page, ">Connection test failed:") || strings.Contains(page, marker) {
		t.Fatal("rendered page displays an English diagnostic or contains a secret canary")
	}
}

func TestHandleRootIncludesBilingualConstraintValidation(t *testing.T) {
	app := &app{configPath: filepath.Join(t.TempDir(), "missing.json"), csrf: "test-csrf"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	app.handleRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	page := w.Body.String()
	for _, want := range []string{
		"constraintRequired: ['Please fill out this field.', '이 입력란을 작성하세요.']",
		"constraintURL: ['Enter a valid URL.', '유효한 URL을 입력하세요.']",
		"constraintPattern: ['Enter a value in the required format.', '요청한 형식에 맞게 입력하세요.']",
		"control.setCustomValidity(localizedConstraintMessage(control))",
		"control.addEventListener('invalid'",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered page lacks localized constraint validation %q", want)
		}
	}
}

func TestPythonTaskTranslationMarkersProtectUserIdentifiers(t *testing.T) {
	for _, test := range []struct{ name string }{
		{name: "Language"},
		{name: "Repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			task := pythonTask{
				ID: "python:" + strings.Repeat("e", 32), Name: test.name,
				InterpreterPath: `C:\Synthetic\python.exe`, ScriptPath: `C:\Synthetic\task.py`,
				SecretEnvName: test.name, SecretRef: "cred:" + strings.Repeat("e", 32),
				Disabled: true,
			}
			if err := writeConfig(path, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: path, csrf: "locale-test-csrf"}
			request := httptest.NewRequest(http.MethodGet, "/?python_task_id="+url.QueryEscape(task.ID), nil)
			response := httptest.NewRecorder()
			a.handleRoot(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("page status = %d, want 200", response.Code)
			}
			page := response.Body.String()
			hintStart := strings.Index(page, `id="python-secret-hint"`)
			if hintStart < 0 {
				t.Fatal("saved secret hint is missing")
			}
			hintEnd := strings.Index(page[hintStart:], "</p>")
			if hintEnd < 0 || !strings.Contains(page[hintStart:hintStart+hintEnd], "<code>"+test.name+"</code>") {
				t.Fatal("saved environment variable name is not excluded from translation")
			}
			pickerStart := strings.Index(page, `<select id="python-task-selection"`)
			if pickerStart < 0 {
				t.Fatal("Python task selector is missing")
			}
			pickerEnd := strings.Index(page[pickerStart:], "</select>")
			if pickerEnd < 0 {
				t.Fatal("Python task selector has no closing tag")
			}
			picker := page[pickerStart : pickerStart+pickerEnd]
			optionStart := strings.Index(picker, `<option value="`+task.ID+`"`)
			if optionStart < 0 {
				t.Fatal("saved Python task option is missing")
			}
			optionEnd := strings.Index(picker[optionStart:], "</option>")
			if optionEnd < 0 {
				t.Fatal("saved Python task option has no closing tag")
			}
			option := picker[optionStart : optionStart+optionEnd]
			for _, want := range []string{`data-i18n-disabled="true"`, "selected", ">" + test.name + " — disabled"} {
				if !strings.Contains(option, want) {
					t.Errorf("saved Python task option lacks %q", want)
				}
			}
		})
	}
}

func TestStaticInlineCopyTranslatesAroundIdentifiers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless static-copy translation fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	koStart := strings.Index(pageText, "      const koText = {")
	if koStart < 0 {
		t.Fatal("Korean translation dictionary is missing")
	}
	koEndOffset := strings.Index(pageText[koStart:], "\n      };")
	if koEndOffset < 0 {
		t.Fatal("Korean translation dictionary boundary is missing")
	}
	koDictionary := pageText[koStart : koStart+koEndOffset+len("\n      };")]

	applyStart := strings.Index(pageText, "      function applyLanguage(root = document.documentElement)")
	if applyStart < 0 {
		t.Fatal("applyLanguage function is missing from ui.html")
	}
	applyEndOffset := strings.Index(pageText[applyStart:], "languageSelect.addEventListener('change'")
	if applyEndOffset < 0 {
		t.Fatal("applyLanguage function boundary is missing")
	}
	applyLanguage := pageText[applyStart : applyStart+applyEndOffset]

	fixture := `
let language = 'ko';
const languageLabel = {textContent: 'Language'};
const languageSelect = {value: 'en', ariaLabel: 'Language', setAttribute(name, value) {
  if (name === 'aria-label') this.ariaLabel = value;
}};
const originalText = new WeakMap();
const originalAria = new WeakMap();
const disabledOptionNames = new WeakMap();
const dynamicNodes = new Map();
const localizedConstraintControls = new WeakSet();
const document = {documentElement: {}, cookie: '', querySelector(selector) {
  return selector === 'label[for="language-select"]' ? languageLabel : null;
}, createTreeWalker(root) {
  let index = 0;
  return {nextNode() { return root.nodes[index++] || null; }};
}};
const NodeFilter = {SHOW_TEXT: 1};
function bindLocalizedConstraintValidation() {}
function makeText(value, {code = false, staticText = false} = {}) {
  return {
    textContent: value,
    isConnected: true,
    parentElement: {closest(selector) {
      if (code && selector.startsWith('script, style, code,')) return {};
      if (staticText && selector === '[data-i18n-static]') return {};
      return null;
    }}
  };
}
const catalog = [
  makeText('To check a client tool call without contacting a saved service, start with '),
  makeText('registered_targets', {code: true}),
  makeText(' or '),
  makeText('registered_service_bundles', {code: true}),
  makeText('. These catalogs list local registrations only.')
];
const secretNames = ['LAH_TASK_SECRET', 'Language', 'Repository'];
const savedSecrets = secretNames.map(name => [
  makeText('A secret is saved for ', {staticText: true}),
  makeText(name, {code: true}),
  makeText(' in Windows Credential Manager. Leave the value blank to keep it.', {staticText: true})
]);
const runbookTitle = makeText('Preview a Markdown or text Runbook');
const diagnosisHint = [makeText('The diagnosis sends at most five bounded GET requests to the saved Dashboard target mapped to the selected connection group and environment. It reads the saved Dashboard credential and does not save settings or connection history. After diagnosis, you can separately request recent logs for a listed Pod/container; that request rechecks the saved mapping and uses separate limits. Logs may contain sensitive application data, so review them before sharing. Run reads only for a target you are authorized to inspect.')];
const runbookHint = [
  makeText('Select one .md or .txt file, up to 64 KiB. Only lines in the form '),
  makeText('lah:repository <Git remote URL>', {code: true}),
  makeText(' are read; other document text is ignored. The app receives only the filename and cannot verify its actual path. Preview does not save settings, read or write credentials, or contact a service. Remote URLs are not returned.')
];
const runbookLabel = makeText('Markdown or text Runbook file');
const disabledTaskOption = {value: 'python:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', textContent: 'Repository — disabled', selected: true};
const disabledTaskText = {
  get textContent() { return disabledTaskOption.textContent; },
  set textContent(value) { disabledTaskOption.textContent = value; },
  parentElement: {closest(selector) { return selector === 'option' ? disabledTaskOption : null; }}
};
const root = {
  nodes: [...catalog, ...savedSecrets.flat(), runbookTitle, ...diagnosisHint, ...runbookHint, runbookLabel, disabledTaskText],
  querySelectorAll(selector) { return selector === 'option[data-i18n-disabled]' ? [disabledTaskOption] : []; }
};
`
	checks := `
applyLanguage(root);
if (languageSelect.value !== 'ko' || languageLabel.textContent !== '언어' || languageSelect.ariaLabel !== '언어') {
  throw new Error('Korean language control has an incorrect value or accessible name');
}
const renderedCatalog = catalog.map(node => node.textContent).join('');
const wantedCatalog = '저장된 서비스에 요청하지 않고 클라이언트 도구 호출을 확인하려면 registered_targets 또는 registered_service_bundles로 시작하세요. 이 catalog에는 로컬 등록 정보만 표시됩니다.';
if (renderedCatalog !== wantedCatalog) throw new Error('catalog hint = ' + renderedCatalog);
if (catalog[1].textContent !== 'registered_targets' || catalog[3].textContent !== 'registered_service_bundles') {
  throw new Error('CLI command identifiers changed: ' + catalog.map(node => node.textContent).join('|'));
}
for (const [index, savedSecret] of savedSecrets.entries()) {
  const name = secretNames[index];
  const renderedSecret = savedSecret.map(node => node.textContent).join('');
  const wantedSecret = '비밀값을 전달할 환경 변수명: ' + name + ' (값은 Windows Credential Manager에 저장되어 있습니다.) 유지하려면 값을 비워 두세요.';
  if (renderedSecret !== wantedSecret) throw new Error(name + ': saved-secret hint = ' + renderedSecret);
  if (savedSecret[1].textContent !== name) throw new Error(name + ': environment variable name changed');
}
if (runbookTitle.textContent !== 'Markdown 또는 텍스트 Runbook 미리보기') throw new Error('Runbook title = ' + runbookTitle.textContent);
const renderedDiagnosisHint = diagnosisHint.map(node => node.textContent).join('');
const wantedDiagnosisHint = '최대 5회의 제한된 GET 요청을 선택한 연결 그룹·환경에 연결된 저장된 Dashboard 대상으로 보냅니다. 저장된 Dashboard 자격 증명을 읽으며 설정이나 연결 이력을 저장하지 않습니다. 진단 후 목록에 있는 Pod/container의 최근 로그를 별도로 요청할 수 있습니다. 이 요청은 저장된 매핑을 다시 확인하고 별도 제한을 적용합니다. 로그에는 민감한 애플리케이션 데이터가 포함될 수 있으므로 공유 전에 검토하세요. 조회 권한이 있는 대상만 읽으세요.';
if (renderedDiagnosisHint !== wantedDiagnosisHint) throw new Error('Dashboard diagnosis hint = ' + renderedDiagnosisHint);
const renderedRunbookHint = runbookHint.map(node => node.textContent).join('');
const wantedRunbookHint = '64 KiB 이하의 .md 또는 .txt 파일 하나를 선택하세요. 다음 지시자만 처리합니다: lah:repository <Git remote URL> 형식의 줄만 읽으며 다른 문서 내용은 무시합니다. 앱은 파일 이름만 받으므로 실제 경로는 확인할 수 없습니다. 미리보기는 설정을 저장하지 않고 자격 증명을 읽거나 쓰지 않으며 서비스에도 접속하지 않습니다. 원격 주소는 결과에 포함하지 않습니다.';
if (renderedRunbookHint !== wantedRunbookHint) throw new Error('Runbook hint = ' + renderedRunbookHint);
if (runbookLabel.textContent !== 'Markdown 또는 텍스트 Runbook 파일') throw new Error('Runbook label = ' + runbookLabel.textContent);
if (disabledTaskOption.textContent !== 'Repository — 비활성화됨') throw new Error('Korean disabled task label = ' + disabledTaskOption.textContent);
language = 'en';
applyLanguage(root);
if (languageSelect.value !== 'en' || languageLabel.textContent !== 'Language' || languageSelect.ariaLabel !== 'Language') {
  throw new Error('English language control has an incorrect value or accessible name');
}
for (const [index, savedSecret] of savedSecrets.entries()) {
  const name = secretNames[index];
  const renderedSecret = savedSecret.map(node => node.textContent).join('');
  const wantedSecret = 'A secret is saved for ' + name + ' in Windows Credential Manager. Leave the value blank to keep it.';
  if (renderedSecret !== wantedSecret) throw new Error(name + ': English saved-secret hint = ' + renderedSecret);
  if (savedSecret[1].textContent !== name) throw new Error(name + ': environment variable name changed after switching back');
}
if (disabledTaskOption.textContent !== 'Repository — disabled') throw new Error('English disabled task label = ' + disabledTaskOption.textContent);
if (!disabledTaskOption.selected || disabledTaskOption.value !== 'python:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee') {
  throw new Error('language change altered the selected Python task');
}
if (runbookTitle.textContent !== 'Preview a Markdown or text Runbook' || runbookLabel.textContent !== 'Markdown or text Runbook file') {
  throw new Error('Runbook English title or label changed after switching back');
}
if (diagnosisHint.map(node => node.textContent).join('') !== 'The diagnosis sends at most five bounded GET requests to the saved Dashboard target mapped to the selected connection group and environment. It reads the saved Dashboard credential and does not save settings or connection history. After diagnosis, you can separately request recent logs for a listed Pod/container; that request rechecks the saved mapping and uses separate limits. Logs may contain sensitive application data, so review them before sharing. Run reads only for a target you are authorized to inspect.') {
  throw new Error('Dashboard diagnosis English hint changed after switching back');
}
const englishRunbookHint = runbookHint.map(node => node.textContent).join('');
if (englishRunbookHint !== 'Select one .md or .txt file, up to 64 KiB. Only lines in the form lah:repository <Git remote URL> are read; other document text is ignored. The app receives only the filename and cannot verify its actual path. Preview does not save settings, read or write credentials, or contact a service. Remote URLs are not returned.') {
  throw new Error('Runbook English hint changed after switching back: ' + englishRunbookHint);
}
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(koDictionary + fixture + applyLanguage + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static inline copy translation fixture failed: %v\n%s", err, output)
	}
}

func TestLanguageSwitchPreservesNavigationAndQuickActionLinks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless navigation locale fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	koStart := strings.Index(pageText, "      const koText = {")
	if koStart < 0 {
		t.Fatal("Korean translation dictionary is missing")
	}
	koEndOffset := strings.Index(pageText[koStart:], "\n      };")
	if koEndOffset < 0 {
		t.Fatal("Korean translation dictionary boundary is missing")
	}
	koDictionary := pageText[koStart : koStart+koEndOffset+len("\n      };")]
	applyStart := strings.Index(pageText, "      function applyLanguage(root = document.documentElement)")
	if applyStart < 0 {
		t.Fatal("applyLanguage function is missing from ui.html")
	}
	applyEndOffset := strings.Index(pageText[applyStart:], "languageSelect.addEventListener('change'")
	if applyEndOffset < 0 {
		t.Fatal("applyLanguage function boundary is missing")
	}
	applyLanguage := pageText[applyStart : applyStart+applyEndOffset]

	navStart := strings.Index(pageText, `<nav id="lah-shell-nav"`)
	if navStart < 0 {
		t.Fatal("settings navigation is missing from ui.html")
	}
	navTagEndOffset := strings.Index(pageText[navStart:], ">")
	if navTagEndOffset < 0 {
		t.Fatal("settings navigation opening tag is incomplete")
	}
	navTagEnd := navStart + navTagEndOffset + 1
	navTag := pageText[navStart:navTagEnd]
	if strings.Contains(navTag, "data-message-en") || !strings.Contains(navTag, `aria-label="Settings and operations"`) {
		t.Fatalf("settings navigation must preserve child links and keep its English aria-label source: %s", navTag)
	}
	navCloseOffset := strings.Index(pageText[navTagEnd:], `</nav>`)
	if navCloseOffset < 0 {
		t.Fatal("settings navigation closing tag is missing")
	}
	navBlock := pageText[navTagEnd : navTagEnd+navCloseOffset]
	navLinkPattern := regexp.MustCompile(`<a class="lah-nav-link" href="([^"]+)"[^>]*data-message-en="([^"]*)" data-message-ko="([^"]*)">([^<]*)</a>`)
	navMatches := navLinkPattern.FindAllStringSubmatch(navBlock, -1)
	if len(navMatches) != 10 {
		t.Fatalf("settings navigation must expose all 10 localized leaf links, found %d", len(navMatches))
	}
	type linkSpec struct {
		Href        string `json:"href"`
		English     string `json:"english"`
		Korean      string `json:"korean"`
		Description string `json:"description,omitempty"`
		KoreanDesc  string `json:"koreanDescription,omitempty"`
	}
	navLinks := make([]linkSpec, 0, len(navMatches))
	for _, match := range navMatches {
		if match[2] != match[4] {
			t.Fatalf("settings navigation link text %q differs from its English source %q", match[4], match[2])
		}
		navLinks = append(navLinks, linkSpec{Href: match[1], English: match[2], Korean: match[3]})
	}

	quickStart := strings.Index(pageText, `<nav class="lah-quick-actions"`)
	if quickStart < 0 {
		t.Fatal("related local operations navigation is missing from ui.html")
	}
	quickTagEndOffset := strings.Index(pageText[quickStart:], ">")
	if quickTagEndOffset < 0 {
		t.Fatal("quick-action navigation opening tag is incomplete")
	}
	quickTagEnd := quickStart + quickTagEndOffset + 1
	quickTag := pageText[quickStart:quickTagEnd]
	if strings.Contains(quickTag, "data-message-en") || !strings.Contains(quickTag, `aria-label="Related local operations"`) {
		t.Fatalf("quick-action navigation must preserve child links and keep its English aria-label source: %s", quickTag)
	}
	quickCloseOffset := strings.Index(pageText[quickTagEnd:], `</nav>`)
	if quickCloseOffset < 0 {
		t.Fatal("quick-action navigation closing tag is missing")
	}
	quickBlock := pageText[quickTagEnd : quickTagEnd+quickCloseOffset]
	quickLinkPattern := regexp.MustCompile(`<a class="lah-quick-action" href="([^"]+)"><strong data-message-en="([^"]*)" data-message-ko="([^"]*)">([^<]*)</strong><span data-message-en="([^"]*)" data-message-ko="([^"]*)">([^<]*)</span></a>`)
	quickMatches := quickLinkPattern.FindAllStringSubmatch(quickBlock, -1)
	if len(quickMatches) != 3 {
		t.Fatalf("quick-action navigation must expose all 3 nested links, found %d", len(quickMatches))
	}
	quickLinks := make([]linkSpec, 0, len(quickMatches))
	for _, match := range quickMatches {
		if match[2] != match[4] {
			t.Fatalf("quick-action title %q differs from its English source %q", match[4], match[2])
		}
		quickLinks = append(quickLinks, linkSpec{
			Href: match[1], English: match[2], Korean: match[3],
			Description: match[5], KoreanDesc: match[6],
		})
	}
	navJSON, err := json.Marshal(navLinks)
	if err != nil {
		t.Fatal(err)
	}
	quickJSON, err := json.Marshal(quickLinks)
	if err != nil {
		t.Fatal(err)
	}

	fixture := `
class Element {
  constructor({text = '', attrs = {}, children = []} = {}) {
    this._text = text;
    this.attributes = new Map(Object.entries(attrs));
    this.children = children;
    this.dataset = {};
    if (attrs['data-message-en'] !== undefined) this.dataset.messageEn = attrs['data-message-en'];
    if (attrs['data-message-ko'] !== undefined) this.dataset.messageKo = attrs['data-message-ko'];
    this.isConnected = true;
  }
  get textContent() { return this._text + this.children.map(child => child.textContent).join(''); }
  set textContent(value) { this._text = String(value); this.children = []; }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  matches(selector) { return selector === '.remove-environment' && this.className === 'remove-environment'; }
}
const navSpecs = __NAV_SPECS__;
const quickSpecs = __QUICK_SPECS__;
const navLinks = navSpecs.map(spec => new Element({
  text: spec.english,
  attrs: {href: spec.href, 'data-message-en': spec.english, 'data-message-ko': spec.korean}
}));
const quickActions = quickSpecs.map(spec => new Element({
  attrs: {href: spec.href},
  children: [
    new Element({text: spec.english, attrs: {'data-message-en': spec.english, 'data-message-ko': spec.korean}}),
    new Element({text: spec.description, attrs: {'data-message-en': spec.description, 'data-message-ko': spec.koreanDescription}})
  ]
}));
const settingsNav = new Element({attrs: {'aria-label': 'Settings and operations'}, children: navLinks});
const quickNav = new Element({attrs: {'aria-label': 'Related local operations'}, children: quickActions});
const languageLabel = {textContent: 'Language'};
const languageSelect = {value: 'en', setAttribute() {}};
const originalText = new WeakMap();
const originalAria = new WeakMap();
const disabledOptionNames = new WeakMap();
const dynamicNodes = new Map();
const localizedConstraintControls = new WeakSet();
let language = 'ko';
const document = {
  documentElement: {lang: 'en'}, cookie: '',
  querySelector(selector) { return selector === 'label[for="language-select"]' ? languageLabel : null; },
  createTreeWalker() { return {nextNode() { return null; }}; }
};
const NodeFilter = {SHOW_TEXT: 1};
function bindLocalizedConstraintValidation() {}
const root = {
  querySelectorAll(selector) {
    if (selector === '[aria-label]') return [settingsNav, quickNav];
    if (selector === '[data-message-en][data-message-ko]') {
      return [...navLinks, ...quickActions.flatMap(action => action.children)];
    }
    return [];
  }
};
`
	fixture = strings.Replace(fixture, "__NAV_SPECS__", string(navJSON), 1)
	fixture = strings.Replace(fixture, "__QUICK_SPECS__", string(quickJSON), 1)
	checks := `
function assertLinks(languageName) {
  if (settingsNav.children.length !== navSpecs.length) throw new Error(languageName + ': settings link count changed');
  for (const [index, spec] of navSpecs.entries()) {
    const link = settingsNav.children[index];
    if (link.getAttribute('href') !== spec.href || link.textContent !== (languageName === 'ko' ? spec.korean : spec.english)) {
      throw new Error(languageName + ': settings link was lost or mislabeled: ' + index + ' ' + link.textContent);
    }
  }
  if (quickNav.children.length !== quickSpecs.length) throw new Error(languageName + ': quick-action link count changed');
  for (const [index, spec] of quickSpecs.entries()) {
    const link = quickNav.children[index];
    const [title, description] = link.children;
    if (link.getAttribute('href') !== spec.href || link.children.length !== 2 ||
        title.textContent !== (languageName === 'ko' ? spec.korean : spec.english) ||
        description.textContent !== (languageName === 'ko' ? spec.koreanDescription : spec.description)) {
      throw new Error(languageName + ': quick-action descendants were lost or mislabeled: ' + index);
    }
  }
  const expectedSettingsName = languageName === 'ko' ? '설정과 운영' : 'Settings and operations';
  const expectedQuickName = languageName === 'ko' ? '관련 로컬 작업' : 'Related local operations';
  if (settingsNav.getAttribute('aria-label') !== expectedSettingsName || quickNav.getAttribute('aria-label') !== expectedQuickName) {
    throw new Error(languageName + ': navigation accessible names were not localized');
  }
}
applyLanguage(root);
assertLinks('ko');
language = 'en';
applyLanguage(root);
assertLinks('en');
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(koDictionary + fixture + applyLanguage + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("navigation locale fixture failed: %v\n%s", err, output)
	}
}

func TestLocalizedConstraintValidationBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless constraint validation fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	start := strings.Index(pageText, "      function localizedConstraintMessage")
	if start < 0 {
		t.Fatal("localizedConstraintMessage function is missing from ui.html")
	}
	endOffset := strings.Index(pageText[start:], "      function clearDynamicChildren")
	if endOffset < 0 {
		t.Fatal("localized constraint validation functions are not followed by the expected script section")
	}
	validationFunctions := pageText[start : start+endOffset]
	fixture := `
const constraintMessages = {
  constraintRequired: ['Please fill out this field.', '이 입력란을 작성하세요.'],
  constraintURL: ['Enter a valid URL.', '유효한 URL을 입력하세요.'],
  constraintPattern: ['Enter a value in the required format.', '요청한 형식에 맞게 입력하세요.'],
  constraintLength: ['Shorten this text.', '입력한 텍스트를 줄이세요.'],
  constraintRange: ['Enter a value in the allowed range.', '허용된 범위의 값을 입력하세요.'],
  constraintType: ['Enter a valid value.', '유효한 값을 입력하세요.']
};
let language = 'en';
function translated(key) { return constraintMessages[key][language === 'ko' ? 1 : 0]; }
const localizedConstraintControls = new WeakSet();
`
	checks := `
function control(type, validity) {
  return {
    type,
    validity,
    customValidity: '',
    listeners: new Map(),
    addEventListener(name, listener) { this.listeners.set(name, listener); },
    setCustomValidity(message) { this.customValidity = message; }
  };
}
const url = control('url', {valueMissing: false, typeMismatch: true, patternMismatch: false, tooLong: false, rangeOverflow: false, rangeUnderflow: false, stepMismatch: false});
const project = control('text', {valueMissing: false, typeMismatch: false, patternMismatch: true, tooLong: false, rangeOverflow: false, rangeUnderflow: false, stepMismatch: false});
bindLocalizedConstraintValidation({querySelectorAll() { return [url, project]; }});
for (const [locale, expectedURL, expectedPattern] of [
  ['en', 'Enter a valid URL.', 'Enter a value in the required format.'],
  ['ko', '유효한 URL을 입력하세요.', '요청한 형식에 맞게 입력하세요.']
]) {
  language = locale;
  url.listeners.get('invalid')();
  if (url.customValidity !== expectedURL) throw new Error(locale + ': URL message was ' + url.customValidity);
  url.listeners.get('input')();
  if (url.customValidity !== '') throw new Error(locale + ': URL message did not clear after editing');
  project.listeners.get('invalid')();
  if (project.customValidity !== expectedPattern) throw new Error(locale + ': pattern message was ' + project.customValidity);
  project.listeners.get('change')();
  if (project.customValidity !== '') throw new Error(locale + ': pattern message did not clear after change');
}
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture + validationFunctions + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("localized constraint validation fixture failed: %v\n%s", err, output)
	}
}

func TestHandleRootLocalizesConfigReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &app{configPath: path, csrf: "test-csrf"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	w := httptest.NewRecorder()
	app.handleRoot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "로컬 설정을 읽을 수 없습니다. 설정 파일을 확인하고 앱을 다시 시작하세요.") {
		t.Fatal("config-read error was not localized")
	}
}

func TestCheckPostLocalizesRejection(t *testing.T) {
	app := &app{csrf: "expected"}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("csrf=wrong"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	w := httptest.NewRecorder()
	if app.checkPost(w, r) {
		t.Fatal("invalid CSRF passed validation")
	}
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "요청이 거부되었습니다.") {
		t.Fatalf("rejection status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestJenkinsApprovalPageLocalizesWithoutTrigger(t *testing.T) {
	const host = "127.0.0.1:43140"
	const route = "/approve/0123456789abcdef"
	const csrf = "test-csrf"
	decisions := make(chan bool, 1)
	state := &jenkinsApprovalPageState{
		host: host, route: route, target: "QA & <unsafe>",
		environment: "qa", baseURL: "https://jenkins.example.invalid",
		jobPath: "folder/smoke", csrf: csrf, decision: decisions,
	}
	handler := (&app{host: host}).securityHeaders(state)
	get := httptest.NewRequest(http.MethodGet, "http://"+host+route, nil)
	get.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, get)
	if page.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%q", page.Code, page.Body.String())
	}
	for _, want := range []string{
		`<html lang="ko">`,
		"Jenkins 실행 확인",
		"이 대상은 비운영 대상으로 사전 승인되지 않았습니다.",
		"이 실행 승인", "거부", "취소",
		"QA &amp; &lt;unsafe&gt;",
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("Korean approval page lacks %q", want)
		}
	}
	if strings.Contains(page.Body.String(), "Approve this trigger") ||
		strings.Contains(page.Body.String(), "QA & <unsafe>") {
		t.Fatal("page contains English control text or unescaped target name")
	}
	form := url.Values{"csrf": {csrf}, "decision": {"deny"}}
	post := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Origin", "http://"+host)
	post.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusOK || response.Body.String() != "요청을 거부했습니다. 이 창을 닫아도 됩니다." {
		t.Fatalf("deny status=%d body=%q", response.Code, response.Body.String())
	}
	select {
	case approved := <-decisions:
		if approved {
			t.Fatal("deny yielded an approval decision")
		}
	default:
		t.Fatal("deny did not produce a decision")
	}
}
