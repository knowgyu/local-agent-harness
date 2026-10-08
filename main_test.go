package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type memorySecrets map[string][]byte

func (m memorySecrets) Save(ref string, value []byte) error {
	m[ref] = append([]byte(nil), value...)
	return nil
}

func (m memorySecrets) Load(ref string) ([]byte, error) {
	value, ok := m[ref]
	if !ok {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), value...), nil
}

func (m memorySecrets) Delete(ref string) error {
	delete(m, ref)
	return nil
}

func assertFirstInvalidFieldFocused(t *testing.T, page, fieldID string) {
	t.Helper()
	start := strings.Index(page, `<input id="`+fieldID+`"`)
	if start < 0 {
		t.Fatalf("invalid field %q is missing", fieldID)
	}
	end := strings.Index(page[start:], ">")
	if end < 0 {
		t.Fatalf("invalid field %q has no closing angle bracket", fieldID)
	}
	input := page[start : start+end]
	if !strings.Contains(input, "autofocus") || !strings.Contains(input, `aria-invalid="true"`) {
		t.Fatalf("invalid field %q is not focused and marked invalid: %s", fieldID, input)
	}
	counts, err := countHTMLValidationAttributes(page)
	if err != nil {
		t.Fatalf("could not inspect validation page autofocus attributes: %v", err)
	}
	if counts.autofocusElements != 1 {
		t.Fatalf("validation page has %d autofocus elements, want exactly one", counts.autofocusElements)
	}
}

func approvedJenkinsTarget(target jenkinsTarget) jenkinsTarget {
	target.NonProductionPreapproved = true
	target.NonProductionApprovalScope = jenkinsTriggerApprovalScope(target)
	return target
}

type recordingSecrets struct{ writes, loads int }

func (s *recordingSecrets) Save(string, []byte) error   { s.writes++; return nil }
func (s *recordingSecrets) Load(string) ([]byte, error) { s.loads++; return nil, http.ErrNoCookie }
func (*recordingSecrets) Delete(string) error           { return nil }

type cleanupFailureSecrets struct {
	configPath string
	deletes    int
}

func (s *cleanupFailureSecrets) Save(string, []byte) error {
	return os.Mkdir(s.configPath, 0o700)
}
func (*cleanupFailureSecrets) Load(string) ([]byte, error) { return nil, http.ErrNoCookie }
func (s *cleanupFailureSecrets) Delete(string) error {
	s.deletes++
	return errors.New("credential deletion failed")
}

func TestMCPRepositoryReadBoundary(t *testing.T) {
	const token = "github_pat_canary_secret_0123456789"
	const secondToken = "ghp_canary_value_not_for_model_123456"
	var secondHits atomic.Int32

	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v3/repos/ops/agent"; got != want {
			t.Errorf("request path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
			t.Errorf("authorization was not the registered token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main","html_url":"https://github.example.invalid/ops/agent","description":"` + token + ` ghp_canary_value_not_for_model_123456 token=hidden-value Authorization: Bearer hidden-bearer","unprojected":"must-not-appear"}`))
	}))
	defer mock.Close()

	ref := "cred:0123456789abcdef0123456789abcdef"
	secrets := memorySecrets{ref: []byte(token)}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, Target: &target{Name: "Engineering", Origin: mock.URL, Repository: "ops/agent", SecretRef: ref}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, secrets: secrets, client: mock.Client()}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "boundary-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "github_repository", Arguments: map[string]any{"target": "Engineering"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{token, secondToken, "hidden-value", "hidden-bearer", "unprojected"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("MCP result leaked %q: %s", leaked, encoded)
		}
	}
	if result.IsError || !strings.Contains(string(encoded), `"full_name":"ops/agent"`) || !strings.Contains(string(encoded), `"private":true`) {
		t.Fatalf("unexpected MCP result: %s", encoded)
	}

	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"agent","full_name":"ops/agent"}`))
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/api/v3/repos/ops/agent", http.StatusFound)
	}))
	defer redirectSource.Close()
	client := redirectSource.Client()
	transport := client.Transport.(*http.Transport).Clone()
	pool := transport.TLSClientConfig.RootCAs.Clone()
	pool.AddCert(redirectTarget.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	client.Transport = transport
	client.CheckRedirect = newGitHubClient().CheckRedirect
	client.Timeout = 3 * time.Second
	redirected := target{Name: "Engineering", Origin: redirectSource.URL, Repository: "ops/agent", SecretRef: ref}
	if _, err := fetchRepository(ctx, redirected, token, client); err == nil || secondHits.Load() != 0 {
		t.Fatal("cross-origin redirect was followed")
	}

	oversize := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxAPIBytes+1)))
	}))
	defer oversize.Close()
	large := target{Name: "Engineering", Origin: oversize.URL, Repository: "ops/agent", SecretRef: ref}
	if _, err := fetchRepository(ctx, large, token, oversize.Client()); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("oversized response was not rejected safely")
	}
}

func TestGitHubRepositoryIdentityAndHTMLURLProjection(t *testing.T) {
	tests := []struct {
		name         string
		fullName     string
		htmlURL      string
		wantErr      bool
		wantFullName string
	}{
		{
			name:         "canonical case identity",
			fullName:     "Ops/Agent",
			htmlURL:      "https://enterprise.example.invalid/Ops/Agent",
			wantFullName: "Ops/Agent",
		},
		{
			name:         "cross-origin response URL ignored",
			fullName:     "ops/agent",
			htmlURL:      "https://attacker.example.invalid/other/repository",
			wantFullName: "ops/agent",
		},
		{
			name:     "different repository rejected",
			fullName: "other/repository",
			htmlURL:  "https://attacker.example.invalid/other/repository",
			wantErr:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := json.Marshal(repository{
				Name:          "agent",
				FullName:      test.fullName,
				Private:       true,
				DefaultBranch: "main",
				HTMLURL:       test.htmlURL,
			})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v3/repos/ops/agent" {
					t.Errorf("request = %s %s, want GET /api/v3/repos/ops/agent", r.Method, r.URL.Path)
				}
				if r.URL.Scheme != "https" || r.URL.Host != "enterprise.example.invalid" {
					t.Errorf("request origin = %s://%s, want registered HTTPS origin", r.URL.Scheme, r.URL.Host)
				}
				return harborTestResponse(r, http.StatusOK, string(response)), nil
			})}

			registered := target{
				ID:         "github:0123456789abcdef0123456789abcdef",
				Name:       "Engineering",
				Origin:     "https://enterprise.example.invalid",
				Repository: "ops/agent",
				SecretRef:  "cred:0123456789abcdef0123456789abcdef",
			}
			got, err := fetchRepository(context.Background(), registered, "test-token", client)
			if test.wantErr {
				if err == nil {
					t.Fatalf("fetchRepository accepted response identity %q", test.fullName)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetchRepository returned error: %v", err)
			}
			if got.FullName != test.wantFullName {
				t.Errorf("full_name = %q, want %q", got.FullName, test.wantFullName)
			}
			if want := registered.Origin + "/ops/agent"; got.HTMLURL != want {
				t.Errorf("html_url = %q, want registered repository URL %q", got.HTMLURL, want)
			}
		})
	}
}

func TestUIRejectsUntrustedPostsBeforeCredentialWrite(t *testing.T) {
	const host = "127.0.0.1:43127"
	tests := []struct {
		name        string
		requestHost string
		origin      string
		csrf        string
		wantStatus  int
		wantWrites  int
	}{
		{name: "same-origin", requestHost: host, origin: "http://" + host, csrf: "valid-csrf", wantStatus: http.StatusSeeOther, wantWrites: 1},
		{name: "missing-origin-valid-csrf", requestHost: host, csrf: "valid-csrf", wantStatus: http.StatusSeeOther, wantWrites: 1},
		{name: "missing-origin-invalid-csrf", requestHost: host, csrf: "wrong", wantStatus: http.StatusForbidden},
		{name: "opaque-origin", requestHost: host, origin: "null", csrf: "valid-csrf", wantStatus: http.StatusForbidden},
		{name: "cross-origin", requestHost: host, origin: "http://evil.example", csrf: "valid-csrf", wantStatus: http.StatusForbidden},
		{name: "wrong-host", requestHost: "127.0.0.1:43128", origin: "http://" + host, csrf: "valid-csrf", wantStatus: http.StatusNotFound},
		{name: "invalid-csrf", requestHost: host, origin: "http://" + host, csrf: "wrong", wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secrets := &recordingSecrets{}
			a := &app{
				configPath: filepath.Join(t.TempDir(), "config.json"),
				secrets:    secrets,
				host:       host,
				csrf:       "valid-csrf",
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/save", a.handleSave)
			handler := a.securityHeaders(mux)
			form := url.Values{
				"csrf":       {test.csrf},
				"name":       {"Engineering"},
				"origin":     {"https://github.example.invalid"},
				"repository": {"ops/agent"},
				"token":      {"github_pat_must_not_be_saved_0123456789"},
			}
			req := httptest.NewRequest(http.MethodPost, "http://"+test.requestHost+"/save", strings.NewReader(form.Encode()))
			req.Host = test.requestHost
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, test.wantStatus)
			}
			if secrets.writes != test.wantWrites || secrets.loads != 0 {
				t.Fatalf("credential operations = save %d, load %d; want save %d, load 0", secrets.writes, secrets.loads, test.wantWrites)
			}
		})
	}
}

func TestRootResponseUsesSameOriginReferrerPolicy(t *testing.T) {
	const host = "127.0.0.1:43129"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, host: host, csrf: "test-csrf"}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	request.Host = host
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("root status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Referrer-Policy"); got != localUIReferrerPolicy {
		t.Fatalf("root Referrer-Policy = %q, want %q", got, localUIReferrerPolicy)
	}
}

func TestChangingGitHubOriginRequiresNewCredential(t *testing.T) {
	secrets := &recordingSecrets{}
	path := filepath.Join(t.TempDir(), "config.json")
	old := target{Name: "Engineering", Origin: "https://old.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	if err := writeConfig(path, config{Version: configVersion, Target: &old}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, secrets: secrets, host: "127.0.0.1:43127", csrf: "valid-csrf"}
	form := url.Values{
		"csrf":       {a.csrf},
		"name":       {old.Name},
		"origin":     {"https://new.example.invalid"},
		"repository": {old.Repository},
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/save", strings.NewReader(form.Encode()))
	req.Host = a.host
	req.Header.Set("Origin", "http://"+a.host)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/save", a.handleSave)
	a.securityHeaders(mux).ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want redirect with a retry message", res.Code)
	}
	got, err := readConfig(path)
	if err != nil || got.Target.Origin != old.Origin {
		t.Fatalf("saved origin changed without a new token: %#v, %v", got.Target, err)
	}
	if secrets.writes != 0 || secrets.loads != 0 {
		t.Fatalf("credential operations = save %d, load %d; want neither", secrets.writes, secrets.loads)
	}
}

func TestWriteConfigReplacesAndPreservesDestinationOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := config{Version: configVersion, Target: &target{Name: "Original", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}}
	updated := config{Version: configVersion, Target: &target{Name: "Updated", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:fedcba9876543210fedcba9876543210"}}
	if err := writeConfig(path, original); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, updated); err != nil {
		t.Fatalf("replace existing config: %v", err)
	}
	got, err := readConfig(path)
	if err != nil || got.Target.Name != "Updated" {
		t.Fatalf("replacement result = %#v, %v", got.Target, err)
	}
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatalf("write empty target set: %v", err)
	}
	got, err = readConfig(path)
	if err != nil || len(got.GitHubTargets)+len(got.JenkinsTargets) != 0 {
		t.Fatalf("empty settings = %#v, %v", got, err)
	}
	if err := writeConfig(path, updated); err != nil {
		t.Fatalf("restore updated config: %v", err)
	}
	if err := writeConfig(path, config{Version: configVersion + 1}); err == nil {
		t.Fatal("invalid replacement unexpectedly succeeded")
	}
	if err := replaceFile(filepath.Join(filepath.Dir(path), "missing-temp"), path); err == nil {
		t.Fatal("replacement with missing source unexpectedly succeeded")
	}
	got, err = readConfig(path)
	if err != nil || got.Target.Name != "Updated" {
		t.Fatalf("failed replacement changed the existing config: %#v, %v", got.Target, err)
	}
}

func TestConfigLockSerializesTransactions(t *testing.T) {
	const workers = 8
	var active, maximum atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := withConfigLock(func() error {
				current := active.Add(1)
				for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
				}
				time.Sleep(2 * time.Millisecond)
				active.Add(-1)
				return nil
			}); err != nil {
				t.Errorf("acquire config lock: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent transactions = %d, want 1", maximum.Load())
	}
}

func TestSaveReportsOrphanCredentialAfterCleanupFailure(t *testing.T) {
	const token = "ghp_canary_must_not_reach_ui"
	configPath := filepath.Join(t.TempDir(), "config.json")
	secrets := &cleanupFailureSecrets{configPath: configPath}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		host:       "127.0.0.1:43127",
		csrf:       "valid-csrf",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleRoot)
	mux.HandleFunc("/save", a.handleSave)
	handler := a.securityHeaders(mux)

	form := url.Values{
		"csrf":       {a.csrf},
		"name":       {"Engineering"},
		"origin":     {"https://github.example.invalid"},
		"repository": {"ops/agent"},
		"token":      {token},
	}
	post := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/save", strings.NewReader(form.Encode()))
	post.Header.Set("Origin", "http://"+a.host)
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postResult := httptest.NewRecorder()
	handler.ServeHTTP(postResult, post)
	if postResult.Code != http.StatusSeeOther {
		t.Fatalf("POST status = %d, want redirect", postResult.Code)
	}
	if secrets.deletes != 1 {
		t.Fatalf("credential cleanup attempts = %d, want 1", secrets.deletes)
	}

	get := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	body := getResult.Body.String()
	for _, want := range []string{
		"unused credential remains in Windows Credential Manager",
		"Remove the Local Agent Harness credential entry",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI response missing %q", want)
		}
	}
	if strings.Contains(body, token) {
		t.Fatal("UI response exposed the submitted token")
	}
}

func TestJenkinsRegistrationStoresOnlyCredentialReferenceAndDefaultsTriggerOff(t *testing.T) {
	const token = "jenkins_api_token_canary_registration_123456"
	secrets := memorySecrets{}
	appHost := "127.0.0.1:43127"
	configPath := filepath.Join(t.TempDir(), "config.json")
	a := &app{configPath: configPath, secrets: secrets, host: appHost, csrf: "valid-csrf"}
	mux := http.NewServeMux()
	mux.HandleFunc("/save-jenkins", a.handleJenkinsSave)
	form := url.Values{
		"csrf": {a.csrf}, "jenkins_name": {"QA pipeline"},
		"jenkins_url": {"https://jenkins.example.invalid/proxy"}, "jenkins_username": {"build-user"},
		"jenkins_job": {"folder/smoke"}, "jenkins_environment": {"QA cluster"}, "jenkins_token": {token},
	}
	req := httptest.NewRequest(http.MethodPost, "http://"+appHost+"/save-jenkins", strings.NewReader(form.Encode()))
	req.Host = appHost
	req.Header.Set("Origin", "http://"+appHost)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	a.securityHeaders(mux).ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, want redirect", res.Code)
	}
	cfg, err := readConfig(configPath)
	if err != nil || cfg.Jenkins == nil {
		t.Fatalf("saved Jenkins config = %#v, err %v", cfg.Jenkins, err)
	}
	if cfg.Jenkins.NonProductionPreapproved {
		t.Fatal("trigger authorization defaulted on")
	}
	a.client = nil
	a.openApprovalBrowser = func(string) error { return errors.New("browser launcher unavailable") }
	if _, err := a.runRegisteredJenkinsJob(context.Background(), cfg.Jenkins.Name); err == nil {
		t.Fatal("unapproved Jenkins target was triggerable")
	}
	if got := string(secrets[cfg.Jenkins.SecretRef]); got != token {
		t.Fatal("Jenkins API token was not saved in the credential store")
	}
	stored, err := os.ReadFile(configPath)
	if err != nil || strings.Contains(string(stored), token) {
		t.Fatal("Jenkins API token was written to the settings file")
	}
}

func TestJenkinsApprovalBoundToTriggerScope(t *testing.T) {
	const ref = "cred:0123456789abcdef0123456789abcdef"
	base := approvedJenkinsTarget(jenkinsTarget{
		Name: "QA pipeline", BaseURL: "https://jenkins.example.invalid/proxy",
		Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref,
	})
	if !jenkinsTriggerApproved(base) {
		t.Fatal("approval for the matching target was rejected")
	}

	t.Run("unchanged target keeps approval", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := writeConfig(path, config{Version: configVersion, Jenkins: &base}); err != nil {
			t.Fatal(err)
		}
		a := &app{configPath: path, secrets: memorySecrets{ref: []byte("initial-token")}}
		if _, err := a.saveJenkinsTarget(base.ID, base.Name, base.BaseURL, base.Username, base.JobPath, base.Environment, "", true); err != nil {
			t.Fatal(err)
		}
		saved, err := readConfig(path)
		if err != nil || saved.Jenkins == nil || !jenkinsTriggerApproved(*saved.Jenkins) {
			t.Fatalf("unchanged approval = %#v, err=%v", saved.Jenkins, err)
		}
	})

	tests := []struct {
		name   string
		change func(*jenkinsTarget)
		secret string
	}{
		{name: "job path", change: func(target *jenkinsTarget) { target.JobPath = "folder/production" }},
		{name: "base URL", change: func(target *jenkinsTarget) { target.BaseURL = "https://jenkins-next.example.invalid/proxy" }, secret: "replacement-token"},
		{name: "username", change: func(target *jenkinsTarget) { target.Username = "other-user" }},
		{name: "target name", change: func(target *jenkinsTarget) { target.Name = "production pipeline" }},
		{name: "environment label", change: func(target *jenkinsTarget) { target.Environment = "production" }},
		{name: "credential identity", secret: "replacement-token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(path, config{Version: configVersion, Jenkins: &base}); err != nil {
				t.Fatal(err)
			}
			secrets := memorySecrets{ref: []byte("initial-token")}
			a := &app{configPath: path, secrets: secrets}
			candidate := base
			if test.change != nil {
				test.change(&candidate)
			}
			if _, err := a.saveJenkinsTarget(base.ID, candidate.Name, candidate.BaseURL, candidate.Username, candidate.JobPath, candidate.Environment, test.secret, true); err != nil {
				t.Fatal(err)
			}
			saved, err := readConfig(path)
			if err != nil || saved.Jenkins == nil {
				t.Fatalf("saved target = %#v, err=%v", saved.Jenkins, err)
			}
			if saved.Jenkins.NonProductionPreapproved || saved.Jenkins.NonProductionApprovalScope != "" || jenkinsTriggerApproved(*saved.Jenkins) {
				t.Fatalf("changed target retained trigger approval: %+v", *saved.Jenkins)
			}
		})
	}
	t.Run("changed target needs explicit reapproval", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := writeConfig(path, config{Version: configVersion, Jenkins: &base}); err != nil {
			t.Fatal(err)
		}
		a := &app{configPath: path, secrets: memorySecrets{ref: []byte("initial-token")}}
		if _, err := a.saveJenkinsTarget(base.ID, base.Name, base.BaseURL, base.Username, "folder/production", base.Environment, "", true); err != nil {
			t.Fatal(err)
		}
		saved, err := readConfig(path)
		if err != nil || saved.Jenkins == nil || saved.Jenkins.NonProductionPreapproved {
			t.Fatalf("changed target should require another approval: target=%#v err=%v", saved.Jenkins, err)
		}
		if _, err := a.saveJenkinsTarget(base.ID, base.Name, base.BaseURL, base.Username, "folder/production", base.Environment, "", true); err != nil {
			t.Fatal(err)
		}
		saved, err = readConfig(path)
		if err != nil || saved.Jenkins == nil || !jenkinsTriggerApproved(*saved.Jenkins) {
			t.Fatalf("explicitly reapproved target = %#v, err=%v", saved.Jenkins, err)
		}
	})
	t.Run("A to B to A does not revive approval", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := writeConfig(path, config{Version: configVersion, Jenkins: &base}); err != nil {
			t.Fatal(err)
		}
		a := &app{configPath: path, secrets: memorySecrets{ref: []byte("initial-token")}}
		if _, err := a.saveJenkinsTarget(base.ID, base.Name, base.BaseURL, base.Username, "folder/production", base.Environment, "", true); err != nil {
			t.Fatal(err)
		}
		if _, err := a.saveJenkinsTarget(base.ID, base.Name, base.BaseURL, base.Username, base.JobPath, base.Environment, "", false); err != nil {
			t.Fatal(err)
		}
		saved, err := readConfig(path)
		if err != nil || saved.Jenkins == nil || jenkinsTriggerApproved(*saved.Jenkins) {
			t.Fatalf("returning to original target revived approval: target=%#v err=%v", saved.Jenkins, err)
		}
	})

	tampered := base
	tampered.JobPath = "folder/production"
	if jenkinsTriggerApproved(tampered) {
		t.Fatal("directly changed config retained trigger approval")
	}
	if _, err := runJenkinsJob(context.Background(), tampered, "token", nil); err == nil {
		t.Fatal("directly changed target passed trigger authorization")
	}
	legacy := base
	legacy.NonProductionApprovalScope = ""
	if jenkinsTriggerApproved(legacy) {
		t.Fatal("unbound legacy approval remained valid")
	}
	if _, err := runJenkinsJob(context.Background(), legacy, "token", nil); err == nil {
		t.Fatal("unbound legacy approval passed trigger authorization")
	}
}

func TestJenkinsTriggerRejectsUnboundOrChangedScopeBeforeHTTP(t *testing.T) {
	const ref = "cred:1123456789abcdef0123456789abcdef"
	const token = "jenkins_api_token_canary_approval_scope_123456"
	for _, test := range []struct {
		name   string
		change func(*jenkinsTarget)
	}{
		{name: "changed target scope", change: func(target *jenkinsTarget) { target.JobPath = "folder/production" }},
		{name: "legacy boolean only", change: func(target *jenkinsTarget) { target.NonProductionApprovalScope = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests, posts atomic.Int32
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
			}))
			defer mock.Close()

			target := approvedJenkinsTarget(jenkinsTarget{
				Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user",
				JobPath: "folder/smoke", Environment: "qa", SecretRef: ref,
			})
			test.change(&target)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(path, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			secrets := memorySecrets{ref: []byte(token)}
			if got, err := secrets.Load(ref); err != nil || string(got) != token {
				t.Fatal("test credential was not available")
			}
			a := &app{
				configPath: path,
				secrets:    secrets,
				client:     mock.Client(),
				jenkinsApprovalResult: func(context.Context) error {
					return errors.New("approval not granted")
				},
			}
			if _, err := a.runRegisteredJenkinsJob(context.Background(), target.Name); err == nil {
				t.Fatal("unbound or changed target was triggerable")
			}
			if requests.Load() != 0 || posts.Load() != 0 {
				t.Fatalf("rejected trigger sent %d HTTP requests including %d POSTs", requests.Load(), posts.Load())
			}
		})
	}
}

func TestMCPRejectsUnregisteredJenkinsTarget(t *testing.T) {
	const token = "jenkins_api_token_canary_scope_123456"
	const ref = "cred:1123456789abcdef0123456789abcdef"
	var hits atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
	}))
	defer mock.Close()
	target := jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "jenkins-scope-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "jenkins_registered_job", Arguments: map[string]any{"target": "unregistered"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || hits.Load() != 0 {
		t.Fatalf("unregistered target result error=%v, Jenkins hits=%d", result.IsError, hits.Load())
	}
	encoded, _ := json.Marshal(result.Content)
	if strings.Contains(string(encoded), token) {
		t.Fatal("MCP error exposed the registered API token")
	}
}

func TestJenkinsRejectsRedirectsAndUnsafeQueueLocations(t *testing.T) {
	const token = "jenkins_api_token_canary_queue_123456"
	const ref = "cred:2123456789abcdef0123456789abcdef"
	locations := []string{"", "https://evil.example/proxy/queue/item/17/", "/proxy-other/queue/item/17/", "/proxy/queue/item/not-a-number/"}
	for _, location := range locations {
		t.Run(location, func(t *testing.T) {
			var queueHits atomic.Int32
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if strings.Contains(r.URL.Path, "/queue/") {
						queueHits.Add(1)
					}
					_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
					return
				}
				if location != "" {
					w.Header().Set("Location", location)
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer mock.Close()
			target := approvedJenkinsTarget(jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref})
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
			result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
			if err != nil || result.State != "outcome_unknown" || result.QueueID != 0 || !strings.Contains(result.Guidance, "Do not retry") {
				t.Fatalf("ambiguous trigger result=%+v err=%v", result, err)
			}
			if queueHits.Load() != 0 {
				t.Fatalf("unsafe queue location caused %d queue requests", queueHits.Load())
			}
		})
	}
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/followed")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirectSource.Close()
	target := jenkinsTarget{Name: "QA pipeline", BaseURL: redirectSource.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
	configPath := filepath.Join(t.TempDir(), "redirect-config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: redirectSource.Client()}
	if _, err := a.registeredJenkinsJob(context.Background(), target.Name); err == nil || redirectHits.Load() != 0 {
		t.Fatalf("redirect result err=%v, target hits=%d", err, redirectHits.Load())
	}
}

func TestJenkinsRunBoundsAndMasksProgressiveLog(t *testing.T) {
	const token = "jenkins_api_token_canary_log_123456"
	const ref = "cred:3123456789abcdef0123456789abcdef"
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "build-user" || password != token {
			t.Errorf("Jenkins request did not use the registered credential")
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/json") && strings.Contains(r.URL.Path, "/job/") && !strings.Contains(r.URL.Path, "/42/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/build"):
			if r.URL.RawQuery != "" || r.ContentLength != 0 {
				t.Errorf("trigger included parameters: query=%q content-length=%d", r.URL.RawQuery, r.ContentLength)
			}
			w.Header().Set("Location", "/proxy/queue/item/17/")
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/proxy/queue/item/17/api/json":
			_, _ = w.Write([]byte(`{"id":17,"task":{"name":"smoke","url":"/proxy/job/folder/job/smoke/"},"executable":{"number":42}}`))
		case r.URL.Path == "/proxy/job/folder/job/smoke/42/api/json":
			_, _ = w.Write([]byte(`{"number":42,"result":"SUCCESS","building":false}`))
		case r.URL.Path == "/proxy/job/folder/job/smoke/42/logText/progressiveText":
			w.Header().Set("X-Text-Size", strconv.Itoa(maxJenkinsLogBytes))
			w.Header().Set("X-More-Data", "true")
			_, _ = w.Write([]byte(token + strings.Repeat("x", maxJenkinsLogBytes-len(token))))
		default:
			t.Errorf("unexpected Jenkins request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()
	target := approvedJenkinsTarget(jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref})
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
	result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if result.BuildNumber != 42 || result.Result != "SUCCESS" || !result.LogTruncated || len(result.Log) > maxJenkinsLogBytes || result.NextOffset == nil || *result.NextOffset != maxJenkinsLogBytes || result.MoreData == nil || !*result.MoreData || strings.Contains(result.Log, token) {
		t.Fatalf("unexpected Jenkins result: build=%d result=%q truncated=%v logBytes=%d", result.BuildNumber, result.Result, result.LogTruncated, len(result.Log))
	}
}

func TestJenkinsRunLongQueueReturnsQueueIDWithoutRetrying(t *testing.T) {
	const token = "jenkins_api_token_canary_queue_wait_123456"
	const ref = "cred:4123456789abcdef0123456789abcdef"
	var posts atomic.Int32
	var queueReads atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/json") && strings.Contains(r.URL.Path, "/job/"):
			_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/build"):
			posts.Add(1)
			w.Header().Set("Location", "/proxy/queue/item/17/")
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/proxy/queue/item/17/api/json":
			queueReads.Add(1)
			_, _ = w.Write([]byte(`{"id":17,"task":{"name":"smoke","url":"/proxy/job/folder/job/smoke/"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()
	target := approvedJenkinsTarget(jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref})
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
	result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Queued || result.QueueID != 17 || result.State != "" {
		t.Fatalf("unexpected queued result: %+v", result)
	}
	if posts.Load() != 1 || queueReads.Load() != jenkinsQueuePolls {
		t.Fatalf("trigger POSTs=%d queue reads=%d, want 1 and %d", posts.Load(), queueReads.Load(), jenkinsQueuePolls)
	}
}

func TestJenkinsQueueInspectionBindsTaskAndRejectsInvalidID(t *testing.T) {
	const token = "jenkins_api_token_canary_queue_binding_123456"
	const ref = "cred:5123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name     string
		taskName string
		taskURL  string
	}{
		{name: "wrong task name", taskName: "other", taskURL: "/proxy/job/folder/job/smoke/"},
		{name: "wrong task URL", taskName: "smoke", taskURL: "/proxy/job/folder/job/other/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hits atomic.Int32
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				_, _ = w.Write([]byte(`{"id":17,"task":{"name":"` + test.taskName + `","url":"` + test.taskURL + `"}}`))
			}))
			defer mock.Close()
			target := jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
			if _, err := a.registeredJenkinsQueue(context.Background(), target.Name, 17); err == nil {
				t.Fatal("queue item for a different job was accepted")
			}
			if hits.Load() != 1 {
				t.Fatalf("queue API hits=%d, want 1", hits.Load())
			}
		})
	}

	var hits atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer mock.Close()
	target := jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
	if _, err := a.registeredJenkinsQueue(context.Background(), target.Name, 0); err == nil {
		t.Fatal("zero queue ID was accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid queue ID caused %d requests", hits.Load())
	}
}

func TestJenkinsAmbiguousPOSTOutcomeDoesNotReturnToolError(t *testing.T) {
	const token = "jenkins_api_token_canary_post_unknown_123456"
	const ref = "cred:7123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name      string
		status    int
		dropReply bool
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
		{name: "connection closed after request", dropReply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posts atomic.Int32
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
					return
				}
				posts.Add(1)
				if test.dropReply {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack failed: %v", err)
						return
					}
					_ = connection.Close()
					return
				}
				w.WriteHeader(test.status)
			}))
			defer mock.Close()
			target := approvedJenkinsTarget(jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref})
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
			result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
			if err != nil || result.State != "outcome_unknown" || !strings.Contains(result.Guidance, "Do not retry") {
				t.Fatalf("ambiguous POST result=%+v err=%v", result, err)
			}
			if posts.Load() != 1 {
				t.Fatalf("trigger POSTs=%d, want 1", posts.Load())
			}
		})
	}
}

func TestJenkinsAcceptedTriggerReturnsPendingAfterInspectionFailure(t *testing.T) {
	const token = "jenkins_api_token_canary_pending_123456"
	const ref = "cred:6123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name        string
		queueStatus int
		queueBody   string
		buildStatus int
		logStatus   int
	}{
		{name: "queue unauthorized", queueStatus: http.StatusUnauthorized},
		{name: "queue forbidden", queueStatus: http.StatusForbidden},
		{name: "queue server error", queueStatus: http.StatusInternalServerError},
		{name: "queue malformed JSON", queueStatus: http.StatusOK, queueBody: "{"},
		{name: "build lookup failure", queueStatus: http.StatusOK, queueBody: `{"id":17,"task":{"name":"smoke","url":"/proxy/job/folder/job/smoke/"},"executable":{"number":42}}`, buildStatus: http.StatusInternalServerError},
		{name: "log lookup failure", queueStatus: http.StatusOK, queueBody: `{"id":17,"task":{"name":"smoke","url":"/proxy/job/folder/job/smoke/"},"executable":{"number":42}}`, logStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posts atomic.Int32
			var allowLog atomic.Bool
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/json") && strings.Contains(r.URL.Path, "/job/") && !strings.Contains(r.URL.Path, "/42/"):
					_, _ = w.Write([]byte(`{"name":"smoke","fullName":"folder/smoke","buildable":true}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/build"):
					posts.Add(1)
					w.Header().Set("Location", "/proxy/queue/item/17/")
					w.WriteHeader(http.StatusCreated)
				case r.URL.Path == "/proxy/queue/item/17/api/json":
					if test.queueStatus != 0 {
						w.WriteHeader(test.queueStatus)
					}
					if test.queueBody != "" {
						_, _ = w.Write([]byte(test.queueBody))
					} else if test.queueStatus == http.StatusOK {
						_, _ = w.Write([]byte(`{"id":17,"task":{"name":"smoke","url":"/proxy/job/folder/job/smoke/"},"executable":{"number":42}}`))
					}
				case r.URL.Path == "/proxy/job/folder/job/smoke/42/api/json":
					if test.buildStatus != 0 {
						w.WriteHeader(test.buildStatus)
						return
					}
					_, _ = w.Write([]byte(`{"number":42,"result":"SUCCESS","building":false}`))
				case r.URL.Path == "/proxy/job/folder/job/smoke/42/logText/progressiveText":
					if test.logStatus != 0 && !allowLog.Load() {
						w.WriteHeader(test.logStatus)
						return
					}
					logBody := []byte("build completed with credential=" + token)
					w.Header().Set("X-Text-Size", strconv.Itoa(len(logBody)))
					w.Header().Set("X-More-Data", "false")
					_, _ = w.Write(logBody)
				default:
					http.NotFound(w, r)
				}
			}))
			defer mock.Close()
			target := approvedJenkinsTarget(jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref})
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
			result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
			if err != nil || result.State != "inspection_pending" || result.QueueID != 17 || result.Queued {
				t.Fatalf("accepted trigger did not return pending state: result=%+v err=%v", result, err)
			}
			if posts.Load() != 1 {
				t.Fatalf("trigger POSTs=%d, want 1", posts.Load())
			}
			if test.name == "log lookup failure" {
				allowLog.Store(true)
				queue, err := a.registeredJenkinsQueue(context.Background(), target.Name, 17)
				if err != nil {
					t.Fatal(err)
				}
				if queue.State != "completed" || queue.BuildNumber != 42 || queue.Result != "SUCCESS" || !strings.Contains(queue.Log, "[REDACTED]") || strings.Contains(queue.Log, token) || queue.NextOffset == nil || *queue.NextOffset == 0 || queue.MoreData == nil || *queue.MoreData {
					t.Fatalf("read-only queue inspection did not recover build/log: %+v", queue)
				}
				if posts.Load() != 1 {
					t.Fatalf("queue inspection repeated trigger; POSTs=%d", posts.Load())
				}
			}
		})
	}
}

func TestJenkinsRegisteredBuildLogPagesAreBoundedAndReadOnly(t *testing.T) {
	const token = "jenkins_api_token_canary_log_pages_123456"
	const ref = "cred:8123456789abcdef0123456789abcdef"
	pageOne := "PAGE_ONE_CANARY\n"
	pageTwoRaw := append([]byte("PAGE_TWO_CANARY "+token), 0xff)
	var postHits atomic.Int32
	var getHits atomic.Int32
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postHits.Add(1)
			http.Error(w, "POST forbidden in log reader test", http.StatusMethodNotAllowed)
			return
		}
		getHits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/proxy/job/folder/job/smoke/42/logText/progressiveText" {
			t.Errorf("unexpected Jenkins log request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		if user, password, ok := r.BasicAuth(); !ok || user != "build-user" || password != token {
			t.Errorf("Jenkins log request did not use the registered credential")
		}
		switch r.URL.RawQuery {
		case "start=0":
			w.Header().Set("X-Text-Size", strconv.Itoa(len(pageOne)))
			w.Header().Set("X-More-Data", "true")
			_, _ = w.Write([]byte(pageOne))
		case "start=" + strconv.Itoa(len(pageOne)):
			w.Header().Set("X-Text-Size", strconv.Itoa(len(pageOne)+len(pageTwoRaw)))
			w.Header().Set("X-More-Data", "false")
			_, _ = w.Write(pageTwoRaw)
		default:
			t.Errorf("unexpected progressiveText query: %q", r.URL.RawQuery)
			http.Error(w, "bad query", http.StatusBadRequest)
		}
	}))
	defer mock.Close()
	target := jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "jenkins-log-pages-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	first, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jenkins_registered_build_log",
		Arguments: map[string]any{"target": target.Name, "build_number": 42, "start_offset": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if first.IsError || !strings.Contains(string(firstJSON), `"log":"PAGE_ONE_CANARY`) || !strings.Contains(string(firstJSON), `"next_offset":`+strconv.Itoa(len(pageOne))) || !strings.Contains(string(firstJSON), `"more_data":true`) {
		t.Fatalf("unexpected first log segment: %s", firstJSON)
	}
	second, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "jenkins_registered_build_log",
		Arguments: map[string]any{"target": target.Name, "build_number": 42, "start_offset": len(pageOne)},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if second.IsError || !strings.Contains(string(secondJSON), `"next_offset":`+strconv.Itoa(len(pageOne)+len(pageTwoRaw))) || !strings.Contains(string(secondJSON), `"more_data":false`) || !strings.Contains(string(secondJSON), "PAGE_TWO_CANARY [REDACTED]�") || strings.Contains(string(secondJSON), token) {
		t.Fatalf("unexpected second log segment: %s", secondJSON)
	}
	if getHits.Load() != 2 || postHits.Load() != 0 {
		t.Fatalf("log GETs=%d POSTs=%d, want 2 GETs and no POST", getHits.Load(), postHits.Load())
	}
}

func TestJenkinsRegisteredBuildLogRejectsInvalidOffsetAndHeaders(t *testing.T) {
	const token = "jenkins_api_token_canary_bad_headers_123456"
	const ref = "cred:9123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name          string
		startOffset   int64
		textSize      string
		moreData      string
		body          string
		wantNoRequest bool
	}{
		{name: "negative offset", startOffset: -1, wantNoRequest: true},
		{name: "text size overflow", textSize: "9223372036854775808", moreData: "false", body: "x"},
		{name: "invalid more header", textSize: "1", moreData: "sometimes", body: "x"},
		{name: "text size before offset", startOffset: 2, textSize: "1", moreData: "false"},
		{name: "text size skips body bytes", textSize: "2", moreData: "false", body: "x"},
		{name: "oversized body", textSize: strconv.Itoa(maxJenkinsLogBytes + 1), moreData: "false", body: strings.Repeat("x", maxJenkinsLogBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hits atomic.Int32
			mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/proxy/job/folder/job/smoke/42/logText/progressiveText" || r.URL.Query().Get("start") != strconv.FormatInt(test.startOffset, 10) {
					t.Errorf("unexpected progressiveText request: %s %s", r.Method, r.URL.String())
				}
				if test.textSize != "" {
					w.Header().Set("X-Text-Size", test.textSize)
				}
				if test.moreData != "" {
					w.Header().Set("X-More-Data", test.moreData)
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer mock.Close()
			target := jenkinsTarget{Name: "QA pipeline", BaseURL: mock.URL + "/proxy", Username: "build-user", JobPath: "folder/smoke", Environment: "qa", SecretRef: ref}
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, Jenkins: &target}); err != nil {
				t.Fatal(err)
			}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: mock.Client()}
			_, err := a.registeredJenkinsLog(context.Background(), target.Name, 42, test.startOffset)
			if err == nil {
				t.Fatal("invalid offset or progressiveText response was accepted")
			}
			if test.wantNoRequest && hits.Load() != 0 {
				t.Fatalf("invalid offset caused %d Jenkins requests", hits.Load())
			}
			if !test.wantNoRequest && hits.Load() != 1 {
				t.Fatalf("header/body failure produced %d Jenkins requests, want 1", hits.Load())
			}
		})
	}
}

func TestCleanOutputRedactsStructuredCredentialsAndKeepsJSONValid(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
		want   string
	}{
		{name: "client secret", input: "client_secret=client-canary", secret: "unused-canary", want: "client_secret=[REDACTED]"},
		{name: "access token", input: "access_token=access-canary", secret: "unused-canary", want: "access_token=[REDACTED]"},
		{name: "private key", input: "private_key=private-canary", secret: "unused-canary", want: "[REDACTED]"},
		{name: "prefixed private key", input: "ssh_private_key=ssh-private-canary", secret: "unused-canary", want: "[REDACTED]"},
		{name: "multiline PEM without field", input: "log line\n-----BEGIN RSA PRIVATE KEY-----\nPEM_BODY_CANARY_012345\n-----END RSA PRIVATE KEY-----\nmore log", secret: "unused-canary", want: "[REDACTED]"},
		{name: "multiline PGP armor without field", input: "log line\n-----BEGIN PGP PRIVATE KEY BLOCK-----\nPGP_BODY_CANARY_012345\n-----END PGP PRIVATE KEY BLOCK-----\nmore log", secret: "unused-canary", want: "[REDACTED]"},
		{name: "authorization header", input: "Authorization: Bearer auth-canary", secret: "unused-canary", want: "Authorization: [REDACTED]"},
		{name: "plain value without explicit secret", input: "ops/agent", secret: "", want: "ops/agent"},
		{name: "registered canary", input: "registered-canary", secret: "registered-canary", want: "[REDACTED]"},
		{name: "JSON fields", input: `{"client_secret":"client-canary","access_token":"access-canary","token":"token-canary","Authorization":"Bearer auth-canary"}`, secret: "unused-canary", want: `{"client_secret":"[REDACTED]","access_token":"[REDACTED]","token":"[REDACTED]","Authorization":"[REDACTED]"}`},
		{name: "nested object", input: `prefix {"token":{"value":"nested-object-canary"}} suffix`, secret: "unused-canary", want: "[REDACTED]"},
		{name: "nested array", input: `prefix {"access_token":[{"value":"nested-array-canary"}]} suffix`, secret: "unused-canary", want: "[REDACTED]"},
		{name: "unquoted scalar JSON", input: `{"token":123456}`, secret: "unused-canary", want: `{"token":"[REDACTED]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanOutput(tc.input, tc.secret, 2048)
			if got != tc.want {
				t.Fatalf("cleanOutput() = %q, want %q", got, tc.want)
			}
			if tc.secret != "" && tc.secret != "unused-canary" && strings.Contains(got, tc.secret) {
				t.Fatalf("cleanOutput retained secret %q", tc.secret)
			}
			if strings.HasPrefix(tc.input, "{") && !json.Valid([]byte(got)) {
				t.Fatalf("redacted JSON is invalid: %s", got)
			}
		})
	}
}

func githubPullRequestTestConfig(t *testing.T) (string, config, target) {
	t.Helper()
	registered := target{
		ID:         "github:0123456789abcdef0123456789abcdef",
		Name:       "Engineering",
		Origin:     "https://enterprise.example.invalid",
		Repository: "ops/agent",
		SecretRef:  "cred:0123456789abcdef0123456789abcdef",
	}
	cfg := config{Version: configVersion, GitHubTargets: []target{registered}}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path, cfg, registered
}

func githubPullRequestFixture(number int, baseRepository, headRepository, token string) string {
	body, _ := json.Marshal(map[string]any{
		"number": number, "title": "Improve deploy checks with " + token, "state": "open", "draft": true, "merged": false,
		"updated_at": "2026-09-26T01:02:03Z",
		"base":       map[string]any{"ref": "main", "repo": map[string]any{"full_name": baseRepository}},
		"head":       map[string]any{"ref": "feature/remote-change", "sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": headRepository}},
		"body":       "body-canary-must-not-escape", "comments": 12,
		"html_url": "https://attacker.example.invalid/pull/42", "diff_url": "https://attacker.example.invalid/pull/42.diff",
		"files": []map[string]string{{"filename": "private.txt", "patch": "diff-canary-must-not-escape"}},
	})
	return string(body)
}

func TestGitHubPullRequestMCPUsesRegisteredRepositoryAndProjectsFields(t *testing.T) {
	const token = "ghp_pull_canary_not_for_model_123456"
	path, _, registered := githubPullRequestTestConfig(t)
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "enterprise.example.invalid" || r.URL.EscapedPath() != "/api/v3/repos/ops/agent/pulls/42" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s, want registered GET /api/v3/repos/ops/agent/pulls/42", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "local-agent-harness" {
			t.Errorf("pull request headers did not match the saved target contract: %+v", r.Header)
		}
		return harborTestResponse(r, http.StatusOK, githubPullRequestFixture(42, "OPS/AGENT", "contributor/fork", token)), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{registered.SecretRef: []byte(token)}, client: client}
	targets, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets.Targets) != 1 || len(targets.Targets[0].Actions) != 3 || targets.Targets[0].Actions[0] != "github_repository" || targets.Targets[0].Actions[1] != "github_pull_request" || targets.Targets[0].Actions[2] != "registered_target_connection_test" {
		t.Fatalf("registered target actions = %+v", targets.Targets)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "github-pull-request-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "github_pull_request", Arguments: map[string]any{"target": "Engineering", "number": 42}})
	if err != nil || result.IsError {
		t.Fatalf("GitHub pull request MCP result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"number":42`, `"state":"open"`, `"draft":true`, `"merged":false`, `"base_ref":"main"`, `"head_ref":"feature/remote-change"`, `"head_sha":"` + strings.Repeat("a", 40) + `"`, `"updated_at":"2026-09-26T01:02:03Z"`, "[REDACTED]"} {
		if !strings.Contains(string(encoded), expected) {
			t.Errorf("pull request result missing %q: %s", expected, encoded)
		}
	}
	for _, forbidden := range []string{token, "body-canary", "diff-canary", "private.txt", "html_url", "attacker.example.invalid", "contributor/fork", "full_name"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("pull request result exposed %q: %s", forbidden, encoded)
		}
	}
	if requests != 1 {
		t.Fatalf("valid pull request call made %d GitHub requests, want 1", requests)
	}
	extra, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "github_pull_request", Arguments: map[string]any{"target": "Engineering", "number": 42, "repository": "other/repo", "url": "https://other.example.invalid"}})
	if err != nil || !extra.IsError || requests != 1 {
		t.Fatalf("unregistered selectors result=%#v err=%v requests=%d", extra, err, requests)
	}
}

func TestGitHubPullRequestRejectsInvalidNumberBeforeSecretOrNetwork(t *testing.T) {
	path, _, _ := githubPullRequestTestConfig(t)
	secrets := &recordingSecrets{}
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		return harborTestResponse(r, http.StatusOK, `{}`), nil
	})}
	a := &app{configPath: path, secrets: secrets, client: client}
	for _, number := range []int{0, -1} {
		if _, err := a.registeredGitHubPullRequest(context.Background(), "Engineering", number); err == nil {
			t.Errorf("invalid pull request number %d accepted", number)
		}
	}
	if secrets.loads != 0 || requests != 0 {
		t.Fatalf("invalid number loaded secrets %d times and made %d requests", secrets.loads, requests)
	}
}

func TestGitHubPullRequestRejectsRedirectsBaseMismatchOversizeAndMalformedJSON(t *testing.T) {
	const token = "ghp_pull_error_canary_0123456789"
	_, _, registered := githubPullRequestTestConfig(t)
	tests := []struct {
		name      string
		status    int
		headers   http.Header
		body      string
		wantError string
	}{
		{name: "redirect", status: http.StatusFound, body: token},
		{name: "unauthorized", status: http.StatusUnauthorized, body: token, wantError: "GitHub rejected credential (401)"},
		{name: "forbidden", status: http.StatusForbidden, body: token, wantError: "GitHub denied pull request access (403)"},
		{
			name:      "primary rate limit with 403",
			status:    http.StatusForbidden,
			headers:   http.Header{"X-RateLimit-Remaining": {"0"}, "X-GitHub-Request-Id": {"github-rate-limit-header-canary"}},
			body:      token,
			wantError: "GitHub rate limited request.",
		},
		{
			name:      "secondary rate limit with 403",
			status:    http.StatusForbidden,
			headers:   http.Header{"Retry-After": {"60"}, "X-GitHub-Request-Id": {"github-rate-limit-header-canary"}},
			body:      token,
			wantError: "GitHub rate limited request.",
		},
		{
			name:      "malformed retry-after with 403 stays access denied",
			status:    http.StatusForbidden,
			headers:   http.Header{"Retry-After": {"later"}, "X-GitHub-Request-Id": {"github-rate-limit-header-canary"}},
			body:      token,
			wantError: "GitHub denied pull request access (403)",
		},
		{name: "not found", status: http.StatusNotFound, body: token, wantError: "GitHub could not find the registered pull request (404)"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: token, wantError: "GitHub rate limited request."},
		{name: "base repository mismatch", status: http.StatusOK, body: githubPullRequestFixture(42, "other/repository", "contributor/fork", token)},
		{name: "malformed JSON", status: http.StatusOK, body: `{`},
		{name: "missing required field", status: http.StatusOK, body: `{"number":42}`},
		{name: "wrong PR number", status: http.StatusOK, body: githubPullRequestFixture(43, "ops/agent", "contributor/fork", token)},
		{name: "oversized response", status: http.StatusOK, body: strings.Repeat("x", maxAPIBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v3/repos/ops/agent/pulls/42" {
					t.Errorf("request escaped fixed registered route: %s %s", r.Method, r.URL)
				}
				resp := harborTestResponse(r, test.status, test.body)
				for name, values := range test.headers {
					for _, value := range values {
						resp.Header.Add(name, value)
					}
				}
				if test.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/pull/42")
				}
				return resp, nil
			})}
			got, err := fetchGitHubPullRequest(context.Background(), registered, token, 42, client)
			if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), test.body) ||
				strings.Contains(err.Error(), "github-rate-limit-header-canary") ||
				(test.wantError != "" && !strings.Contains(err.Error(), test.wantError)) || requests != 1 {
				t.Fatalf("result=%+v error=%v requests=%d; want one bounded non-leaking failure", got, err, requests)
			}
		})
	}
}

func TestGitHubPullRequestSuppressesTransportDeadlineError(t *testing.T) {
	const token = "ghp_pull_timeout_canary_0123456789"
	_, _, registered := githubPullRequestTestConfig(t)
	requests := 0
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v3/repos/ops/agent/pulls/42" {
			t.Errorf("request escaped fixed registered route: %s %s", r.Method, r.URL)
		}
		return nil, context.DeadlineExceeded
	})}

	result, err := fetchGitHubPullRequest(context.Background(), registered, token, 42, client)
	if err == nil || err.Error() != "Could not reach registered GitHub Enterprise origin." || strings.Contains(err.Error(), token) || requests != 1 {
		t.Fatalf("result=%+v error=%v requests=%d; want one generic non-leaking transport error", result, err, requests)
	}
}
