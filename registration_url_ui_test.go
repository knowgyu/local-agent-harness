package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	registrationURLUITestHost = "127.0.0.1:43288"
	registrationURLUITestCSRF = "registration-url-ui-csrf"
)

type registrationURLUIPreviewEnvelope struct {
	Candidate *registrationURLPreviewCandidateDTO `json:"candidate"`
	Error     string                              `json:"error"`
	ErrorCode string                              `json:"error_code"`
}

func TestHandleRegistrationURLPreviewCandidatesThroughLocalUIHandler(t *testing.T) {
	t.Parallel()

	a := &app{host: registrationURLUITestHost, csrf: registrationURLUITestCSRF}
	handler := newLocalUIHandler(a)
	tests := []struct {
		name    string
		service string
		rawURL  string
		want    registrationURLPreviewCandidateDTO
	}{
		{
			name:    "GitHub repository candidate",
			service: string(SetupServiceGitHub),
			rawURL:  "https://github.enterprise.invalid/Platform/Agent-Harness.git/",
			want: registrationURLPreviewCandidateDTO{
				Origin: "https://github.enterprise.invalid", Repository: "Platform/Agent-Harness",
				MissingFields: []string{}, ReviewFields: []string{},
			},
		},
		{
			name:    "Jenkins candidate flags inferred values for review",
			service: string(SetupServiceJenkins),
			rawURL:  "https://ci.invalid/jenkins/job/Platform/job/Release-Gate/",
			want: registrationURLPreviewCandidateDTO{
				BaseURL: "https://ci.invalid/jenkins", JobPath: "Platform/Release-Gate",
				MissingFields: []string{}, ReviewFields: []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Dashboard candidate preserves path for review",
			service: string(SetupServiceDashboard),
			rawURL:  "https://dash.invalid/proxy/web/v1.7.0/",
			want: registrationURLPreviewCandidateDTO{
				BaseURL:       "https://dash.invalid/proxy/web/v1.7.0",
				MissingFields: []string{}, ReviewFields: []string{"base_url"},
			},
		},
		{
			name:    "GitHub host requests missing repository",
			service: string(SetupServiceGitHub),
			rawURL:  "https://github.enterprise.invalid/",
			want: registrationURLPreviewCandidateDTO{
				Origin: "https://github.enterprise.invalid", MissingFields: []string{"repository"}, ReviewFields: []string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{
				"csrf":    {registrationURLUITestCSRF},
				"service": {tt.service},
				"url":     {tt.rawURL},
			}
			response := serveRegistrationURLPreview(handler, registrationURLUITestHost, http.MethodPost, "/preview-registration-url", form.Encode(), "application/x-www-form-urlencoded", "http://"+registrationURLUITestHost)
			if response.Code != http.StatusOK {
				t.Fatalf("preview status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			var decoded registrationURLUIPreviewEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decode preview response: %v", err)
			}
			if decoded.Candidate == nil || decoded.Error != "" || decoded.ErrorCode != "" {
				t.Fatalf("preview envelope = %#v, want candidate only", decoded)
			}
			if !reflect.DeepEqual(*decoded.Candidate, tt.want) {
				t.Fatalf("candidate = %#v, want %#v", *decoded.Candidate, tt.want)
			}
		})
	}
}

func TestHandleRegistrationURLPreviewRejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	const secretCanary = "registration-url-secret-canary"
	validURL := "https://github.invalid/org/repo"
	validForm := func() url.Values {
		return url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"github"}, "url": {validURL}}
	}
	validMultipart := func(t *testing.T) (string, string) {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for key, value := range validForm() {
			if err := writer.WriteField(key, value[0]); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return body.String(), writer.FormDataContentType()
	}
	mpBody, mpContentType := validMultipart(t)
	var fileBody bytes.Buffer
	fileWriter := multipart.NewWriter(&fileBody)
	for key, value := range validForm() {
		if err := fileWriter.WriteField(key, value[0]); err != nil {
			t.Fatal(err)
		}
	}
	filePart, err := fileWriter.CreateFormFile("upload", "canary.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filePart.Write([]byte(secretCanary)); err != nil {
		t.Fatal(err)
	}
	if err := fileWriter.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		origin      string
		host        string
		addType     bool
		wantStatus  int
		wantCode    string
		wantAllow   string
	}{
		{name: "method", method: http.MethodGet, path: "/preview-registration-url", body: validForm().Encode(), contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusMethodNotAllowed, wantCode: "request_rejected", wantAllow: http.MethodPost},
		{name: "wrong CSRF", method: http.MethodPost, path: "/preview-registration-url", body: strings.Replace(validForm().Encode(), registrationURLUITestCSRF, secretCanary, 1), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusForbidden, wantCode: "request_rejected"},
		{name: "missing CSRF", method: http.MethodPost, path: "/preview-registration-url", body: "service=github&url=" + url.QueryEscape(validURL), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusForbidden, wantCode: "request_rejected"},
		{name: "untrusted origin", method: http.MethodPost, path: "/preview-registration-url", body: validForm().Encode(), contentType: "application/x-www-form-urlencoded", origin: "https://attacker.invalid", wantStatus: http.StatusForbidden},
		{name: "wrong host", method: http.MethodPost, path: "/preview-registration-url", body: validForm().Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, host: "attacker.invalid", wantStatus: http.StatusNotFound},
		{name: "wrong content type", method: http.MethodPost, path: "/preview-registration-url", body: validForm().Encode(), contentType: "application/json", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusForbidden, wantCode: "request_rejected"},
		{name: "multipart rejected", method: http.MethodPost, path: "/preview-registration-url", body: mpBody, contentType: mpContentType, origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "multipart file rejected", method: http.MethodPost, path: "/preview-registration-url", body: fileBody.String(), contentType: fileWriter.FormDataContentType(), origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "duplicate csrf", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF, secretCanary}, "service": {"github"}, "url": {validURL}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "duplicate service", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"github", "jenkins"}, "url": {validURL}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown field", method: http.MethodPost, path: "/preview-registration-url", body: validFormWithExtra(validURL, secretCanary), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "query string rejected", method: http.MethodPost, path: "/preview-registration-url?service=dashboard", body: validForm().Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "query values cannot supply csrf", method: http.MethodPost, path: "/preview-registration-url?csrf=" + registrationURLUITestCSRF, body: "service=github&url=" + url.QueryEscape(validURL), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "duplicate content type", method: http.MethodPost, path: "/preview-registration-url", body: validForm().Encode(), contentType: "application/x-www-form-urlencoded", addType: true, origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unsupported service", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"harbor"}, "url": {validURL}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "HTTP URL", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"github"}, "url": {"http://github.invalid/org/repo?token=" + secretCanary}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_url"},
		{name: "userinfo URL", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"github"}, "url": {"https://user:" + secretCanary + "@github.invalid/org/repo"}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_url"},
		{name: "encoded separator URL", method: http.MethodPost, path: "/preview-registration-url", body: url.Values{"csrf": {registrationURLUITestCSRF}, "service": {"jenkins"}, "url": {"https://ci.invalid/job/Build%2f" + secretCanary}}.Encode(), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusBadRequest, wantCode: "invalid_url"},
		{name: "oversized body", method: http.MethodPost, path: "/preview-registration-url", body: "csrf=" + registrationURLUITestCSRF + "&service=github&url=" + strings.Repeat("x", maxRegistrationURLPreviewRequestBytes), contentType: "application/x-www-form-urlencoded", origin: "http://" + registrationURLUITestHost, wantStatus: http.StatusForbidden, wantCode: "request_rejected"},
	}

	a := &app{host: registrationURLUITestHost, csrf: registrationURLUITestCSRF}
	handler := newLocalUIHandler(a)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, "http://"+registrationURLUITestHost+tt.path, strings.NewReader(tt.body))
			request.Host = registrationURLUITestHost
			if tt.host != "" {
				request.Host = tt.host
			}
			if tt.contentType != "" {
				request.Header.Set("Content-Type", tt.contentType)
			}
			if tt.addType {
				request.Header.Add("Content-Type", tt.contentType)
			}
			if tt.origin != "" {
				request.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.wantAllow != "" && response.Header().Get("Allow") != tt.wantAllow {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), tt.wantAllow)
			}
			if tt.wantCode != "" {
				var decoded registrationURLUIPreviewEnvelope
				if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
					t.Fatalf("decode error response: %v (body %q)", err, response.Body.String())
				}
				if decoded.ErrorCode != tt.wantCode || decoded.Error == "" || decoded.Candidate != nil {
					t.Fatalf("error envelope = %#v, want code %q only", decoded, tt.wantCode)
				}
			}
			if strings.Contains(response.Body.String(), secretCanary) {
				t.Fatalf("error response echoed secret canary: %s", response.Body.String())
			}
		})
	}
}

func TestHandleRegistrationURLPreviewDoesNotReadOrMutateLocalStateOrContactTargets(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "settings.json")
	configBefore := []byte("registration-url-config-canary: do not inspect or modify\n")
	if err := os.WriteFile(configPath, configBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := &registrationURLUIPreviewSecretSpy{}
	requests := &registrationURLUIPreviewRequestSpy{}
	a := &app{
		host:       registrationURLUITestHost,
		csrf:       registrationURLUITestCSRF,
		configPath: configPath,
		secrets:    secrets,
		client:     &http.Client{Transport: requests},
	}
	handler := newLocalUIHandler(a)
	form := url.Values{
		"csrf":    {registrationURLUITestCSRF},
		"service": {"github"},
		"url":     {"https://github.invalid/org/repo"},
	}
	response := serveRegistrationURLPreview(handler, a.host, http.MethodPost, "/preview-registration-url", form.Encode(), "application/x-www-form-urlencoded", "http://"+a.host)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configAfter, configBefore) {
		t.Fatalf("preview mutated local config: got %q", configAfter)
	}
	if secrets.loads+secrets.saves+secrets.deletes != 0 {
		t.Fatalf("preview accessed credential storage: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("preview contacted an external service %d times", requests.count)
	}
	if strings.Contains(response.Body.String(), "registration-url-config-canary") {
		t.Fatalf("preview leaked local config content: %s", response.Body.String())
	}
}

func serveRegistrationURLPreview(handler http.Handler, host, method, path, body, contentType, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	request.Host = host
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validFormWithExtra(rawURL, extra string) string {
	return url.Values{
		"csrf":    {registrationURLUITestCSRF},
		"service": {"github"},
		"url":     {rawURL},
		"extra":   {extra},
	}.Encode()
}

type registrationURLUIPreviewSecretSpy struct {
	loads, saves, deletes int
}

func (s *registrationURLUIPreviewSecretSpy) Save(string, []byte) error {
	s.saves++
	return errors.New("unexpected credential save")
}

func (s *registrationURLUIPreviewSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, errors.New("unexpected credential read")
}

func (s *registrationURLUIPreviewSecretSpy) Delete(string) error {
	s.deletes++
	return errors.New("unexpected credential delete")
}

type registrationURLUIPreviewRequestSpy struct {
	count int
}

func (s *registrationURLUIPreviewRequestSpy) RoundTrip(*http.Request) (*http.Response, error) {
	s.count++
	return nil, errors.New("unexpected outbound request")
}
