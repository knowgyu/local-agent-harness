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

func TestParseSelectedSSHConfigAcceptsLiteralAliasesAndOpaqueDirectives(t *testing.T) {
	config := "# ignored comment-canary\r\n" +
		"HOST edge-1 edge_2 # aliases only\r\n" +
		"  HostName backend-canary.example.invalid\r\n" +
		"  User user-canary\r\n" +
		"  IdentityFile C:\\private\\identity-canary\r\n" +
		"  ProxyCommand ssh proxy-command-canary\r\n" +
		"  ProxyJump jump-canary\r\n" +
		"Host backup.node-3\r\n" +
		"  HostName 192.0.2.11\r\n"

	aliases, err := parseSelectedSSHConfig([]byte(config))
	if err != nil {
		t.Fatalf("parseSelectedSSHConfig() error = %v", err)
	}
	want := []string{"edge-1", "edge_2", "backup.node-3"}
	if len(aliases) != len(want) {
		t.Fatalf("aliases = %#v, want %#v", aliases, want)
	}
	for index := range want {
		if aliases[index] != want[index] {
			t.Errorf("aliases[%d] = %q, want %q", index, aliases[index], want[index])
		}
	}
}

func TestSSHConfigPreviewReturnsAliasesOnlyWithoutSideEffects(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	settingsCanary := []byte("unchanged settings-canary\n")
	if err := os.WriteFile(configPath, settingsCanary, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := &sshImportSecretSpy{}
	requests := &sshImportRequestSpy{}
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		client:     &http.Client{Transport: requests},
		host:       "127.0.0.1:43260",
		csrf:       "ssh-preview-csrf-canary",
	}
	config := []byte("" +
		"Host prod-gateway staging.gateway\r\n" +
		" HostName backend-host-canary.example.invalid\r\n" +
		" User user-canary\r\n" +
		" IdentityFile /private/identity-file-canary\r\n" +
		" ProxyCommand echo proxy-command-canary\r\n" +
		" ProxyJump jump-canary\r\n" +
		"Host 192-0-2-20\r\n")
	response := serveSSHConfigPreview(t, a, config, "config", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	assertSSHJSONResponse(t, response)
	var preview sshConfigPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Source != sshConfigPreviewSource || preview.Error != "" || len(preview.Aliases) != 3 {
		t.Fatalf("preview = %#v", preview)
	}
	if got := []string{preview.Aliases[0].Name, preview.Aliases[1].Name, preview.Aliases[2].Name}; strings.Join(got, ",") != "prod-gateway,staging.gateway,192-0-2-20" {
		t.Fatalf("preview aliases = %#v", got)
	}
	for _, privateValue := range []string{
		"backend-host-canary.example.invalid", "user-canary", "identity-file-canary",
		"proxy-command-canary", "jump-canary", "settings-canary", a.csrf,
	} {
		if strings.Contains(response.Body.String(), privateValue) {
			t.Errorf("preview exposed %q: %s", privateValue, response.Body)
		}
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("preview accessed secret store: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("preview contacted a client %d times", requests.count)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(settingsCanary, after) {
		t.Fatalf("preview modified settings: err=%v, contents=%q", err, after)
	}
}

func TestSSHConfigPreviewReturnsEmptyAliasArray(t *testing.T) {
	a := newSSHImportTestApp(t)
	response := serveSSHConfigPreview(t, a, []byte("# comments only\n"), "config", a.csrf, false, false)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body)
	}
	assertSSHJSONResponse(t, response)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["source"]; !ok {
		t.Fatal("successful preview omitted source")
	}
	aliases, ok := fields["aliases"]
	if !ok || string(aliases) != "[]" {
		t.Fatalf("empty aliases field = %s, want []", aliases)
	}
}

func TestParseSelectedSSHConfigRejectsUnsupportedDirectivesAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "Host without aliases", config: "Host\n"},
		{name: "empty Host equals", config: "Host=\n"},
		{name: "wildcard alias", config: "Host *.example.invalid\n"},
		{name: "question mark pattern", config: "Host gateway?\n"},
		{name: "negated alias", config: "Host !gateway\n"},
		{name: "invalid alias punctuation", config: "Host gateway/path\n"},
		{name: "duplicate alias case insensitive", config: "Host Gateway\nHost gateway\n"},
		{name: "Include directive", config: "Include /private/include-canary\n"},
		{name: "Include key value", config: "include=/private/include-canary\n"},
		{name: "Include spaced equals", config: "Include = /private/include-canary\n"},
		{name: "Match directive", config: "Match host *.example.invalid\n"},
		{name: "Match key value", config: "MATCH=host\n"},
		{name: "global HostName", config: "HostName global-canary.example.invalid\n"},
		{name: "HostName before first Host", config: "User opaque\nHostName global-canary.example.invalid\nHost edge\n"},
		{name: "duplicate HostName", config: "Host edge\nHostName one.example.invalid\nHostName two.example.invalid\n"},
		{name: "duplicate HostName key value", config: "Host edge\nHostName=one.example.invalid\nHostName=one.example.invalid\n"},
		{name: "HostName pattern", config: "Host edge\nHostName *.example.invalid\n"},
		{name: "HostName token expansion", config: "Host edge\nHostName %h\n"},
		{name: "HostName userinfo", config: "Host edge\nHostName user@host.example.invalid\n"},
		{name: "HostName multiple values", config: "Host edge\nHostName one.example.invalid two.example.invalid\n"},
		{name: "continued line", config: "Host edge \\\n other\n"},
		{name: "credential-like alias rejected by output filter", config: "Host github_pat_abcdefghijklmnopqrstuvwxyz123456\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSelectedSSHConfig([]byte(test.config)); err == nil {
				t.Fatal("unsupported SSH config was accepted")
			} else if strings.Contains(err.Error(), "canary") {
				t.Fatalf("parser error exposed input: %v", err)
			}
		})
	}
}

func TestParseSelectedSSHConfigRejectsInvalidEncodingControlsAndBOM(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "invalid UTF-8", data: []byte{0xff, 0xfe}},
		{name: "NUL", data: []byte("Host edge\x00\n")},
		{name: "C0 control", data: []byte("Host edge\x01\n")},
		{name: "DEL control", data: []byte("Host edge\x7f\n")},
		{name: "C1 control", data: []byte("Host edge\u0085\n")},
		{name: "bare CR", data: []byte("Host edge\rHost next\n")},
		{name: "leading BOM", data: []byte("\uFEFFHost edge\n")},
		{name: "embedded BOM", data: []byte("Host edge\n# \uFEFF\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseSelectedSSHConfig(test.data); err == nil {
				t.Fatal("invalid encoding or control character was accepted")
			}
		})
	}
	if _, err := parseSelectedSSHConfig(nil); err == nil {
		t.Fatal("empty config was accepted")
	}
}

func TestParseSelectedSSHConfigEnforcesLineAliasAndFileLimits(t *testing.T) {
	validLines := []byte(strings.Repeat("# line\n", maxSSHConfigLines))
	if _, err := parseSelectedSSHConfig(validLines); err != nil {
		t.Fatalf("%d physical lines rejected: %v", maxSSHConfigLines, err)
	}
	if _, err := parseSelectedSSHConfig([]byte(strings.Repeat("# line\n", maxSSHConfigLines+1))); err == nil {
		t.Fatal("config above line limit was accepted")
	}

	aliases := make([]string, 0, maxSSHConfigAliases)
	for index := 0; index < maxSSHConfigAliases; index++ {
		aliases = append(aliases, "host-"+strconv.Itoa(index))
	}
	parsed, err := parseSelectedSSHConfig([]byte("Host " + strings.Join(aliases, " ") + "\n"))
	if err != nil || len(parsed) != maxSSHConfigAliases {
		t.Fatalf("exact alias limit returned %d aliases, err=%v", len(parsed), err)
	}
	tooManyAliases := append(append([]string(nil), aliases...), "last-host")
	if _, err := parseSelectedSSHConfig([]byte("Host " + strings.Join(tooManyAliases, " ") + "\n")); err == nil {
		t.Fatal("config above alias limit was accepted")
	}

	if _, err := parseSelectedSSHConfig(bytes.Repeat([]byte("#"), maxSSHConfigFileBytes)); err != nil {
		t.Fatalf("file at exact size limit rejected: %v", err)
	}
	if _, err := parseSelectedSSHConfig(bytes.Repeat([]byte("#"), maxSSHConfigFileBytes+1)); err == nil {
		t.Fatal("file above size limit was accepted")
	}
}

func TestSSHConfigPreviewRejectsWrongFilenameExtraPartsAndBadCSRF(t *testing.T) {
	for _, test := range []struct {
		name       string
		filename   string
		config     []byte
		csrf       string
		extraText  bool
		extraFile  bool
		wantStatus int
	}{
		{name: "wrong case filename", filename: "Config", config: []byte("Host edge\n"), wantStatus: http.StatusBadRequest},
		{name: "filename with path", filename: "folder/config", config: []byte("Host edge\n"), wantStatus: http.StatusBadRequest},
		{name: "empty filename", filename: "", config: []byte("Host edge\n"), wantStatus: http.StatusBadRequest},
		{name: "empty config", filename: "config", config: nil, wantStatus: http.StatusBadRequest},
		{name: "wrong CSRF", filename: "config", config: []byte("Host edge\n"), csrf: "wrong-csrf-canary", wantStatus: http.StatusForbidden},
		{name: "extra text field", filename: "config", config: []byte("Host edge\n"), extraText: true, wantStatus: http.StatusBadRequest},
		{name: "extra file field", filename: "config", config: []byte("Host edge\n"), extraFile: true, wantStatus: http.StatusBadRequest},
		{name: "unsafe directives do not echo", filename: "config", config: []byte("Host edge\nInclude /private/path-canary\n"), wantStatus: http.StatusBadRequest},
		{name: "oversized file", filename: "config", config: bytes.Repeat([]byte("#"), maxSSHConfigFileBytes+1), wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newSSHImportTestApp(t)
			csrf := test.csrf
			if csrf == "" {
				csrf = a.csrf
			}
			response := serveSSHConfigPreview(t, a, test.config, test.filename, csrf, test.extraText, test.extraFile)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body)
			}
			assertSSHJSONResponse(t, response)
			for _, privateValue := range []string{"wrong-csrf-canary", "extra-field-canary", "extra-file-canary", "path-canary"} {
				if strings.Contains(response.Body.String(), privateValue) {
					t.Fatalf("error response echoed %q: %s", privateValue, response.Body)
				}
			}
			if a.secrets.(*sshImportSecretSpy).loads != 0 || a.secrets.(*sshImportSecretSpy).saves != 0 || a.secrets.(*sshImportSecretSpy).deletes != 0 {
				t.Fatalf("invalid preview accessed secret store: %#v", a.secrets)
			}
			if a.client.Transport.(*sshImportRequestSpy).count != 0 {
				t.Fatalf("invalid preview contacted a client %d times", a.client.Transport.(*sshImportRequestSpy).count)
			}
		})
	}
}

func TestSSHConfigPreviewRejectsDuplicateContentDisposition(t *testing.T) {
	a := newSSHImportTestApp(t)
	boundary := "ssh-preview-boundary-canary"
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"csrf\"\r\n\r\n" + a.csrf + "\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"ssh_config\"; filename=\"config\"\r\n" +
		"Content-Disposition: form-data; name=\"ssh_config\"; filename=\"other\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\nHost edge\n\r\n" +
		"--" + boundary + "--\r\n"
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-ssh-config", strings.NewReader(body))
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleSSHConfigPreview)).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate Content-Disposition status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body)
	}
	assertSSHJSONResponse(t, response)
	if strings.Contains(response.Body.String(), "edge") || strings.Contains(response.Body.String(), "other") {
		t.Fatalf("duplicate header response exposed request values: %s", response.Body)
	}
	if a.client.Transport.(*sshImportRequestSpy).count != 0 {
		t.Fatalf("invalid preview contacted a client %d times", a.client.Transport.(*sshImportRequestSpy).count)
	}
}

func TestSSHConfigPreviewRejectsRequestAboveBodyLimitWithJSON(t *testing.T) {
	a := newSSHImportTestApp(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", a.csrf); err != nil {
		t.Fatal(err)
	}
	padding, err := writer.CreateFormField("padding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := padding.Write(bytes.Repeat([]byte("p"), maxSSHConfigRequestBytes)); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("ssh_config", "config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "Host edge\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-ssh-config", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleSSHConfigPreview)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("oversized request status = %d, body=%s", response.Code, response.Body)
	}
	assertSSHJSONResponse(t, response)
	if response.Body.Len() > maxSSHConfigResponseBytes {
		t.Fatalf("oversized request response = %d bytes", response.Body.Len())
	}
}

func TestSSHConfigPreviewResponseLimitIncludesTrailingNewline(t *testing.T) {
	aliases := make([]sshConfigPreviewAlias, 1024)
	for index := range aliases {
		aliases[index] = sshConfigPreviewAlias{Name: strings.Repeat("response-canary", 8)}
	}
	response := httptest.NewRecorder()
	writeSSHConfigJSON(response, http.StatusOK, sshConfigPreview{Source: sshConfigPreviewSource, Aliases: aliases})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized response status = %d, body=%s", response.Code, response.Body)
	}
	assertSSHJSONResponse(t, response)
	if response.Body.Len() > maxSSHConfigResponseBytes || !bytes.HasSuffix(response.Body.Bytes(), []byte("\n")) {
		t.Fatalf("bounded response has %d bytes or no trailing newline", response.Body.Len())
	}
	if strings.Contains(response.Body.String(), "response-canary") {
		t.Fatalf("oversized response fallback exposed preview values: %s", response.Body)
	}
}

func TestSSHConfigPreviewGetReturnsJSONMethodError(t *testing.T) {
	a := newSSHImportTestApp(t)
	request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/preview-ssh-config", nil)
	response := httptest.NewRecorder()
	a.handleSSHConfigPreview(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method response = status %d, Allow %q", response.Code, response.Header().Get("Allow"))
	}
	assertSSHJSONResponse(t, response)
}

type sshImportSecretSpy struct {
	loads, saves, deletes int
}

func (s *sshImportSecretSpy) Save(string, []byte) error {
	s.saves++
	return nil
}

func (s *sshImportSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, errors.New("unexpected secret access")
}

func (s *sshImportSecretSpy) Delete(string) error {
	s.deletes++
	return nil
}

type sshImportRequestSpy struct{ count int }

func (s *sshImportRequestSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.count++
	return nil, errors.New("unexpected outbound request")
}

func newSSHImportTestApp(t *testing.T) *app {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(configPath, []byte("settings-canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &app{
		configPath: configPath,
		secrets:    &sshImportSecretSpy{},
		client:     &http.Client{Transport: &sshImportRequestSpy{}},
		host:       "127.0.0.1:43261",
		csrf:       "expected-ssh-csrf",
	}
}

func serveSSHConfigPreview(t *testing.T, a *app, config []byte, filename, csrf string, extraText, extraFile bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("ssh_config", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(config); err != nil {
		t.Fatal(err)
	}
	if extraText {
		if err := writer.WriteField("unexpected", "extra-field-canary"); err != nil {
			t.Fatal(err)
		}
	}
	if extraFile {
		file, err := writer.CreateFormFile("another_file", "notes")
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
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/preview-ssh-config", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "http://"+a.host)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handleSSHConfigPreview)).ServeHTTP(response, request)
	return response
}

func assertSSHJSONResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", contentType)
	}
	if response.Body.Len() > maxSSHConfigResponseBytes || !bytes.HasSuffix(response.Body.Bytes(), []byte("\n")) {
		t.Fatalf("response is %d bytes or lacks final newline", response.Body.Len())
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, response.Body)
	}
}
