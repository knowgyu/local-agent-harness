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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const manualWindowsLocaleFormBrowserQAGate = "LAH_ENABLE_MANUAL_WINDOWS_LOCALE_FORM_BROWSER_QA"

type manualWindowsLocaleQASecretStore struct {
	loads   atomic.Int32
	writes  atomic.Int32
	deletes atomic.Int32
}

func (s *manualWindowsLocaleQASecretStore) Save(string, []byte) error {
	s.writes.Add(1)
	return nil
}

func (s *manualWindowsLocaleQASecretStore) Load(string) ([]byte, error) {
	s.loads.Add(1)
	return nil, http.ErrNoCookie
}

func (s *manualWindowsLocaleQASecretStore) Delete(string) error {
	s.deletes.Add(1)
	return nil
}

type manualWindowsLocaleQAResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *manualWindowsLocaleQAResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *manualWindowsLocaleQAResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(body)
	return w.ResponseWriter.Write(body)
}

func manualWindowsLocaleQAOriginClass(values []string, want string) string {
	switch len(values) {
	case 0:
		return "missing"
	case 1:
		switch values[0] {
		case "null":
			return "null"
		case want:
			return "exact"
		default:
			return "other"
		}
	default:
		return "multiple"
	}
}

func manualWindowsLocaleQAExpectedBundleValidationPost(r *http.Request, host, origin, csrf string) ([]byte, string) {
	const maxBodySize = 4 << 10
	if r.Method != http.MethodPost || r.URL.Path != "/save-bundle" || r.URL.RawQuery != "" {
		return nil, "request"
	}
	if r.Host != host {
		return nil, "host"
	}
	if r.Body == nil || r.ContentLength < 0 || r.ContentLength > maxBodySize {
		return nil, "body-size"
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != origin {
		return nil, "origin"
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, "content-type"
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/x-www-form-urlencoded" || len(params) != 0 {
		return nil, "content-type"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		return nil, "body-read"
	}
	if len(body) > maxBodySize || int64(len(body)) != r.ContentLength {
		return nil, "body-size"
	}
	parsed := r.Clone(r.Context())
	parsed.Body = io.NopCloser(bytes.NewReader(body))
	parsed.ContentLength = int64(len(body))
	if err := parsed.ParseForm(); err != nil {
		return nil, "form-parse"
	}
	if len(parsed.PostForm) != 3 || len(parsed.Form) != 3 {
		return nil, "form-fields"
	}
	want := map[string]string{
		"csrf":        csrf,
		"bundle_id":   "new",
		"bundle_name": " ",
	}
	for key, value := range want {
		values := parsed.PostForm[key]
		if len(values) != 1 || values[0] != value {
			return nil, "form-values"
		}
	}
	return body, ""
}

func TestManualWindowsLocaleQAExpectedBundleValidationPost(t *testing.T) {
	const (
		host   = "127.0.0.1:49671"
		origin = "http://127.0.0.1:49671"
		csrf   = "synthetic-locale-qa-csrf"
	)
	body := url.Values{
		"csrf":        {csrf},
		"bundle_id":   {"new"},
		"bundle_name": {" "},
	}.Encode()
	tests := []struct {
		name      string
		body      string
		configure func(*http.Request)
		want      string
	}{
		{name: "exact same-origin whitespace name", body: body, want: ""},
		{name: "wrong Host", body: body, configure: func(r *http.Request) { r.Host = "127.0.0.1:49672" }, want: "host"},
		{name: "wrong Origin", body: body, configure: func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:49672") }, want: "origin"},
		{name: "duplicate Origin", body: body, configure: func(r *http.Request) { r.Header.Add("Origin", origin) }, want: "origin"},
		{name: "query string", body: body, configure: func(r *http.Request) { r.URL.RawQuery = "unexpected=1" }, want: "request"},
		{name: "extra field", body: body + "&unexpected=1", want: "form-fields"},
		{name: "duplicate field", body: body + "&bundle_name=+", want: "form-values"},
		{name: "unexpected content type", body: body, configure: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		}, want: "content-type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, origin+"/save-bundle", strings.NewReader(tt.body))
			r.Host = host
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.configure != nil {
				tt.configure(r)
			}
			_, got := manualWindowsLocaleQAExpectedBundleValidationPost(r, host, origin, csrf)
			if got != tt.want {
				t.Fatalf("manual locale QA reject category=%q, want %q", got, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "missing", want: "missing"},
		{name: "null", values: []string{"null"}, want: "null"},
		{name: "exact", values: []string{origin}, want: "exact"},
		{name: "other", values: []string{"http://127.0.0.1:49672"}, want: "other"},
		{name: "multiple", values: []string{origin, origin}, want: "multiple"},
	} {
		t.Run("origin category/"+tt.name, func(t *testing.T) {
			if got := manualWindowsLocaleQAOriginClass(tt.values, origin); got != tt.want {
				t.Fatalf("origin category=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestManualWindowsLocaleAfterBundleValidationBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("manual browser QA runs only on native Windows")
	}
	if os.Getenv(manualWindowsLocaleFormBrowserQAGate) != "1" {
		t.Skipf("set %s=1 to start the optional loopback browser QA harness", manualWindowsLocaleFormBrowserQAGate)
	}

	const (
		csrf = "manual-windows-locale-form-browser-qa-csrf"
		want = "서비스 묶음이 올바르지 않거나 사용할 수 없는 대상을 참조합니다. 설정은 변경되지 않았습니다."
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	secrets := &manualWindowsLocaleQASecretStore{}
	var outboundRequests atomic.Int32
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		csrf:       csrf,
		client: &http.Client{Transport: harborTestRoundTripper(func(*http.Request) (*http.Response, error) {
			outboundRequests.Add(1)
			return nil, errors.New("unexpected outbound request")
		})},
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	a.host = host
	baseURL := "http://" + host
	productHandler := newLocalUIHandler(a)

	type bundlePostResult struct {
		status   int
		accepted bool
		origin   []string
		guard    string
	}
	type validationPageResult struct {
		status int
		body   []byte
	}
	bundlePosts := make(chan bundlePostResult, 2)
	validationPages := make(chan validationPageResult, 1)
	var stateMu sync.Mutex
	var bundlePostCount int
	var rootCount int
	var blockedRequests atomic.Int32
	validated := false

	observedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		allowedRoot := r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.RawQuery == "bundle_id=new"
		isBundlePost := r.Method == http.MethodPost && r.URL.Path == "/save-bundle" && r.URL.RawQuery == ""
		if !allowedRoot && !isBundlePost {
			blockedRequests.Add(1)
			http.Error(w, "This request is disabled in the manual locale QA harness.", http.StatusConflict)
			return
		}

		if isBundlePost {
			body, guard := manualWindowsLocaleQAExpectedBundleValidationPost(r, host, baseURL, csrf)
			stateMu.Lock()
			duplicate := bundlePostCount != 0
			if guard == "" && !duplicate {
				bundlePostCount++
			}
			stateMu.Unlock()
			if guard != "" || duplicate {
				if duplicate {
					guard = "duplicate-post"
				}
				blockedRequests.Add(1)
				http.Error(w, "This service bundle validation request is not allowlisted.", http.StatusConflict)
				bundlePosts <- bundlePostResult{
					status: http.StatusConflict, origin: append([]string(nil), r.Header.Values("Origin")...), guard: guard,
				}
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			recorder := &manualWindowsLocaleQAResponseWriter{ResponseWriter: w}
			productHandler.ServeHTTP(recorder, r)
			stateMu.Lock()
			validated = recorder.status == http.StatusSeeOther
			stateMu.Unlock()
			bundlePosts <- bundlePostResult{
				status: recorder.status, accepted: recorder.status == http.StatusSeeOther,
				origin: append([]string(nil), r.Header.Values("Origin")...),
				guard:  "accepted",
			}
			return
		}

		recorder := &manualWindowsLocaleQAResponseWriter{ResponseWriter: w}
		productHandler.ServeHTTP(recorder, r)
		stateMu.Lock()
		rootCount++
		postWasValidated := validated
		stateMu.Unlock()
		if postWasValidated {
			validationPages <- validationPageResult{status: recorder.status, body: append([]byte(nil), recorder.body.Bytes()...)}
		}
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

	fmt.Fprintf(os.Stderr, "Manual Windows locale form QA URL: %s/?bundle_id=new\n", baseURL)
	fmt.Fprintln(os.Stderr, "Open the URL in Windows Chrome. Switch Language to Korean, enter exactly one space in Service bundle name, then submit Save service bundle once.")
	fmt.Fprintln(os.Stderr, "The expected fixed validation error must render in Korean after the redirect. Only the root page, favicon, and this exact same-origin synthetic POST are allowed; no target, secret, or external service is used.")
	fmt.Fprintln(os.Stderr, "The harness rejects any other POST before the production handler and waits for at most 3 minutes.")
	_ = os.Stderr.Sync()

	var post bundlePostResult
	select {
	case post = <-bundlePosts:
		if !post.accepted || post.status != http.StatusSeeOther {
			t.Fatalf("synthetic validation POST was rejected before the product handler (status=%d, origin=%s, guard=%s)",
				post.status, manualWindowsLocaleQAOriginClass(post.origin, baseURL), post.guard)
		}
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("manual locale QA server stopped: %v", err)
		}
		t.Fatal("manual locale QA server stopped before the form submission")
	case <-time.After(3 * time.Minute):
		t.Fatal("manual locale form browser QA timed out")
	}

	var page validationPageResult
	select {
	case page = <-validationPages:
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("manual locale QA server stopped: %v", err)
		}
		t.Fatal("manual locale QA server stopped before the redirect page rendered")
	case <-time.After(15 * time.Second):
		t.Fatal("browser did not follow the validation redirect")
	}

	if page.status != http.StatusOK || !bytes.Contains(page.body, []byte(`<html lang="ko">`)) || !bytes.Contains(page.body, []byte(want)) {
		t.Fatal("form validation redirect did not render the Korean page language and fixed error")
	}
	stateMu.Lock()
	gotBundlePostCount, gotRootCount := bundlePostCount, rootCount
	stateMu.Unlock()
	if gotBundlePostCount != 1 || gotRootCount != 2 {
		t.Fatalf("browser flow made %d exact form POST(s) and %d root GET(s), want one POST and initial plus redirect GET", gotBundlePostCount, gotRootCount)
	}
	if blockedRequests.Load() != 0 {
		t.Fatalf("browser made %d requests outside the exact QA allowlist", blockedRequests.Load())
	}
	if secrets.loads.Load() != 0 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("validation accessed the secret store: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	if outboundRequests.Load() != 0 {
		t.Fatalf("validation made %d outbound service request(s)", outboundRequests.Load())
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatal("invalid service bundle changed the isolated settings file")
	}
}
