package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestLoopbackSSHOperationApproverCompletesSyntheticHTTPApproval(t *testing.T) {
	csrfPattern := regexp.MustCompile(`name="csrf" value="([a-f0-9]{64})"`)
	approver := &loopbackSSHOperationApprover{timeout: time.Second}
	approver.open = func(address string) error {
		host, route, ok := sshApprovalRoute(address)
		if !ok {
			return errors.New("approval URL did not remain loopback")
		}
		response, err := http.Get(address)
		if err != nil {
			return err
		}
		page, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return errors.New("local approval page could not be read")
		}
		match := csrfPattern.FindSubmatch(page)
		if len(match) != 2 {
			return errors.New("local approval page did not contain its CSRF value")
		}
		form := url.Values{"csrf": {string(match[1])}, "decision": {"allow"}}
		request, err := http.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		request.Header.Set("Origin", "http://"+host)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("local approval decision was rejected")
		}
		return nil
	}

	approved, err := approver.Confirm(context.Background(), sshApprovalRequest{
		TargetName: "Synthetic target", Operation: "Read status", Summary: "One safe local test.",
	})
	if err != nil || !approved {
		t.Fatalf("synthetic local approval failed: approved=%v err=%v", approved, err)
	}
}

func TestSSHApprovalPageEscapesReviewTextAndAcceptsOnlyOneLocalDecision(t *testing.T) {
	state := &sshApprovalPageState{
		host: "127.0.0.1:43210", route: "/approve-ssh-operation/route-token", csrf: "csrf-token",
		request: sshApprovalRequest{
			TargetName: `<script>alert("target")</script>`, Operation: "Restart service",
			Summary: "One reviewed operation.", Parameters: []sshApprovalParameter{{Name: "service", Value: "worker"}},
		},
		decision: make(chan bool, 1),
	}

	get := httptest.NewRequest(http.MethodGet, "http://"+state.host+state.route, nil)
	get.Host = state.host
	page := httptest.NewRecorder()
	state.ServeHTTP(page, get)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `<script>alert("target")</script>`) || !strings.Contains(page.Body.String(), "&lt;script&gt;") {
		t.Fatalf("approval page did not safely render reviewed text: status=%d body=%q", page.Code, page.Body.String())
	}

	for _, test := range []struct {
		name        string
		path        string
		host        string
		origin      string
		contentType string
		body        string
		want        int
	}{
		{name: "foreign host", path: state.route, host: "127.0.0.1:43211", origin: "http://" + state.host, contentType: "application/x-www-form-urlencoded", body: "csrf=csrf-token&decision=allow", want: http.StatusNotFound},
		{name: "query string", path: state.route + "?x=1", host: state.host, origin: "http://" + state.host, contentType: "application/x-www-form-urlencoded", body: "csrf=csrf-token&decision=allow", want: http.StatusNotFound},
		{name: "foreign origin", path: state.route, host: state.host, origin: "http://127.0.0.1:43211", contentType: "application/x-www-form-urlencoded", body: "csrf=csrf-token&decision=allow", want: http.StatusForbidden},
		{name: "wrong media type", path: state.route, host: state.host, origin: "http://" + state.host, contentType: "text/plain", body: "csrf=csrf-token&decision=allow", want: http.StatusBadRequest},
		{name: "wrong csrf", path: state.route, host: state.host, origin: "http://" + state.host, contentType: "application/x-www-form-urlencoded", body: "csrf=wrong&decision=allow", want: http.StatusForbidden},
		{name: "invalid decision", path: state.route, host: state.host, origin: "http://" + state.host, contentType: "application/x-www-form-urlencoded", body: "csrf=csrf-token&decision=maybe", want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://"+state.host+test.path, strings.NewReader(test.body))
			request.Host = test.host
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			state.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.want, response.Body.String())
			}
		})
	}
	select {
	case decision := <-state.decision:
		t.Fatalf("rejected request reached approval decision channel: %v", decision)
	default:
	}

	post := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://"+state.host+state.route, strings.NewReader("csrf=csrf-token&decision=allow"))
		request.Host = state.host
		request.Header.Set("Origin", "http://"+state.host)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		state.ServeHTTP(response, request)
		return response
	}
	accepted := post()
	if accepted.Code != http.StatusOK {
		t.Fatalf("valid approval rejected: status=%d body=%q", accepted.Code, accepted.Body.String())
	}
	select {
	case decision := <-state.decision:
		if !decision {
			t.Fatal("valid approval was recorded as denial")
		}
	default:
		t.Fatal("valid approval did not notify the waiting operation")
	}
	if duplicate := post(); duplicate.Code != http.StatusGone {
		t.Fatalf("one-shot decision accepted twice: status=%d body=%q", duplicate.Code, duplicate.Body.String())
	}
}

func TestWaitForSSHOperationApprovalRequiresOpenAndDecisionAndHonorsFailure(t *testing.T) {
	t.Run("decision waits for browser launch", func(t *testing.T) {
		decision := make(chan bool, 1)
		openResult := make(chan error, 1)
		serveResult := make(chan error)
		decision <- true
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result := make(chan struct {
			approved bool
			err      error
		}, 1)
		go func() {
			approved, err := waitForSSHOperationApproval(ctx, time.Second, decision, openResult, serveResult)
			result <- struct {
				approved bool
				err      error
			}{approved, err}
		}()
		select {
		case <-result:
			t.Fatal("approval completed before browser launch succeeded")
		case <-time.After(10 * time.Millisecond):
		}
		openResult <- nil
		select {
		case got := <-result:
			if got.err != nil || !got.approved {
				t.Fatalf("approval result=%v err=%v", got.approved, got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("approval result did not complete after page opened")
		}
	})

	t.Run("open error denies", func(t *testing.T) {
		openResult := make(chan error, 1)
		openResult <- context.Canceled
		approved, err := waitForSSHOperationApproval(context.Background(), time.Second, make(chan bool), openResult, make(chan error))
		if approved || err == nil {
			t.Fatalf("browser open failure was accepted: approved=%v err=%v", approved, err)
		}
	})

	t.Run("context cancellation denies", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		approved, err := waitForSSHOperationApproval(ctx, time.Second, make(chan bool), make(chan error), make(chan error))
		if approved || err == nil {
			t.Fatalf("canceled approval was accepted: approved=%v err=%v", approved, err)
		}
	})

	t.Run("timeout denies", func(t *testing.T) {
		approved, err := waitForSSHOperationApproval(context.Background(), time.Millisecond, make(chan bool), make(chan error), make(chan error))
		if approved || err == nil {
			t.Fatalf("approval timeout was accepted: approved=%v err=%v", approved, err)
		}
	})
}
