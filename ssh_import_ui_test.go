package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigPreviewUIIsBilingualReadOnlyAndDoesNotLinkTargets(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, host: "127.0.0.1:43271", csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	page := response.Body.String()
	for _, expected := range []string{
		`id="ssh-import-form"`,
		`action="/preview-ssh-config"`,
		`name="ssh_config"`,
		`aria-describedby="ssh-import-hint"`,
		`Select one file named config, up to 64 KiB.`,
		`Only simple literal Host aliases are listed.`,
		`Include and Match directives are rejected.`,
		`HostName is syntax-checked but never returned; account, key, and proxy values are ignored.`,
		`An alias does not identify a repository or registered target.`,
		`Preview does not run commands, contact a service, change settings`,
		`'Preview SSH aliases': 'SSH 별칭 미리보기'`,
		`'Preview aliases': '별칭 미리보기'`,
		`'No supported literal SSH aliases were found in the selected file.'`,
		`'The selected config contains unsupported or unsafe SSH directives.': '선택한 config에 지원하지 않거나 안전하지 않은 SSH 지시자가 있습니다.'`,
		`HostName은 구문만 확인하고 결과에 넣지 않으며 계정·키·프록시 값은 무시합니다.`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("SSH alias preview UI missing %q", expected)
		}
	}

	formIndex := strings.Index(page, `id="ssh-import-form"`)
	saveIndex := strings.Index(page, `action="/save-bundle"`)
	if formIndex < 0 || saveIndex < 0 || formIndex > saveIndex {
		t.Fatal("SSH preview form must remain separate from the service bundle save form")
	}
	scriptStart := strings.Index(page, "const sshImportForm")
	if scriptStart < 0 {
		t.Fatal("SSH alias preview script was not rendered")
	}
	scriptEnd := strings.Index(page[scriptStart:], "</script>")
	if scriptEnd < 0 {
		t.Fatal("SSH alias preview script was not rendered")
	}
	sshScript := page[scriptStart : scriptStart+scriptEnd]
	for _, forbidden := range []string{
		"github_target_ids",
		"candidate.github_matches",
		"action = '/save-bundle'",
		"innerHTML",
	} {
		if strings.Contains(sshScript, forbidden) {
			t.Errorf("SSH preview script contains target selection or unsafe rendering path %q", forbidden)
		}
	}
	for _, required := range []string{
		"new FormData(sshImportForm)",
		"title.textContent = alias.name",
		"sshImportPreview.append(item)",
		"sshNoMapping",
	} {
		if !strings.Contains(sshScript, required) {
			t.Errorf("SSH preview script is missing %q", required)
		}
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("preview fetch CSP is incomplete: %q", csp)
	}

	koreanRequest := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	koreanRequest.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	koreanResponse := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(koreanResponse, koreanRequest)
	if !strings.Contains(koreanResponse.Body.String(), `<html lang="ko">`) {
		t.Fatal("Korean page did not preserve its language attribute")
	}
}
