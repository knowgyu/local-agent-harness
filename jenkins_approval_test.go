package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type jenkinsApprovalRoundTripper func(*http.Request) (*http.Response, error)

func (f jenkinsApprovalRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestJenkinsApprovalPageAllowsOnceAndTriggersOnce(t *testing.T) {
	const host = "127.0.0.1:43127"
	const route = "/approve/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const csrf = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	const canary = "jenkins_api_token_canary_0123456789"
	decisions := make(chan bool, 1)
	state := &jenkinsApprovalPageState{host: host, route: route, target: "QA pipeline", environment: "qa-green", baseURL: "https://jenkins.example.invalid/proxy/ci", jobPath: "folder/smoke", csrf: csrf, decision: decisions}
	handler := (&app{host: host}).securityHeaders(state)

	get := httptest.NewRequest(http.MethodGet, "http://"+host+route, nil)
	get.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, get)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "QA pipeline") || !strings.Contains(page.Body.String(), "qa-green") || !strings.Contains(page.Body.String(), "https://jenkins.example.invalid/proxy/ci") || !strings.Contains(page.Body.String(), "folder/smoke") || !strings.Contains(page.Body.String(), "not preapproved as non-production") {
		t.Fatalf("approval page status=%d body=%q", page.Code, page.Body.String())
	}
	for _, want := range []string{"button:focus-visible", "outline:3px", "min-height:44px", "Approve this trigger", "Deny", "Cancel"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("approval page missing accessible control detail %q", want)
		}
	}
	if strings.Contains(page.Body.String(), canary) {
		t.Fatal("approval page contains the Jenkins credential canary")
	}

	postApproval := func() *httptest.ResponseRecorder {
		form := url.Values{"csrf": {csrf}, "decision": {"allow"}}
		req := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+host)
		req.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if res := postApproval(); res.Code != http.StatusOK {
		t.Fatalf("approval POST status=%d body=%q", res.Code, res.Body.String())
	}
	select {
	case approved := <-decisions:
		if !approved {
			t.Fatal("valid approval returned deny")
		}
	default:
		t.Fatal("valid approval did not produce a decision")
	}
	if res := postApproval(); res.Code != http.StatusGone {
		t.Fatalf("replayed approval status=%d, want 410", res.Code)
	}
	select {
	case <-decisions:
		t.Fatal("replayed approval produced another decision")
	default:
	}

	target := jenkinsTarget{
		ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA pipeline",
		BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user",
		JobPath: "folder/smoke", Environment: "qa", SecretRef: "cred:1123456789abcdef0123456789abcdef",
	}
	var posts atomic.Int32
	client := &http.Client{Transport: jenkinsApprovalRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return approvalHTTPResponse(r, http.StatusCreated, ""), nil
		}
		return approvalHTTPResponse(r, http.StatusOK, `{"name":"smoke","fullName":"folder/smoke","buildable":true}`), nil
	})}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{target}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, secrets: memorySecrets{target.SecretRef: []byte(canary)}, client: client}
	result, err := a.runJenkinsJobAfterApproval(context.Background(), target.Name, target.ID, jenkinsTriggerApprovalScope(target))
	if err != nil || posts.Load() != 1 {
		t.Fatalf("confirmed trigger result=%+v err=%v POSTs=%d", result, err, posts.Load())
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), canary) {
		t.Fatal("MCP result contains the Jenkins credential canary")
	}
}

func TestJenkinsApprovalHandlerRejectsDenyOriginAndCSRF(t *testing.T) {
	const host = "127.0.0.1:43128"
	const route = "/approve/1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const csrf = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	for _, test := range []struct {
		name       string
		origin     string
		csrf       string
		decision   string
		wantStatus int
		wantResult *bool
	}{
		{name: "missing origin", csrf: csrf, decision: "allow", wantStatus: http.StatusForbidden},
		{name: "cross origin", origin: "http://evil.example", csrf: csrf, decision: "allow", wantStatus: http.StatusForbidden},
		{name: "invalid csrf", origin: "http://" + host, csrf: "wrong", decision: "allow", wantStatus: http.StatusForbidden},
		{name: "deny", origin: "http://" + host, csrf: csrf, decision: "deny", wantStatus: http.StatusOK, wantResult: boolPointer(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			decisions := make(chan bool, 1)
			state := &jenkinsApprovalPageState{host: host, route: route, target: "QA", environment: "qa", baseURL: "https://jenkins.example.invalid/proxy", jobPath: "folder/qa", csrf: csrf, decision: decisions}
			handler := (&app{host: host}).securityHeaders(state)
			form := url.Values{"csrf": {test.csrf}, "decision": {test.decision}}
			req := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != test.wantStatus {
				t.Fatalf("status=%d body=%q, want %d", res.Code, res.Body.String(), test.wantStatus)
			}
			select {
			case result := <-decisions:
				if test.wantResult == nil || result != *test.wantResult {
					t.Fatalf("unexpected decision %v", result)
				}
			default:
				if test.wantResult != nil {
					t.Fatal("valid deny did not produce a decision")
				}
			}
		})
	}
}

func TestJenkinsApprovalWaitFailsClosed(t *testing.T) {
	target := jenkinsTarget{
		ID: "jenkins:4123456789abcdef0123456789abcdef", Name: "QA",
		BaseURL: "https://jenkins.example.invalid", Username: "build-user",
		JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:4123456789abcdef0123456789abcdef",
	}
	for _, test := range []struct {
		name      string
		decision  *bool
		openError error
		timeout   bool
	}{
		{name: "deny", decision: boolPointer(false)},
		{name: "launch failure after early approval", decision: boolPointer(true), openError: errors.New("launcher failure")},
		{name: "timeout", timeout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			decisions := make(chan bool, 1)
			if test.decision != nil {
				decisions <- *test.decision
			}
			openResult := make(chan error, 1)
			openResult <- test.openError
			ctx := context.Background()
			if test.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			err := waitForJenkinsApproval(ctx, decisions, openResult, nil)
			var posts atomic.Int32
			client := &http.Client{Transport: jenkinsApprovalRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					return approvalHTTPResponse(r, http.StatusCreated, ""), nil
				}
				return approvalHTTPResponse(r, http.StatusOK, `{"name":"qa","fullName":"folder/qa","buildable":true}`), nil
			})}
			if err == nil {
				_, err = runJenkinsJobAfterConfirmation(context.Background(), target, "test-token", client, func() error { return nil })
			}
			if err == nil || strings.Contains(err.Error(), "launcher failure") || posts.Load() != 0 {
				t.Fatalf("failed confirmation sent %d POSTs", posts.Load())
			}
		})
	}
}

func TestJenkinsApprovalTargetChangeStopsBeforeTriggerPost(t *testing.T) {
	const ref = "cred:2123456789abcdef0123456789abcdef"
	const token = "jenkins_api_token_canary_changed_scope_123456"
	target := jenkinsTarget{
		ID: "jenkins:2123456789abcdef0123456789abcdef", Name: "QA",
		BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user",
		JobPath: "folder/smoke", Environment: "qa", SecretRef: ref,
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{target}}); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	client := &http.Client{Transport: jenkinsApprovalRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return approvalHTTPResponse(r, http.StatusCreated, ""), nil
		}
		changed := target
		changed.JobPath = "folder/other"
		if err := writeConfig(path, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{changed}}); err != nil {
			return nil, err
		}
		return approvalHTTPResponse(r, http.StatusOK, `{"name":"smoke","fullName":"folder/smoke","buildable":true}`), nil
	})}
	a := &app{configPath: path, secrets: memorySecrets{ref: []byte(token)}, client: client}
	_, err := a.runJenkinsJobAfterApproval(context.Background(), target.Name, target.ID, jenkinsTriggerApprovalScope(target))
	if err == nil || !strings.Contains(err.Error(), "changed after confirmation") || posts.Load() != 0 {
		t.Fatalf("changed target err=%v POSTs=%d, want rejection and zero POSTs", err, posts.Load())
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("target-change error contains credential canary")
	}
}

func TestJenkinsPreapprovedTargetUsesNoBrowserPrompt(t *testing.T) {
	const ref = "cred:3123456789abcdef0123456789abcdef"
	const token = "jenkins_api_token_canary_fast_path_123456"
	target := approvedJenkinsTarget(jenkinsTarget{
		ID: "jenkins:3123456789abcdef0123456789abcdef", Name: "QA",
		BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user",
		JobPath: "folder/qa", Environment: "qa", SecretRef: ref,
	})
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{target}}); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	client := &http.Client{Transport: jenkinsApprovalRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return approvalHTTPResponse(r, http.StatusCreated, ""), nil
		}
		return approvalHTTPResponse(r, http.StatusOK, `{"name":"qa","fullName":"folder/qa","buildable":true}`), nil
	})}
	a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: client}
	a.openApprovalBrowser = func(string) error { return errors.New("preapproved path unexpectedly opened browser") }
	if _, err := a.runRegisteredJenkinsJob(context.Background(), target.Name); err != nil {
		t.Fatalf("preapproved trigger failed: %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("preapproved trigger POSTs=%d, want 1", posts.Load())
	}
}

func TestJenkinsRegisteredUnapprovedTriggerUsesConfirmationResult(t *testing.T) {
	const ref = "cred:5123456789abcdef0123456789abcdef"
	const token = "jenkins_api_token_canary_registered_flow_123456"
	for _, test := range []struct {
		name      string
		approval  error
		wantPosts int32
		wantErr   bool
	}{
		{name: "allow", wantPosts: 1},
		{name: "deny", approval: errors.New("user denied"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := jenkinsTarget{
				ID: "jenkins:5123456789abcdef0123456789abcdef", Name: "QA",
				BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user",
				JobPath: "folder/qa", Environment: "qa", SecretRef: ref,
			}
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{target}}); err != nil {
				t.Fatal(err)
			}
			var posts, approvals atomic.Int32
			client := &http.Client{Transport: jenkinsApprovalRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					return approvalHTTPResponse(r, http.StatusCreated, ""), nil
				}
				return approvalHTTPResponse(r, http.StatusOK, `{"name":"qa","fullName":"folder/qa","buildable":true}`), nil
			})}
			a := &app{configPath: configPath, secrets: memorySecrets{ref: []byte(token)}, client: client}
			a.jenkinsApprovalResult = func(context.Context) error {
				approvals.Add(1)
				return test.approval
			}
			result, err := a.runRegisteredJenkinsJob(context.Background(), target.Name)
			if (err != nil) != test.wantErr || approvals.Load() != 1 || posts.Load() != test.wantPosts {
				t.Fatalf("result=%+v err=%v approvals=%d POSTs=%d", result, err, approvals.Load(), posts.Load())
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Fatal("approval error contains the Jenkins credential canary")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), token) {
				t.Fatal("MCP result contains the Jenkins credential canary")
			}
		})
	}
}

func approvalHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func boolPointer(value bool) *bool { return &value }
