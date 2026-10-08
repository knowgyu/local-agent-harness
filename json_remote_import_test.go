package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestParseSelectedJSONGitRemotesInspectsOnlyWholeStringValues(t *testing.T) {
	data := []byte(`{
		"https://key.example.invalid/key/repo.git": "ordinary text",
		"payload": [
			{"remote": "https://github.example.invalid/ops/agent.git"},
			"git@GITHUB.EXAMPLE.INVALID:ops/agent",
			{"nested": {"value": "ssh://git@github.example.invalid/platform/service.git"}},
			"repository is https://ignored.example.invalid/org/repo.git",
			" https://ignored.example.invalid/org/leading-space.git",
			"https://ignored.example.invalid/org/trailing-space.git ",
			"http://ignored.example.invalid/org/http.git",
			"file:///tmp/org/local.git"
		]
	}`)
	remotes, err := parseSelectedJSONGitRemotes(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedJSONGitRemote{
		{Origin: "https://github.example.invalid", Repository: "ops/agent"},
		{Origin: "https://github.example.invalid", Repository: "platform/service"},
	}
	if !equalJSONGitRemotes(remotes, want) {
		t.Fatalf("parsed remotes = %#v, want %#v", remotes, want)
	}
}

func TestParseSelectedJSONGitRemotesDeduplicatesNormalizedPairs(t *testing.T) {
	data := []byte(`[
		"https://github.example.invalid/org/repo.git",
		"git@GITHUB.EXAMPLE.INVALID:org/repo",
		"ssh://git@github.example.invalid/org/repo.git",
		"https://github.example.invalid:443/org/repo.git",
		"https://github.example.invalid/Org/repo.git"
	]`)
	remotes, err := parseSelectedJSONGitRemotes(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedJSONGitRemote{
		{Origin: "https://github.example.invalid", Repository: "org/repo"},
		{Origin: "https://github.example.invalid", Repository: "Org/repo"},
	}
	if !equalJSONGitRemotes(remotes, want) {
		t.Fatalf("parsed remotes = %#v, want %#v", remotes, want)
	}
}

func TestParseSelectedJSONGitRemotesRejectsAmbiguousOrOverLimitDocuments(t *testing.T) {
	deep64 := strings.Repeat("[", maxJSONGitRemoteDepth) + `"https://github.example.invalid/org/repo.git"` + strings.Repeat("]", maxJSONGitRemoteDepth)
	deep65 := strings.Repeat("[", maxJSONGitRemoteDepth+1) + `"https://github.example.invalid/org/repo.git"` + strings.Repeat("]", maxJSONGitRemoteDepth+1)
	withinTokenLimit := `[` + strings.TrimSuffix(strings.Repeat("null,", maxJSONGitRemoteTokens-2), ",") + `]`
	overTokenLimit := `[` + strings.TrimSuffix(strings.Repeat("null,", maxJSONGitRemoteTokens-1), ",") + `]`
	exactTokenObject := make([]string, (maxJSONGitRemoteTokens-2)/2)
	overTokenObject := make([]string, len(exactTokenObject)+1)
	for index := range exactTokenObject {
		exactTokenObject[index] = fmt.Sprintf(`"key-%05d":null`, index)
		overTokenObject[index] = exactTokenObject[index]
	}
	overTokenObject[len(overTokenObject)-1] = `"extra-key":null`
	withinObjectTokenLimit := `{` + strings.Join(exactTokenObject, ",") + `}`
	overObjectTokenLimit := `{` + strings.Join(overTokenObject, ",") + `}`
	longValue := `"` + strings.Repeat("x", maxJSONGitRemoteStringBytes+1) + `"`
	tooManyCandidates := make([]string, maxJSONGitRemoteCandidates+1)
	for index := range tooManyCandidates {
		tooManyCandidates[index] = fmt.Sprintf(`"https://github.example.invalid/org/repo-%02d.git"`, index)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "duplicate root keys", data: []byte(`{"name":1,"name":2}`)},
		{name: "duplicate escaped keys at nested depth", data: []byte(`{"nested":{"name":1,"na\u006de":2}}`)},
		{name: "duplicate keys in array object", data: []byte(`[{"name":1,"name":2}]`)},
		{name: "trailing JSON value", data: []byte(`"https://github.example.invalid/org/repo.git" null`)},
		{name: "trailing malformed data after candidate", data: []byte(`{"remote":"https://github.example.invalid/org/repo.git"} trailing`)},
		{name: "invalid UTF-8", data: []byte{'[', '"', 0xff, '"', ']'}},
		{name: "malformed JSON", data: []byte(`{"remote":`)},
		{name: "depth boundary plus one", data: []byte(deep65)},
		{name: "decoder token boundary plus one", data: []byte(overTokenLimit)},
		{name: "object keys count toward token limit", data: []byte(overObjectTokenLimit)},
		{name: "candidate boundary plus one", data: []byte(`[` + strings.Join(tooManyCandidates, ",") + `]`)},
		{name: "file boundary plus one", data: bytes.Repeat([]byte(" "), maxJSONGitRemoteFileBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			remotes, err := parseSelectedJSONGitRemotes(test.data)
			if err == nil || remotes != nil {
				t.Fatalf("parse returned remotes=%#v, err=%v; want nil list and error", remotes, err)
			}
			for _, sourceValue := range []string{"github.example.invalid", "canary"} {
				if strings.Contains(err.Error(), sourceValue) {
					t.Fatalf("parser error echoed source value %q: %v", sourceValue, err)
				}
			}
		})
	}

	if remotes, err := parseSelectedJSONGitRemotes([]byte(deep64)); err != nil || len(remotes) != 1 {
		t.Fatalf("depth 64 document remotes=%#v, err=%v; want one candidate", remotes, err)
	}
	if remotes, err := parseSelectedJSONGitRemotes([]byte(withinTokenLimit)); err != nil || len(remotes) != 0 {
		t.Fatalf("token boundary document remotes=%#v, err=%v; want empty result", remotes, err)
	}
	if remotes, err := parseSelectedJSONGitRemotes([]byte(withinObjectTokenLimit)); err != nil || len(remotes) != 0 {
		t.Fatalf("object token boundary document remotes=%#v, err=%v; want empty result", remotes, err)
	}
	validLongValue := `"` + strings.Repeat("x", maxJSONGitRemoteStringBytes) + `"`
	if remotes, err := parseSelectedJSONGitRemotes([]byte(validLongValue)); err != nil || len(remotes) != 0 {
		t.Fatalf("string boundary document remotes=%#v, err=%v; want empty result", remotes, err)
	}
	if remotes, err := parseSelectedJSONGitRemotes([]byte(longValue)); err != nil || len(remotes) != 0 {
		t.Fatalf("overlong string document remotes=%#v, err=%v; want ignored value", remotes, err)
	}
	withinCandidateLimit := tooManyCandidates[:maxJSONGitRemoteCandidates]
	if remotes, err := parseSelectedJSONGitRemotes([]byte(`[` + strings.Join(withinCandidateLimit, ",") + `]`)); err != nil || len(remotes) != maxJSONGitRemoteCandidates {
		t.Fatalf("candidate boundary document count=%d, err=%v; want %d", len(remotes), err, maxJSONGitRemoteCandidates)
	}
}

func TestJSONGitRemotePreviewMatchesEnabledTargetsAndReturnsOnlyProjection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "active-settings.json")
	active := config{Version: configVersion, GitHubTargets: []target{
		{ID: "github:0123456789abcdef0123456789abcdef", Name: "registered-name-canary", Origin: "https://GITHUB.EXAMPLE.INVALID:443/", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"},
		{ID: "github:1123456789abcdef0123456789abcdef", Name: "disabled-name-canary", Origin: "https://github.example.invalid", Repository: "ops/disabled", SecretRef: "cred:1123456789abcdef0123456789abcdef", Disabled: true},
	}}
	if err := writeConfig(configPath, active); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &jsonGitRemoteSecretSpy{}
	requests := &jsonGitRemoteRequestSpy{}
	a := &app{configPath: configPath, secrets: secrets, client: &http.Client{Transport: requests}, host: "127.0.0.1:43381", csrf: "json-preview-csrf-canary"}
	data := []byte(`{
		"https://key-canary.example.invalid/key/repo.git": "non-candidate prose",
		"nested": [
			"https://github.example.invalid/ops/agent.git",
			"https://github.example.invalid/ops/disabled.git",
			"https://github.example.invalid/ops/Agent.git",
			"https://other.example.invalid/ops/agent.git",
			"https://github.example.invalid/org/github_pat_abcdefghijklmnopqrstuvwxyz123456.git",
			"/input/path-canary.json"
		]
	}`)
	response := serveJSONGitRemotePreview(t, a, data, "user-selected-filename-canary.json", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Body.Len() > maxJSONGitRemoteResponseBytes {
		t.Fatalf("response headers or size invalid: headers=%v, size=%d", response.Header(), response.Body.Len())
	}
	var envelope struct {
		Candidates []map[string]json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Candidates) != 5 {
		t.Fatalf("candidate count = %d, want 5: %s", len(envelope.Candidates), response.Body)
	}
	for index, wantMatch := range []bool{true, false, false, false, false} {
		candidate := envelope.Candidates[index]
		if len(candidate) != 4 {
			t.Fatalf("candidate fields = %#v, want only four approved fields", candidate)
		}
		var source, repository, confidence string
		var match bool
		if err := json.Unmarshal(candidate["source"], &source); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(candidate["repository"], &repository); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(candidate["confidence"], &confidence); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(candidate["registered_match"], &match); err != nil {
			t.Fatal(err)
		}
		if source != jsonGitRemotePreviewSource || confidence != "low" || match != wantMatch {
			t.Errorf("candidate[%d] source=%q confidence=%q registered_match=%v", index, source, confidence, match)
		}
		if index == 4 && repository != "org/[REDACTED]" {
			t.Errorf("sensitive repository was not sanitized: %q", repository)
		}
	}
	for _, privateValue := range []string{
		"github.example.invalid", "other.example.invalid", "key-canary", "path-canary", "user-selected-filename-canary",
		"json-preview-csrf-canary", "github_pat_abcdefghijklmnopqrstuvwxyz123456", "registered-name-canary", "disabled-name-canary",
		"github:0123456789abcdef0123456789abcdef", "github:1123456789abcdef0123456789abcdef", "cred:",
		"https://", "git@", "/input/path-canary.json",
	} {
		if strings.Contains(response.Body.String(), privateValue) {
			t.Errorf("preview exposed %q: %s", privateValue, response.Body)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preview modified active config: err=%v", err)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("preview accessed credential store: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("preview sent %d outbound requests", requests.count)
	}
}

func TestJSONGitRemotePreviewRejectsBadInputBeforeConfigRead(t *testing.T) {
	configDirectory := filepath.Join(t.TempDir(), "settings-directory")
	if err := os.Mkdir(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets := &jsonGitRemoteSecretSpy{}
	requests := &jsonGitRemoteRequestSpy{}
	a := &app{configPath: configDirectory, secrets: secrets, client: &http.Client{Transport: requests}, host: "127.0.0.1:43382", csrf: "csrf-token"}
	response := serveJSONGitRemotePreview(t, a, []byte(`{"duplicate":1,"duplicate":2}`), "input.json", a.csrf, false, false)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "settings") {
		t.Fatalf("invalid JSON status/body = %d %s; want parser rejection before settings read", response.Code, response.Body)
	}
	assertJSONGitRemoteNoSideEffects(t, secrets, requests)
}

func TestJSONGitRemotePreviewRejectsMultipartAndSizeViolations(t *testing.T) {
	valid := []byte(`{"url":"https://github.example.invalid/org/repo.git"}`)
	for _, test := range []struct {
		name       string
		filename   string
		data       []byte
		csrf       string
		extraText  bool
		extraFile  bool
		duplicate  bool
		wantStatus int
	}{
		{name: "wrong extension", filename: "input.txt", data: valid, wantStatus: http.StatusBadRequest},
		{name: "empty file", filename: "input.json", wantStatus: http.StatusBadRequest},
		{name: "file over 1 MiB", filename: "input.json", data: bytes.Repeat([]byte(" "), maxJSONGitRemoteFileBytes+1), wantStatus: http.StatusBadRequest},
		{name: "wrong CSRF", filename: "input.json", data: valid, csrf: "wrong-csrf-canary", wantStatus: http.StatusForbidden},
		{name: "extra text field", filename: "input.json", data: valid, extraText: true, wantStatus: http.StatusBadRequest},
		{name: "extra file field", filename: "input.json", data: valid, extraFile: true, wantStatus: http.StatusBadRequest},
		{name: "duplicate CSRF value", filename: "input.json", data: valid, duplicate: true, wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newJSONGitRemoteTestApp(t)
			csrf := test.csrf
			if csrf == "" {
				csrf = a.csrf
			}
			response := serveJSONGitRemotePreview(t, a, test.data, test.filename, csrf, test.extraText, test.extraFile, test.duplicate)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body)
			}
			for _, privateValue := range []string{"wrong-csrf-canary", "input.json", "extra-field-canary", "extra-file-canary", "github.example.invalid"} {
				if strings.Contains(response.Body.String(), privateValue) {
					t.Errorf("error response exposed %q: %s", privateValue, response.Body)
				}
			}
			assertJSONGitRemoteNoSideEffects(t, a.secrets.(*jsonGitRemoteSecretSpy), a.client.Transport.(*jsonGitRemoteRequestSpy))
		})
	}
}

func TestJSONGitRemotePreviewRejectsDuplicateDispositionAndOversizedRequest(t *testing.T) {
	if isExactJSONGitRemoteFilePart([]string{`form-data; name="json_remotes"; filename="other.json"`}, "input.json") {
		t.Fatal("mismatched Content-Disposition filename was accepted")
	}
	if isExactJSONGitRemoteFilePart([]string{`form-data; name="other"; filename="input.json"`}, "input.json") {
		t.Fatal("mismatched Content-Disposition field name was accepted")
	}
	if isExactJSONGitRemoteFilePart([]string{
		`form-data; name="json_remotes"; filename="input.json"`,
		`form-data; name="json_remotes"; filename="input.json"`,
	}, "input.json") {
		t.Fatal("multiple Content-Disposition headers were accepted")
	}

	t.Run("duplicate Content-Disposition", func(t *testing.T) {
		a := newJSONGitRemoteTestApp(t)
		boundary := "json-remote-boundary"
		body := "--" + boundary + "\r\n" +
			"Content-Disposition: form-data; name=\"csrf\"\r\n\r\n" + a.csrf + "\r\n" +
			"--" + boundary + "\r\n" +
			"Content-Disposition: form-data; name=\"json_remotes\"; filename=\"one.json\"\r\n" +
			"Content-Disposition: form-data; name=\"json_remotes\"; filename=\"two.json\"\r\n" +
			"Content-Type: application/json\r\n\r\n{}\r\n" +
			"--" + boundary + "--\r\n"
		response := serveJSONGitRemoteRawRequest(t, a, []byte(body), "multipart/form-data; boundary="+boundary)
		if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "two.json") {
			t.Fatalf("duplicate disposition response = %d %s", response.Code, response.Body)
		}
		assertJSONGitRemoteNoSideEffects(t, a.secrets.(*jsonGitRemoteSecretSpy), a.client.Transport.(*jsonGitRemoteRequestSpy))
	})

	t.Run("whole request above limit", func(t *testing.T) {
		a := newJSONGitRemoteTestApp(t)
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("csrf", a.csrf); err != nil {
			t.Fatal(err)
		}
		padding, err := writer.CreateFormField("padding")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := padding.Write(bytes.Repeat([]byte("p"), maxJSONGitRemoteRequestBytes)); err != nil {
			t.Fatal(err)
		}
		file, err := writer.CreateFormFile("json_remotes", "input.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(`{"url":"https://github.example.invalid/org/repo.git"}`)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		response := serveJSONGitRemoteRawRequest(t, a, body.Bytes(), writer.FormDataContentType())
		if response.Code != http.StatusForbidden || response.Body.Len() > maxJSONGitRemoteResponseBytes {
			t.Fatalf("oversized request response = %d %s", response.Code, response.Body)
		}
		assertJSONGitRemoteNoSideEffects(t, a.secrets.(*jsonGitRemoteSecretSpy), a.client.Transport.(*jsonGitRemoteRequestSpy))
	})
}

func TestJSONGitRemotePreviewResponseLimitAndMethod(t *testing.T) {
	large := jsonGitRemotePreview{Candidates: make([]jsonGitRemotePreviewCandidate, 1024)}
	for index := range large.Candidates {
		large.Candidates[index] = jsonGitRemotePreviewCandidate{Source: jsonGitRemotePreviewSource, Repository: strings.Repeat("x", 400), Confidence: "low"}
	}
	response := httptest.NewRecorder()
	writeJSONGitRemoteJSON(response, http.StatusOK, large)
	if response.Code != http.StatusRequestEntityTooLarge || response.Body.Len() > maxJSONGitRemoteResponseBytes || strings.Contains(response.Body.String(), strings.Repeat("x", 100)) {
		t.Fatalf("response limit fallback = status %d, body bytes %d", response.Code, response.Body.Len())
	}

	a := newJSONGitRemoteTestApp(t)
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/preview-json-remotes", nil)
	methodResponse := httptest.NewRecorder()
	a.handleJSONGitRemotePreview(methodResponse, request)
	if methodResponse.Code != http.StatusMethodNotAllowed || methodResponse.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET response = status %d, Allow %q", methodResponse.Code, methodResponse.Header().Get("Allow"))
	}
	assertJSONGitRemoteJSONResponse(t, methodResponse)
}

func newJSONGitRemoteTestApp(t *testing.T) *app {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "active-settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	return &app{
		configPath: configPath,
		secrets:    &jsonGitRemoteSecretSpy{},
		client:     &http.Client{Transport: &jsonGitRemoteRequestSpy{}},
		host:       "127.0.0.1:43383",
		csrf:       "json-preview-csrf",
	}
}

func serveJSONGitRemotePreview(t *testing.T, a *app, data []byte, filename, csrf string, extraText, extraFile bool, duplicateCSRF ...bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	if len(duplicateCSRF) > 0 && duplicateCSRF[0] {
		if err := writer.WriteField("csrf", csrf); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("json_remotes", filename)
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
		file, err := writer.CreateFormFile("another_file", "other.json")
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
	return serveJSONGitRemoteRawRequest(t, a, body.Bytes(), writer.FormDataContentType())
}

func serveJSONGitRemoteRawRequest(t *testing.T, a *app, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-json-remotes", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleJSONGitRemotePreview)).ServeHTTP(response, request)
	assertJSONGitRemoteJSONResponse(t, response)
	return response
}

func assertJSONGitRemoteJSONResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if response.Body.Len() > maxJSONGitRemoteResponseBytes {
		t.Fatalf("response size = %d, limit %d", response.Body.Len(), maxJSONGitRemoteResponseBytes)
	}
}

func assertJSONGitRemoteNoSideEffects(t *testing.T, secrets *jsonGitRemoteSecretSpy, requests *jsonGitRemoteRequestSpy) {
	t.Helper()
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("JSON remote preview accessed secrets: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("JSON remote preview sent %d outbound requests", requests.count)
	}
}

func equalJSONGitRemotes(left, right []parsedJSONGitRemote) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type jsonGitRemoteSecretSpy struct {
	loads, saves, deletes int
}

func (s *jsonGitRemoteSecretSpy) Save(string, []byte) error {
	s.saves++
	return nil
}

func (s *jsonGitRemoteSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, errors.New("unexpected credential access")
}

func (s *jsonGitRemoteSecretSpy) Delete(string) error {
	s.deletes++
	return nil
}

type jsonGitRemoteRequestSpy struct {
	count int
}

func (s *jsonGitRemoteRequestSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.count++
	return nil, errors.New("unexpected outbound request")
}
