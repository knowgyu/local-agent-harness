package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var pythonTaskApprovalCSRFPattern = regexp.MustCompile(`name="csrf" value="([a-f0-9]{64})"`)

func TestPythonTaskApprovalAllowsOnlyAfterOneTimeLocalConfirmation(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "success.py")
	if err := os.WriteFile(interpreter, []byte("synthetic interpreter placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := pythonTask{
		ID: "python:" + strings.Repeat("a", 32), Name: "Synthetic local check",
		InterpreterPath: interpreter, ScriptPath: script,
	}
	task = pinPythonTaskForTest(t, task)
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	opened := make(chan string, 1)
	approve := make(chan struct{})
	var launched atomic.Int32
	a := &app{
		configPath: configPath,
		openApprovalBrowser: func(address string) error {
			opened <- address
			<-approve
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script, task.InterpreterSHA256, task.ScriptSHA256)
		},
		pythonTaskCommand: func(ctx context.Context, executable string, args ...string) *exec.Cmd {
			launched.Add(1)
			if executable != interpreter || len(args) != 3 || args[0] != "-I" || args[1] != "-B" || args[2] != script {
				t.Errorf("launched command = %q %#v, want saved executable and fixed arguments", executable, args)
			}
			return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", script)
		},
	}

	type execution struct {
		result pythonTaskResult
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
		done <- execution{result: result, err: err}
	}()
	address := <-opened
	if launched.Load() != 0 {
		t.Fatal("process launched before the local approval decision")
	}
	close(approve)
	finished := <-done
	if finished.err != nil || finished.result.Status != "completed" || launched.Load() != 1 {
		t.Fatalf("approved execution result=%#v err=%v launches=%d", finished.result, finished.err, launched.Load())
	}
	if host, route, ok := pythonTaskApprovalRoute(address); !ok || host == "" || route == "" {
		t.Fatalf("approval browser address is not a loopback route: %q", address)
	}
}

func TestPythonTaskApprovalDenyPrecedesSecretLookupAndExecution(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for _, path := range []string{interpreter, script} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pythonTask{
		ID: "python:" + strings.Repeat("b", 32), Name: "Missing files task",
		InterpreterPath: interpreter, ScriptPath: script,
		SecretEnvName: "LAH_DENIED_SECRET", SecretRef: "cred:" + strings.Repeat("b", 32),
	}
	task = pinPythonTaskForTest(t, task)
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	var launched atomic.Int32
	secrets := newPythonTaskSecretStoreFake()
	secrets.values[task.SecretRef] = []byte("must-not-load-on-denial")
	a := &app{
		configPath: configPath, secrets: secrets,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "deny", nil, task.InterpreterPath, task.ScriptPath)
		},
		pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
			launched.Add(1)
			return nil
		},
	}
	_, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	if err == nil || !strings.Contains(err.Error(), "denied or canceled") {
		t.Fatalf("denied result error = %v, want denial before missing-file check", err)
	}
	if launched.Load() != 0 {
		t.Fatalf("denied execution launched %d processes", launched.Load())
	}
	if len(secrets.loads) != 0 {
		t.Fatalf("denied execution loaded a task secret: %#v", secrets.loads)
	}
}

func TestPythonTaskApprovalFailsClosedOnOpenFailureAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name       string
		open       func(string) error
		want       string
		ctxTimeout time.Duration
	}{
		{
			name: "browser open failure",
			open: func(string) error { return errors.New("synthetic browser failure") },
			want: "Could not open the local task approval page",
		},
		{
			name:       "caller cancellation timeout",
			open:       func(string) error { return nil },
			want:       "expired or was canceled",
			ctxTimeout: 20 * time.Millisecond,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			interpreter := filepath.Join(root, "python.exe")
			script := filepath.Join(root, "task.py")
			for _, path := range []string{interpreter, script} {
				if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			task := pythonTask{
				ID: "python:" + strings.Repeat("c", 32), Name: "Approval failure task",
				InterpreterPath: interpreter, ScriptPath: script,
			}
			task = pinPythonTaskForTest(t, task)
			configPath := filepath.Join(root, "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
				t.Fatal(err)
			}
			var launched atomic.Int32
			a := &app{
				configPath:          configPath,
				openApprovalBrowser: test.open,
				pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
					launched.Add(1)
					return nil
				},
			}
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.ctxTimeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, test.ctxTimeout)
				defer cancel()
			}
			_, err := a.runRegisteredPythonTask(ctx, pythonTaskInput{Task: task.Name})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("execution error = %v, want substring %q", err, test.want)
			}
			if launched.Load() != 0 {
				t.Fatalf("failed approval launched %d processes", launched.Load())
			}
		})
	}
}

func TestWaitForPythonTaskApprovalRejectsExpiredApprovalWithReadyChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	decision := make(chan bool, 1)
	decision <- true
	openResult := make(chan error, 1)
	openResult <- nil
	serveResult := make(chan error, 1)
	serveResult <- nil
	cancel()

	err := waitForPythonTaskApproval(ctx, decision, openResult, serveResult)
	if err == nil || !strings.Contains(err.Error(), "expired or was canceled") {
		t.Fatalf("expired approval error = %v", err)
	}
}

func TestPythonTaskApprovalLimitsPendingBrowserPages(t *testing.T) {
	started := make(chan string, cap(pythonTaskApprovalSlots)+1)
	finishOpen := make(chan struct{})
	defer close(finishOpen)
	a := &app{openApprovalBrowser: func(address string) error {
		started <- address
		<-finishOpen
		return nil
	}}
	type approvalResult struct{ err error }
	start := func() (context.CancelFunc, <-chan approvalResult) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan approvalResult, 1)
		go func() { done <- approvalResult{err: a.confirmPythonTaskRun(ctx, "Synthetic task")} }()
		return cancel, done
	}
	cancelFirst, doneFirst := start()
	if address := <-started; address == "" {
		t.Fatal("first approval page did not open")
	}
	cancelSecond, doneSecond := start()
	if address := <-started; address == "" {
		t.Fatal("second approval page did not open")
	}

	ctx, cancelThird := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelThird()
	err := a.confirmPythonTaskRun(ctx, "Synthetic task")
	if err == nil || !strings.Contains(err.Error(), "expired or was canceled") {
		t.Fatalf("third pending approval error = %v", err)
	}
	select {
	case address := <-started:
		t.Fatalf("approval slot limit opened an extra browser page: %q", address)
	default:
	}

	cancelFirst()
	cancelSecond()
	if result := <-doneFirst; result.err == nil {
		t.Fatal("first canceled approval unexpectedly succeeded")
	}
	if result := <-doneSecond; result.err == nil {
		t.Fatal("second canceled approval unexpectedly succeeded")
	}
}

func TestPythonTaskApprovalPageRejectsInvalidRequestsAndReplay(t *testing.T) {
	const (
		host  = "127.0.0.1:45678"
		route = "/approve-python-task/test-route"
		csrf  = "approval-csrf-token"
	)
	decision := make(chan bool, 1)
	page := &pythonTaskApprovalPageState{
		host: host, route: route, taskName: `<task>`, secretEnvName: "LAH_TASK_SECRET", csrf: csrf, decision: decision,
	}
	handler := (&app{host: host}).securityHeaders(page)
	validForm := url.Values{"csrf": {csrf}, "decision": {"allow"}}.Encode()
	request := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	get := request(http.MethodGet, "http://"+host+route, "")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "&lt;task&gt;") ||
		strings.Contains(get.Body.String(), `<task>`) || strings.Contains(get.Body.String(), `C:\private`) {
		t.Fatalf("approval GET leaked/unescaped page content: status=%d body=%q", get.Code, get.Body.String())
	}
	if !strings.Contains(get.Body.String(), "LAH_TASK_SECRET") || strings.Contains(get.Body.String(), "cred:") || strings.Contains(get.Body.String(), "secret-canary") {
		t.Fatalf("approval page omitted the environment name or exposed a reference/value: %q", get.Body.String())
	}
	if get.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("approval page cache header = %q", get.Header().Get("Cache-Control"))
	}

	for _, test := range []struct {
		name   string
		target string
		origin []string
		body   string
		ctype  string
		method string
		want   int
	}{
		{name: "wrong host", target: "http://127.0.0.1:45679" + route, method: http.MethodGet, want: http.StatusNotFound},
		{name: "wrong path", target: "http://" + host + "/other", method: http.MethodGet, want: http.StatusNotFound},
		{name: "query", target: "http://" + host + route + "?extra=1", method: http.MethodGet, want: http.StatusNotFound},
		{name: "unsupported method", target: "http://" + host + route, method: http.MethodPut, want: http.StatusMethodNotAllowed},
		{name: "missing origin", target: "http://" + host + route, body: validForm, ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
		{name: "foreign origin", target: "http://" + host + route, origin: []string{"http://attacker.invalid"}, body: validForm, ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
		{name: "duplicate origin", target: "http://" + host + route, origin: []string{"http://" + host, "http://" + host}, body: validForm, ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
		{name: "wrong content type", target: "http://" + host + route, origin: []string{"http://" + host}, body: validForm, ctype: "text/plain", method: http.MethodPost, want: http.StatusBadRequest},
		{name: "wrong csrf", target: "http://" + host + route, origin: []string{"http://" + host}, body: url.Values{"csrf": {"wrong"}, "decision": {"allow"}}.Encode(), ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
		{name: "extra field", target: "http://" + host + route, origin: []string{"http://" + host}, body: validForm + "&extra=1", ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
		{name: "invalid decision", target: "http://" + host + route, origin: []string{"http://" + host}, body: url.Values{"csrf": {csrf}, "decision": {"maybe"}}.Encode(), ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusBadRequest},
		{name: "oversized body", target: "http://" + host + route, origin: []string{"http://" + host}, body: strings.Repeat("x", pythonTaskApprovalMaxBody+1), ctype: "application/x-www-form-urlencoded", method: http.MethodPost, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			for _, origin := range test.origin {
				req.Header.Add("Origin", origin)
			}
			if test.ctype != "" {
				req.Header.Set("Content-Type", test.ctype)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.want, response.Body.String())
			}
		})
	}

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(validForm))
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := post(); response.Code != http.StatusOK {
		t.Fatalf("first valid decision status = %d body=%q", response.Code, response.Body.String())
	}
	if approved := <-decision; !approved {
		t.Fatal("allow decision was not delivered")
	}
	if response := post(); response.Code != http.StatusGone {
		t.Fatalf("replayed approval status = %d, want %d", response.Code, http.StatusGone)
	}
	select {
	case <-decision:
		t.Fatal("replayed approval delivered a second decision")
	default:
	}
}

func TestPythonTaskApprovalPageDefaultsToKorean(t *testing.T) {
	const (
		host  = "127.0.0.1:45679"
		route = "/approve-python-task/default-language"
	)
	page := &pythonTaskApprovalPageState{
		host: host, route: route, taskName: "Synthetic task", csrf: "default-language-csrf",
		decision: make(chan bool, 1),
	}
	handler := (&app{host: host}).securityHeaders(page)
	request := httptest.NewRequest(http.MethodGet, "http://"+host+route, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("approval page status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	for _, want := range []string{`<html lang="ko">`, "이번 실행 승인", "거부 / 취소"} {
		if !strings.Contains(body, want) {
			t.Errorf("no-cookie approval page lacks Korean default %q: %s", want, body)
		}
	}
}

func TestPythonTaskApprovalPageUsesKoreanCookieWithoutPrivateValues(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "private-interpreter.exe")
	script := filepath.Join(root, "private-script.py")
	const (
		secretCanary  = "python-task-korean-secret-canary"
		secretEnvName = "LAH_TASK_SECRET"
	)
	task := pythonTask{
		ID: "python:" + strings.Repeat("e", 32), Name: "Synthetic Korean approval task",
		InterpreterPath: interpreter, ScriptPath: script,
		SecretEnvName: secretEnvName, SecretRef: "cred:" + strings.Repeat("e", 32),
	}
	for _, path := range []string{interpreter, script} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task = pinPythonTaskForTest(t, task)
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	secrets := newPythonTaskSecretStoreFake()
	secrets.values[task.SecretRef] = []byte(secretCanary)
	opened := make(chan string, 1)
	a := &app{
		configPath: configPath,
		secrets:    secrets,
		openApprovalBrowser: func(address string) error {
			opened <- address
			return nil
		},
		pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
			t.Error("task process launched without a local approval decision")
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.runRegisteredPythonTask(ctx, pythonTaskInput{Task: task.Name})
		done <- err
	}()

	var address string
	select {
	case address = <-opened:
	case err := <-done:
		t.Fatalf("task ended before opening its approval page: %v", err)
	case <-ctx.Done():
		t.Fatal("approval page did not open before the test timeout")
	}
	host, route, ok := pythonTaskApprovalRoute(address)
	if !ok {
		t.Fatalf("approval browser address is not a loopback route: %q", address)
	}
	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, "http://"+host+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "ko"})
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("get Korean approval page: %v", err)
	}
	bodyBytes, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatalf("read Korean approval page: %v", readErr)
	}
	body := string(bodyBytes)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Korean approval GET status = %d, want %d; body=%q", response.StatusCode, http.StatusOK, body)
	}
	for _, text := range []string{
		`<html lang="ko">`,
		"이 작업은 현재 Windows 사용자의 권한으로 실행되며",
		"실행 파일과 스크립트 내용 지문을 승인 전 확인했으며 실행 직전 다시 확인합니다.",
		"마지막 확인 뒤 파일이 바뀌는 경쟁까지 막지는 않습니다.",
		"이번 실행은 저장된 비밀값을 자식 프로세스의",
		secretEnvName,
		"이번 실행 승인",
		"거부 / 취소",
	} {
		if !strings.Contains(body, text) {
			t.Errorf("Korean approval page omitted %q: %q", text, body)
		}
	}
	for _, privateValue := range []string{interpreter, script, task.InterpreterSHA256, task.ScriptSHA256, secretCanary, task.SecretRef} {
		if strings.Contains(body, privateValue) {
			t.Errorf("Korean approval page disclosed a private value %q", privateValue)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "expired or was canceled") {
			t.Fatalf("execution after page inspection returned %v, want cancellation before approval", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task did not stop after canceling the unapproved confirmation")
	}
	if len(secrets.loads) != 0 {
		t.Fatalf("unapproved page inspection loaded a task secret: %#v", secrets.loads)
	}
}

func TestPythonTaskConfigChangeAfterApprovalIsRejected(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "script.py")
	changedScript := filepath.Join(root, "changed.py")
	for _, path := range []string{interpreter, script, changedScript} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pythonTask{
		ID: "python:" + strings.Repeat("d", 32), Name: "Mutable task",
		InterpreterPath: interpreter, ScriptPath: script,
		SecretEnvName: "LAH_MUTABLE_SECRET", SecretRef: "cred:" + strings.Repeat("d", 32),
	}
	task = pinPythonTaskForTest(t, task)
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	var launched atomic.Int32
	secrets := newPythonTaskSecretStoreFake()
	secrets.values[task.SecretRef] = []byte("must-not-load-before-recheck")
	a := &app{
		configPath: configPath, secrets: secrets,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "allow", func() error {
				changed := task
				changed.ScriptPath = changedScript
				changed = pinPythonTaskForTest(t, changed)
				return withConfigLock(func() error {
					return writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{changed}})
				})
			})
		},
		pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
			launched.Add(1)
			return nil
		},
	}
	_, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	if err == nil || !strings.Contains(err.Error(), "changed before it could start") {
		t.Fatalf("changed config error = %v, want rejection after approval", err)
	}
	if launched.Load() != 0 {
		t.Fatalf("stale approval launched %d processes", launched.Load())
	}
	if len(secrets.loads) != 0 {
		t.Fatalf("stale approval loaded a task secret: %#v", secrets.loads)
	}
}

func TestPythonTaskRunRejectsFileDriftBeforeAndAfterApproval(t *testing.T) {
	for _, driftPoint := range []string{"before approval", "after approval"} {
		t.Run(driftPoint, func(t *testing.T) {
			root := t.TempDir()
			interpreter := filepath.Join(root, "private-python.exe")
			script := filepath.Join(root, "private-task.py")
			if err := os.WriteFile(interpreter, []byte("synthetic executable"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(script, []byte("alpha-123"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(script)
			if err != nil {
				t.Fatal(err)
			}
			ref := "cred:" + strings.Repeat("f", 32)
			task := pythonTask{
				ID: "python:" + strings.Repeat("f", 32), Name: "Drift check task",
				InterpreterPath: interpreter, ScriptPath: script,
				SecretEnvName: "LAH_DRIFT_SECRET", SecretRef: ref,
			}
			task = pinPythonTaskForTest(t, task)
			configPath := filepath.Join(root, "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
				t.Fatal(err)
			}
			secrets := newPythonTaskSecretStoreFake()
			secrets.values[ref] = []byte("drift-secret-canary")
			var approvals, launched atomic.Int32
			a := &app{
				configPath: configPath,
				secrets:    secrets,
				openApprovalBrowser: func(address string) error {
					approvals.Add(1)
					return submitPythonTaskApproval(t, address, "allow", func() error {
						if driftPoint != "after approval" {
							return nil
						}
						if err := os.WriteFile(script, []byte("bravo-123"), 0o600); err != nil {
							return err
						}
						return os.Chtimes(script, info.ModTime(), info.ModTime())
					}, interpreter, script)
				},
				pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
					launched.Add(1)
					return nil
				},
			}
			if driftPoint == "before approval" {
				if err := os.WriteFile(script, []byte("bravo-123"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(script, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			_, err = a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
			if err == nil || !strings.Contains(err.Error(), "changed or are unavailable") ||
				strings.Contains(err.Error(), interpreter) || strings.Contains(err.Error(), script) || strings.Contains(err.Error(), task.InterpreterSHA256) {
				t.Fatalf("file drift result was not generic and fail-closed: %v", err)
			}
			wantApprovals := int32(0)
			if driftPoint == "after approval" {
				wantApprovals = 1
			}
			if approvals.Load() != wantApprovals || launched.Load() != 0 || len(secrets.loads) != 0 {
				t.Fatalf("drift boundary approvals=%d launches=%d secret loads=%#v", approvals.Load(), launched.Load(), secrets.loads)
			}
		})
	}
}

func TestRegisteredPythonTasksCatalogDoesNotRequireExistingFiles(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "not-installed.exe")
	script := filepath.Join(root, "not-created.py")
	for _, path := range []string{interpreter, script} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pythonTask{
		ID: "python:" + strings.Repeat("e", 32), Name: "Unavailable but registered",
		InterpreterPath: interpreter, ScriptPath: script,
	}
	task = pinPythonTaskForTest(t, task)
	if err := os.Remove(interpreter); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath}
	result, err := a.registeredPythonTasks()
	if err != nil || len(result.Tasks) != 1 || result.Tasks[0].Name != task.Name {
		t.Fatalf("passive task catalog = %#v, err=%v", result, err)
	}
}

func TestPythonTaskPathsRejectWindowsUNCAndDeviceNamespaces(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path namespace validation is platform-specific")
	}
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "UNC", path: `\\fileserver\share\python.exe`},
		{name: "forward slash UNC", path: `//fileserver/share/python.exe`},
		{name: "extended length", path: `\\?\C:\Python\python.exe`},
		{name: "device namespace", path: `\\.\C:\Python\python.exe`},
		{name: "NT object namespace", path: `\??\C:\Python\python.exe`},
		{name: "NT device object path", path: `\Device\HarddiskVolume1\Python\python.exe`},
		{name: "global DOS devices namespace", path: `\GLOBAL??\C:\Python\python.exe`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validPythonTaskPath(test.path, ".exe") {
				t.Fatalf("network or device namespace path accepted: %q", test.path)
			}
		})
	}
}

func TestPythonTaskApprovalPageEnforcesLoopbackOriginCSRFAndOneTimeDecision(t *testing.T) {
	host := "127.0.0.1:43811"
	route := "/approve-python-task/" + strings.Repeat("f", 64)
	state := &pythonTaskApprovalPageState{
		host: host, route: route, taskName: "Synthetic task", secretEnvName: "LAH_TASK_SECRET", csrf: strings.Repeat("a", 64),
		decision: make(chan bool, 1),
	}
	handler := (&app{host: host}).securityHeaders(state)
	for _, test := range []struct {
		name        string
		host        string
		path        string
		origin      string
		contentType string
		form        url.Values
		wantStatus  int
	}{
		{
			name: "wrong host", host: "127.0.0.1:43812", path: route,
			wantStatus: http.StatusNotFound,
		},
		{
			name: "query string", host: host, path: route + "?debug=1",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "missing origin", host: host, path: route, contentType: "application/x-www-form-urlencoded",
			form: url.Values{"csrf": {state.csrf}, "decision": {"allow"}}, wantStatus: http.StatusForbidden,
		},
		{
			name: "foreign origin", host: host, path: route, origin: "http://attacker.example.invalid",
			contentType: "application/x-www-form-urlencoded", form: url.Values{"csrf": {state.csrf}, "decision": {"allow"}},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "wrong content type", host: host, path: route, origin: "http://" + host,
			contentType: "text/plain", form: url.Values{"csrf": {state.csrf}, "decision": {"allow"}},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "duplicate csrf", host: host, path: route, origin: "http://" + host,
			contentType: "application/x-www-form-urlencoded",
			form:        url.Values{"csrf": {state.csrf, state.csrf}, "decision": {"allow"}}, wantStatus: http.StatusForbidden,
		},
		{
			name: "invalid csrf", host: host, path: route, origin: "http://" + host,
			contentType: "application/x-www-form-urlencoded",
			form:        url.Values{"csrf": {strings.Repeat("0", 64)}, "decision": {"allow"}}, wantStatus: http.StatusForbidden,
		},
		{
			name: "body too large", host: host, path: route, origin: "http://" + host,
			contentType: "application/x-www-form-urlencoded",
			form:        url.Values{"csrf": {state.csrf}, "decision": {"allow"}, "padding": {strings.Repeat("x", pythonTaskApprovalMaxBody)}},
			wantStatus:  http.StatusForbidden,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := ""
			if test.form != nil {
				body = test.form.Encode()
			}
			request := httptest.NewRequest(http.MethodPost, "http://"+test.host+test.path, strings.NewReader(body))
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("approval request status=%d, want %d body=%q", response.Code, test.wantStatus, response.Body)
			}
		})
	}
	select {
	case decision := <-state.decision:
		t.Fatalf("invalid request produced approval decision %v", decision)
	default:
	}

	validForm := url.Values{"csrf": {state.csrf}, "decision": {"allow"}}
	valid := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(validForm.Encode()))
	valid.Header.Set("Origin", "http://"+host)
	valid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	approved := httptest.NewRecorder()
	handler.ServeHTTP(approved, valid)
	if approved.Code != http.StatusOK {
		t.Fatalf("valid approval status=%d body=%q", approved.Code, approved.Body)
	}
	select {
	case decision := <-state.decision:
		if !decision {
			t.Fatal("valid allow decision was recorded as deny")
		}
	default:
		t.Fatal("valid allow decision was not recorded")
	}
	replay := httptest.NewRequest(http.MethodPost, "http://"+host+route, strings.NewReader(validForm.Encode()))
	replay.Header.Set("Origin", "http://"+host)
	replay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayed := httptest.NewRecorder()
	handler.ServeHTTP(replayed, replay)
	if replayed.Code != http.StatusGone {
		t.Fatalf("approval replay status=%d, want %d", replayed.Code, http.StatusGone)
	}
}

func submitPythonTaskApproval(t testing.TB, address, decision string, beforeSubmit func() error, privatePaths ...string) error {
	t.Helper()
	host, route, ok := pythonTaskApprovalRoute(address)
	if !ok {
		return fmt.Errorf("invalid loopback approval address %q", address)
	}
	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	defer transport.CloseIdleConnections()
	pageURL := "http://" + host + route
	getRequest, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return fmt.Errorf("build approval page request: %w", err)
	}
	getRequest.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	response, err := client.Do(getRequest)
	if err != nil {
		return fmt.Errorf("get approval page: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		return fmt.Errorf("approval page status=%d read err=%v", response.StatusCode, readErr)
	}
	for _, privatePath := range privatePaths {
		if strings.Contains(string(body), privatePath) {
			return fmt.Errorf("approval page exposed a saved path")
		}
	}
	match := pythonTaskApprovalCSRFPattern.FindSubmatch(body)
	if len(match) != 2 {
		return fmt.Errorf("approval page omitted its CSRF field")
	}
	if beforeSubmit != nil {
		if err := beforeSubmit(); err != nil {
			return fmt.Errorf("before approval submit: %w", err)
		}
	}
	form := url.Values{"csrf": {string(match[1])}, "decision": {decision}}
	request, err := http.NewRequest(http.MethodPost, pageURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build approval request: %w", err)
	}
	request.Header.Set("Origin", "http://"+host)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "lah_lang", Value: "en"})
	response, err = client.Do(request)
	if err != nil {
		return fmt.Errorf("post approval: %w", err)
	}
	body, readErr = io.ReadAll(io.LimitReader(response.Body, 8<<10))
	_ = response.Body.Close()
	wantBody := "Approval recorded. You may close this page."
	if decision == "deny" {
		wantBody = "Request denied. You may close this page."
	}
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), wantBody) {
		return fmt.Errorf("approval submit status=%d read err=%v", response.StatusCode, readErr)
	}
	return nil
}
