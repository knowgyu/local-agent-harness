package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

const manualWindowsDashboardBrowserQAGate = "LAH_ENABLE_MANUAL_WINDOWS_DASHBOARD_BROWSER_QA"

type manualWindowsDashboardQAFakeSecretStore struct {
	ref     string
	token   []byte
	loads   atomic.Int32
	writes  atomic.Int32
	deletes atomic.Int32
}

func (s *manualWindowsDashboardQAFakeSecretStore) Save(string, []byte) error {
	s.writes.Add(1)
	return nil
}

func (s *manualWindowsDashboardQAFakeSecretStore) Load(ref string) ([]byte, error) {
	s.loads.Add(1)
	if ref != s.ref {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), s.token...), nil
}

func (s *manualWindowsDashboardQAFakeSecretStore) Delete(string) error {
	s.deletes.Add(1)
	return nil
}

type manualWindowsDashboardQAResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func manualWindowsDashboardQAExpectedRejectedSave(r *http.Request, host, origin, csrf, targetID, targetName string) ([]byte, bool) {
	const maxBodySize = 4 << 10
	if r.Method != http.MethodPost || r.URL.Path != "/save-dashboard" || r.URL.RawQuery != "" ||
		r.Host != host || r.Body == nil || r.ContentLength < 0 || r.ContentLength > maxBodySize {
		return nil, false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != origin {
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
	parsed := r.Clone(r.Context())
	parsed.Body = io.NopCloser(bytes.NewReader(body))
	parsed.ContentLength = int64(len(body))
	if err := parsed.ParseForm(); err != nil || len(parsed.PostForm) != 5 || len(parsed.Form) != 5 {
		return nil, false
	}
	want := map[string]string{
		"csrf":            csrf,
		"target_id":       targetID,
		"dashboard_name":  targetName,
		"dashboard_url":   "http://dashboard.example.invalid/proxy",
		"dashboard_token": "",
	}
	for key, value := range want {
		values := parsed.PostForm[key]
		if len(values) != 1 || values[0] != value {
			return nil, false
		}
	}
	return body, true
}

func (w *manualWindowsDashboardQAResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *manualWindowsDashboardQAResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(body)
	return w.ResponseWriter.Write(body)
}

func TestManualWindowsDashboardQAExpectedRejectedSave(t *testing.T) {
	const (
		host       = "127.0.0.1:49671"
		origin     = "http://127.0.0.1:49671"
		csrf       = "synthetic-csrf"
		targetID   = "dashboard:synthetic"
		targetName = "Synthetic Dashboard"
	)
	form := url.Values{
		"csrf":            {csrf},
		"target_id":       {targetID},
		"dashboard_name":  {targetName},
		"dashboard_url":   {"http://dashboard.example.invalid/proxy"},
		"dashboard_token": {""},
	}
	body := form.Encode()
	tests := []struct {
		name      string
		body      string
		configure func(*http.Request)
		want      bool
	}{
		{name: "exact same-origin invalid scheme", body: body, want: true},
		{name: "wrong Host", body: body, configure: func(r *http.Request) { r.Host = "127.0.0.1:49672" }},
		{name: "wrong Origin", body: body, configure: func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:49672") }},
		{name: "duplicate Origin", body: body, configure: func(r *http.Request) { r.Header.Add("Origin", origin) }},
		{name: "query string", body: body, configure: func(r *http.Request) { r.URL.RawQuery = "unexpected=1" }},
		{name: "HTTPS URL", body: strings.Replace(body, "http%3A%2F%2F", "https%3A%2F%2F", 1)},
		{name: "extra field", body: body + "&unexpected=1"},
		{name: "duplicate field", body: body + "&dashboard_url=http%3A%2F%2Fdashboard.example.invalid%2Fproxy"},
		{name: "nonempty token", body: strings.Replace(body, "dashboard_token=", "dashboard_token=not-a-real-secret", 1)},
		{name: "unexpected content type", body: body, configure: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, origin+"/save-dashboard", strings.NewReader(tt.body))
			r.Host = host
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.configure != nil {
				tt.configure(r)
			}
			_, got := manualWindowsDashboardQAExpectedRejectedSave(r, host, origin, csrf, targetID, targetName)
			if got != tt.want {
				t.Fatalf("manual save allowlist accepted=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestManualWindowsDashboardDiagnosisBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("manual browser QA runs only on native Windows")
	}
	if os.Getenv(manualWindowsDashboardBrowserQAGate) != "1" {
		t.Skipf("set %s=1 to start the optional loopback browser QA harness", manualWindowsDashboardBrowserQAGate)
	}

	const (
		token    = "manual_dashboard_qa_fake_token_0123456789"
		rawError = "RAW_MANUAL_DASHBOARD_QA_ERROR_CANARY"
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	dashboard := cfg.DashboardTargets[0]
	cfg.ConnectionTests = map[string]connectionTest{
		dashboard.ID: {Result: "failure", CompletedAt: "2026-09-26T01:02:03Z"},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	secrets := &manualWindowsDashboardQAFakeSecretStore{ref: dashboard.SecretRef, token: []byte(token)}
	var dashboardRequests atomic.Int32
	var dashboardLogRequests atomic.Int32
	var dashboardRequestsBeforeLogs atomic.Int32
	var dashboardRequestsBeforeSave atomic.Int32
	dashboardClient := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		dashboardRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "dashboard.example.invalid" {
			t.Errorf("synthetic Dashboard transport received an unexpected request shape")
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("synthetic Dashboard request did not use its registered credential")
		}

		var status int
		var body string
		switch r.URL.Path {
		case "/proxy/api/v1/deployment/apps/api.v2":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api.v2","namespace":"apps","annotations":{"private":"` + token + `"}},"statusInfo":{"replicas":2,"updated":2,"available":1,"unavailable":1},"conditions":[]}`
		case "/proxy/api/v1/deployment/apps/api.v2/event":
			status = http.StatusForbidden
			body = `{"error":"` + rawError + `","url":"https://private.example.invalid/response"}`
		case "/proxy/api/v1/deployment/apps/api.v2/newreplicaset":
			status = http.StatusOK
			body = `{"objectMeta":{"name":"api-v2-abc"}}`
		case "/proxy/api/v1/deployment/apps/api.v2/oldreplicaset":
			status = http.StatusOK
			body = `{"listMeta":{"totalItems":0},"replicaSets":[],"errors":[]}`
		case "/proxy/api/v1/replicaset/apps/api-v2-abc/pod":
			status = http.StatusOK
			body = dashboardPodListFixture(1, []map[string]any{
				dashboardPodFixture("api-v2-abc-x1", "apps", "Running", "Running", "node-1", "web", "Running", 0),
			})
		case "/proxy/api/v1/log/apps/api-v2-abc-x1/web":
			dashboardLogRequests.Add(1)
			status = http.StatusOK
			body = dashboardLogFixture("api-v2-abc-x1", "web", false, []map[string]any{
				{"timestamp": "2026-09-26T01:02:03Z", "content": "ready " + token},
			})
		default:
			t.Errorf("unexpected synthetic Dashboard path: %s", r.URL.Path)
			return harborTestResponse(r, http.StatusNotFound, ""), nil
		}
		return harborTestResponse(r, status, body), nil
	})}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		client:     dashboardClient,
		csrf:       "manual-windows-dashboard-browser-qa-csrf",
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	a.host = host
	baseURL := "http://" + host
	productHandler := newLocalUIHandler(a)

	var diagnosisMu sync.Mutex
	var diagnosisCount int
	var diagnosisHosts []string
	var diagnosisOrigins [][]string
	var diagnosisStatuses []int
	var diagnosisBodies [][]byte
	var logsCount int
	var logsHosts []string
	var logsOrigins [][]string
	var logsStatuses []int
	var logsBodies [][]byte
	var saveCount int
	var saveHosts []string
	var saveOrigins [][]string
	var saveStatuses []int
	var saveBodies [][]byte
	var blockedUnexpectedRequests atomic.Int32
	var qaRequestMu sync.Mutex
	qaFinished := false
	observedProductHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qaRequestMu.Lock()
		defer qaRequestMu.Unlock()
		if qaFinished {
			blockedUnexpectedRequests.Add(1)
			http.Error(w, "The manual browser QA harness is complete.", http.StatusConflict)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		allowedRoot := r.Method == http.MethodGet && r.URL.Path == "/"
		isDiagnosis := r.URL.Path == "/dashboard-diagnosis" && r.Method == http.MethodPost
		isLogs := r.URL.Path == "/dashboard-diagnosis-logs" && r.Method == http.MethodPost
		isSave := r.URL.Path == "/save-dashboard" && r.Method == http.MethodPost
		if !allowedRoot && !isDiagnosis && !isLogs && !isSave {
			blockedUnexpectedRequests.Add(1)
			http.Error(w, "This request is disabled in the manual browser QA harness.", http.StatusConflict)
			return
		}
		if isSave {
			body, expected := manualWindowsDashboardQAExpectedRejectedSave(r, host, baseURL, a.csrf, dashboard.ID, dashboard.Name)
			diagnosisMu.Lock()
			alreadySubmitted := saveCount != 0
			diagnosisMu.Unlock()
			if !expected || alreadySubmitted {
				blockedUnexpectedRequests.Add(1)
				http.Error(w, "This save request is disabled in the manual browser QA harness.", http.StatusConflict)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			diagnosisMu.Lock()
			saveCount++
			saveHosts = append(saveHosts, r.Host)
			saveOrigins = append(saveOrigins, append([]string(nil), r.Header.Values("Origin")...))
			diagnosisMu.Unlock()
			dashboardRequestsBeforeSave.Store(dashboardRequests.Load())
			recorder := &manualWindowsDashboardQAResponseWriter{ResponseWriter: w}
			productHandler.ServeHTTP(recorder, r)
			diagnosisMu.Lock()
			saveStatuses = append(saveStatuses, recorder.status)
			saveBodies = append(saveBodies, append([]byte(nil), recorder.body.Bytes()...))
			diagnosisMu.Unlock()
			return
		}
		if !isDiagnosis && !isLogs {
			productHandler.ServeHTTP(w, r)
			return
		}
		diagnosisMu.Lock()
		if isDiagnosis {
			diagnosisCount++
			diagnosisHosts = append(diagnosisHosts, r.Host)
			diagnosisOrigins = append(diagnosisOrigins, append([]string(nil), r.Header.Values("Origin")...))
		} else {
			logsCount++
			logsHosts = append(logsHosts, r.Host)
			logsOrigins = append(logsOrigins, append([]string(nil), r.Header.Values("Origin")...))
			dashboardRequestsBeforeLogs.Store(dashboardRequests.Load())
		}
		diagnosisMu.Unlock()

		recorder := &manualWindowsDashboardQAResponseWriter{ResponseWriter: w}
		productHandler.ServeHTTP(recorder, r)
		diagnosisMu.Lock()
		if isDiagnosis {
			diagnosisStatuses = append(diagnosisStatuses, recorder.status)
			diagnosisBodies = append(diagnosisBodies, append([]byte(nil), recorder.body.Bytes()...))
		} else {
			logsStatuses = append(logsStatuses, recorder.status)
			logsBodies = append(logsBodies, append([]byte(nil), recorder.body.Bytes()...))
		}
		diagnosisMu.Unlock()
	})

	completionTokenBytes := make([]byte, 32)
	if _, err := rand.Read(completionTokenBytes); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	completionToken := hex.EncodeToString(completionTokenBytes)
	completed := make(chan struct{}, 1)
	completionPageHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Host != host || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'none'"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>Manual browser QA</title><body><main><h1>Manual browser QA</h1><p>Complete only after reviewing the validation errors, synthetic diagnosis, and masked log output.</p><form method=\"post\" action=\"/__test__/complete\"><input type=\"hidden\" name=\"qa_signal\" value=\"%s\"><button type=\"submit\">Complete browser review</button></form></main></body></html>", completionToken)
	})
	completionHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
			return
		}
		origins := r.Header.Values("Origin")
		if r.Host != host || len(origins) != 1 || origins[0] != baseURL || r.URL.RawQuery != "" {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		contentTypes := r.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			http.Error(w, "Request rejected.", http.StatusBadRequest)
			return
		}
		mediaType, _, parseErr := mime.ParseMediaType(contentTypes[0])
		if parseErr != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
			http.Error(w, "Request rejected.", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || len(r.PostForm["qa_signal"]) != 1 || r.PostForm.Get("qa_signal") != completionToken {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}

		qaRequestMu.Lock()
		defer qaRequestMu.Unlock()
		if qaFinished {
			http.Error(w, "QA completion was already received.", http.StatusConflict)
			return
		}
		diagnosisMu.Lock()
		validDiagnosis := diagnosisCount == 1 && len(diagnosisHosts) == 1 && diagnosisHosts[0] == host &&
			len(diagnosisOrigins) == 1 && len(diagnosisOrigins[0]) == 1 && diagnosisOrigins[0][0] == baseURL &&
			len(diagnosisStatuses) == 1 && diagnosisStatuses[0] == http.StatusOK && len(diagnosisBodies) == 1
		validLogs := logsCount == 1 && len(logsHosts) == 1 && logsHosts[0] == host &&
			len(logsOrigins) == 1 && len(logsOrigins[0]) == 1 && logsOrigins[0][0] == baseURL &&
			len(logsStatuses) == 1 && logsStatuses[0] == http.StatusOK && len(logsBodies) == 1
		validSave := saveCount == 1 && len(saveHosts) == 1 && saveHosts[0] == host &&
			len(saveOrigins) == 1 && len(saveOrigins[0]) == 1 && saveOrigins[0][0] == baseURL &&
			len(saveStatuses) == 1 && saveStatuses[0] == http.StatusUnprocessableEntity && len(saveBodies) == 1 &&
			bytes.Contains(saveBodies[0], []byte(`id="dashboard-url-error"`)) &&
			bytes.Contains(saveBodies[0], []byte(`id="dashboard-url"`)) &&
			bytes.Contains(saveBodies[0], []byte(`autofocus aria-invalid="true"`)) &&
			bytes.Contains(saveBodies[0], []byte("http://dashboard.example.invalid/proxy"))
		diagnosisMu.Unlock()
		if !validDiagnosis || !validLogs || !validSave || blockedUnexpectedRequests.Load() != 0 {
			http.Error(w, "Complete the same-origin validation, diagnosis, and log review without any other app request.", http.StatusConflict)
			return
		}
		qaFinished = true
		select {
		case completed <- struct{}{}:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "QA completion was already received.", http.StatusConflict)
		}
	})
	testMux := http.NewServeMux()
	testMux.Handle("/__test__/complete", completionHandler)
	testMux.Handle("/__test__/complete-page", completionPageHandler)
	testMux.Handle("/", observedProductHandler)
	server := &http.Server{
		Handler:           testMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	})

	uiClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	pageResponse, err := uiClient.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(pageResponse.Body)
	pageResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pageResponse.StatusCode != http.StatusOK ||
		!strings.Contains(string(page), "action=\"/dashboard-diagnosis\"") ||
		!strings.Contains(string(page), "data-service-bundle=\"Inventory\"") ||
		!strings.Contains(string(page), "/dashboard-diagnosis-logs") {
		t.Fatalf("root page did not render the saved Dashboard diagnosis and logs flow: status=%d", pageResponse.StatusCode)
	}
	completionPageResponse, err := uiClient.Get(baseURL + "/__test__/complete-page")
	if err != nil {
		t.Fatal(err)
	}
	completionPage, err := io.ReadAll(completionPageResponse.Body)
	completionPageResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if completionPageResponse.StatusCode != http.StatusOK ||
		!strings.Contains(string(completionPage), "action=\"/__test__/complete\"") ||
		!strings.Contains(string(completionPage), "name=\"qa_signal\"") {
		t.Fatalf("test-only completion page was unavailable: status=%d", completionPageResponse.StatusCode)
	}
	fmt.Fprintf(os.Stderr, "\nManual Windows Dashboard browser QA URL: %s/\n", baseURL)
	fmt.Fprintln(os.Stderr, "Open this page in a Windows browser. First check Dashboard URL and Harbor pattern validation in English and Korean using only synthetic invalid text. Confirm submit is blocked and focus stays on the invalid field.")
	fmt.Fprintln(os.Stderr, "Only the root page, same-origin diagnosis/log POSTs, and one exact synthetic save attempt are allowed. Every other app request is rejected before the production handler. The save URL is HTTP and must return 422 before mutation.")
	fmt.Fprintln(os.Stderr, "Then run the read-only Dashboard diagnosis once and inspect its partial result.")
	fmt.Fprintln(os.Stderr, "Then use its explicit log action once for the listed synthetic Pod/container. Confirm the log text is masked and shown as text.")
	fmt.Fprintln(os.Stderr, "Then select the existing synthetic Dashboard target, set its URL to exactly http://dashboard.example.invalid/proxy, leave the token blank, and submit once. This request must receive HTTP 422 before any save/credential/service operation.")
	fmt.Fprintf(os.Stderr, "After reviewing those results, open %s/__test__/complete-page and press its same-origin completion button.\n", baseURL)
	fmt.Fprintln(os.Stderr, "The loopback server waits for at most 6 minutes.")
	_ = os.Stderr.Sync()

	select {
	case <-completed:
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("manual QA server stopped: %v", err)
		}
		t.Fatal("manual QA server stopped before completion")
	case <-time.After(6 * time.Minute):
		t.Fatal("manual QA browser completion signal timed out")
	}

	diagnosisMu.Lock()
	postCount := diagnosisCount
	diagnosisWasSameOrigin := postCount == 1 && len(diagnosisHosts) == 1 && diagnosisHosts[0] == host &&
		len(diagnosisOrigins) == 1 && len(diagnosisOrigins[0]) == 1 && diagnosisOrigins[0][0] == baseURL &&
		len(diagnosisStatuses) == 1 && diagnosisStatuses[0] == http.StatusOK && len(diagnosisBodies) == 1
	var diagnosisBody []byte
	if len(diagnosisBodies) == 1 {
		diagnosisBody = diagnosisBodies[0]
	}
	logsWereSameOrigin := logsCount == 1 && len(logsHosts) == 1 && logsHosts[0] == host &&
		len(logsOrigins) == 1 && len(logsOrigins[0]) == 1 && logsOrigins[0][0] == baseURL &&
		len(logsStatuses) == 1 && logsStatuses[0] == http.StatusOK && len(logsBodies) == 1
	saveWasSameOrigin := saveCount == 1 && len(saveHosts) == 1 && saveHosts[0] == host &&
		len(saveOrigins) == 1 && len(saveOrigins[0]) == 1 && saveOrigins[0][0] == baseURL &&
		len(saveStatuses) == 1 && saveStatuses[0] == http.StatusUnprocessableEntity && len(saveBodies) == 1 &&
		bytes.Contains(saveBodies[0], []byte(`id="dashboard-url-error"`)) &&
		bytes.Contains(saveBodies[0], []byte(`id="dashboard-url"`)) &&
		bytes.Contains(saveBodies[0], []byte(`autofocus aria-invalid="true"`)) &&
		bytes.Contains(saveBodies[0], []byte("http://dashboard.example.invalid/proxy"))
	var logsBody []byte
	if len(logsBodies) == 1 {
		logsBody = logsBodies[0]
	}
	var saveBody []byte
	if len(saveBodies) == 1 {
		saveBody = saveBodies[0]
	}
	diagnosisMu.Unlock()
	if !diagnosisWasSameOrigin {
		t.Fatalf("diagnosis flow did not make exactly one successful same-origin POST (count=%d)", postCount)
	}
	if !logsWereSameOrigin {
		t.Fatalf("log flow did not make exactly one successful same-origin POST (count=%d)", logsCount)
	}
	if !saveWasSameOrigin {
		t.Fatalf("save validation did not make exactly one same-origin HTTP 422 POST (count=%d)", saveCount)
	}
	if blockedUnexpectedRequests.Load() != 0 {
		t.Fatalf("browser made %d unexpected request(s); the harness blocked them before the production handlers", blockedUnexpectedRequests.Load())
	}
	requestsBeforeLogs := dashboardRequestsBeforeLogs.Load()
	logRequests := dashboardRequests.Load() - requestsBeforeLogs
	if requestsBeforeLogs == 0 || requestsBeforeLogs > 5 || logRequests < 1 || logRequests > 4 || dashboardLogRequests.Load() != 1 {
		t.Fatalf("synthetic Dashboard GET budgets were exceeded: diagnosis=%d logs=%d log-endpoint=%d", requestsBeforeLogs, logRequests, dashboardLogRequests.Load())
	}
	if dashboardRequests.Load() != dashboardRequestsBeforeSave.Load() {
		t.Fatal("Dashboard save validation made an outbound service request")
	}
	var diagnosisResponse dashboardDiagnosisUIResponse
	if err := json.Unmarshal(diagnosisBody, &diagnosisResponse); err != nil || diagnosisResponse.Result == nil || diagnosisResponse.Result.Pods == nil {
		t.Fatal("diagnosis response did not include the synthetic Pod/container choice")
	}
	foundChoice := false
	for _, pod := range diagnosisResponse.Result.Pods.Pods {
		if pod.Name != "api-v2-abc-x1" {
			continue
		}
		for _, container := range pod.Containers {
			if container.Name == "web" {
				foundChoice = true
			}
		}
	}
	if !foundChoice {
		t.Fatal("diagnosis response did not include the synthetic Pod/container choice")
	}
	for _, forbidden := range []string{token, rawError, "dashboard.example.invalid", "private.example.invalid"} {
		if bytes.Contains(diagnosisBody, []byte(forbidden)) {
			t.Fatalf("diagnosis response included a forbidden synthetic marker")
		}
		if bytes.Contains(logsBody, []byte(forbidden)) {
			t.Fatalf("log response included a forbidden synthetic marker")
		}
	}
	if !bytes.Contains(logsBody, []byte("[REDACTED]")) {
		t.Fatal("log response did not show the synthetic credential as redacted")
	}
	var logsResponse dashboardDiagnosisLogsUIResponse
	if err := json.Unmarshal(logsBody, &logsResponse); err != nil || logsResponse.Result == nil || len(logsResponse.Result.Lines) != 1 ||
		logsResponse.Result.Lines[0].Content != "ready [REDACTED]" {
		t.Fatal("log response did not contain the one masked synthetic line")
	}
	if secrets.loads.Load() != 2 || secrets.writes.Load() != 0 || secrets.deletes.Load() != 0 {
		t.Fatalf("secret-store operations: loads=%d writes=%d deletes=%d", secrets.loads.Load(), secrets.writes.Load(), secrets.deletes.Load())
	}
	if bytes.Contains(saveBody, []byte(token)) || bytes.Contains(saveBody, []byte(rawError)) {
		t.Fatal("Dashboard save validation response included a forbidden synthetic marker")
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Fatal("manual diagnosis changed settings or connection-test history")
	}
}
