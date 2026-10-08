package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pythonTaskApprovalTimeout = 2 * time.Minute
	pythonTaskApprovalMaxBody = 1024
)

var pythonTaskApprovalPage = template.Must(template.New("python-task-approval").Parse(`<!doctype html>
<html lang="{{.Language}}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Korean}}Python 작업 실행 확인{{else}}Confirm registered task{{end}}</title>
<style>body{font:16px system-ui,sans-serif;line-height:1.5;max-width:42rem;margin:2rem auto;padding:0 1rem;color:#161616;background:#fff}button{min-height:44px;padding:.5rem 1rem;font:inherit;margin:.5rem .5rem .5rem 0}button:focus-visible{outline:3px solid #075dcc;outline-offset:3px}.warning{border-left:4px solid #8a3200;padding:.25rem .75rem;overflow-wrap:anywhere}</style>
<h1>{{if .Korean}}Python 작업 실행 확인{{else}}Confirm registered task{{end}}</h1>
<p>{{if .Korean}}등록된 작업 이름{{else}}Registered task name{{end}}: <strong>{{.TaskName}}</strong></p>
<p class="warning">{{if .Korean}}이 작업은 현재 Windows 사용자의 권한으로 실행되며 로컬 파일을 변경하거나 네트워크에 접속할 수 있습니다. 샌드박스가 아닙니다. 실행 파일과 스크립트 내용 지문을 승인 전 확인했으며 실행 직전 다시 확인합니다. 확인된 변경이 있으면 실행하지 않습니다. 마지막 확인 뒤 파일이 바뀌는 경쟁까지 막지는 않습니다. 실행 파일과 스크립트 경로는 이 확인 페이지에 표시하지 않습니다.{{else}}This task runs with the current Windows user's permissions. It can change local files or contact the network and is not sandboxed. The executable and script fingerprints were checked before approval and are checked again immediately before launch. Detected changes prevent execution; a file may still change after its final check. Executable and script paths are not shown on this page.{{end}}</p>
{{if .SecretEnvName}}<p class="warning">{{if .Korean}}이번 실행은 저장된 비밀값을 자식 프로세스의 <strong>{{.SecretEnvName}}</strong> 환경 변수로 전달합니다. 후손 프로세스도 이 값을 물려받을 수 있습니다.{{else}}This run passes the saved secret to the child process in environment variable <strong>{{.SecretEnvName}}</strong>. Descendant processes may inherit it.{{end}}</p>{{end}}
<form method="post" action="{{.Route}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button type="submit" name="decision" value="allow">{{if .Korean}}이번 실행 승인{{else}}Approve this run{{end}}</button>
<button type="submit" name="decision" value="deny">{{if .Korean}}거부 / 취소{{else}}Deny / cancel{{end}}</button>
</form></html>`))

var pythonTaskApprovalSlots = make(chan struct{}, 2)

type pythonTaskApprovalPageData struct {
	Language      string
	Korean        bool
	TaskName      string
	SecretEnvName string
	Route         string
	CSRF          string
}

type pythonTaskApprovalPageState struct {
	host          string
	route         string
	taskName      string
	secretEnvName string
	csrf          string
	decision      chan bool

	mu       sync.Mutex
	consumed bool
}

func (s *pythonTaskApprovalPageState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	locale := uiLocaleForRequest(r)
	if r.Host != s.host || r.URL.Path != s.route || r.URL.RawQuery != "" || r.URL.Fragment != "" {
		http.Error(w, localizeUIMessage(locale, "Not found"), http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pythonTaskApprovalPage.Execute(w, pythonTaskApprovalPageData{
			Language: string(locale), Korean: locale == uiLocaleKorean,
			TaskName: s.taskName, SecretEnvName: s.secretEnvName, Route: s.route, CSRF: s.csrf,
		}); err != nil {
			http.Error(w, localizeUIMessage(locale, "Could not render confirmation page"), http.StatusInternalServerError)
		}
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, localizeUIMessage(locale, "Method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != "http://"+s.host {
		http.Error(w, localizeUIMessage(locale, "Request origin rejected"), http.StatusForbidden)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		http.Error(w, localizeUIMessage(locale, "Invalid confirmation request"), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pythonTaskApprovalMaxBody)
	if err := r.ParseForm(); err != nil || len(r.PostForm) != 2 ||
		len(r.PostForm["csrf"]) != 1 || len(r.PostForm["decision"]) != 1 ||
		subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(s.csrf)) != 1 {
		http.Error(w, localizeUIMessage(locale, "Confirmation rejected"), http.StatusForbidden)
		return
	}
	approved := r.PostForm.Get("decision") == "allow"
	if !approved && r.PostForm.Get("decision") != "deny" {
		http.Error(w, localizeUIMessage(locale, "Invalid confirmation decision"), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.consumed {
		s.mu.Unlock()
		http.Error(w, localizeUIMessage(locale, "Confirmation already used"), http.StatusGone)
		return
	}
	s.consumed = true
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if approved {
		_, _ = io.WriteString(w, localizeUIMessage(locale, "Approval recorded. You may close this page."))
	} else {
		_, _ = io.WriteString(w, localizeUIMessage(locale, "Request denied. You may close this page."))
	}
	// Deliver the complete confirmation page before the waiting run closes its server.
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	s.decision <- approved
}

func (a *app) confirmPythonTaskRun(ctx context.Context, taskName string, secretEnvNames ...string) error {
	if ctx == nil || taskName == "" {
		return errors.New("Python task approval is unavailable; no task was run.")
	}
	if err := ctx.Err(); err != nil {
		return errors.New("Python task approval expired or was canceled; no task was run.")
	}
	select {
	case pythonTaskApprovalSlots <- struct{}{}:
		defer func() { <-pythonTaskApprovalSlots }()
	case <-ctx.Done():
		return errors.New("Python task approval expired or was canceled; no task was run.")
	}
	if err := ctx.Err(); err != nil {
		return errors.New("Python task approval expired or was canceled; no task was run.")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("Could not start the local task approval page; no task was run.")
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() || addr.Port == 0 {
		return errors.New("Could not start the local task approval page; no task was run.")
	}
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
	routeBytes, csrfBytes := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(routeBytes); err != nil {
		return errors.New("Could not initialize the local task approval page; no task was run.")
	}
	if _, err := rand.Read(csrfBytes); err != nil {
		return errors.New("Could not initialize the local task approval page; no task was run.")
	}
	route, csrf := "/approve-python-task/"+hex.EncodeToString(routeBytes), hex.EncodeToString(csrfBytes)
	decision := make(chan bool, 1)
	secretEnvName := ""
	if len(secretEnvNames) > 0 {
		secretEnvName = secretEnvNames[0]
	}
	page := &pythonTaskApprovalPageState{host: host, route: route, taskName: taskName, secretEnvName: secretEnvName, csrf: csrf, decision: decision}
	server := &http.Server{
		Handler:           (&app{host: host}).securityHeaders(page),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	defer server.Close()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	address := "http://" + host + route
	open := a.openApprovalBrowser
	if open == nil {
		open = openBrowser
	}
	openResult := make(chan error, 1)
	go func() { openResult <- open(address) }()
	return waitForPythonTaskApproval(ctx, decision, openResult, serveResult)
}

func waitForPythonTaskApproval(ctx context.Context, decision <-chan bool, openResult <-chan error, serveResult <-chan error) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("Python task approval expired or was canceled; no task was run.")
	}
	timer := time.NewTimer(pythonTaskApprovalTimeout)
	defer timer.Stop()
	opened, decided, approved := false, false, false
	openDone, serveDone := openResult, serveResult
	for {
		if decided {
			if ctx.Err() != nil {
				return errors.New("Python task approval expired or was canceled; no task was run.")
			}
			if !approved {
				return errors.New("Python task execution was denied or canceled; no task was run.")
			}
			if opened {
				if ctx.Err() != nil {
					return errors.New("Python task approval expired or was canceled; no task was run.")
				}
				return nil
			}
		}
		select {
		case approved = <-decision:
			decided = true
		case err := <-openDone:
			if err != nil {
				return errors.New("Could not open the local task approval page; no task was run.")
			}
			opened = true
			openDone = nil
		case err := <-serveDone:
			if !errors.Is(err, http.ErrServerClosed) {
				return errors.New("The local task approval page stopped; no task was run.")
			}
			serveDone = nil
		case <-ctx.Done():
			return errors.New("Python task approval expired or was canceled; no task was run.")
		case <-timer.C:
			return errors.New("Python task approval timed out; no task was run.")
		}
	}
}

func pythonTaskApprovalRoute(address string) (host, route string, ok bool) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Host == "" || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/approve-python-task/") {
		return "", "", false
	}
	hostname, _, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(hostname) == nil || !net.ParseIP(hostname).IsLoopback() {
		return "", "", false
	}
	return parsed.Host, parsed.Path, true
}
