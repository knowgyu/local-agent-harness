package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type a05SyntheticSecretStore struct {
	loads   atomic.Int32
	writes  atomic.Int32
	deletes atomic.Int32
}

func (s *a05SyntheticSecretStore) Save(string, []byte) error {
	s.writes.Add(1)
	return nil
}

func (s *a05SyntheticSecretStore) Load(string) ([]byte, error) {
	s.loads.Add(1)
	return nil, http.ErrNoCookie
}

func (s *a05SyntheticSecretStore) Delete(string) error {
	s.deletes.Add(1)
	return nil
}

// TestA05ProductionDashboardValidationSameOrigin exercises the security wrapper,
// production local UI handler, validation renderer, and locale cookie on a real
// loopback HTTP server. It uses only synthetic values and must not contact a
// service or access the operating-system credential store.
func TestA05ProductionDashboardValidationSameOrigin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("write isolated settings")
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read isolated settings")
	}

	secrets := &a05SyntheticSecretStore{}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		csrf:       "a05-synthetic-csrf",
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen on loopback")
	}
	a.host = listener.Addr().String()
	server := &http.Server{
		Handler:           newLocalUIHandler(a),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil && err != http.ErrServerClosed {
			t.Errorf("close loopback handler: %v", err)
		}
	})

	baseURL := "http://" + a.host
	client := &http.Client{Timeout: 5 * time.Second}
	root, err := client.Get(baseURL + "/?dashboard_id=new")
	if err != nil {
		t.Fatal("GET the production settings page")
	}
	io.Copy(io.Discard, root.Body)
	root.Body.Close()
	if root.StatusCode != http.StatusOK {
		t.Fatalf("production settings page status=%d, want 200", root.StatusCode)
	}

	form := url.Values{
		"csrf":            {a.csrf},
		"target_id":       {"new"},
		"dashboard_name":  {"A05 Synthetic Dashboard"},
		"dashboard_url":   {"http://dashboard.example.invalid/proxy"},
		"dashboard_token": {"a05-synthetic-token-canary"},
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/save-dashboard", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal("create same-origin synthetic save request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", baseURL)
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("POST to the production save handler")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if readErr != nil {
		t.Fatal("read validation response")
	}
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("production save status=%d, want 422", response.StatusCode)
	}
	page := string(body)
	for _, expected := range []string{
		`<html lang="ko">`,
		"Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
		`id="dashboard-url"`,
		`autofocus aria-invalid="true"`,
		`aria-describedby="dashboard-url-hint dashboard-url-error"`,
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("production validation response is missing expected Korean/focus marker %q", expected)
		}
	}
	counts, err := countHTMLValidationAttributes(page)
	if err != nil {
		t.Fatalf("could not inspect production response autofocus attributes: %v", err)
	}
	if counts.autofocusElements != 1 || counts.ariaInvalidTrueElements != 1 {
		t.Fatalf("production validation response has %d autofocus elements and %d invalid fields, want one each", counts.autofocusElements, counts.ariaInvalidTrueElements)
	}
	if strings.Contains(page, "a05-synthetic-token-canary") || strings.Contains(response.Header.Get("Location"), "a05-synthetic-token-canary") {
		t.Fatal("synthetic token was returned in the validation response")
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "same-origin" {
		t.Fatal("production response security headers were not preserved")
	}

	blockedRequest, err := http.NewRequest(http.MethodPost, baseURL+"/save-dashboard", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal("create rejected-origin request")
	}
	blockedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	blockedRequest.Header.Set("Origin", "null")
	blockedRequest.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	blockedResponse, err := client.Do(blockedRequest)
	if err != nil {
		t.Fatal("POST with opaque Origin")
	}
	blockedBody, readErr := io.ReadAll(io.LimitReader(blockedResponse.Body, 1<<20))
	blockedResponse.Body.Close()
	if readErr != nil {
		t.Fatal("read rejected-origin response")
	}
	if blockedResponse.StatusCode != http.StatusForbidden || strings.Contains(string(blockedBody), "dashboard-url-error") {
		t.Fatal("production security middleware did not reject Origin:null before save validation")
	}
	if secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("validation touched synthetic credential store: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("re-read isolated settings")
	}
	if !bytes.Equal(before, after) {
		t.Fatal("invalid save changed the isolated settings")
	}
	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("loopback handler stopped unexpectedly: %v", err)
		}
	default:
	}
}
