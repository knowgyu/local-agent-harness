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
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	registrationUILiveQADirEnv           = "LAH_REGISTRATION_UI_QA_DIR"
	registrationUILiveQAReuseSettingsEnv = "LAH_REGISTRATION_UI_QA_REUSE_SETTINGS"
	registrationUILiveQALimit            = 12 * time.Hour
	registrationUILiveQAGitHubHost       = "github.qa.invalid"
	registrationUILiveQAJenkinsHost      = "jenkins.qa.invalid"
	registrationUILiveQADashboardHost    = "dashboard.qa.invalid"
	registrationUILiveQAGitHubToken      = "SYNTHETIC_QA_GITHUB_TOKEN_20261011"
	registrationUILiveQAJenkinsToken     = "SYNTHETIC_QA_JENKINS_TOKEN_20261011"
	registrationUILiveQADashboardToken   = "SYNTHETIC_QA_DASHBOARD_BEARER_20261011"
)

const registrationUILiveQABanner = `<div role="alert" style="position:sticky;top:0;z-index:9999;padding:9px;background:#fff1cc;color:#262626;border:2px solid #8b4513;text-align:center;font-weight:700">합성 서비스 검증 화면 · 실제 비밀값 입력 금지</div>`

type registrationUILiveQAMetadata struct {
	URL                         string `json:"url"`
	PID                         int    `json:"pid"`
	StartedAt                   string `json:"started_at"`
	ConfigPath                  string `json:"config_path"`
	SettingsMode                string `json:"settings_mode"`
	SetupDraftProposalQueueMode string `json:"setup_draft_proposal_queue_mode"`
	StopFile                    string `json:"stop_file"`
	StderrFile                  string `json:"server_stderr_file"`
}

type registrationUILiveQAMemorySecretStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (s *registrationUILiveQAMemorySecretStore) Save(ref string, value []byte) error {
	if s == nil || ref == "" {
		return errors.New("synthetic credential store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string][]byte)
	}
	if previous := s.values[ref]; previous != nil {
		clear(previous)
	}
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *registrationUILiveQAMemorySecretStore) Load(ref string) ([]byte, error) {
	if s == nil {
		return nil, errors.New("synthetic credential store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	if !ok {
		return nil, errors.New("synthetic credential is unavailable")
	}
	return append([]byte(nil), value...), nil
}

func (s *registrationUILiveQAMemorySecretStore) Delete(ref string) error {
	if s == nil {
		return errors.New("synthetic credential store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if value := s.values[ref]; value != nil {
		clear(value)
	}
	delete(s.values, ref)
	return nil
}

// registrationUILiveQASyntheticTransport never opens a socket. It responds
// only to the three exact synthetic hosts and expected read-only API paths;
// every other destination stays blocked. Credentials are compared in memory
// and never included in responses or errors.
type registrationUILiveQASyntheticTransport struct{}

func (registrationUILiveQASyntheticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Context().Err() != nil {
		return nil, errors.New("synthetic service request is unavailable")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" || req.URL.Opaque != "" ||
		req.URL.RawPath != "" || req.URL.Fragment != "" || req.URL.ForceQuery {
		return nil, errors.New("synthetic QA transport blocked the destination")
	}

	status := http.StatusOK
	body := ""
	switch strings.ToLower(req.URL.Hostname()) {
	case registrationUILiveQAGitHubHost:
		if req.Method != http.MethodGet || req.URL.Path != "/api/v3/repos/synthetic/hello-world" || req.URL.RawQuery != "" {
			return nil, errors.New("synthetic QA transport blocked the destination")
		}
		if req.Header.Get("Authorization") != "Bearer "+registrationUILiveQAGitHubToken {
			status, body = http.StatusUnauthorized, `{"message":"Synthetic QA credentials rejected"}`
		} else {
			body = `{"name":"hello-world","full_name":"synthetic/hello-world","private":false,"default_branch":"main"}`
		}
	case registrationUILiveQAJenkinsHost:
		if req.Method != http.MethodGet || req.URL.Path != "/job/qa-smoke/api/json" ||
			req.URL.RawQuery != "tree=name,fullName,buildable,lastBuild[number,result,building]" {
			return nil, errors.New("synthetic QA transport blocked the destination")
		}
		username, password, hasAuth := req.BasicAuth()
		if !hasAuth || username != "qa-bot" || password != registrationUILiveQAJenkinsToken {
			status, body = http.StatusUnauthorized, `{"message":"Synthetic QA credentials rejected"}`
		} else {
			body = `{"name":"qa-smoke","fullName":"qa-smoke","buildable":true,"lastBuild":{"number":7,"result":"SUCCESS","building":false}}`
		}
	case registrationUILiveQADashboardHost:
		if req.Method != http.MethodGet || req.URL.Path != "/proxy/api/v1/namespace" || req.URL.RawQuery != "itemsPerPage=100" {
			return nil, errors.New("synthetic QA transport blocked the destination")
		}
		if req.Header.Get("Authorization") != "Bearer "+registrationUILiveQADashboardToken {
			status, body = http.StatusUnauthorized, `{"message":"Synthetic QA credentials rejected"}`
		} else {
			body = `{"listMeta":{"totalItems":2},"namespaces":[{"objectMeta":{"name":"default"}},{"objectMeta":{"name":"qa-sandbox"}}]}`
		}
	default:
		return nil, errors.New("synthetic QA transport blocked the destination")
	}

	responseBody := io.NopCloser(strings.NewReader(body))
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          responseBody,
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// TestRegistrationUILiveQAHelper runs only when an explicit output directory
// is provided. It uses an in-memory fake credential store and either a fresh
// settings file or one explicitly selected, validated synthetic settings file
// below output/ux-preview/runtime. Reusing the same settings path preserves its
// durable onboarding sidecar. The test process exits after the stop file appears
// or the bounded live-review interval expires.
func TestRegistrationUILiveQAHelper(t *testing.T) {
	outputDir := strings.TrimSpace(os.Getenv(registrationUILiveQADirEnv))
	if outputDir == "" {
		t.Skip("set LAH_REGISTRATION_UI_QA_DIR under output/ux-preview/runtime to start the isolated live QA server")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the live registration UI review requires Windows so the local target forms are enabled")
	}

	baseDir, err := registrationUILiveQABoundOutputDir(outputDir)
	if err != nil {
		t.Fatalf("invalid registration UI QA output directory: %v", err)
	}
	runDir, err := registrationUILiveQANewRunDir(baseDir)
	if err != nil {
		t.Fatalf("could not create an isolated registration UI QA directory: %v", err)
	}
	configPath := filepath.Join(runDir, "settings.json")
	settingsMode := "fresh_synthetic_fixture"
	reuseSettingsPath := strings.TrimSpace(os.Getenv(registrationUILiveQAReuseSettingsEnv))
	var reusableConfig *config
	if reuseSettingsPath == "" {
		if err := registrationUILiveQAWriteEmptyConfig(configPath); err != nil {
			t.Fatalf("could not initialize isolated settings: %v", err)
		}
	} else {
		var err error
		configPath, reusableConfig, err = registrationUILiveQALoadReusableSettings(reuseSettingsPath)
		if err != nil {
			t.Fatalf("could not reuse the selected synthetic settings: %v", err)
		}
		settingsMode = "reused_synthetic_fixture"
	}
	csrf, err := registrationUILiveQARandomHex(32)
	if err != nil {
		t.Fatalf("could not create an isolated request token: %v", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not bind the loopback UI server: %v", err)
	}
	defer listener.Close()
	host := listener.Addr().String()
	baseURL := "http://" + host

	serverLogPath := filepath.Join(runDir, "server.stderr.log")
	serverLog, err := os.OpenFile(serverLogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("could not create the isolated server error log: %v", err)
	}
	defer serverLog.Close()

	secrets := &registrationUILiveQAMemorySecretStore{values: make(map[string][]byte)}
	if reusableConfig != nil {
		if err := registrationUILiveQASeedFixtureCredentials(secrets, *reusableConfig); err != nil {
			t.Fatalf("could not restore synthetic fixture credentials: %v", err)
		}
	}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		client:     &http.Client{Timeout: 12 * time.Second, Transport: registrationUILiveQASyntheticTransport{}},
		host:       host,
		csrf:       csrf,
		ssh:        newSSHOperationController(newSSHConfigStore(configPath), nil, nil),
		setupDrafts: newSetupDraftControllerWithQueue(
			newSetupDraftConfigStore(configPath),
			&fakeSetupDraftQueueStore{},
		),
	}
	server := &http.Server{
		Handler:           registrationUILiveQABannerHandler(newLocalUIHandler(a)),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(serverLog, "http: ", log.LstdFlags),
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		select {
		case <-serveDone:
		case <-ctx.Done():
			_ = server.Close()
		}
	}()

	if err := registrationUILiveQAVerifyServer(baseURL, csrf); err != nil {
		t.Fatalf("live QA server did not pass its local HTTP checks: %v", err)
	}
	stopPath := filepath.Join(runDir, "stop")
	metadata := registrationUILiveQAMetadata{
		URL:                         baseURL + "/",
		PID:                         os.Getpid(),
		StartedAt:                   time.Now().UTC().Format(time.RFC3339),
		ConfigPath:                  configPath,
		SettingsMode:                settingsMode,
		SetupDraftProposalQueueMode: "empty_process_local",
		StopFile:                    stopPath,
		StderrFile:                  serverLogPath,
	}
	metadataPath := filepath.Join(runDir, "metadata.json")
	if err := registrationUILiveQAWriteExclusiveJSON(metadataPath, metadata); err != nil {
		t.Fatalf("could not write live QA server metadata: %v", err)
	}
	urlPath := filepath.Join(runDir, "url.txt")
	if err := registrationUILiveQAWriteExclusive(urlPath, []byte(metadata.URL+"\n")); err != nil {
		t.Fatalf("could not write live QA URL: %v", err)
	}
	pidPath := filepath.Join(runDir, "pid.txt")
	if err := registrationUILiveQAWriteExclusive(pidPath, []byte(fmt.Sprintf("%d\n", metadata.PID))); err != nil {
		t.Fatalf("could not write live QA process ID: %v", err)
	}

	fmt.Fprintf(os.Stdout, "REGISTRATION_UI_QA_URL=%s\n", metadata.URL)
	fmt.Fprintf(os.Stdout, "REGISTRATION_UI_QA_METADATA=%s\n", metadataPath)
	fmt.Fprintf(os.Stdout, "REGISTRATION_UI_QA_STOP_FILE=%s\n", stopPath)
	t.Logf("live QA server ready at %s (metadata: %s)", metadata.URL, metadataPath)

	timer := time.NewTimer(registrationUILiveQALimit)
	defer timer.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("live QA HTTP server stopped unexpectedly: %v", err)
			}
			return
		case <-ticker.C:
			if _, err := os.Stat(stopPath); err == nil {
				t.Log("stop file detected; shutting down the isolated live QA server")
				return
			} else if !os.IsNotExist(err) {
				t.Fatalf("could not check the live QA stop file: %v", err)
			}
		case <-timer.C:
			t.Logf("live QA server reached its %s limit and is shutting down", registrationUILiveQALimit)
			return
		}
	}
}

func registrationUILiveQABoundOutputDir(provided string) (string, error) {
	allowed, err := filepath.Abs(filepath.Join("output", "ux-preview", "runtime"))
	if err != nil {
		return "", err
	}
	selected, err := filepath.Abs(provided)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(allowed, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(selected, 0o700); err != nil {
		return "", err
	}
	allowedReal, err := filepath.EvalSymlinks(allowed)
	if err != nil {
		return "", err
	}
	selectedReal, err := filepath.EvalSymlinks(selected)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(allowedReal, selectedReal)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("the directory must stay inside output/ux-preview/runtime")
	}
	return selectedReal, nil
}

func registrationUILiveQALoadReusableSettings(provided string) (string, *config, error) {
	selected := strings.TrimSpace(provided)
	if selected == "" {
		return "", nil, errors.New("the settings path is empty")
	}
	allowed, err := filepath.Abs(filepath.Join("output", "ux-preview", "runtime"))
	if err != nil {
		return "", nil, err
	}
	selected, err = filepath.Abs(selected)
	if err != nil {
		return "", nil, err
	}
	allowedReal, err := filepath.EvalSymlinks(allowed)
	if err != nil {
		return "", nil, err
	}
	selectedReal, err := filepath.EvalSymlinks(selected)
	if err != nil {
		return "", nil, err
	}
	relative, err := filepath.Rel(allowedReal, selectedReal)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", nil, errors.New("the settings file must stay inside output/ux-preview/runtime")
	}
	selectedInfo, err := os.Lstat(selected)
	if err != nil || !selectedInfo.Mode().IsRegular() || selectedInfo.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("the settings path must name a regular file")
	}
	cfg, err := readConfig(selectedReal)
	if err != nil {
		return "", nil, errors.New("the selected settings file is invalid")
	}
	if err := registrationUILiveQAVerifyReusableConfig(cfg); err != nil {
		return "", nil, err
	}
	return selectedReal, &cfg, nil
}

func registrationUILiveQAVerifyReusableConfig(cfg config) error {
	if len(cfg.GitHubTargets) != 1 || len(cfg.JenkinsTargets) != 1 || len(cfg.DashboardTargets) != 1 ||
		len(cfg.HarborTargets) != 0 || len(cfg.ServiceBundles) != 0 || len(cfg.PythonTasks) != 0 || len(cfg.SSHTargets) != 0 {
		return errors.New("the settings file must contain exactly the three supported synthetic targets")
	}
	github := cfg.GitHubTargets[0]
	jenkins := cfg.JenkinsTargets[0]
	dashboard := cfg.DashboardTargets[0]
	if github.Origin != "https://"+registrationUILiveQAGitHubHost || github.Repository != "synthetic/hello-world" || github.Disabled ||
		jenkins.BaseURL != "https://"+registrationUILiveQAJenkinsHost || jenkins.Username != "qa-bot" || jenkins.JobPath != "qa-smoke" || jenkins.Environment != "QA" || jenkins.NonProductionPreapproved || jenkins.Disabled ||
		dashboard.BaseURL != "https://"+registrationUILiveQADashboardHost+"/proxy" || dashboard.Disabled {
		return errors.New("the settings file does not match the exact GitHub, Jenkins, and Dashboard QA fixtures")
	}
	refs := map[string]bool{
		github.SecretRef:    true,
		jenkins.SecretRef:   true,
		dashboard.SecretRef: true,
	}
	if len(refs) != 3 || refs[""] || len(cfg.NamedSecrets) != 3 {
		return errors.New("the settings file must use exactly three distinct synthetic credential references")
	}
	for _, secret := range cfg.NamedSecrets {
		if !refs[secret.CredentialRef] {
			return errors.New("the settings file contains an unexpected named credential")
		}
	}
	return nil
}

func registrationUILiveQASeedFixtureCredentials(store *registrationUILiveQAMemorySecretStore, cfg config) error {
	if store == nil {
		return errors.New("synthetic credential store is unavailable")
	}
	credentials := []struct {
		ref   string
		value string
	}{
		{ref: cfg.GitHubTargets[0].SecretRef, value: registrationUILiveQAGitHubToken},
		{ref: cfg.JenkinsTargets[0].SecretRef, value: registrationUILiveQAJenkinsToken},
		{ref: cfg.DashboardTargets[0].SecretRef, value: registrationUILiveQADashboardToken},
	}
	for _, credential := range credentials {
		value := []byte(credential.value)
		if err := store.Save(credential.ref, value); err != nil {
			clear(value)
			return err
		}
		clear(value)
	}
	return nil
}

func registrationUILiveQANewRunDir(base string) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		suffix, err := registrationUILiveQARandomHex(6)
		if err != nil {
			return "", err
		}
		runDir := filepath.Join(base, fmt.Sprintf("registration-ui-qa-%d-%s", os.Getpid(), suffix))
		if err := os.Mkdir(runDir, 0o700); err == nil {
			return runDir, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique output directory")
}

func registrationUILiveQAWriteEmptyConfig(path string) error {
	data, err := json.Marshal(config{Version: configVersion})
	if err != nil {
		return err
	}
	return registrationUILiveQAWriteExclusive(path, append(data, '\n'))
}

func registrationUILiveQAWriteExclusiveJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return registrationUILiveQAWriteExclusive(path, append(data, '\n'))
}

func registrationUILiveQAWriteExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func registrationUILiveQARandomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func registrationUILiveQAVerifyServer(baseURL, csrf string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(baseURL + "/")
	if err != nil {
		return err
	}
	page, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK || !bytes.Contains(page, []byte(registrationUILiveQABanner)) || !bytes.Contains(page, []byte("lah-registration-preview-form")) {
		return fmt.Errorf("root page returned status %d or is missing the isolated QA banner/registration form", response.StatusCode)
	}

	form := url.Values{
		"csrf":    {csrf},
		"service": {string(SetupServiceGitHub)},
		"url":     {"https://github.example.invalid/example/repository"},
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/preview-registration-url", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", baseURL)
	response, err = client.Do(request)
	if err != nil {
		return err
	}
	previewBody, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	closeErr = response.Body.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK || !bytes.Contains(previewBody, []byte(`"repository":"example/repository"`)) {
		return fmt.Errorf("registration preview returned status %d or an unexpected candidate", response.StatusCode)
	}
	return nil
}

func registrationUILiveQABannerHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		body := append([]byte(nil), recorder.Body.Bytes()...)
		if strings.HasPrefix(strings.ToLower(recorder.Header().Get("Content-Type")), "text/html") {
			body = registrationUILiveQAInjectBanner(body)
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.Header().Set("X-Registration-UI-QA", "isolated-fake-store")
		if recorder.Header().Get("Content-Length") != "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		}
		status := recorder.Code
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func registrationUILiveQAInjectBanner(body []byte) []byte {
	lowerBody := bytes.ToLower(body)
	start := bytes.Index(lowerBody, []byte("<body"))
	if start < 0 {
		return body
	}
	endOffset := bytes.IndexByte(body[start:], '>')
	if endOffset < 0 {
		return body
	}
	end := start + endOffset + 1
	result := make([]byte, 0, len(body)+len(registrationUILiveQABanner))
	result = append(result, body[:end]...)
	result = append(result, registrationUILiveQABanner...)
	result = append(result, body[end:]...)
	return result
}
