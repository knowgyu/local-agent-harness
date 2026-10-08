package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type gitImportSecretSpy struct {
	loads, saves, deletes int
}

type gitImportRoundTripper func(*http.Request) (*http.Response, error)

func (f gitImportRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (s *gitImportSecretSpy) Save(string, []byte) error {
	s.saves++
	return nil
}

func (s *gitImportSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, nil
}

func (s *gitImportSecretSpy) Delete(string) error {
	s.deletes++
	return nil
}

func TestParseSelectedGitConfigRemoteFormsAndIgnoresIncludes(t *testing.T) {
	data := []byte("" +
		"[core]\nrepositoryformatversion = 0\n" +
		"[remote \"origin\"]\nurl = \"https://github.example.invalid/ops/agent.git\"\n" +
		"[remote \"fork\"]\nurl = git@github.example.invalid:contrib/agent.git\n" +
		"[remote \"ssh-url\"]\nurl = ssh://git@github.example.invalid/platform/agent.git\n" +
		"[include]\npath = /must/not/be/read\n")
	remotes, err := parseSelectedGitConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedGitRemote{
		{name: "origin", origin: "https://github.example.invalid", repository: "ops/agent"},
		{name: "fork", origin: "https://github.example.invalid", repository: "contrib/agent"},
		{name: "ssh-url", origin: "https://github.example.invalid", repository: "platform/agent"},
	}
	if len(remotes) != len(want) {
		t.Fatalf("parsed remotes = %#v, want %#v", remotes, want)
	}
	for i := range want {
		if remotes[i] != want[i] {
			t.Errorf("remote[%d] = %#v, want %#v", i, remotes[i], want[i])
		}
	}
}

func TestParseSelectedGitConfigRejectsUnsafeRemoteURLs(t *testing.T) {
	cases := map[string]string{
		"userinfo password":  `https://secret-user:password-canary@github.example.invalid/ops/agent.git`,
		"query secret":       `https://github.example.invalid/ops/agent.git?token=query-canary`,
		"fragment":           `https://github.example.invalid/ops/agent.git#fragment-canary`,
		"non HTTPS":          `http://github.example.invalid/ops/agent.git`,
		"unsupported scheme": `file:///tmp/ops/agent.git`,
		"SSH user":           `ssh://other@github.example.invalid/ops/agent.git`,
		"SSH password":       `ssh://git:ssh-password-canary@github.example.invalid/ops/agent.git`,
		"unsupported path":   `https://github.example.invalid/group/subgroup/agent.git`,
		"escaped path":       `https://github.example.invalid/%6fops/agent.git`,
		"unsupported host":   `https://github_example.invalid/ops/agent.git`,
	}
	for name, remoteURL := range cases {
		t.Run(name, func(t *testing.T) {
			data := []byte("[remote \"origin\"]\nurl = " + remoteURL + "\n")
			if _, err := parseSelectedGitConfig(data); err == nil {
				t.Fatal("unsafe or unsupported remote was accepted")
			} else if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "token") {
				t.Fatalf("parser error echoed sensitive input: %v", err)
			}
		})
	}
	for name, data := range map[string]string{
		"legacy assignment syntax": "url https://github.example.invalid/ops/agent.git",
		"multiple URL values":      "url = https://github.example.invalid/ops/agent.git\nurl = git@github.example.invalid:fork/agent.git",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSelectedGitConfig([]byte("[remote \"origin\"]\n" + data + "\n")); err == nil {
				t.Fatal("unsupported remote assignment was accepted")
			}
		})
	}
}

func TestGitRemotePreviewMatchesOnlyExactEnabledTargetWithoutWrites(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	enabled := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	disabled := target{ID: "github:1123456789abcdef0123456789abcdef", Name: "Disabled mirror", Origin: "https://github.example.invalid", Repository: "ops/disabled", SecretRef: "cred:1123456789abcdef0123456789abcdef", Disabled: true}
	if err := writeConfig(configPath, config{Version: configVersion, GitHubTargets: []target{enabled, disabled}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43210", csrf: "csrf-canary"}
	data := []byte("" +
		"[remote \"origin\"]\nurl = git@github.example.invalid:ops/agent.git\n" +
		"[remote \"disabled\"]\nurl = https://github.example.invalid/ops/disabled.git\n" +
		"[remote \"case-different\"]\nurl = https://github.example.invalid/ops/Agent.git\n" +
		"[remote \"other-origin\"]\nurl = https://other.example.invalid/ops/agent.git\n")
	response := serveGitRemotePreview(t, a, data, "config", false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	var result gitImportPreview
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 4 {
		t.Fatalf("candidate count = %d, want 4", len(result.Candidates))
	}
	if got := result.Candidates[0]; got.Source != "selected file named config; actual path unverified" || got.Remote != "origin" || got.Repository != "ops/agent" || got.Confidence != "high" || len(got.GitHubMatches) != 1 || got.GitHubMatches[0] != (gitImportTarget{ID: enabled.ID, Name: enabled.Name}) {
		t.Fatalf("exact target match = %#v", got)
	}
	for _, index := range []int{1, 2, 3} {
		if len(result.Candidates[index].GitHubMatches) != 0 {
			t.Errorf("candidate %q unexpectedly matched a target: %#v", result.Candidates[index].Remote, result.Candidates[index].GitHubMatches)
		}
	}
	wantUnknown := []string{"Jenkins", "Harbor", "Dashboard", "permissions"}
	if !bytes.Equal(mustJSON(t, result.Candidates[0].Unverified), mustJSON(t, wantUnknown)) {
		t.Fatalf("unverified fields = %#v, want %#v", result.Candidates[0].Unverified, wantUnknown)
	}
	for _, privateValue := range []string{"https://github.example.invalid", "git@github.example.invalid"} {
		if strings.Contains(response.Body.String(), privateValue) {
			t.Errorf("preview echoed raw remote URL %q", privateValue)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preview modified config: err=%v", err)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("preview accessed secret store: %#v", secrets)
	}
}

func TestGitRemotePreviewBoundsDuplicateMatchesAndButtons(t *testing.T) {
	const targetCount = 1000
	configPath := filepath.Join(t.TempDir(), "settings.json")
	cfg := config{Version: configVersion, GitHubTargets: make([]target, 0, targetCount)}
	for index := 0; index < targetCount; index++ {
		cfg.GitHubTargets = append(cfg.GitHubTargets, target{
			ID:         fmt.Sprintf("github:%032x", index),
			Name:       fmt.Sprintf("Registered alias %04d", index),
			Origin:     "https://github.example.invalid",
			Repository: "ops/agent",
			SecretRef:  "cred:0123456789abcdef0123456789abcdef",
		})
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: &gitImportSecretSpy{}, host: "127.0.0.1:43216", csrf: "csrf-token"}
	var remotes strings.Builder
	for index := 0; index < maxGitImportRemotes; index++ {
		fmt.Fprintf(&remotes, "[remote \"remote-%02d\"]\nurl = https://github.example.invalid/ops/agent.git\n", index)
	}
	response := serveGitRemotePreview(t, a, []byte(remotes.String()), "config", false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("bounded preview status = %d, body=%s", response.Code, response.Body)
	}
	if response.Body.Len() > maxGitImportPreviewBytes {
		t.Fatalf("preview JSON bytes = %d, limit %d", response.Body.Len(), maxGitImportPreviewBytes)
	}
	var preview gitImportPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != maxGitImportRemotes {
		t.Fatalf("candidate count = %d, want %d", len(preview.Candidates), maxGitImportRemotes)
	}
	potentialButtons := 0
	for _, candidate := range preview.Candidates {
		if len(candidate.GitHubMatches) != maxGitImportMatchesPerRemote || !candidate.MatchesTruncated || candidate.Confidence != "low" {
			t.Fatalf("duplicate matches were not bounded and marked: %#v", candidate)
		}
		potentialButtons += len(candidate.GitHubMatches)
	}
	buttonLimit := maxGitImportRemotes * maxGitImportMatchesPerRemote
	if potentialButtons != buttonLimit || potentialButtons > buttonLimit {
		t.Fatalf("potential manual selection buttons = %d, want capped at %d", potentialButtons, buttonLimit)
	}
	if !strings.Contains(response.Body.String(), `"matches_truncated":true`) {
		t.Fatal("preview JSON omitted the truncation marker")
	}
}

func TestGitRemotePreviewRejectsSecretCanariesWithoutEcho(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"userinfo": `https://user:git-url-password-canary@github.example.invalid/ops/agent.git`,
		"query":    `https://github.example.invalid/ops/agent.git?token=git-query-token-canary`,
	}
	for name, remoteURL := range cases {
		t.Run(name, func(t *testing.T) {
			secrets := &gitImportSecretSpy{}
			a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43211", csrf: "csrf-token"}
			data := []byte("[remote \"origin\"]\nurl = " + remoteURL + "\n")
			response := serveGitRemotePreview(t, a, data, "config", false, false)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("unsafe remote status = %d, body=%s", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), "canary") || strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "token") || strings.Contains(response.Body.String(), remoteURL) {
				t.Fatalf("error response echoed uploaded remote data: %s", response.Body)
			}
			after, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected preview modified config: err=%v", err)
			}
			if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
				t.Fatalf("rejected preview accessed secret store: %#v", secrets)
			}
		})
	}
}

func TestGitRemotePreviewRedactsKnownCredentialPatternsWithoutSideEffects(t *testing.T) {
	const (
		remoteName  = "github_pat_remote_canary_0123456789abcdef"
		repository  = "ghp_owner_canary_0123456789abcdef/service"
		targetName  = "ghp_target_name_canary_0123456789abcdef"
		remoteURL   = "https://github.example.invalid/" + repository + ".git"
		remoteEntry = "[remote \"" + remoteName + "\"]\nurl = " + remoteURL + "\n"
	)
	configPath := filepath.Join(t.TempDir(), "settings.json")
	registeredTarget := target{
		ID:         "github:0123456789abcdef0123456789abcdef",
		Name:       targetName,
		Origin:     "https://github.example.invalid",
		Repository: repository,
		SecretRef:  "cred:0123456789abcdef0123456789abcdef",
	}
	if err := writeConfig(configPath, config{Version: configVersion, GitHubTargets: []target{registeredTarget}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	requests := 0
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		host:       "127.0.0.1:43218",
		csrf:       "csrf-token",
		client: &http.Client{Transport: gitImportRoundTripper(func(*http.Request) (*http.Response, error) {
			requests++
			return nil, fmt.Errorf("unexpected preview network request")
		})},
	}
	response := serveGitRemotePreview(t, a, []byte(remoteEntry), "config", false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	var result gitImportPreview
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(result.Candidates))
	}
	candidate := result.Candidates[0]
	if candidate.Remote != "[REDACTED]" {
		t.Errorf("remote alias = %q, want redacted", candidate.Remote)
	}
	if candidate.Repository != "[REDACTED]/service" {
		t.Errorf("repository = %q, want redacted", candidate.Repository)
	}
	if len(candidate.GitHubMatches) != 1 {
		t.Fatalf("exact match count = %d, want 1", len(candidate.GitHubMatches))
	}
	if candidate.Confidence != "high" {
		t.Errorf("match confidence = %q, want high", candidate.Confidence)
	}
	if candidate.GitHubMatches[0].ID != registeredTarget.ID {
		t.Errorf("matched target ID = %q, want exact registered target ID", candidate.GitHubMatches[0].ID)
	}
	if candidate.GitHubMatches[0].Name != "[REDACTED]" {
		t.Fatalf("credential-like preview fields were not redacted: %#v", candidate)
	}
	for _, canary := range []string{remoteName, repository, targetName, "github_pat_", "ghp_"} {
		if strings.Contains(response.Body.String(), canary) {
			t.Errorf("preview response exposed credential-like value %q: %s", canary, response.Body)
		}
	}
	if strings.Contains(response.Body.String(), remoteURL) {
		t.Fatal("preview response echoed the raw remote URL")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preview modified config: err=%v", err)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("preview accessed secret store: %#v", secrets)
	}
	if requests != 0 {
		t.Fatalf("preview made %d network requests", requests)
	}
}

func TestGitRemotePreviewBoundsFileAndRemoteCount(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: &gitImportSecretSpy{}, host: "127.0.0.1:43212", csrf: "csrf-token"}
	prefix := []byte("[remote \"origin\"]\nurl = https://github.example.invalid/ops/agent.git\n")
	maxSize := append(append([]byte(nil), prefix...), bytes.Repeat([]byte("#"), maxGitConfigSize-len(prefix))...)
	response := serveGitRemotePreview(t, a, maxSize, "config", false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("exactly 64 KiB config response = %d, body=%s", response.Code, response.Body)
	}
	oversized := bytes.Repeat([]byte("x"), maxGitConfigSize+1)
	response = serveGitRemotePreview(t, a, oversized, "config", false, false)
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "64 KiB") {
		t.Fatalf("oversized file response = %d, %s", response.Code, response.Body)
	}
	var many strings.Builder
	for index := 0; index <= maxGitImportRemotes; index++ {
		fmt.Fprintf(&many, "[remote \"remote-%02d\"]\nurl = https://github.example.invalid/ops/repo-%02d.git\n", index, index)
	}
	response = serveGitRemotePreview(t, a, []byte(many.String()), "config", false, false)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "repo-00") {
		t.Fatalf("too-many-remotes response = %d, %s", response.Code, response.Body)
	}
}

func TestGitRemotePreviewRejectsMalformedMultipartAndExtraParts(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: &gitImportSecretSpy{}, host: "127.0.0.1:43213", csrf: "csrf-token"}
	validFile := []byte("[remote \"origin\"]\nurl = https://github.example.invalid/ops/agent.git\n")
	for _, test := range []struct {
		name      string
		extraText bool
		extraFile bool
	}{
		{name: "extra text field", extraText: true},
		{name: "extra file", extraFile: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serveGitRemotePreview(t, a, validFile, "config", test.extraText, test.extraFile)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("extra-part status = %d, body=%s", response.Code, response.Body)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-git-remotes", strings.NewReader("--broken"))
	request.Host = a.host
	request.Header.Set("Origin", "http://"+a.host)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=missing-end-boundary")
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleGitRemotePreview)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden && response.Code != http.StatusBadRequest {
		t.Fatalf("malformed multipart status = %d, body=%s", response.Code, response.Body)
	}
}

func TestGitRemotePreviewRejectsBodyOverRequestLimitBeforeSettingsOrSecrets(t *testing.T) {
	settingsDirectory := filepath.Join(t.TempDir(), "settings-directory")
	if err := os.Mkdir(settingsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	a := &app{configPath: settingsDirectory, secrets: secrets, host: "127.0.0.1:43217", csrf: "valid-csrf"}
	validFile := []byte("[remote \"origin\"]\nurl = https://github.example.invalid/ops/agent.git\n")
	if len(validFile) >= maxGitConfigSize {
		t.Fatal("test config unexpectedly exceeds the file limit")
	}
	var formBody bytes.Buffer
	writer := multipart.NewWriter(&formBody)
	if err := writer.WriteField("csrf", a.csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("git_config", "config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(validFile); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var preamble strings.Builder
	for preamble.Len() <= maxGitImportRequest {
		preamble.WriteString(strings.Repeat("p", 2048))
		preamble.WriteString("\r\n")
	}
	body := bytes.NewBufferString(preamble.String())
	body.Write(formBody.Bytes())
	if body.Len() <= maxGitImportRequest {
		t.Fatalf("test request body is %d bytes; want more than %d", body.Len(), maxGitImportRequest)
	}
	baseline := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-git-remotes", bytes.NewReader(body.Bytes()))
	baseline.Header.Set("Content-Type", writer.FormDataContentType())
	if err := baseline.ParseMultipartForm(int64(body.Len())); err != nil {
		t.Fatalf("test multipart is not valid without the request cap: %v", err)
	}
	if baseline.FormValue("csrf") != a.csrf || len(baseline.MultipartForm.Value) != 1 || len(baseline.MultipartForm.File) != 1 || len(baseline.MultipartForm.File["git_config"]) != 1 || baseline.MultipartForm.File["git_config"][0].Filename != "config" || baseline.MultipartForm.File["git_config"][0].Size != int64(len(validFile)) {
		t.Fatal("test multipart does not contain the expected valid CSRF and file fields")
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-git-remotes", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleGitRemotePreview)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("oversized multipart status = %d, body=%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), string(validFile)) {
		t.Fatalf("oversized multipart response echoed file content: %s", response.Body)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("oversized multipart accessed secret store: %#v", secrets)
	}
}

func TestGitRemotePreviewRequiresMultipartCSRF(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43215", csrf: "expected-csrf"}
	rawURL := `https://github.example.invalid/ops/agent.git`
	response := serveGitRemotePreviewWithCSRF(t, a, []byte("[remote \"origin\"]\nurl = "+rawURL+"\n"), "config", "wrong-csrf", false, false)
	if response.Code != http.StatusForbidden {
		t.Fatalf("invalid CSRF status = %d, body=%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), rawURL) {
		t.Fatalf("CSRF error echoed file content: %s", response.Body)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("CSRF rejection modified config: err=%v", err)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("CSRF rejection accessed secret store: %#v", secrets)
	}
}

func TestGitRemotePreviewUIHasAccessibleSelectionAndSafePreselection(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43214", csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`id="git-import-form"`,
		`name="git_config"`,
		`type="file"`,
		`aria-describedby="git-import-hint"`,
		`file named config`,
		`cannot verify whether it came from the project’s .git folder`,
		`Select a file named config first.`,
		`Only the selected file, up to 64 KiB`,
		`new FormData(gitImportForm)`,
		`candidate.repository`,
		`for (const match of matches)`,
		`checkbox.checked = true`,
		`checkbox.focus()`,
		`Save the connection group to keep this selection.`,
		`candidate.matches_truncated`,
		`Only the first 8 are shown`,
		`No target was selected automatically.`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("Git import UI missing %q", expected)
		}
	}
	if formIndex, saveIndex := strings.Index(body, `id="git-import-form"`), strings.Index(body, `action="/save-bundle"`); formIndex < 0 || saveIndex < 0 || formIndex > saveIndex {
		t.Fatal("Git import form is not separate from the existing bundle save form")
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("preview fetch CSP is incomplete: %q", csp)
	}
}

func serveGitRemotePreview(t *testing.T, a *app, data []byte, filename string, extraText, extraFile bool) *httptest.ResponseRecorder {
	return serveGitRemotePreviewWithCSRF(t, a, data, filename, a.csrf, extraText, extraFile)
}

func serveGitRemotePreviewWithCSRF(t *testing.T, a *app, data []byte, filename, csrf string, extraText, extraFile bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("git_config", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if extraText {
		if err := writer.WriteField("unexpected", "extra-field-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if extraFile {
		file, err := writer.CreateFormFile("another_file", "config")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "extra-file-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-git-remotes", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleGitRemotePreview)).ServeHTTP(response, request)
	return response
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
