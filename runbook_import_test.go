package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseSelectedRunbookReadsOnlyRepositoryDirectivesAndMergesDuplicates(t *testing.T) {
	document := []byte("" +
		"# local notes\n" +
		"Do not read this token-canary or this URL https://private.example.invalid/path.\n" +
		"lah:repository https://github.example.invalid/ops/agent.git\n" +
		"lah:repository git@github.example.invalid:platform/agent.git\n" +
		"lah:repository https://github.example.invalid/ops/agent.git\n")
	references, err := parseSelectedRunbook(document)
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedRunbookReference{
		{origin: "https://github.example.invalid", repository: "ops/agent", occurrences: 2},
		{origin: "https://github.example.invalid", repository: "platform/agent", occurrences: 1},
	}
	if len(references) != len(want) {
		t.Fatalf("references = %#v, want %#v", references, want)
	}
	for index := range want {
		if references[index] != want[index] {
			t.Errorf("reference[%d] = %#v, want %#v", index, references[index], want[index])
		}
	}
}

func TestParseSelectedRunbookRejectsUnsafeDirectivesAndBounds(t *testing.T) {
	unsafe := []string{
		"lah:repository https://secret:password@github.example.invalid/ops/agent.git",
		"lah:repository https://github.example.invalid/ops/agent.git?token=query-canary",
		"lah:repository https://github.example.invalid/ops/agent.git#fragment-canary",
		"lah:repository http://github.example.invalid/ops/agent.git",
		"lah:repository file:///private/agent.git",
		"lah:repository https://github.example.invalid/org/subgroup/agent.git",
		"lah:repository https://github.example.invalid/%6fops/agent.git",
		"lah:repository https://github.example.invalid/org/ghp_abcdefghijklmnopqrstuvwxyz123456.git",
		"lah:repository",
		"lah:repositoryx https://github.example.invalid/ops/agent.git",
		"lah:repository https://github.example.invalid/ops/agent.git extra",
	}
	for index, directive := range unsafe {
		t.Run("unsafe_directive_"+strconv.Itoa(index), func(t *testing.T) {
			if _, err := parseSelectedRunbook([]byte(directive)); err == nil {
				t.Fatal("unsafe or malformed directive was accepted")
			}
		})
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "invalid UTF-8", data: []byte{0xff, 0xfe}},
		{name: "NUL byte", data: []byte("notes\x00more")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSelectedRunbook(test.data); err == nil {
				t.Fatal("unsupported document encoding was accepted")
			}
		})
	}
	if _, err := parseSelectedRunbook([]byte(strings.Repeat("\n", maxRunbookLines))); err == nil {
		t.Fatal("document above the line limit was accepted")
	}
	tooMany := strings.Builder{}
	for index := 0; index <= maxRunbookReferences; index++ {
		tooMany.WriteString("lah:repository https://github.example.invalid/owner/repo")
		tooMany.WriteString(strconv.Itoa(index))
		tooMany.WriteString(".git\n")
	}
	if _, err := parseSelectedRunbook([]byte(tooMany.String())); err == nil {
		t.Fatal("document above the unique reference limit was accepted")
	}
}

func TestRunbookPreviewMatchesExactEnabledTargetWithoutWritesOrSecretAccess(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	credentialLikeName := "github_pat_abcdefghijklmnopqrstuvwxyz123456"
	enabled := target{ID: "github:0123456789abcdef0123456789abcdef", Name: credentialLikeName, Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	disabled := target{ID: "github:1123456789abcdef0123456789abcdef", Name: "Disabled mirror", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:1123456789abcdef0123456789abcdef", Disabled: true}
	if err := writeConfig(configPath, config{Version: configVersion, GitHubTargets: []target{enabled, disabled}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	requests := &runbookRequestSpy{}
	a := &app{configPath: configPath, secrets: secrets, client: &http.Client{Transport: requests}, host: "127.0.0.1:43250", csrf: "csrf-canary"}
	rawURL := "https://github.example.invalid/ops/agent.git"
	document := []byte("# Selected runbook\n\n" +
		"token-canary and https://private.example.invalid/secret are unrelated notes.\n" +
		"lah:repository " + rawURL + "\n" +
		"lah:repository " + rawURL + "\n")
	for _, filename := range []string{"selected-filename-canary.md", "selected-filename-canary.MD", "selected-filename-canary.txt", "selected-filename-canary.TXT"} {
		t.Run(filename, func(t *testing.T) {
			response := serveRunbookPreview(t, a, document, filename, a.csrf, false, false)
			if response.Code != http.StatusOK {
				t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
			}
			var result runbookImportPreview
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Candidates) != 1 {
				t.Fatalf("candidate count = %d, want 1: %#v", len(result.Candidates), result.Candidates)
			}
			candidate := result.Candidates[0]
			if candidate.Source != "selected document file; actual path unverified" || candidate.Repository != "ops/agent" || candidate.Confidence != "high" || candidate.Occurrences != 2 || len(candidate.GitHubMatches) != 1 || candidate.GitHubMatches[0] != (gitImportTarget{ID: enabled.ID, Name: "[REDACTED]"}) {
				t.Fatalf("exact match candidate = %#v", candidate)
			}
			for _, privateValue := range []string{rawURL, "github.example.invalid", "token-canary", "private.example.invalid", credentialLikeName, enabled.SecretRef, disabled.SecretRef, filename} {
				if strings.Contains(response.Body.String(), privateValue) {
					t.Errorf("preview exposed %q: %s", privateValue, response.Body)
				}
			}
			if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
				t.Fatalf("preview accessed secret store: %#v", secrets)
			}
			if requests.count != 0 {
				t.Fatalf("preview contacted a service %d times", requests.count)
			}
			after, err := os.ReadFile(configPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preview modified config: err=%v", err)
			}
		})
	}
}

func TestRunbookPreviewRejectsInvalidFileAndMultipartWithoutEcho(t *testing.T) {
	for _, test := range []struct {
		name       string
		filename   string
		document   []byte
		csrf       string
		extraText  bool
		extraFile  bool
		wantStatus int
	}{
		{name: "unsupported extension", filename: "notes.json", document: []byte("lah:repository https://github.example.invalid/ops/agent.git"), wantStatus: http.StatusBadRequest},
		{name: "unsupported final extension", filename: "notes.txt.env", document: []byte("lah:repository https://github.example.invalid/ops/agent.git"), wantStatus: http.StatusBadRequest},
		{name: "empty file", filename: "RUNBOOK.md", document: []byte{}, wantStatus: http.StatusBadRequest},
		{name: "empty text file", filename: "notes.txt", document: []byte{}, wantStatus: http.StatusBadRequest},
		{name: "invalid UTF-8 text file", filename: "notes.txt", document: []byte{0xff, 0xfe}, wantStatus: http.StatusBadRequest},
		{name: "malformed URL does not echo", filename: "RUNBOOK.md", document: []byte("lah:repository https://secret:password@github.example.invalid/ops/agent.git"), wantStatus: http.StatusBadRequest},
		{name: "credential-like repository does not echo", filename: "RUNBOOK.md", document: []byte("lah:repository https://github.example.invalid/org/github_pat_abcdefghijklmnopqrstuvwxyz123456.git"), wantStatus: http.StatusBadRequest},
		{name: "wrong CSRF", filename: "RUNBOOK.md", document: []byte("lah:repository https://github.example.invalid/ops/agent.git"), csrf: "wrong-csrf", wantStatus: http.StatusForbidden},
		{name: "extra text part", filename: "RUNBOOK.md", document: []byte("notes"), extraText: true, wantStatus: http.StatusBadRequest},
		{name: "extra file part", filename: "RUNBOOK.md", document: []byte("notes"), extraFile: true, wantStatus: http.StatusBadRequest},
		{name: "oversized document", filename: "RUNBOOK.md", document: bytes.Repeat([]byte("x"), maxRunbookFileBytes+1), wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "settings.json")
			if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
				t.Fatal(err)
			}
			secrets := &gitImportSecretSpy{}
			a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43251", csrf: "expected-csrf"}
			csrf := test.csrf
			if csrf == "" {
				csrf = a.csrf
			}
			response := serveRunbookPreview(t, a, test.document, test.filename, csrf, test.extraText, test.extraFile)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body)
			}
			for _, privateValue := range []string{"secret:password", "github.example.invalid", "github_pat_abcdefghijklmnopqrstuvwxyz123456"} {
				if strings.Contains(response.Body.String(), privateValue) {
					t.Fatalf("error echoed %q: %s", privateValue, response.Body)
				}
			}
			if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
				t.Fatalf("invalid preview accessed secret store: %#v", secrets)
			}
		})
	}
}

func TestRunbookPreviewBodyLimitBeforeConfigOrSecrets(t *testing.T) {
	settingsDirectory := filepath.Join(t.TempDir(), "settings-directory")
	if err := os.Mkdir(settingsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := &gitImportSecretSpy{}
	a := &app{configPath: settingsDirectory, secrets: secrets, host: "127.0.0.1:43252", csrf: "valid-csrf"}
	var formBody bytes.Buffer
	writer := multipart.NewWriter(&formBody)
	if err := writer.WriteField("csrf", a.csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("runbook", "RUNBOOK.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("notes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var preamble strings.Builder
	for preamble.Len() <= maxRunbookRequestBytes {
		preamble.WriteString(strings.Repeat("p", 2048))
		preamble.WriteString("\r\n")
	}
	body := bytes.NewBufferString(preamble.String())
	body.Write(formBody.Bytes())
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-runbook", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRunbookPreview)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("oversized multipart status = %d, body=%s", response.Code, response.Body)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("oversized request accessed secret store: %#v", secrets)
	}
}

func TestRunbookPreviewUIHasBoundedSelectionAndSafeManualMatching(t *testing.T) {
	a := &app{configPath: filepath.Join(t.TempDir(), "settings.json"), host: "127.0.0.1:43253", csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/", nil)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRoot)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("page status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`id="runbook-import-form"`,
		`action="/preview-runbook"`,
		`name="runbook"`,
		`accept=".md,.txt,text/markdown,text/plain"`,
		`aria-describedby="runbook-import-hint"`,
		`Only lines in the form`,
		`lah:repository &lt;Git remote URL&gt;`,
		`does not save settings, read or write credentials, or contact a service`,
		`다음 지시자만 처리합니다:`,
		`형식의 줄만 읽으며 다른 문서 내용은 무시합니다.`,
		`Remote URLs are not returned.`,
		`new FormData(runbookImportForm)`,
		`candidate.repository`,
		`checkbox.checked = true`,
		`checkbox.focus()`,
		`No target was selected automatically.`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("Runbook import UI missing %q", expected)
		}
	}
	if runbookIndex, saveIndex := strings.Index(body, `id="runbook-import-form"`), strings.Index(body, `action="/save-bundle"`); runbookIndex < 0 || saveIndex < 0 || runbookIndex > saveIndex {
		t.Fatal("Runbook preview form is not separate from the existing bundle save form")
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("preview fetch CSP is incomplete: %q", csp)
	}
}

type runbookRequestSpy struct {
	count int
}

func (s *runbookRequestSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.count++
	return nil, errors.New("unexpected outbound request")
}

func serveRunbookPreview(t *testing.T, a *app, document []byte, filename, csrf string, extraText, extraFile bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("runbook", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(document); err != nil {
		t.Fatal(err)
	}
	if extraText {
		if err := writer.WriteField("unexpected", "extra-field-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if extraFile {
		file, err := writer.CreateFormFile("another_file", "notes.md")
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
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-runbook", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleRunbookPreview)).ServeHTTP(response, request)
	return response
}
