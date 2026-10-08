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
	sshApprovalTimeout = 2 * time.Minute
	sshApprovalMaxBody = 2 << 10
)

var sshApprovalSlots = make(chan struct{}, 2)

type sshApprovalParameter struct {
	Name  string
	Value string
}

type loopbackSSHOperationApprover struct {
	open    func(string) error
	timeout time.Duration
}

type sshApprovalPageData struct {
	Language   string
	Korean     bool
	Target     string
	Operation  string
	Summary    string
	Parameters []sshApprovalParameter
	Route      string
	CSRF       string
}

type sshApprovalPageState struct {
	host     string
	route    string
	csrf     string
	request  sshApprovalRequest
	decision chan bool
	mu       sync.Mutex
	consumed bool
}

var sshApprovalPage = template.Must(template.New("ssh-operation-approval").Parse(`<!doctype html>
<html lang="{{.Language}}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Korean}}SSH 작업 승인{{else}}Approve SSH operation{{end}}</title>
<style>body{font:16px system-ui,sans-serif;line-height:1.5;max-width:46rem;margin:2rem auto;padding:0 1rem;color:#161616;background:#fff}button{min-height:44px;padding:.5rem 1rem;font:inherit;margin:.5rem .5rem .5rem 0}button:focus-visible{outline:3px solid #075dcc;outline-offset:3px}.warning{border-left:4px solid #8a3200;padding:.25rem .75rem;overflow-wrap:anywhere}dt{font-weight:700}dd{margin:0 0 .75rem;overflow-wrap:anywhere}</style>
<h1>{{if .Korean}}SSH 작업 승인{{else}}Approve SSH operation{{end}}</h1>
<dl><dt>{{if .Korean}}등록 대상{{else}}Registered target{{end}}</dt><dd>{{.Target}}</dd><dt>{{if .Korean}}작업{{else}}Operation{{end}}</dt><dd>{{.Operation}}</dd>{{if .Summary}}<dt>{{if .Korean}}설명{{else}}Summary{{end}}</dt><dd>{{.Summary}}</dd>{{end}}</dl>
{{if .Parameters}}<h2>{{if .Korean}}이번 호출 매개변수{{else}}Parameters for this call{{end}}</h2><dl>{{range .Parameters}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>{{end}}</dl>{{end}}
<p class="warning">{{if .Korean}}저장된 SSH 작업을 한 번 실행할지 확인합니다. 서버가 명령을 시작한 뒤 연결이 끊기면 원격 종료 여부를 확인할 수 없을 수 있습니다. 승인하지 않으면 SSH를 시작하지 않습니다.{{else}}This confirms one run of the saved SSH operation. If the connection ends after the server starts the command, remote termination may be unknown. SSH will not start unless you approve.{{end}}</p>
<form method="post" action="{{.Route}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit" name="decision" value="allow">{{if .Korean}}이번 호출 승인{{else}}Approve this call{{end}}</button><button type="submit" name="decision" value="deny">{{if .Korean}}거부 / 취소{{else}}Deny / cancel{{end}}</button></form></html>`))

func (a *loopbackSSHOperationApprover) Confirm(ctx context.Context, request sshApprovalRequest) (bool, error) {
	if ctx == nil || ctx.Err() != nil {
		return false, context.Canceled
	}
	timeout := a.timeout
	if timeout <= 0 {
		timeout = sshApprovalTimeout
	}
	select {
	case sshApprovalSlots <- struct{}{}:
		defer func() { <-sshApprovalSlots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return false, errors.New("Could not start the local SSH approval page.")
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() || addr.Port == 0 {
		return false, errors.New("Could not start the local SSH approval page.")
	}
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
	pathBytes, csrfBytes := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(pathBytes); err != nil {
		return false, errors.New("Could not initialize the local SSH approval page.")
	}
	if _, err := rand.Read(csrfBytes); err != nil {
		return false, errors.New("Could not initialize the local SSH approval page.")
	}
	route := "/approve-ssh-operation/" + hex.EncodeToString(pathBytes)
	csrf := hex.EncodeToString(csrfBytes)
	decision := make(chan bool, 1)
	state := &sshApprovalPageState{host: host, route: route, csrf: csrf, request: request, decision: decision}
	server := &http.Server{
		Handler:           (&app{host: host}).securityHeaders(state),
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
	open := a.open
	if open == nil {
		open = openBrowser
	}
	openResult := make(chan error, 1)
	go func() { openResult <- open(address) }()
	return waitForSSHOperationApproval(ctx, timeout, decision, openResult, serveResult)
}

func (s *sshApprovalPageState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	locale := uiLocaleForRequest(r)
	if r.Host != s.host || r.URL.Path != s.route || r.URL.RawQuery != "" || r.URL.Fragment != "" {
		http.Error(w, localizeUIMessage(locale, "Not found"), http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := sshApprovalPage.Execute(w, sshApprovalPageData{
			Language: string(locale), Korean: locale == uiLocaleKorean,
			Target: s.request.TargetName, Operation: s.request.Operation, Summary: s.request.Summary,
			Parameters: append([]sshApprovalParameter(nil), s.request.Parameters...), Route: s.route, CSRF: s.csrf,
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
	r.Body = http.MaxBytesReader(w, r.Body, sshApprovalMaxBody)
	if err := r.ParseForm(); err != nil || len(r.PostForm) != 2 || len(r.PostForm["csrf"]) != 1 || len(r.PostForm["decision"]) != 1 ||
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
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	s.decision <- approved
}

func waitForSSHOperationApproval(ctx context.Context, timeout time.Duration, decision <-chan bool, openResult, serveResult <-chan error) (bool, error) {
	if ctx == nil || ctx.Err() != nil {
		return false, context.Canceled
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	opened, decided, approved := false, false, false
	openDone, serveDone := openResult, serveResult
	for {
		if decided {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if !approved {
				return false, nil
			}
			if opened {
				return true, nil
			}
		}
		select {
		case approved = <-decision:
			decided = true
		case err := <-openDone:
			if err != nil {
				return false, errors.New("Could not open the local SSH approval page.")
			}
			opened = true
			openDone = nil
		case err := <-serveDone:
			if !errors.Is(err, http.ErrServerClosed) {
				return false, errors.New("The local SSH approval page stopped.")
			}
			serveDone = nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, context.DeadlineExceeded
		}
	}
}

func sshApprovalRoute(address string) (host, route string, ok bool) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Host == "" || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/approve-ssh-operation/") {
		return "", "", false
	}
	hostname, _, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(hostname) == nil || !net.ParseIP(hostname).IsLoopback() {
		return "", "", false
	}
	return parsed.Host, parsed.Path, true
}
