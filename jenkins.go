package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"unicode"
	"unicode/utf8"
)

const (
	maxJenkinsLogBytes     = 32 << 10
	jenkinsQueuePolls      = 10
	jenkinsQueueDelay      = 200 * time.Millisecond
	jenkinsApprovalTimeout = 2 * time.Minute
)

var jenkinsApprovalPage = template.Must(template.New("jenkins-approval").Parse(`<!doctype html>
<html lang="{{.Language}}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Korean}}Jenkins 실행 확인{{else}}Confirm Jenkins trigger{{end}}</title>
<style>body{font:16px system-ui,sans-serif;line-height:1.5;max-width:48rem;margin:2rem auto;padding:0 1rem;color:#161616;background:#fff}button{min-height:44px;padding:.5rem 1rem;font:inherit;margin:.5rem .5rem .5rem 0}button:focus-visible{outline:3px solid #075dcc;outline-offset:3px}.warning{border-left:4px solid #8a3200;padding:.25rem .75rem}code{overflow-wrap:anywhere}</style>
<h1>{{if .Korean}}Jenkins 실행 확인{{else}}Confirm Jenkins trigger{{end}}</h1>
<p>{{if .Korean}}등록된 대상{{else}}Registered target{{end}}: <strong>{{.Target}}</strong><br>{{if .Korean}}환경{{else}}Environment{{end}}: <strong>{{.Environment}}</strong><br>{{if .Korean}}등록된 Jenkins URL{{else}}Registered Jenkins URL{{end}}: <code>{{.BaseURL}}</code><br>{{if .Korean}}고정 작업 경로{{else}}Fixed job path{{end}}: <code>{{.JobPath}}</code></p>
<p class="warning">{{if .Korean}}이 대상은 비운영 대상으로 사전 승인되지 않았습니다. 승인하면 등록된 Jenkins 작업에 실행 요청을 한 번 보냅니다.{{else}}This target was not preapproved as non-production. Approving sends one trigger request to this registered Jenkins job.{{end}}</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button type="submit" name="decision" value="allow">{{if .Korean}}이 실행 승인{{else}}Approve this trigger{{end}}</button>
<button type="submit" name="decision" value="deny">{{if .Korean}}거부{{else}}Deny{{end}}</button>
<button type="submit" name="decision" value="deny">{{if .Korean}}취소{{else}}Cancel{{end}}</button>
</form></html>`))

type jenkinsApprovalPageData struct {
	Language    string
	Korean      bool
	Target      string
	Environment string
	BaseURL     string
	JobPath     string
	Action      string
	CSRF        string
}

type jenkinsApprovalPageState struct {
	host        string
	route       string
	target      string
	environment string
	baseURL     string
	jobPath     string
	csrf        string
	decision    chan bool
	mu          sync.Mutex
	consumed    bool
}

func (s *jenkinsApprovalPageState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	locale := uiLocaleForRequest(r)
	w.Header().Set("Content-Language", string(locale))
	if r.Host != s.host || r.URL.Path != s.route || r.URL.RawQuery != "" || r.URL.Fragment != "" {
		http.Error(w, localizeUIMessage(locale, "Not found"), http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := jenkinsApprovalPage.Execute(w, jenkinsApprovalPageData{
			Language: string(locale), Korean: locale == uiLocaleKorean,
			Target: s.target, Environment: s.environment,
			BaseURL: s.baseURL, JobPath: s.jobPath, Action: s.route, CSRF: s.csrf,
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
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil || len(r.PostForm["csrf"]) != 1 || len(r.PostForm["decision"]) != 1 || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(s.csrf)) != 1 {
		http.Error(w, localizeUIMessage(locale, "Confirmation rejected"), http.StatusForbidden)
		return
	}
	approve := r.PostForm.Get("decision") == "allow"
	if !approve && r.PostForm.Get("decision") != "deny" {
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
	if approve {
		_, _ = io.WriteString(w, localizeUIMessage(locale, "Approval recorded. You may close this page."))
	} else {
		_, _ = io.WriteString(w, localizeUIMessage(locale, "Request denied. You may close this page."))
	}
	s.decision <- approve
}

type jenkinsTarget struct {
	ID                         string `json:"id"`
	Name                       string `json:"name"`
	BaseURL                    string `json:"base_url"`
	Username                   string `json:"username"`
	JobPath                    string `json:"job_path"`
	Environment                string `json:"environment"`
	SecretRef                  string `json:"secret_ref"`
	NonProductionPreapproved   bool   `json:"nonproduction_preapproved"`
	NonProductionApprovalScope string `json:"nonproduction_approval_scope,omitempty"`
	Disabled                   bool   `json:"disabled,omitempty"`
}

type jenkinsTargetInput struct {
	Target string `json:"target"`
}

type jenkinsQueueInput struct {
	Target  string `json:"target"`
	QueueID uint64 `json:"queue_id"`
}

type jenkinsLogInput struct {
	Target      string `json:"target"`
	BuildNumber int    `json:"build_number"`
	StartOffset int64  `json:"start_offset"`
}

type jenkinsLogSegmentResult struct {
	Target       string `json:"target"`
	BuildNumber  int    `json:"build_number"`
	StartOffset  int64  `json:"start_offset"`
	NextOffset   int64  `json:"next_offset"`
	MoreData     bool   `json:"more_data"`
	Log          string `json:"log"`
	LogTruncated bool   `json:"log_truncated"`
}

type jenkinsLogSegment struct {
	Text       string
	NextOffset int64
	MoreData   bool
	Truncated  bool
}

type jenkinsJobInfo struct {
	Target            string `json:"target"`
	FullName          string `json:"full_name"`
	Buildable         bool   `json:"buildable"`
	LastBuildNumber   int    `json:"last_build_number,omitempty"`
	LastBuildResult   string `json:"last_build_result,omitempty"`
	LastBuildBuilding bool   `json:"last_build_building"`
}

type jenkinsRunResult struct {
	Target       string `json:"target"`
	QueueID      uint64 `json:"queue_id,omitempty"`
	Queued       bool   `json:"queued"`
	State        string `json:"state,omitempty"`
	Guidance     string `json:"guidance,omitempty"`
	BuildNumber  int    `json:"build_number,omitempty"`
	Result       string `json:"result,omitempty"`
	Building     bool   `json:"building,omitempty"`
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"log_truncated,omitempty"`
	NextOffset   *int64 `json:"next_offset,omitempty"`
	MoreData     *bool  `json:"more_data,omitempty"`
}

type jenkinsQueueInfo struct {
	Target       string `json:"target"`
	QueueID      uint64 `json:"queue_id"`
	State        string `json:"state"`
	Blocked      bool   `json:"blocked"`
	Stuck        bool   `json:"stuck"`
	Why          string `json:"why,omitempty"`
	BuildNumber  int    `json:"build_number,omitempty"`
	Result       string `json:"result,omitempty"`
	Building     bool   `json:"building,omitempty"`
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"log_truncated,omitempty"`
	NextOffset   *int64 `json:"next_offset,omitempty"`
	MoreData     *bool  `json:"more_data,omitempty"`
}

type jenkinsJobResponse struct {
	Name      string `json:"name"`
	FullName  string `json:"fullName"`
	Buildable bool   `json:"buildable"`
	LastBuild *struct {
		Number   int    `json:"number"`
		Result   string `json:"result"`
		Building bool   `json:"building"`
	} `json:"lastBuild"`
}

type jenkinsQueueResponse struct {
	ID        uint64 `json:"id"`
	Cancelled bool   `json:"cancelled"`
	Blocked   bool   `json:"blocked"`
	Stuck     bool   `json:"stuck"`
	Why       string `json:"why"`
	Task      struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"task"`
	Executable *struct {
		Number int `json:"number"`
	} `json:"executable"`
}

type jenkinsBuildResponse struct {
	Number   int    `json:"number"`
	Result   string `json:"result"`
	Building bool   `json:"building"`
}

func (a *app) saveJenkinsTarget(targetID, name, baseURL, username, jobPath, environment, secret string, nonProductionPreapproved bool) (string, error) {
	return a.saveJenkinsTargetWithNamedSecret(targetID, name, baseURL, username, jobPath, environment, secret, nonProductionPreapproved, "")
}

func (a *app) saveJenkinsTargetWithNamedSecret(targetID, name, baseURL, username, jobPath, environment, secret string, nonProductionPreapproved bool, namedSecretID string) (string, error) {
	current, err := readConfig(a.configPath)
	if err != nil {
		return "", errors.New("Could not read current settings. They were not changed.")
	}
	index := -1
	if targetID != "" {
		index = findJenkinsTargetIndex(current.JenkinsTargets, targetID)
		if index < 0 {
			return "", errors.New("The selected Jenkins target is not registered.")
		}
	}
	for i, existing := range current.JenkinsTargets {
		if existing.Name == name && i != index {
			return "", errors.New("A Jenkins target with that name is already registered.")
		}
	}
	namedRef, selectedNamedSecret, err := resolveTargetNamedSecret(current, namedSecretID, secret)
	if err != nil {
		return "", err
	}
	if secret == "" && !selectedNamedSecret && index < 0 {
		return "", errors.New("Enter a Jenkins API token to register a new target.")
	}
	if secret == "" && !selectedNamedSecret && index >= 0 && current.JenkinsTargets[index].BaseURL != baseURL {
		return "", errors.New("Changing Jenkins base URL requires entering a new API token. The saved credential was not sent.")
	}
	updated := &jenkinsTarget{Name: name, BaseURL: baseURL, Username: username, JobPath: jobPath, Environment: environment}
	var old jenkinsTarget
	if index >= 0 {
		old = current.JenkinsTargets[index]
		if err := validateJenkinsTarget(old); err != nil {
			return "", errors.New("The saved Jenkins target is invalid. It was not changed.")
		}
		updated.ID, updated.SecretRef, updated.Disabled = old.ID, old.SecretRef, old.Disabled
	} else {
		updated.ID, err = newTargetID("jenkins")
		if err != nil {
			return "", errors.New("Could not create target ID. Settings were not changed.")
		}
	}
	if !validJenkinsUsername(username) || !validJenkinsJobPath(jobPath) || !validJenkinsEnvironmentLabel(environment) || name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return "", errors.New("The Jenkins target is invalid. Settings were not changed.")
	}
	newRef := ""
	createdCredential := false
	if secret != "" {
		refBytes := make([]byte, 16)
		if _, err := rand.Read(refBytes); err != nil {
			return "", errors.New("Could not create credential reference. Settings were not changed.")
		}
		newRef = "cred:" + hex.EncodeToString(refBytes)
		if err := a.secrets.Save(newRef, []byte(secret)); err != nil {
			return "", errors.New(secretStoreError())
		}
		updated.SecretRef = newRef
		createdCredential = true
	} else if selectedNamedSecret {
		newRef = namedRef
		updated.SecretRef = namedRef
	}
	if index >= 0 && jenkinsTriggerApproved(old) && jenkinsTriggerApprovalScope(*updated) != old.NonProductionApprovalScope {
		nonProductionPreapproved = false
	}
	if updated.Disabled {
		nonProductionPreapproved = false
	}
	if nonProductionPreapproved {
		updated.NonProductionPreapproved = true
		updated.NonProductionApprovalScope = jenkinsTriggerApprovalScope(*updated)
	}
	if err := validateJenkinsTarget(*updated); err != nil {
		if createdCredential {
			_ = a.secrets.Delete(newRef)
		}
		return "", errors.New("The Jenkins target is invalid. Settings were not changed.")
	}
	next := current
	next.Version = configVersion
	if index < 0 {
		next.JenkinsTargets = append(next.JenkinsTargets, *updated)
	} else {
		next.JenkinsTargets[index] = *updated
	}
	clearConnectionTest(&next, updated.ID)
	if err := writeConfig(a.configPath, next); err != nil {
		if createdCredential && a.secrets.Delete(newRef) != nil {
			return updated.ID, errors.New("Could not save settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager. Remove the Local Agent Harness credential entry before retrying.")
		}
		return "", errors.New("Could not save settings. Previous settings remain in place.")
	}
	if old.SecretRef != "" && old.SecretRef != newRef && !configReferencesSecret(next, old.SecretRef) {
		if err := a.secrets.Delete(old.SecretRef); err != nil {
			return updated.ID, errors.New("Jenkins target saved, but the previous unused credential could not be removed.")
		}
	}
	return updated.ID, nil
}

func findJenkinsTargetIndex(targets []jenkinsTarget, id string) int {
	for i := range targets {
		if targets[i].ID == id {
			return i
		}
	}
	return -1
}

func findJenkinsTargetNameIndex(targets []jenkinsTarget, name string) int {
	for i := range targets {
		if targets[i].Name == name {
			return i
		}
	}
	return -1
}

func (a *app) registeredJenkinsJob(ctx context.Context, requestedName string) (jenkinsJobInfo, error) {
	t, token, err := a.loadJenkinsTarget(requestedName)
	if err != nil {
		return jenkinsJobInfo{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchJenkinsJob(ctx, t, string(token), a.client)
}

func (a *app) runRegisteredJenkinsJob(ctx context.Context, requestedName string) (jenkinsRunResult, error) {
	t, token, err := a.loadJenkinsTarget(requestedName)
	if err != nil {
		return jenkinsRunResult{}, err
	}
	defer func() { clear(token) }()
	if !jenkinsTriggerApproved(t) {
		targetID, triggerScope := t.ID, jenkinsTriggerApprovalScope(t)
		displayTarget := cleanOutput(t.Name, string(token), 80)
		displayEnvironment := cleanOutput(t.Environment, string(token), 80)
		displayBaseURL := cleanOutput(t.BaseURL, string(token), 2048)
		displayJob := cleanOutput(t.JobPath, string(token), 512)
		clear(token)
		token = nil
		approvalCtx, cancel := context.WithTimeout(ctx, jenkinsApprovalTimeout)
		var err error
		if a.jenkinsApprovalResult != nil {
			err = a.jenkinsApprovalResult(approvalCtx)
		} else {
			err = a.confirmJenkinsTrigger(approvalCtx, displayTarget, displayEnvironment, displayBaseURL, displayJob)
		}
		cancel()
		if err != nil {
			return jenkinsRunResult{}, err
		}
		return a.runJenkinsJobAfterApproval(ctx, requestedName, targetID, triggerScope)
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return runJenkinsJob(ctx, t, string(token), a.client)
}

func (a *app) runJenkinsJobAfterApproval(ctx context.Context, requestedName, targetID, triggerScope string) (jenkinsRunResult, error) {
	target, token, err := a.loadJenkinsTarget(requestedName)
	if err != nil {
		return jenkinsRunResult{}, err
	}
	defer clear(token)
	if target.ID != targetID || jenkinsTriggerApprovalScope(target) != triggerScope {
		return jenkinsRunResult{}, errors.New("The Jenkins target changed after confirmation. Review it and try again.")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	verifyTarget := func() error {
		current, currentToken, err := a.loadJenkinsTarget(requestedName)
		clear(currentToken)
		if err != nil || current.ID != targetID || jenkinsTriggerApprovalScope(current) != triggerScope {
			return errors.New("The Jenkins target changed after confirmation. Review it and try again.")
		}
		return nil
	}
	return runJenkinsJobAfterConfirmation(ctx, target, string(token), a.client, verifyTarget)
}

func (a *app) confirmJenkinsTrigger(ctx context.Context, targetName, environment, baseURL, jobPath string) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("Could not start the local Jenkins confirmation page; no trigger was sent.")
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() || addr.Port == 0 {
		return errors.New("Could not start the local Jenkins confirmation page; no trigger was sent.")
	}
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
	routeBytes, csrfBytes := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(routeBytes); err != nil {
		return errors.New("Could not initialize the local Jenkins confirmation page; no trigger was sent.")
	}
	if _, err := rand.Read(csrfBytes); err != nil {
		return errors.New("Could not initialize the local Jenkins confirmation page; no trigger was sent.")
	}
	route, csrf := "/approve/"+hex.EncodeToString(routeBytes), hex.EncodeToString(csrfBytes)
	decision := make(chan bool, 1)
	page := &jenkinsApprovalPageState{host: host, route: route, target: targetName, environment: environment, baseURL: baseURL, jobPath: jobPath, csrf: csrf, decision: decision}
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
	return waitForJenkinsApproval(ctx, decision, openResult, serveResult)
}

func waitForJenkinsApproval(ctx context.Context, decision <-chan bool, openResult <-chan error, serveResult <-chan error) error {
	timer := time.NewTimer(jenkinsApprovalTimeout)
	defer timer.Stop()
	opened, decided, approved := false, false, false
	openDone, serveDone := openResult, serveResult
	for {
		if decided && (!approved || opened) {
			if !approved {
				return errors.New("Jenkins trigger was denied or canceled; no trigger was sent.")
			}
			return nil
		}
		select {
		case approved = <-decision:
			decided = true
		case err := <-openDone:
			if err != nil {
				return errors.New("Could not open the local Jenkins confirmation page; no trigger was sent.")
			}
			opened = true
			openDone = nil
		case err := <-serveDone:
			if !errors.Is(err, http.ErrServerClosed) {
				return errors.New("The local Jenkins confirmation page stopped; no trigger was sent.")
			}
			serveDone = nil
		case <-ctx.Done():
			return errors.New("Jenkins trigger confirmation expired or was canceled; no trigger was sent.")
		case <-timer.C:
			return errors.New("Jenkins trigger confirmation timed out; no trigger was sent.")
		}
	}
}

func (a *app) loadJenkinsTarget(requestedName string) (jenkinsTarget, []byte, error) {
	var target jenkinsTarget
	var token []byte
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("No valid Jenkins targets are configured. Open local settings and save one.")
		}
		index := findJenkinsTargetNameIndex(cfg.JenkinsTargets, requestedName)
		if index < 0 {
			return errors.New("The requested Jenkins target name is not registered.")
		}
		target = cfg.JenkinsTargets[index]
		if target.Disabled {
			return errors.New("The registered Jenkins target is disabled.")
		}
		if err := validateJenkinsTarget(target); err != nil {
			return errors.New("The saved Jenkins target is invalid. Review it in local settings.")
		}
		token, err = a.secrets.Load(target.SecretRef)
		if err != nil || len(token) == 0 || len(token) > maxSecretSize || strings.ContainsAny(string(token), "\r\n\x00") {
			clear(token)
			token = nil
			return errors.New("The saved Jenkins credential is unavailable. Replace it in local settings.")
		}
		return nil
	})
	if err != nil {
		return jenkinsTarget{}, nil, err
	}
	return target, token, nil
}

func fetchJenkinsJob(ctx context.Context, target jenkinsTarget, token string, client *http.Client) (jenkinsJobInfo, error) {
	if validateJenkinsTarget(target) != nil || token == "" || strings.ContainsAny(token, "\r\n\x00") || client == nil {
		return jenkinsJobInfo{}, errors.New("The registered Jenkins target is invalid.")
	}
	endpoint, err := jenkinsJobURL(target, "api", "json")
	if err != nil {
		return jenkinsJobInfo{}, errors.New("The registered Jenkins target is invalid.")
	}
	endpoint.RawQuery = "tree=name,fullName,buildable,lastBuild[number,result,building]"
	resp, err := jenkinsRequest(ctx, client, http.MethodGet, endpoint, target.Username, token)
	if err != nil {
		return jenkinsJobInfo{}, connectionTransportFailure(
			err,
			"Could not contact the registered Jenkins job. Check network access and TLS.",
		)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jenkinsJobInfo{}, jenkinsStatusError(resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return jenkinsJobInfo{}, errors.New("Jenkins job response exceeded 1 MiB or could not be read.")
	}
	var raw jenkinsJobResponse
	if json.Unmarshal(body, &raw) != nil || raw.FullName != target.JobPath || raw.Name != lastJobSegment(target.JobPath) {
		return jenkinsJobInfo{}, errors.New("Jenkins returned an invalid registered job response.")
	}
	result := jenkinsJobInfo{Target: cleanOutput(target.Name, token, 80), FullName: cleanOutput(raw.FullName, token, 512), Buildable: raw.Buildable}
	if raw.LastBuild != nil && raw.LastBuild.Number > 0 {
		result.LastBuildNumber = raw.LastBuild.Number
		result.LastBuildResult = safeBuildResult(raw.LastBuild.Result)
		result.LastBuildBuilding = raw.LastBuild.Building
	}
	return result, nil
}

func runJenkinsJob(ctx context.Context, target jenkinsTarget, token string, client *http.Client) (jenkinsRunResult, error) {
	if !jenkinsTriggerApproved(target) {
		return jenkinsRunResult{}, errors.New("Triggering is not authorized for this registered Jenkins target.")
	}
	return runJenkinsJobWithAuthorization(ctx, target, token, client, nil)
}

func runJenkinsJobAfterConfirmation(ctx context.Context, target jenkinsTarget, token string, client *http.Client, verifyTarget func() error) (jenkinsRunResult, error) {
	if verifyTarget == nil {
		return jenkinsRunResult{}, errors.New("A current Jenkins target confirmation is required.")
	}
	return runJenkinsJobWithAuthorization(ctx, target, token, client, verifyTarget)
}

func runJenkinsJobWithAuthorization(ctx context.Context, target jenkinsTarget, token string, client *http.Client, verifyTarget func() error) (jenkinsRunResult, error) {
	if validateJenkinsTarget(target) != nil || token == "" || strings.ContainsAny(token, "\r\n\x00") || client == nil {
		return jenkinsRunResult{}, errors.New("The registered Jenkins target is invalid.")
	}
	job, err := fetchJenkinsJob(ctx, target, token, client)
	if err != nil {
		return jenkinsRunResult{}, err
	}
	if !job.Buildable {
		return jenkinsRunResult{}, errors.New("The registered Jenkins job is not buildable.")
	}
	endpoint, err := jenkinsJobURL(target, "build")
	if err != nil {
		return jenkinsRunResult{}, errors.New("The registered Jenkins target is invalid.")
	}
	if verifyTarget != nil {
		if err := verifyTarget(); err != nil {
			return jenkinsRunResult{}, err
		}
	}
	resp, err := jenkinsRequest(ctx, client, http.MethodPost, endpoint, target.Username, token)
	if err != nil {
		return unknownJenkinsTrigger(target, token), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		if resp.StatusCode < http.StatusBadRequest || resp.StatusCode >= http.StatusInternalServerError {
			return unknownJenkinsTrigger(target, token), nil
		}
		return jenkinsRunResult{}, jenkinsStatusError(resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if len(location) == 0 || len(location) > 2048 {
		return unknownJenkinsTrigger(target, token), nil
	}
	_, queueID, err := validateQueueLocation(target, endpoint, location)
	if err != nil {
		return unknownJenkinsTrigger(target, token), nil
	}

	queue, stillQueued, err := waitForJenkinsBuild(ctx, target, queueID, token, client)
	if err != nil {
		return pendingJenkinsInspection(target, token, queueID, 0), nil
	}
	if queue.Cancelled {
		return jenkinsRunResult{
			Target: cleanOutput(target.Name, token, 80), QueueID: queueID, State: "cancelled",
		}, nil
	}
	if stillQueued {
		return jenkinsRunResult{
			Target: cleanOutput(target.Name, token, 80), QueueID: queueID, Queued: true,
		}, nil
	}
	buildNumber := queue.Executable.Number
	pending := func() (jenkinsRunResult, error) {
		return pendingJenkinsInspection(target, token, queueID, buildNumber), nil
	}
	build, logSegment, err := inspectJenkinsBuild(ctx, target, buildNumber, token, client)
	if err != nil {
		return pending()
	}
	nextOffset := logSegment.NextOffset
	moreData := logSegment.MoreData
	state := "completed"
	if build.Building {
		state = "building"
	}
	return jenkinsRunResult{
		Target: cleanOutput(target.Name, token, 80), QueueID: queueID, State: state,
		BuildNumber: buildNumber, Result: safeBuildResult(build.Result), Building: build.Building,
		Log: logSegment.Text, LogTruncated: logSegment.Truncated,
		NextOffset: &nextOffset, MoreData: &moreData,
	}, nil
}

func inspectJenkinsBuild(ctx context.Context, target jenkinsTarget, buildNumber int, token string, client *http.Client) (jenkinsBuildResponse, jenkinsLogSegment, error) {
	if buildNumber <= 0 {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, errors.New("invalid Jenkins build number")
	}
	buildURL, err := jenkinsJobURL(target, strconv.Itoa(buildNumber), "api", "json")
	if err != nil {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, err
	}
	buildResp, err := jenkinsRequest(ctx, client, http.MethodGet, buildURL, target.Username, token)
	if err != nil {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, err
	}
	defer buildResp.Body.Close()
	if buildResp.StatusCode != http.StatusOK {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, jenkinsStatusError(buildResp.StatusCode)
	}
	buildBody, err := readLimited(buildResp.Body, maxAPIBytes)
	if err != nil {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, err
	}
	var build jenkinsBuildResponse
	if json.Unmarshal(buildBody, &build) != nil || build.Number != buildNumber {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, errors.New("Jenkins returned an invalid build response")
	}
	logSegment, err := fetchJenkinsLogSegment(ctx, target, buildNumber, 0, token, client)
	if err != nil {
		return jenkinsBuildResponse{}, jenkinsLogSegment{}, err
	}
	return build, logSegment, nil
}

func (a *app) registeredJenkinsLog(ctx context.Context, requestedName string, buildNumber int, startOffset int64) (jenkinsLogSegmentResult, error) {
	if buildNumber <= 0 || startOffset < 0 {
		return jenkinsLogSegmentResult{}, errors.New("The Jenkins build number must be positive and the log offset nonnegative.")
	}
	target, token, err := a.loadJenkinsTarget(requestedName)
	if err != nil {
		return jenkinsLogSegmentResult{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	segment, err := fetchJenkinsLogSegment(ctx, target, buildNumber, startOffset, string(token), a.client)
	if err != nil {
		return jenkinsLogSegmentResult{}, err
	}
	return jenkinsLogSegmentResult{
		Target: cleanOutput(target.Name, string(token), 80), BuildNumber: buildNumber,
		StartOffset: startOffset, NextOffset: segment.NextOffset,
		MoreData: segment.MoreData, Log: segment.Text, LogTruncated: segment.Truncated,
	}, nil
}

func fetchJenkinsLogSegment(ctx context.Context, target jenkinsTarget, buildNumber int, startOffset int64, token string, client *http.Client) (jenkinsLogSegment, error) {
	if validateJenkinsTarget(target) != nil || buildNumber <= 0 || startOffset < 0 || token == "" || strings.ContainsAny(token, "\r\n\x00") || client == nil {
		return jenkinsLogSegment{}, errors.New("The registered Jenkins log request is invalid.")
	}
	logURL, err := jenkinsJobURL(target, strconv.Itoa(buildNumber), "logText", "progressiveText")
	if err != nil {
		return jenkinsLogSegment{}, errors.New("The registered Jenkins log request is invalid.")
	}
	query := url.Values{}
	query.Set("start", strconv.FormatInt(startOffset, 10))
	logURL.RawQuery = query.Encode()
	resp, err := jenkinsRequest(ctx, client, http.MethodGet, logURL, target.Username, token)
	if err != nil {
		return jenkinsLogSegment{}, errors.New("Could not inspect the registered Jenkins build log.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jenkinsLogSegment{}, jenkinsStatusError(resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxJenkinsLogBytes)
	if err != nil {
		return jenkinsLogSegment{}, errors.New("Jenkins progressive log response exceeded 32 KiB or could not be read.")
	}
	nextOffset, moreData, err := parseProgressiveHeaders(resp.Header, startOffset, len(body))
	if err != nil {
		return jenkinsLogSegment{}, errors.New("Jenkins returned invalid progressive log headers.")
	}
	text := cleanOutput(strings.ToValidUTF8(string(body), "�"), token, maxJenkinsLogBytes)
	if len(text) > maxJenkinsLogBytes {
		return jenkinsLogSegment{}, errors.New("Jenkins sanitized progressive log response exceeded 32 KiB.")
	}
	text, outputTruncated := truncateUTF8(strings.ToValidUTF8(text, "�"), maxJenkinsLogBytes)
	return jenkinsLogSegment{
		Text: text, NextOffset: nextOffset, MoreData: moreData,
		Truncated: moreData || outputTruncated,
	}, nil
}

func parseProgressiveHeaders(header http.Header, startOffset int64, bodyLength int) (int64, bool, error) {
	textSizeValues := header.Values("X-Text-Size")
	if len(textSizeValues) != 1 {
		return 0, false, errors.New("missing or repeated X-Text-Size")
	}
	textSize, err := strconv.ParseInt(textSizeValues[0], 10, 64)
	if err != nil || textSize < 0 || strconv.FormatInt(textSize, 10) != textSizeValues[0] || textSize < startOffset || textSize-startOffset != int64(bodyLength) {
		return 0, false, errors.New("invalid X-Text-Size")
	}
	moreData := false
	moreValues := header.Values("X-More-Data")
	if len(moreValues) > 1 {
		return 0, false, errors.New("repeated X-More-Data")
	}
	if len(moreValues) == 1 {
		switch strings.ToLower(moreValues[0]) {
		case "true":
			moreData = true
		case "false":
		default:
			return 0, false, errors.New("invalid X-More-Data")
		}
	}
	return textSize, moreData, nil
}

func unknownJenkinsTrigger(target jenkinsTarget, token string) jenkinsRunResult {
	return jenkinsRunResult{
		Target:   cleanOutput(target.Name, token, 80),
		State:    "outcome_unknown",
		Guidance: "Do not retry this trigger automatically. Check the Jenkins UI queue and job manually before deciding whether to trigger again.",
	}
}

func pendingJenkinsInspection(target jenkinsTarget, token string, queueID uint64, buildNumber int) jenkinsRunResult {
	return jenkinsRunResult{
		Target: cleanOutput(target.Name, token, 80), QueueID: queueID,
		BuildNumber: buildNumber, State: "inspection_pending",
	}
}

func (a *app) registeredJenkinsQueue(ctx context.Context, requestedName string, queueID uint64) (jenkinsQueueInfo, error) {
	if queueID == 0 {
		return jenkinsQueueInfo{}, errors.New("The Jenkins queue ID must be a positive integer.")
	}
	target, token, err := a.loadJenkinsTarget(requestedName)
	if err != nil {
		return jenkinsQueueInfo{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	queue, err := fetchJenkinsQueue(ctx, target, queueID, string(token), a.client)
	if err != nil {
		return jenkinsQueueInfo{}, err
	}
	state := "queued"
	if queue.Cancelled {
		state = "cancelled"
	} else if queue.Executable != nil {
		state = "started"
	}
	result := jenkinsQueueInfo{
		Target: cleanOutput(target.Name, string(token), 80), QueueID: queueID, State: state,
		Blocked: queue.Blocked, Stuck: queue.Stuck,
		Why: cleanOutput(queue.Why, string(token), 500),
	}
	if queue.Executable != nil {
		result.BuildNumber = queue.Executable.Number
		build, logSegment, err := inspectJenkinsBuild(ctx, target, queue.Executable.Number, string(token), a.client)
		if err != nil {
			result.State = "inspection_pending"
			return result, nil
		}
		result.Result = safeBuildResult(build.Result)
		result.Building = build.Building
		result.Log = logSegment.Text
		result.LogTruncated = logSegment.Truncated
		nextOffset := logSegment.NextOffset
		moreData := logSegment.MoreData
		result.NextOffset = &nextOffset
		result.MoreData = &moreData
		if build.Building {
			result.State = "building"
		} else {
			result.State = "completed"
		}
	}
	return result, nil
}

func waitForJenkinsBuild(ctx context.Context, target jenkinsTarget, queueID uint64, token string, client *http.Client) (jenkinsQueueResponse, bool, error) {
	for poll := 0; poll < jenkinsQueuePolls; poll++ {
		queue, err := fetchJenkinsQueue(ctx, target, queueID, token, client)
		if err != nil {
			return jenkinsQueueResponse{}, false, err
		}
		if queue.Cancelled || queue.Executable != nil {
			if queue.Executable != nil && queue.Executable.Number <= 0 {
				return jenkinsQueueResponse{}, false, errors.New("Jenkins returned an invalid build number.")
			}
			return queue, false, nil
		}
		if poll+1 == jenkinsQueuePolls {
			return queue, true, nil
		}
		timer := time.NewTimer(jenkinsQueueDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return jenkinsQueueResponse{}, false, ctx.Err()
		case <-timer.C:
		}
	}
	return jenkinsQueueResponse{}, false, errors.New("Jenkins queue inspection ended unexpectedly.")
}

func fetchJenkinsQueue(ctx context.Context, target jenkinsTarget, queueID uint64, token string, client *http.Client) (jenkinsQueueResponse, error) {
	if validateJenkinsTarget(target) != nil || queueID == 0 || token == "" || strings.ContainsAny(token, "\r\n\x00") || client == nil {
		return jenkinsQueueResponse{}, errors.New("The registered Jenkins queue request is invalid.")
	}
	queueURL, err := jenkinsQueueURL(target, queueID)
	if err != nil {
		return jenkinsQueueResponse{}, errors.New("The registered Jenkins queue request is invalid.")
	}
	queueURL.Path = strings.TrimRight(queueURL.Path, "/") + "/api/json"
	resp, err := jenkinsRequest(ctx, client, http.MethodGet, queueURL, target.Username, token)
	if err != nil {
		return jenkinsQueueResponse{}, errors.New("Could not inspect the registered Jenkins queue item.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jenkinsQueueResponse{}, jenkinsStatusError(resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return jenkinsQueueResponse{}, errors.New("Jenkins queue response exceeded 1 MiB or could not be read.")
	}
	var queue jenkinsQueueResponse
	if json.Unmarshal(body, &queue) != nil || queue.ID != queueID || queue.Task.Name != lastJobSegment(target.JobPath) || !jenkinsQueueTaskURLMatches(target, queue.Task.URL) {
		return jenkinsQueueResponse{}, errors.New("Jenkins returned a queue item for a different registered job.")
	}
	if queue.Executable != nil && queue.Executable.Number <= 0 {
		return jenkinsQueueResponse{}, errors.New("Jenkins returned an invalid build number.")
	}
	if queue.Cancelled && queue.Executable != nil {
		return jenkinsQueueResponse{}, errors.New("Jenkins returned an inconsistent queue status.")
	}
	return queue, nil
}

func jenkinsQueueURL(target jenkinsTarget, queueID uint64) (*url.URL, error) {
	if validateJenkinsTarget(target) != nil || queueID == 0 {
		return nil, errors.New("invalid Jenkins queue ID")
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/queue/item/" + strconv.FormatUint(queueID, 10)
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}

func jenkinsQueueTaskURLMatches(target jenkinsTarget, raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	taskURL, err := url.Parse(raw)
	if err != nil || taskURL.Opaque != "" || taskURL.User != nil || taskURL.RawQuery != "" || taskURL.Fragment != "" {
		return false
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return false
	}
	if !taskURL.IsAbs() {
		taskURL = base.ResolveReference(taskURL)
	}
	expected, err := jenkinsJobURL(target)
	if err != nil || taskURL.Scheme != "https" || !strings.EqualFold(taskURL.Host, expected.Host) {
		return false
	}
	path := taskURL.EscapedPath()
	expectedPath := expected.EscapedPath()
	return taskURL.RawPath == "" && (path == expectedPath || path == expectedPath+"/")
}

func validateJenkinsBaseURL(raw string) (string, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, "?#") {
		return "", errors.New("invalid Jenkins URL")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Host == "" || u.EscapedPath() != u.Path {
		return "", errors.New("invalid Jenkins URL")
	}
	origin, err := validateOrigin("https://" + u.Host)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(u.Path, "/")
	if basePath == "/" {
		basePath = ""
	}
	if basePath != "" {
		if !strings.HasPrefix(basePath, "/") || strings.Contains(basePath, "//") {
			return "", errors.New("invalid Jenkins context path")
		}
		for _, part := range strings.Split(strings.TrimPrefix(basePath, "/"), "/") {
			if part == "" || part == "." || part == ".." || !safeJenkinsSegment(part) {
				return "", errors.New("invalid Jenkins context path")
			}
		}
	}
	return origin + basePath, nil
}

func jenkinsTriggerApprovalScope(target jenkinsTarget) string {
	scope, _ := json.Marshal(struct {
		Name        string `json:"name"`
		BaseURL     string `json:"base_url"`
		Username    string `json:"username"`
		JobPath     string `json:"job_path"`
		Environment string `json:"environment"`
		SecretRef   string `json:"secret_ref"`
	}{target.Name, target.BaseURL, target.Username, target.JobPath, target.Environment, target.SecretRef})
	hash := sha256.Sum256(scope)
	return hex.EncodeToString(hash[:])
}

func jenkinsTriggerApproved(target jenkinsTarget) bool {
	return target.NonProductionPreapproved && target.NonProductionApprovalScope != "" &&
		target.NonProductionApprovalScope == jenkinsTriggerApprovalScope(target)
}

func validateJenkinsTarget(target jenkinsTarget) error {
	baseURL, err := validateJenkinsBaseURL(target.BaseURL)
	if err != nil || baseURL != target.BaseURL || !jenkinsIDPattern.MatchString(target.ID) || target.Name == "" || len(target.Name) > 80 || strings.ContainsAny(target.Name, "\r\n\x00") || !validJenkinsEnvironmentLabel(target.Environment) || !validJenkinsUsername(target.Username) || !validJenkinsJobPath(target.JobPath) || !secretRefPattern.MatchString(target.SecretRef) {
		return errors.New("invalid Jenkins target")
	}
	return nil
}

func validJenkinsUsername(username string) bool {
	if username == "" || len(username) > 128 || strings.ContainsRune(username, ':') {
		return false
	}
	for _, r := range username {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validJenkinsEnvironmentLabel(environment string) bool {
	return len(environment) <= 80 && strings.IndexFunc(environment, unicode.IsControl) == -1
}

func validJenkinsJobPath(jobPath string) bool {
	if jobPath == "" || len(jobPath) > 512 || strings.HasPrefix(jobPath, "/") || strings.HasSuffix(jobPath, "/") {
		return false
	}
	for _, part := range strings.Split(jobPath, "/") {
		if part == "" || part == "." || part == ".." || !safeJenkinsSegment(part) {
			return false
		}
	}
	return true
}

func safeJenkinsSegment(part string) bool {
	for _, r := range part {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '~') {
			return false
		}
	}
	return true
}

func jenkinsJobURL(target jenkinsTarget, suffix ...string) (*url.URL, error) {
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(target.JobPath, "/")
	path := strings.TrimRight(base.Path, "/")
	for _, part := range parts {
		path += "/job/" + url.PathEscape(part)
	}
	for _, part := range suffix {
		path += "/" + url.PathEscape(part)
	}
	base.Path = path
	base.RawPath = ""
	return base, nil
}

func validateQueueLocation(target jenkinsTarget, triggerURL *url.URL, location string) (*url.URL, uint64, error) {
	if validateJenkinsTarget(target) != nil || triggerURL == nil || location == "" || len(location) > 2048 || strings.ContainsAny(location, "\r\n\x00") {
		return nil, 0, errors.New("invalid queue location")
	}
	reference, err := url.Parse(location)
	if err != nil || reference.User != nil || reference.RawQuery != "" || reference.Fragment != "" || reference.Opaque != "" {
		return nil, 0, errors.New("invalid queue location")
	}
	resolved := triggerURL.ResolveReference(reference)
	base, err := url.Parse(target.BaseURL)
	if err != nil || resolved.Scheme != "https" || !strings.EqualFold(resolved.Host, base.Host) || resolved.User != nil || resolved.RawQuery != "" || resolved.Fragment != "" || resolved.Opaque != "" || resolved.RawPath != "" || resolved.EscapedPath() != resolved.Path {
		return nil, 0, errors.New("invalid queue location")
	}
	prefix := strings.TrimRight(base.Path, "/") + "/queue/item/"
	if !strings.HasPrefix(resolved.Path, prefix) {
		return nil, 0, errors.New("invalid queue location")
	}
	item := strings.TrimPrefix(resolved.Path, prefix)
	item = strings.TrimSuffix(item, "/")
	if item == "" || strings.Contains(item, "/") {
		return nil, 0, errors.New("invalid queue location")
	}
	queueID, err := strconv.ParseUint(item, 10, 64)
	if err != nil || queueID == 0 || strconv.FormatUint(queueID, 10) != item {
		return nil, 0, errors.New("invalid queue location")
	}
	resolved.Path = prefix + item
	resolved.RawPath = ""
	return resolved, queueID, nil
}

func jenkinsRequest(ctx context.Context, client *http.Client, method string, endpoint *url.URL, username, token string) (*http.Response, error) {
	if client == nil || endpoint == nil || username == "" || token == "" {
		return nil, errors.New("invalid Jenkins request")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(username, token)
	req.Header.Set("Accept", "application/json, text/plain")
	req.Header.Set("User-Agent", "local-agent-harness")
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return noRedirect.Do(req)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("response exceeded limit or could not be read")
	}
	return body, nil
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit - len("…")
	for cut > 0 && cut < len(value) && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…", true
}

func jenkinsStatusError(status int) error {
	if status >= 300 && status < 400 {
		return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Jenkins redirected the request. Check the registered base URL and context path."))
	}
	switch status {
	case http.StatusRequestTimeout:
		return withConnectionDiagnostic(connectionFailureTimeout, errors.New("Jenkins request timed out (408)."))
	case http.StatusUnauthorized:
		return withConnectionDiagnostic(connectionFailureAuthentication, errors.New("Jenkins rejected the registered credential (401)."))
	case http.StatusForbidden:
		return withConnectionDiagnostic(connectionFailureAccess, errors.New("Jenkins denied access to the registered job (403)."))
	case http.StatusNotFound:
		return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Jenkins could not find the registered job (404)."))
	case http.StatusTooManyRequests:
		return withConnectionDiagnostic(connectionFailureRateLimit, errors.New("Jenkins rate limited the request (429)."))
	default:
		return fmt.Errorf("Jenkins returned HTTP %d.", status)
	}
}

func safeBuildResult(result string) string {
	switch result {
	case "SUCCESS", "FAILURE", "UNSTABLE", "ABORTED", "NOT_BUILT":
		return result
	default:
		return ""
	}
}

func lastJobSegment(jobPath string) string {
	parts := strings.Split(jobPath, "/")
	return parts[len(parts)-1]
}
