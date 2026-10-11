package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseRegistrationURLPortAndLengthBoundaries(t *testing.T) {
	t.Parallel()

	const prefix = "https://dash.invalid/"
	maxLengthURL := prefix + strings.Repeat("a", 2048-len(prefix))
	tests := []struct {
		name    string
		raw     string
		want    registrationURLCandidate
		wantErr bool
	}{
		{
			name: "minimum valid port",
			raw:  "https://dash.invalid:1/proxy",
			want: registrationURLCandidate{BaseURL: "https://dash.invalid:1/proxy", ReviewFields: []string{"base_url"}},
		},
		{
			name: "maximum valid port",
			raw:  "https://dash.invalid:65535/proxy",
			want: registrationURLCandidate{BaseURL: "https://dash.invalid:65535/proxy", ReviewFields: []string{"base_url"}},
		},
		{
			name: "bracketed IPv6 with port",
			raw:  "https://[2001:db8::8]:8443/proxy",
			want: registrationURLCandidate{BaseURL: "https://[2001:db8::8]:8443/proxy", ReviewFields: []string{"base_url"}},
		},
		{
			name: "maximum accepted URL length",
			raw:  maxLengthURL,
			want: registrationURLCandidate{BaseURL: maxLengthURL, ReviewFields: []string{"base_url"}},
		},
		{name: "port zero", raw: "https://dash.invalid:0/proxy", wantErr: true},
		{name: "port above TCP range", raw: "https://dash.invalid:65536/proxy", wantErr: true},
		{name: "nonnumeric port", raw: "https://dash.invalid:https/proxy", wantErr: true},
		{name: "unbracketed IPv6 authority", raw: "https://2001:db8::8/proxy", wantErr: true},
		{name: "URL one byte over limit", raw: maxLengthURL + "a", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRegistrationURL(registrationServiceDashboard, tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseRegistrationURL() error = nil, want rejection")
				}
				if strings.Contains(err.Error(), tt.raw) {
					t.Fatalf("error exposed the supplied URL: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRegistrationURL() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseRegistrationURL() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRegistrationURLRejectsEncodedPathBoundariesForEveryService(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service registrationService
		raw     string
	}{
		{name: "GitHub encoded slash cannot change repository boundaries", service: registrationServiceGitHub, raw: "https://github.invalid/org%2frepo"},
		{name: "Jenkins encoded slash cannot change job boundaries", service: registrationServiceJenkins, raw: "https://ci.invalid/job/Build%2fOther"},
		{name: "Dashboard encoded slash cannot change proxy boundaries", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy%2fadmin"},
		{name: "GitHub double encoded slash is not decoded into a candidate", service: registrationServiceGitHub, raw: "https://github.invalid/org%252frepo"},
		{name: "Jenkins encoded NUL is not accepted in a job name", service: registrationServiceJenkins, raw: "https://ci.invalid/job/Build%00"},
		{name: "Dashboard encoded traversal is not accepted", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy/%2e%2e/admin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseRegistrationURL(tt.service, tt.raw)
			if err == nil {
				t.Fatal("parseRegistrationURL() error = nil, want rejection")
			}
			if strings.Contains(err.Error(), tt.raw) {
				t.Fatalf("error exposed the supplied URL: %v", err)
			}
		})
	}
}

func TestHandleRegistrationURLPreviewPreservesExistingTargetsAndHistory(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "settings.json")
	cfg := serviceBundleConfig()
	githubID := cfg.GitHubTargets[0].ID
	cfg.ConnectionTests = map[string]connectionTest{
		githubID: {Result: "success", CompletedAt: "2026-10-10T12:13:14Z"},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := json.Marshal(before)
	if err != nil {
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
		"service": {string(SetupServiceGitHub)},
		"url":     {"https://candidate.invalid/new-owner/new-repository"},
	}
	response := serveRegistrationURLPreview(handler, a.host, http.MethodPost, "/preview-registration-url", form.Encode(), "application/x-www-form-urlencoded", "http://"+a.host)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}

	after, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterBytes, beforeBytes) {
		t.Fatalf("preview changed saved targets, related settings, or connection history:\nbefore %s\nafter  %s", beforeBytes, afterBytes)
	}
	if secrets.loads+secrets.saves+secrets.deletes != 0 {
		t.Fatalf("preview accessed credential storage: %#v", secrets)
	}
	if requests.count != 0 {
		t.Fatalf("preview contacted an external service %d times", requests.count)
	}
}

func TestHandleRegistrationURLPreviewConcurrentRequestsKeepCandidatesIsolated(t *testing.T) {
	t.Parallel()

	type previewCase struct {
		name    string
		service string
		rawURL  string
		want    registrationURLPreviewCandidateDTO
	}
	tests := []previewCase{
		{
			name:    "GitHub",
			service: string(SetupServiceGitHub),
			rawURL:  "https://github-a.invalid/owner-a/repo-a",
			want:    registrationURLPreviewCandidateDTO{Origin: "https://github-a.invalid", Repository: "owner-a/repo-a", MissingFields: []string{}, ReviewFields: []string{}},
		},
		{
			name:    "Jenkins",
			service: string(SetupServiceJenkins),
			rawURL:  "https://jenkins-b.invalid/context-b/job/team-b/job/build-b",
			want:    registrationURLPreviewCandidateDTO{BaseURL: "https://jenkins-b.invalid/context-b", JobPath: "team-b/build-b", MissingFields: []string{}, ReviewFields: []string{"base_url", "job_path"}},
		},
		{
			name:    "Dashboard",
			service: string(SetupServiceDashboard),
			rawURL:  "https://dashboard-c.invalid/proxy-c/api-c",
			want:    registrationURLPreviewCandidateDTO{BaseURL: "https://dashboard-c.invalid/proxy-c/api-c", MissingFields: []string{}, ReviewFields: []string{"base_url"}},
		},
	}
	a := &app{host: registrationURLUITestHost, csrf: registrationURLUITestCSRF}
	handler := newLocalUIHandler(a)
	type result struct {
		caseName string
		body     string
		status   int
		envelope registrationURLUIPreviewEnvelope
	}
	results := make(chan result, len(tests))
	var wait sync.WaitGroup
	for _, tt := range tests {
		wait.Add(1)
		go func(tt previewCase) {
			defer wait.Done()
			form := url.Values{
				"csrf":    {registrationURLUITestCSRF},
				"service": {tt.service},
				"url":     {tt.rawURL},
			}
			response := serveRegistrationURLPreview(handler, a.host, http.MethodPost, "/preview-registration-url", form.Encode(), "application/x-www-form-urlencoded", "http://"+a.host)
			var envelope registrationURLUIPreviewEnvelope
			_ = json.Unmarshal(response.Body.Bytes(), &envelope)
			results <- result{caseName: tt.name, body: response.Body.String(), status: response.Code, envelope: envelope}
		}(tt)
	}
	wait.Wait()
	close(results)

	wants := make(map[string]registrationURLPreviewCandidateDTO, len(tests))
	for _, tt := range tests {
		wants[tt.name] = tt.want
	}
	for got := range results {
		want := wants[got.caseName]
		if got.status != http.StatusOK {
			t.Errorf("%s preview status = %d, want %d: %s", got.caseName, got.status, http.StatusOK, got.body)
			continue
		}
		if got.envelope.Candidate == nil || !reflect.DeepEqual(*got.envelope.Candidate, want) {
			t.Errorf("%s candidate = %#v, want %#v", got.caseName, got.envelope.Candidate, want)
		}
	}
}
