//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const manualWindowsParallelBaselineQAGate = "LAH_ENABLE_MANUAL_WINDOWS_PARALLEL_BASELINE_QA"

type a05ManualBrowserResult struct {
	status      int
	originClass string
	localeKo    bool
	body        []byte
}

func a05ManualBrowserOriginClass(values []string, expected string) string {
	switch len(values) {
	case 0:
		return "missing"
	case 1:
		switch values[0] {
		case "null":
			return "null"
		case expected:
			return "exact"
		default:
			return "other"
		}
	default:
		return "multiple"
	}
}

func a05ManualBrowserExpectedSave(r *http.Request, host, csrf string) ([]byte, bool) {
	const maxBodySize = 4 << 10
	if r.Method != http.MethodPost || r.URL.Path != "/save-dashboard" || r.URL.RawQuery != "" ||
		r.Host != host || r.Body == nil || r.ContentLength < 0 || r.ContentLength > maxBodySize {
		return nil, false
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/x-www-form-urlencoded" || len(params) != 0 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil || len(body) > maxBodySize || int64(len(body)) != r.ContentLength {
		return nil, false
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 5 {
		return nil, false
	}
	want := map[string]string{
		"csrf":            csrf,
		"target_id":       "new",
		"dashboard_name":  "A05 Synthetic Dashboard",
		"dashboard_url":   "http://dashboard.example.invalid/proxy",
		"dashboard_token": "a05-synthetic-token-canary",
	}
	for key, value := range want {
		items := values[key]
		if len(items) != 1 || items[0] != value {
			return nil, false
		}
	}
	return body, true
}

// TestManualWindowsParallelBaselineBrowserQA is an opt-in real browser flow.
// It serves the actual production settings handler at one loopback origin and
// allowlists a single synthetic invalid save; every other request is rejected.
func TestManualWindowsParallelBaselineBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("manual browser acceptance runs only on native Windows")
	}
	if os.Getenv(manualWindowsParallelBaselineQAGate) != "1" {
		t.Skipf("set %s=1 to start the opt-in loopback browser harness", manualWindowsParallelBaselineQAGate)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("write isolated settings")
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read isolated settings")
	}

	secrets := &a05SyntheticSecretStore{}
	const csrf = "a05-manual-synthetic-csrf"
	a := &app{configPath: configPath, secrets: secrets, csrf: csrf}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen on loopback")
	}
	a.host = listener.Addr().String()
	baseURL := "http://" + a.host
	productHandler := newLocalUIHandler(a)

	var rootGets atomic.Int32
	var blockedRequests atomic.Int32
	var postCount atomic.Int32
	postResult := make(chan a05ManualBrowserResult, 1)
	observedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.RawQuery == "dashboard_id=new" {
			rootGets.Add(1)
			productHandler.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/save-dashboard" {
			body, accepted := a05ManualBrowserExpectedSave(r, a.host, csrf)
			if !accepted || postCount.Add(1) != 1 {
				blockedRequests.Add(1)
				http.Error(w, "This request is outside the synthetic QA allowlist.", http.StatusConflict)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			recorder := httptest.NewRecorder()
			productHandler.ServeHTTP(recorder, r)
			for name, values := range recorder.Header() {
				w.Header()[name] = append([]string(nil), values...)
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			cookie, _ := r.Cookie("lah_lang")
			postResult <- a05ManualBrowserResult{
				status:      recorder.Code,
				originClass: a05ManualBrowserOriginClass(r.Header.Values("Origin"), baseURL),
				localeKo:    cookie != nil && cookie.Value == "ko",
				body:        append([]byte(nil), recorder.Body.Bytes()...),
			}
			return
		}
		blockedRequests.Add(1)
		http.Error(w, "This request is disabled in the synthetic A05 harness.", http.StatusConflict)
	})
	server := &http.Server{
		Handler:           observedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	})

	fmt.Fprintf(os.Stderr, "A05 isolated production-handler browser page: %s/?dashboard_id=new\n", baseURL)
	fmt.Fprintln(os.Stderr, "Use the browser at this loopback URL. Change Language to Korean, set the Dashboard URL to exactly http://dashboard.example.invalid/proxy, keep the synthetic name, enter any nonempty synthetic token, and submit Save once.")
	fmt.Fprintln(os.Stderr, "Expected: a same-origin POST reaches the production handler and renders Korean HTTP 422 with focus on Dashboard URL. No settings, credential store, or service should change. The harness accepts one exact synthetic form only.")
	_ = os.Stderr.Sync()

	var result a05ManualBrowserResult
	select {
	case result = <-postResult:
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("synthetic browser server stopped unexpectedly: %v", err)
		}
		t.Fatal("synthetic browser server stopped before the save form was submitted")
	case <-time.After(3 * time.Minute):
		t.Fatal("timed out waiting for the synthetic same-origin save POST")
	}
	page := string(result.body)
	if result.status != http.StatusUnprocessableEntity {
		t.Errorf("production browser POST status=%d; want 422", result.status)
	}
	if result.originClass != "exact" || !result.localeKo {
		t.Errorf("production browser POST origin=%s Korean-cookie=%t; want exact/true", result.originClass, result.localeKo)
	}
	if result.status == http.StatusUnprocessableEntity {
		for _, expected := range []string{
			`<html lang="ko">`,
			"Dashboard URL 항목이 올바르지 않습니다. 연결 테스트는 실행하지 않았습니다.",
			`id="dashboard-url"`,
			`autofocus aria-invalid="true"`,
			`aria-describedby="dashboard-url-hint dashboard-url-error"`,
		} {
			if !strings.Contains(page, expected) {
				t.Errorf("browser POST response missing expected Korean/focus marker %q", expected)
			}
		}
		counts, err := countHTMLValidationAttributes(page)
		if err != nil {
			t.Fatalf("could not inspect browser response autofocus attributes: %v", err)
		}
		if counts.autofocusElements != 1 || counts.ariaInvalidTrueElements != 1 {
			t.Error("browser POST response must focus exactly one invalid field")
		}
	}
	if strings.Contains(page, "a05-synthetic-token-canary") {
		t.Error("synthetic token appeared in the browser validation response")
	}
	if rootGets.Load() != 1 || postCount.Load() != 1 || blockedRequests.Load() != 0 {
		t.Errorf("browser requests root=%d save=%d blocked=%d; want 1/1/0", rootGets.Load(), postCount.Load(), blockedRequests.Load())
	}
	if secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Errorf("invalid browser save touched synthetic credential store: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("re-read isolated settings")
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Error("invalid browser save changed isolated settings")
	}
}
