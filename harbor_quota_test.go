package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisteredHarborProjectQuotaUsesSavedProjectAndProjectsOnlyQuota(t *testing.T) {
	const secret = "harbor_project_quota_canary_0123456789"
	const targetName = "staging " + secret
	const quotaUsed = int64(37)
	artifactSizeSum := int64(12 + 7)
	var requests int
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/proxy/api/v2.0/projects/platform/summary" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s?%s, want saved project's fixed summary route", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		if user, password, ok := r.BasicAuth(); !ok || user != "robot$staging" || password != secret {
			t.Errorf("Basic auth = (%q, %q, %v)", user, password, ok)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		return harborTestResponse(r, http.StatusOK, `{"project_id":7,"repo_count":2,"quota":{"hard":{"storage":1000,"count":20},"used":{"storage":37,"count":3}},"debug":"`+secret+`"}`), nil
	})}
	a := newHarborProjectQuotaTestApp(t, client, targetName, secret)
	result, err := a.registeredHarborProjectQuota(context.Background(), targetName)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.Target != "staging [REDACTED]" || result.Project != "platform" || result.Quota == nil {
		t.Fatalf("requests=%d result=%#v", requests, result)
	}
	if result.Quota.Hard == nil || (*result.Quota.Hard)["storage"] != 1000 || (*result.Quota.Hard)["count"] != 20 || result.Quota.Used == nil || (*result.Quota.Used)["storage"] != quotaUsed {
		t.Fatalf("quota = %#v", result.Quota)
	}
	if quotaUsed == artifactSizeSum {
		t.Fatal("fixture must distinguish Harbor quota usage from artifact size sums")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "repo_count") || strings.Contains(string(encoded), "debug") {
		t.Fatalf("result exposed an unprojected field or secret: %s", encoded)
	}
	if _, err := a.registeredHarborProjectQuota(context.Background(), "unregistered"); err == nil || requests != 1 {
		t.Fatalf("unregistered target err=%v requests=%d", err, requests)
	}
}

func TestRegisteredHarborProjectQuotaOmitsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "quota absent", body: `{"project_id":9}`},
		{name: "hard absent", body: `{"quota":{"used":{"storage":5}}}`, want: `"used":{"storage":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				return harborTestResponse(r, http.StatusOK, tc.body), nil
			})}
			a := newHarborProjectQuotaTestApp(t, client, "staging", "secret-canary")
			result, err := a.registeredHarborProjectQuota(context.Background(), "staging")
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if result.Quota != nil || strings.Contains(string(encoded), `"quota"`) {
					t.Fatalf("absent quota was reported: %s", encoded)
				}
			} else if result.Quota == nil || result.Quota.Hard != nil || !strings.Contains(string(encoded), tc.want) || strings.Contains(string(encoded), `"hard"`) {
				t.Fatalf("quota fields were defaulted or misprojected: %s", encoded)
			}
		})
	}
}

func TestRegisteredHarborProjectQuotaRejectsErrorsAndBadResponses(t *testing.T) {
	const secret = "harbor_quota_secret_canary"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: secret},
		{name: "quota permission denied", status: http.StatusForbidden, body: secret},
		{name: "redirect", status: http.StatusFound, body: secret},
		{name: "malformed JSON", status: http.StatusOK, body: `{"quota":{"used":` + secret},
		{name: "invalid quota shape", status: http.StatusOK, body: `{"quota":{"hard":"` + secret + `"}}`},
		{name: "oversized response", status: http.StatusOK, body: strings.Repeat("x", maxAPIBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				resp := harborTestResponse(r, tc.status, tc.body)
				if tc.status == http.StatusFound {
					resp.Header.Set("Location", "https://other.example.invalid/quota")
				}
				return resp, nil
			})}
			a := newHarborProjectQuotaTestApp(t, client, "staging", secret)
			result, err := a.registeredHarborProjectQuota(context.Background(), "staging")
			if err == nil || result.Quota != nil {
				t.Fatalf("result=%#v err=%v; denied/invalid response must not become quota data", result, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "other.example.invalid") {
				t.Fatalf("error exposed response data: %v", err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d, want one request (redirects must not be followed)", requests)
			}
		})
	}
}

func newHarborProjectQuotaTestApp(t *testing.T, client *http.Client, targetName, secret string) *app {
	t.Helper()
	a := &app{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		secrets:    memorySecrets{},
		client:     client,
	}
	if _, err := a.saveHarborTarget("", targetName, "https://harbor.example.invalid/proxy", "robot$staging", "platform", "team/service", secret); err != nil {
		t.Fatal(err)
	}
	return a
}
