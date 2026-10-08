package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONRemotePreviewUIIsSeparateBilingualAndReadOnly(t *testing.T) {
	a := &app{host: "127.0.0.1:43271", csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	page := response.Body.String()
	for _, expected := range []string{
		`id="settings-import-form"`,
		`id="json-remote-import-form"`,
		`action="/preview-json-remotes"`,
		`name="json_remotes"`,
		`accept=".json,application/json"`,
		`aria-describedby="json-remote-import-hint"`,
		`role="status" aria-live="polite"`,
		`Select one .json file, up to 1 MiB.`,
		`actual path is unverified`,
		`scans exact whole JSON string values and ignores JSON keys`,
		`String values over 4 KiB are skipped.`,
		`no conversation schema or conversation-count support claim`,
		`never shows raw JSON, source strings, URLs, or registered target names or IDs`,
		`Results with more than 32 candidates are rejected.`,
		`Every candidate has fixed low confidence.`,
		`shown for manual review only`,
		`does not select a target, save settings, access a network service`,
		`'Preview JSON repository strings': 'JSON 저장소 문자열 미리보기'`,
		`'Preview repository candidates': '저장소 후보 미리보기'`,
		`JSON 키는 무시하고 각 문자열 값 전체를 그대로 검색합니다.`,
		`'No repository candidates were found in the selected JSON file.'`,
		`'The JSON repository preview could not be completed.'`,
		`신뢰도: 낮음. 이 후보를 직접 검토하세요.`,
		`수동 검토용이며 대상을 선택하지 않았습니다.`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("JSON repository preview UI missing %q", expected)
		}
	}

	settingsFormIndex := strings.Index(page, `id="settings-import-form"`)
	jsonFormIndex := strings.Index(page, `id="json-remote-import-form"`)
	if settingsFormIndex < 0 || jsonFormIndex < 0 {
		t.Fatal("settings and JSON repository forms must both be rendered")
	}
	settingsCloseIndex := strings.Index(page[settingsFormIndex:], `</form>`)
	if settingsCloseIndex < 0 || jsonFormIndex < settingsFormIndex+settingsCloseIndex {
		t.Fatal("JSON repository preview must be a distinct form next to the settings JSON preview")
	}
	formIndex := strings.Index(page, `id="json-remote-import-form"`)
	formEnd := strings.Index(page[formIndex:], `</form>`)
	if formEnd < 0 || strings.Contains(page[formIndex:formIndex+formEnd], `multiple`) {
		t.Fatal("JSON repository preview must accept only one selected file")
	}

	scriptStart := strings.Index(page, "const jsonRemoteImportForm")
	if scriptStart < 0 {
		t.Fatal("JSON repository preview script was not rendered")
	}
	scriptEnd := strings.Index(page[scriptStart:], "const dashboardDiagnosisForm")
	if scriptEnd < 0 {
		t.Fatal("JSON repository preview script was not rendered")
	}
	jsonScript := page[scriptStart : scriptStart+scriptEnd]
	for _, required := range []string{
		"if (jsonRemoteImportForm)",
		"file.size > 1024 * 1024",
		"new FormData(jsonRemoteImportForm)",
		"let jsonRemoteGeneration = 0",
		"requestGeneration !== jsonRemoteGeneration",
		"candidates.length > 32",
		"jsonRemoteRepositoryIsSafe(candidate.repository)",
		"if (value === '[REDACTED]') return true",
		"repository.textContent = candidate.repository",
		"setDynamic(confidence, 'jsonRemoteConfidence')",
		"candidate.registered_match ? 'jsonRemoteExactMatch' : 'jsonRemoteNoMatch'",
		"jsonRemoteGeneration++",
		"clearDynamicChildren(jsonRemotePreview)",
		"heading.focus()",
		"jsonRemoteSubmitButton.disabled = false",
	} {
		if !strings.Contains(jsonScript, required) {
			t.Errorf("JSON repository preview script is missing %q", required)
		}
	}
	if !strings.Contains(page, `#json-remote-result-heading:focus { outline: 3px solid var(--focus);`) {
		t.Fatal("focused JSON result heading must have visible focus styling")
	}
	for _, forbidden := range []string{
		"candidate.source",
		"candidate.confidence",
		"candidate.id",
		"target_id",
		"target.name",
		"innerHTML",
		"JSON.stringify",
		"textContent = result.error",
		"setDynamic(jsonRemoteStatus, 'jsonRemotePreviewFailed', result.error)",
	} {
		if strings.Contains(jsonScript, forbidden) {
			t.Errorf("JSON repository preview script contains a raw-output or target-identity path %q", forbidden)
		}
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("local preview fetch CSP is incomplete: %q", csp)
	}

	koreanRequest := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	koreanRequest.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	koreanResponse := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(koreanResponse, koreanRequest)
	if !strings.Contains(koreanResponse.Body.String(), `<html lang="ko">`) {
		t.Fatal("Korean page did not preserve its language attribute")
	}
}
