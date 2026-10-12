package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestUserGuidePageRendersLocalizedReadOnlyContent(t *testing.T) {
	tests := []struct {
		name      string
		cookie    *http.Cookie
		wantLang  string
		wantTitle string
	}{
		{name: "Korean default", wantLang: `lang="ko"`, wantTitle: "사용 가이드"},
		{name: "explicit English", cookie: &http.Cookie{Name: "lah_lang", Value: "en"}, wantLang: `lang="en"`, wantTitle: "User guide"},
		{name: "unsupported locale defaults to Korean", cookie: &http.Cookie{Name: "lah_lang", Value: "fr"}, wantLang: `lang="ko"`, wantTitle: "사용 가이드"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/guide?from=test", nil)
			if test.cookie != nil {
				request.AddCookie(test.cookie)
			}
			recorder := httptest.NewRecorder()
			(&app{}).handleUserGuide(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}

			body := recorder.Body.String()
			for _, want := range []string{
				test.wantLang,
				test.wantTitle,
				`id="guide-main"`,
				`id="guide-start"`,
				`id="guide-use-cases"`,
				`id="guide-examples"`,
				`id="guide-operations"`,
				`id="guide-troubleshooting"`,
				`id="guide-clients"`,
				`id="guide-appendix"`,
				`data-guide-example`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("rendered guide does not contain %q", want)
				}
			}

			csp := recorder.Header().Get("Content-Security-Policy")
			noncePattern := regexp.MustCompile(`script-src 'nonce-([a-f0-9]{64})'`)
			match := noncePattern.FindStringSubmatch(csp)
			if len(match) != 2 {
				t.Fatal("response CSP is missing its nonce")
			}
			for _, script := range regexp.MustCompile(`(?s)<script\b[^>]*>`).FindAllString(body, -1) {
				if !strings.Contains(script, `nonce="`+match[1]+`"`) {
					t.Errorf("script does not carry the response CSP nonce: %s", script)
				}
			}
		})
	}
}

func TestUserGuideRejectsNonGetAndWrongPath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "post", method: http.MethodPost, path: "/guide"},
		{name: "wrong path", method: http.MethodGet, path: "/guide/extra"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&app{}).handleUserGuide(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
			}
			if got := recorder.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}

func TestLocalUIMuxMountsGuideBehindSecurityHeadersWithoutReadingSettings(t *testing.T) {
	const host = "127.0.0.1:43139"
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	handler := newLocalUIHandler(&app{configPath: settingsPath, host: host})
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/guide", nil)
	request.Host = host
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("guide GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for name, want := range map[string]string{
		"Referrer-Policy":        localUIReferrerPolicy,
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("guide request created its settings fixture: stat error = %v", err)
	}

	cases := []struct {
		name       string
		host       string
		method     string
		origin     string
		wantStatus int
	}{
		{name: "wrong host", host: "127.0.0.1:43140", method: http.MethodGet, wantStatus: http.StatusNotFound},
		{name: "foreign origin post", host: host, method: http.MethodPost, origin: "http://attacker.invalid", wantStatus: http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "http://"+host+"/guide", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
