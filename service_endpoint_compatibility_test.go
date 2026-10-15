package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestGitHubEndpointCompatibility(t *testing.T) {
	for _, test := range []struct{ origin, endpoint string }{
		{"https://github.com", "https://api.github.com/repos/ops/agent"},
		{"https://GitHub.COM:443", "https://api.github.com/repos/ops/agent"},
		{"https://api.github.com", "https://api.github.com/repos/ops/agent"},
		{"https://github.com:0443", "https://api.github.com/repos/ops/agent"},
		{"https://api.github.com:0443", "https://api.github.com/repos/ops/agent"},
		{"https://github.corp.invalid", "https://github.corp.invalid/api/v3/repos/ops/agent"},
		{"https://code.corp.invalid:8443", "https://code.corp.invalid:8443/api/v3/repos/ops/agent"},
		{"https://github.com.corp.invalid", "https://github.com.corp.invalid/api/v3/repos/ops/agent"},
		{"https://github.com:8443", "https://github.com:8443/api/v3/repos/ops/agent"},
	} {
		t.Run(test.origin, func(t *testing.T) {
			registered := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Repository", Origin: test.origin, Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
			calls := 0
			client := &http.Client{Transport: onboardingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				want := test.endpoint
				body := `{"name":"agent","full_name":"ops/agent"}`
				if calls == 2 {
					want += "/pulls/42"
					body = githubPullRequestFixture(42, "ops/agent", "contributor/fork", "synthetic-token")
				}
				if req.Method != http.MethodGet || req.URL.String() != want || req.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Fatalf("unexpected registered request: method=%s URL=%s", req.Method, req.URL.Redacted())
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			result, err := fetchRepository(context.Background(), registered, "synthetic-token", client)
			if err != nil {
				t.Fatal(err)
			}
			webOrigin := test.origin
			if strings.HasPrefix(test.endpoint, "https://api.github.com/") {
				webOrigin = "https://github.com"
			}
			if result.HTMLURL != webOrigin+"/ops/agent" {
				t.Fatalf("repository link = %q", result.HTMLURL)
			}
			if _, err := fetchGitHubPullRequest(context.Background(), registered, "synthetic-token", 42, client); err != nil {
				t.Fatal(err)
			}
			fields := onboardingDraftFields{Origin: test.origin, Repository: registered.Repository}
			if err := checkOnboardingGitHub(context.Background(), client, fields, "synthetic-token"); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("request count = %d, want 3", calls)
			}
		})
	}
}

func TestJenkinsJobNameCompatibility(t *testing.T) {
	for _, job := range []string{"Platform/Release Gate", "개발팀/배포 (QA)", "folder/release+candidate"} {
		t.Run(job, func(t *testing.T) {
			registered := jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "CI", BaseURL: "https://ci.corp.invalid:8443/proxy/jenkins", Username: "build-user", JobPath: job, SecretRef: "cred:0123456789abcdef0123456789abcdef"}
			var escapedParts []string
			for _, part := range strings.Split(job, "/") {
				escapedParts = append(escapedParts, "job", url.PathEscape(part))
			}
			path := "/proxy/jenkins/" + strings.Join(escapedParts, "/")
			candidate, err := parseRegistrationURL(registrationServiceJenkins, "https://ci.corp.invalid:8443"+path+"/")
			if err != nil || candidate.BaseURL != registered.BaseURL || candidate.JobPath != job {
				t.Fatalf("job URL candidate=%+v error=%v", candidate, err)
			}
			decodedURL := "https://ci.corp.invalid:8443/proxy/jenkins"
			for _, part := range strings.Split(job, "/") {
				decodedURL += "/job/" + part
			}
			decodedCandidate, err := parseRegistrationURL(registrationServiceJenkins, decodedURL)
			if err != nil || decodedCandidate.JobPath != job || decodedCandidate.BaseURL != registered.BaseURL {
				t.Fatalf("decoded job URL candidate=%+v error=%v", decodedCandidate, err)
			}
			calls := 0
			client := &http.Client{Transport: onboardingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				user, token, ok := req.BasicAuth()
				if req.Method != http.MethodGet || req.URL.Host != "ci.corp.invalid:8443" || req.URL.EscapedPath() != path+"/api/json" || !ok || user != registered.Username || token != "synthetic-token" {
					t.Fatalf("unexpected registered Jenkins request: method=%s URL=%s", req.Method, req.URL.Redacted())
				}
				body := fmt.Sprintf(`{"name":%q,"fullName":%q,"buildable":true}`, lastJobSegment(job), job)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			if _, err := fetchJenkinsJob(context.Background(), registered, "synthetic-token", client); err != nil {
				t.Fatal(err)
			}
			if err := checkOnboardingJenkins(context.Background(), client, onboardingDraftFields{BaseURL: registered.BaseURL, JobPath: job, Username: registered.Username}, "synthetic-token"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("request count=%d, want 2", calls)
			}
			for _, suffix := range [][]string{{"build"}, {"42", "api", "json"}, {"42", "logText", "progressiveText"}} {
				endpoint, err := jenkinsJobURL(registered, suffix...)
				if err != nil || endpoint.EscapedPath() != path+"/"+strings.Join(suffix, "/") {
					t.Fatalf("Jenkins action path=%v error=%v", endpoint, err)
				}
			}
			alternatePath := strings.ReplaceAll(strings.ReplaceAll(path, "+", "%2B"), "%EB", "%eb")
			if !jenkinsQueueTaskURLMatches(registered, "https://ci.corp.invalid:8443"+alternatePath+"/") {
				t.Fatal("equivalent safe task URL encoding was rejected")
			}
			encodedSeparator := strings.Replace(path, "/jenkins/job/", "/jenkins%2fjob/", 1)
			if jenkinsQueueTaskURLMatches(registered, "https://ci.corp.invalid:8443"+encodedSeparator+"/") {
				t.Fatal("task URL with an encoded path separator was accepted")
			}
			if !jenkinsQueueTaskURLMatches(registered, registered.BaseURL+strings.TrimPrefix(path, "/proxy/jenkins")+"/") || jenkinsQueueTaskURLMatches(registered, "https://other.corp.invalid"+path+"/") {
				t.Fatal("queue task URL did not remain bound to the registered job and host")
			}
		})
	}
}

func TestJenkinsJobCompatibilityRejectsAmbiguousPaths(t *testing.T) {
	for _, job := range []string{"folder/..", "folder/.", "folder//job", "folder/%2e%2e", "folder/a?b", "folder/a#b", "folder/a\\b", "folder/a\nb", "folder/a\tb", "folder/a\u200bb", "folder/\xff"} {
		if validJenkinsJobPath(job) {
			t.Errorf("unsafe job path accepted: %q", job)
		}
	}
}
