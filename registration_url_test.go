package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRegistrationURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service registrationService
		raw     string
		want    registrationURLCandidate
	}{
		{
			name:    "GitHub host only uses the explicit service and requests a repository",
			service: registrationServiceGitHub,
			raw:     "https://github.enterprise.invalid/",
			want: registrationURLCandidate{
				Origin:        "https://github.enterprise.invalid",
				MissingFields: []string{"repository"},
			},
		},
		{
			name:    "GitHub repository removes git suffix and trailing slash",
			service: registrationServiceGitHub,
			raw:     "https://GitHub.enterprise.invalid:8443/Platform/Agent-Harness.git/",
			want: registrationURLCandidate{
				Origin:     "https://GitHub.enterprise.invalid:8443",
				Repository: "Platform/Agent-Harness",
			},
		},
		{
			name:    "GitHub decodes safe escaped repository characters",
			service: registrationServiceGitHub,
			raw:     "https://github.enterprise.invalid/%50latform/Agent%2DHarness",
			want: registrationURLCandidate{
				Origin:     "https://github.enterprise.invalid",
				Repository: "Platform/Agent-Harness",
			},
		},
		{
			name:    "Jenkins host only uses the explicit service",
			service: registrationServiceJenkins,
			raw:     "https://ci.enterprise.invalid",
			want: registrationURLCandidate{
				BaseURL:       "https://ci.enterprise.invalid",
				MissingFields: []string{"job_path"},
			},
		},
		{
			name:    "Jenkins infers nested job suffix and flags both fields for review",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/job/Platform/job/Release-Gate/",
			want: registrationURLCandidate{
				BaseURL:      "https://ci.invalid/jenkins",
				JobPath:      "Platform/Release-Gate",
				ReviewFields: []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Jenkins context and nested job split remains a review candidate",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/job/team/job/build",
			want: registrationURLCandidate{
				BaseURL:      "https://ci.invalid/jenkins",
				JobPath:      "team/build",
				ReviewFields: []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Jenkins decodes escaped safe job-name characters",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/job/%52elease-Gate",
			want: registrationURLCandidate{
				BaseURL:      "https://ci.invalid/jenkins",
				JobPath:      "Release-Gate",
				ReviewFields: []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Jenkins preserves a context path when job is missing",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/context/",
			want: registrationURLCandidate{
				BaseURL:       "https://ci.invalid/jenkins/context",
				MissingFields: []string{"job_path"},
			},
		},
		{
			name:    "Jenkins infers one-level job suffix and flags both fields for review",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/job/Build",
			want: registrationURLCandidate{
				BaseURL:      "https://ci.invalid/jenkins",
				JobPath:      "Build",
				ReviewFields: []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Jenkins malformed suffix requires review without guessing",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/job/a/job/b/extra",
			want: registrationURLCandidate{
				BaseURL:       "https://ci.invalid",
				MissingFields: []string{"job_path"},
				ReviewFields:  []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Jenkins incomplete marker requires review without guessing",
			service: registrationServiceJenkins,
			raw:     "https://ci.invalid/jenkins/job",
			want: registrationURLCandidate{
				BaseURL:       "https://ci.invalid",
				MissingFields: []string{"job_path"},
				ReviewFields:  []string{"base_url", "job_path"},
			},
		},
		{
			name:    "Dashboard preserves proxy and version-like path for review",
			service: registrationServiceDashboard,
			raw:     "https://dash.invalid/proxy/web/v1.7.0/",
			want: registrationURLCandidate{
				BaseURL:      "https://dash.invalid/proxy/web/v1.7.0",
				ReviewFields: []string{"base_url"},
			},
		},
		{
			name:    "Dashboard host only remains a base URL and needs review",
			service: registrationServiceDashboard,
			raw:     "https://dash.invalid",
			want: registrationURLCandidate{
				BaseURL:      "https://dash.invalid",
				ReviewFields: []string{"base_url"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRegistrationURL(tt.service, tt.raw)
			if err != nil {
				t.Fatalf("parseRegistrationURL() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseRegistrationURL() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRegistrationURLRejectsUnsafeInputWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	const secret = "registration_url_secret_canary"
	tests := []struct {
		name    string
		service registrationService
		raw     string
	}{
		{name: "userinfo", service: registrationServiceGitHub, raw: "https://user:" + secret + "@github.invalid/org/repo"},
		{name: "HTTP scheme", service: registrationServiceJenkins, raw: "http://ci.invalid/job/Build"},
		{name: "query", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy?token=" + secret},
		{name: "fragment", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy#" + secret},
		{name: "opaque URL", service: registrationServiceGitHub, raw: "https:opaque"},
		{name: "malformed DNS host", service: registrationServiceJenkins, raw: "https://-bad.invalid/job/Build"},
		{name: "malformed port", service: registrationServiceJenkins, raw: "https://ci.invalid:port/job/Build"},
		{name: "empty port", service: registrationServiceDashboard, raw: "https://dash.invalid:/proxy"},
		{name: "GitHub extra path segment", service: registrationServiceGitHub, raw: "https://github.invalid/org/repo/tree/main"},
		{name: "GitHub traversal", service: registrationServiceGitHub, raw: "https://github.invalid/%2e%2e/repo"},
		{name: "encoded slash", service: registrationServiceJenkins, raw: "https://ci.invalid/job/Build%2fOther"},
		{name: "encoded backslash", service: registrationServiceJenkins, raw: "https://ci.invalid/job/Build%5cOther"},
		{name: "encoded traversal", service: registrationServiceJenkins, raw: "https://ci.invalid/ctx/job/%2e%2e"},
		{name: "double encoded traversal", service: registrationServiceJenkins, raw: "https://ci.invalid/ctx/job/%252e%252e"},
		{name: "encoded space outside Jenkins target contract", service: registrationServiceJenkins, raw: "https://ci.invalid/job/Release%20Gate"},
		{name: "malformed escape", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy/%zz"},
		{name: "backslash", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy\\admin"},
		{name: "empty path segment", service: registrationServiceDashboard, raw: "https://dash.invalid/proxy//web"},
		{name: "unsupported service", service: SetupServiceKind("harbor"), raw: "https://registry.invalid/project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseRegistrationURL(tt.service, tt.raw)
			if err == nil {
				t.Fatal("parseRegistrationURL() error = nil, want rejection")
			}
			if strings.Contains(err.Error(), tt.raw) || strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposed registration URL or secret: %v", err)
			}
		})
	}
}
