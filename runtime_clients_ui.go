package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxRuntimeClientsUIRequestBytes  = 4 << 10
	maxRuntimeClientsUIResponseBytes = 32 << 10
	runtimeClientsUIOperationLimit   = 10 * time.Second
	runtimeClientsUIPlanLifetime     = 10 * time.Minute
	runtimeClientsUIPlanLimit        = 12

	runtimeClientsUIStatusPath  = "/runtime-status"
	runtimeClientsUIPlanPath    = "/client-registration-plan"
	runtimeClientsUIBackupPath  = "/client-registration-backup"
	runtimeClientsUIApplyPath   = "/client-registration-apply"
	runtimeClientsUIVerifyPath  = "/client-registration-verify"
	runtimeClientsUIRestorePath = "/client-registration-restore"
)

var safeRuntimeClientsUIIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var safeRuntimeClientsUIRevision = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var safeRuntimeClientsUIVersion = regexp.MustCompile(`^[vV]?[0-9][A-Za-z0-9.+_-]{0,63}$`)

type runtimeClientsUIClientView struct {
	ClientID       string `json:"client_id"`
	Supported      bool   `json:"supported"`
	Installed      string `json:"installed"`
	Version        string `json:"version,omitempty"`
	Registration   string `json:"registration"`
	Connection     string `json:"connection"`
	ToolCall       string `json:"tool_call"`
	Backup         string `json:"backup"`
	EvidenceSource string `json:"evidence_source"`
}

type runtimeClientsUIRuntimeView struct {
	State     string `json:"state"`
	ChangedAt string `json:"changed_at,omitempty"`
}

type runtimeClientsUIPlanView struct {
	Token        string   `json:"token"`
	ClientID     string   `json:"client_id"`
	Transport    string   `json:"transport"`
	DisplayName  string   `json:"display_name"`
	Scope        []string `json:"scope"`
	BaseRevision string   `json:"base_revision"`
	Changes      []string `json:"changes"`
	NoChange     bool     `json:"no_change"`
}

type runtimeClientsUIBackupView struct {
	Status    string `json:"status"`
	CreatedAt string `json:"created_at,omitempty"`
}

type runtimeClientsUIResponse struct {
	Runtime          *runtimeClientsUIRuntimeView `json:"runtime,omitempty"`
	ResidentEndpoint *ResidentEndpointObservation `json:"resident_endpoint,omitempty"`
	Clients          []runtimeClientsUIClientView `json:"clients,omitempty"`
	Client           *runtimeClientsUIClientView  `json:"client,omitempty"`
	Plan             *runtimeClientsUIPlanView    `json:"plan,omitempty"`
	Backup           *runtimeClientsUIBackupView  `json:"backup,omitempty"`
	AlreadyApplied   bool                         `json:"already_applied,omitempty"`
	ErrorCode        string                       `json:"error_code,omitempty"`
}

type runtimeClientsUIPlanState struct {
	plan      MCPClientRegistrationPlan
	spec      MCPRegistrationSpec
	createdAt time.Time
	applying  bool
	applied   *MCPClientStatus
}

type runtimeClientsUI struct {
	runtime          MCPRuntimeController
	registration     MCPClientRegistrationController
	residentObserver ResidentEndpointObservationProvider
	csrf             string
	host             string
	specs            map[string]MCPRegistrationSpec
	classify         func(error) string
	now              func() time.Time
	mu               sync.Mutex
	plans            map[string]*runtimeClientsUIPlanState
	backups          map[string]MCPClientBackupReceipt
}

type runtimeClientsUIErrorClassifier func(error) string

// runtimeClientsUIRegistrationSpecs builds the only values the browser may
// select: installed CLI names map to this application's fixed stdio entry.
func runtimeClientsUIRegistrationSpecs(executable string) map[string]MCPRegistrationSpec {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return map[string]MCPRegistrationSpec{}
	}

	specs := make(map[string]MCPRegistrationSpec, 3)
	for _, clientID := range []string{"codex", "claude", "gemini"} {
		specs[clientID] = MCPRegistrationSpec{
			ClientID:    clientID,
			DisplayName: "local-agent-harness",
			Transport:   MCPTransportStdio,
			Command:     executable,
			Args:        []string{"--mcp"},
			Scope:       []string{"user"},
		}
	}
	return specs
}

func newRuntimeClientsUIHandler(
	runtime MCPRuntimeController,
	registration MCPClientRegistrationController,
	csrf string,
	host string,
	executable string,
	classify runtimeClientsUIErrorClassifier,
) http.Handler {
	return newRuntimeClientsUIHandlerWithResidentObserver(
		runtime,
		registration,
		csrf,
		host,
		executable,
		classify,
		nil,
	)
}

func newRuntimeClientsUIHandlerWithResidentObserver(
	runtime MCPRuntimeController,
	registration MCPClientRegistrationController,
	csrf string,
	host string,
	executable string,
	classify runtimeClientsUIErrorClassifier,
	observer ResidentEndpointObservationProvider,
) http.Handler {
	return &runtimeClientsUI{
		runtime:          runtime,
		registration:     registration,
		residentObserver: observer,
		csrf:             csrf,
		host:             host,
		specs:            runtimeClientsUIRegistrationSpecs(executable),
		classify:         classify,
		now:              time.Now,
		plans:            map[string]*runtimeClientsUIPlanState{},
		backups:          map[string]MCPClientBackupReceipt{},
	}
}

func (u *runtimeClientsUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u == nil || r.Host != u.host {
		writeRuntimeClientsUIJSON(w, http.StatusNotFound, runtimeClientsUIResponse{ErrorCode: "not_found"})
		return
	}

	switch r.URL.Path {
	case runtimeClientsUIStatusPath:
		u.handleStatus(w, r)
	case runtimeClientsUIPlanPath:
		u.handlePlan(w, r)
	case runtimeClientsUIBackupPath:
		u.handleBackup(w, r)
	case runtimeClientsUIApplyPath:
		u.handleApply(w, r)
	case runtimeClientsUIVerifyPath:
		u.handleVerify(w, r)
	case runtimeClientsUIRestorePath:
		u.handleRestore(w, r)
	default:
		writeRuntimeClientsUIJSON(w, http.StatusNotFound, runtimeClientsUIResponse{ErrorCode: "not_found"})
	}
}

func (u *runtimeClientsUI) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !u.checkGet(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	runtimeView := &runtimeClientsUIRuntimeView{State: "unavailable"}
	if u.runtime != nil {
		if runtimeStatus, err := u.runtime.Status(ctx); err == nil {
			runtimeView = runtimeClientsUIRuntimeStatus(runtimeStatus)
		}
	}

	clients := make([]runtimeClientsUIClientView, 0, 3)
	for _, clientID := range []string{"codex", "claude", "gemini"} {
		if u.registration == nil {
			clients = append(clients, u.unknownClient(clientID))
			continue
		}
		status, inspectErr := u.registration.InspectClient(ctx, clientID)
		if inspectErr != nil {
			clients = append(clients, u.unknownClient(clientID))
			continue
		}
		clients = append(clients, u.clientView(clientID, status))
	}
	response := runtimeClientsUIResponse{Runtime: runtimeView, Clients: clients}
	if u.residentObserver != nil {
		observeCtx, observeCancel := context.WithTimeout(r.Context(), residentMCPObserverTimeout)
		observation := u.residentObserver.ObserveResidentEndpoint(observeCtx)
		observeCancel()
		response.ResidentEndpoint = &observation
	}
	writeRuntimeClientsUIJSON(w, http.StatusOK, response)
}

func (u *runtimeClientsUI) handlePlan(w http.ResponseWriter, r *http.Request) {
	form, ok := u.checkPost(w, r, "client_id")
	if !ok {
		return
	}
	clientID := form.Get("client_id")
	spec, ok := u.specs[clientID]
	if !ok {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "unsupported_client"})
		return
	}
	if u.registration == nil {
		writeRuntimeClientsUIJSON(w, http.StatusServiceUnavailable, runtimeClientsUIResponse{ErrorCode: "registration_unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	if !u.requireInstalled(ctx, w, clientID) {
		return
	}
	plan, err := u.registration.PlanClientRegistration(ctx, spec)
	if err != nil {
		u.writeControllerError(w, err)
		return
	}
	if !u.validPlan(plan, clientID, spec) {
		writeRuntimeClientsUIJSON(w, http.StatusInternalServerError, runtimeClientsUIResponse{ErrorCode: "failed"})
		return
	}

	token, err := runtimeClientsUINewID()
	if err != nil {
		writeRuntimeClientsUIJSON(w, http.StatusInternalServerError, runtimeClientsUIResponse{ErrorCode: "failed"})
		return
	}
	state := &runtimeClientsUIPlanState{plan: plan, spec: spec, createdAt: u.now()}
	u.mu.Lock()
	u.prunePlansLocked()
	u.plans[token] = state
	u.mu.Unlock()

	changes := append([]string{}, plan.Changes...)
	writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{
		Plan: &runtimeClientsUIPlanView{
			Token: token, ClientID: clientID, Transport: string(spec.Transport), DisplayName: spec.DisplayName,
			Scope: append([]string{}, spec.Scope...), BaseRevision: plan.BaseRevision,
			Changes: changes, NoChange: len(changes) == 1 && changes[0] == "registration_unchanged",
		},
	})
}

func (u *runtimeClientsUI) handleBackup(w http.ResponseWriter, r *http.Request) {
	form, ok := u.checkPost(w, r, "client_id")
	if !ok {
		return
	}
	clientID := form.Get("client_id")
	if _, ok := u.specs[clientID]; !ok {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "unsupported_client"})
		return
	}
	if u.registration == nil {
		writeRuntimeClientsUIJSON(w, http.StatusServiceUnavailable, runtimeClientsUIResponse{ErrorCode: "registration_unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	if !u.requireInstalled(ctx, w, clientID) {
		return
	}
	receipt, err := u.registration.BackupClientRegistration(ctx, clientID)
	if err != nil {
		u.writeControllerError(w, err)
		return
	}
	if !safeRuntimeClientsUIIdentifier.MatchString(receipt.ID) || receipt.Status != "created" || receipt.CreatedAt.IsZero() {
		writeRuntimeClientsUIJSON(w, http.StatusInternalServerError, runtimeClientsUIResponse{ErrorCode: "failed"})
		return
	}
	u.mu.Lock()
	u.backups[clientID] = receipt
	u.mu.Unlock()
	writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{
		Backup: &runtimeClientsUIBackupView{Status: "created", CreatedAt: receipt.CreatedAt.UTC().Format(time.RFC3339)},
	})
}

func (u *runtimeClientsUI) handleApply(w http.ResponseWriter, r *http.Request) {
	form, ok := u.checkPost(w, r, "plan_token")
	if !ok {
		return
	}
	if u.registration == nil {
		writeRuntimeClientsUIJSON(w, http.StatusServiceUnavailable, runtimeClientsUIResponse{ErrorCode: "registration_unavailable"})
		return
	}
	token := form.Get("plan_token")
	u.mu.Lock()
	u.prunePlansLocked()
	planState := u.plans[token]
	if planState == nil {
		u.mu.Unlock()
		writeRuntimeClientsUIJSON(w, http.StatusConflict, runtimeClientsUIResponse{ErrorCode: "plan_expired"})
		return
	}
	if planState.applied != nil {
		status := *planState.applied
		u.mu.Unlock()
		view := u.clientView(planState.plan.ClientID, status)
		writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{Client: &view, AlreadyApplied: true})
		return
	}
	if planState.applying {
		u.mu.Unlock()
		writeRuntimeClientsUIJSON(w, http.StatusConflict, runtimeClientsUIResponse{ErrorCode: "apply_in_progress"})
		return
	}
	if _, backedUp := u.backups[planState.plan.ClientID]; !backedUp {
		u.mu.Unlock()
		writeRuntimeClientsUIJSON(w, http.StatusConflict, runtimeClientsUIResponse{ErrorCode: "backup_required"})
		return
	}
	planState.applying = true
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	if !u.requireInstalled(ctx, w, planState.plan.ClientID) {
		u.resetPlanApplying(token)
		return
	}
	status, err := u.registration.ApplyClientRegistration(ctx, planState.plan.ID, planState.plan.BaseRevision)
	if err != nil {
		u.resetPlanApplying(token)
		code := u.controllerErrorCode(err)
		if code == "stale_settings" || code == "plan_expired" || code == "backup_stale" {
			u.mu.Lock()
			delete(u.plans, token)
			delete(u.backups, planState.plan.ClientID)
			u.mu.Unlock()
		}
		writeRuntimeClientsUIJSON(w, runtimeClientsUIErrorStatus(code), runtimeClientsUIResponse{ErrorCode: code})
		return
	}
	view := u.clientView(planState.plan.ClientID, status)
	u.mu.Lock()
	if current := u.plans[token]; current == planState {
		current.applied = &status
		current.applying = false
	}
	u.mu.Unlock()
	writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{Client: &view})
}

func (u *runtimeClientsUI) handleVerify(w http.ResponseWriter, r *http.Request) {
	form, ok := u.checkPost(w, r, "client_id")
	if !ok {
		return
	}
	clientID := form.Get("client_id")
	if _, ok := u.specs[clientID]; !ok {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "unsupported_client"})
		return
	}
	if u.registration == nil {
		writeRuntimeClientsUIJSON(w, http.StatusServiceUnavailable, runtimeClientsUIResponse{ErrorCode: "registration_unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	if !u.requireInstalled(ctx, w, clientID) {
		return
	}
	status, err := u.registration.VerifyClientRegistration(ctx, clientID)
	if err != nil {
		u.writeControllerError(w, err)
		return
	}
	view := u.clientView(clientID, status)
	writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{Client: &view})
}

func (u *runtimeClientsUI) handleRestore(w http.ResponseWriter, r *http.Request) {
	form, ok := u.checkPost(w, r, "client_id", "confirm")
	if !ok {
		return
	}
	clientID := form.Get("client_id")
	if form.Get("confirm") != "restore" {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "confirmation_required"})
		return
	}
	if _, ok := u.specs[clientID]; !ok {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "unsupported_client"})
		return
	}
	if u.registration == nil {
		writeRuntimeClientsUIJSON(w, http.StatusServiceUnavailable, runtimeClientsUIResponse{ErrorCode: "registration_unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runtimeClientsUIOperationLimit)
	defer cancel()
	if !u.requireInstalled(ctx, w, clientID) {
		return
	}
	u.mu.Lock()
	receipt, backedUp := u.backups[clientID]
	u.mu.Unlock()
	if !backedUp {
		writeRuntimeClientsUIJSON(w, http.StatusConflict, runtimeClientsUIResponse{ErrorCode: "backup_not_found"})
		return
	}
	status, err := u.registration.RestoreClientRegistration(ctx, clientID, receipt.ID)
	if err != nil {
		code := u.controllerErrorCode(err)
		writeRuntimeClientsUIJSON(w, runtimeClientsUIErrorStatus(code), runtimeClientsUIResponse{ErrorCode: code})
		return
	}
	view := u.clientView(clientID, status)
	writeRuntimeClientsUIJSON(w, http.StatusOK, runtimeClientsUIResponse{Client: &view})
}

func (u *runtimeClientsUI) checkGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeRuntimeClientsUIJSON(w, http.StatusMethodNotAllowed, runtimeClientsUIResponse{ErrorCode: "method_not_allowed"})
		return false
	}
	if r.URL.RawQuery != "" {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
		return false
	}
	return true
}

func (u *runtimeClientsUI) checkPost(w http.ResponseWriter, r *http.Request, fields ...string) (url.Values, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRuntimeClientsUIJSON(w, http.StatusMethodNotAllowed, runtimeClientsUIResponse{ErrorCode: "method_not_allowed"})
		return nil, false
	}
	origins := r.Header.Values("Origin")
	if u.csrf == "" || len(origins) != 1 || origins[0] != "http://"+u.host {
		writeRuntimeClientsUIJSON(w, http.StatusForbidden, runtimeClientsUIResponse{ErrorCode: "request_rejected"})
		return nil, false
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || r.URL.RawQuery != "" || r.MultipartForm != nil {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
		return nil, false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRuntimeClientsUIRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
		return nil, false
	}
	if len(r.PostForm["csrf"]) != 1 || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(u.csrf)) != 1 {
		writeRuntimeClientsUIJSON(w, http.StatusForbidden, runtimeClientsUIResponse{ErrorCode: "request_rejected"})
		return nil, false
	}
	for field := range r.PostForm {
		if field != "csrf" && !runtimeClientsUIHasField(fields, field) {
			writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
			return nil, false
		}
	}
	if len(r.PostForm) != len(fields)+1 {
		writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
		return nil, false
	}
	for _, field := range fields {
		if len(r.PostForm[field]) != 1 {
			writeRuntimeClientsUIJSON(w, http.StatusBadRequest, runtimeClientsUIResponse{ErrorCode: "invalid_request"})
			return nil, false
		}
	}
	return r.PostForm, true
}

func (u *runtimeClientsUI) requireInstalled(ctx context.Context, w http.ResponseWriter, clientID string) bool {
	status, err := u.registration.InspectClient(ctx, clientID)
	if err != nil {
		u.writeControllerError(w, err)
		return false
	}
	if status.Installed != "installed" {
		writeRuntimeClientsUIJSON(w, http.StatusConflict, runtimeClientsUIResponse{ErrorCode: "client_not_installed"})
		return false
	}
	return true
}

func (u *runtimeClientsUI) validPlan(plan MCPClientRegistrationPlan, clientID string, spec MCPRegistrationSpec) bool {
	if plan.ClientID != clientID || !safeRuntimeClientsUIIdentifier.MatchString(plan.ID) ||
		!safeRuntimeClientsUIRevision.MatchString(plan.BaseRevision) || spec.Transport != MCPTransportStdio ||
		spec.Endpoint != "" || len(spec.Args) != 1 || spec.Args[0] != "--mcp" ||
		len(spec.Scope) != 1 || spec.Scope[0] != "user" {
		return false
	}
	if len(plan.Changes) != 1 {
		return false
	}
	return plan.Changes[0] == "registration_added" || plan.Changes[0] == "registration_unchanged"
}

func (u *runtimeClientsUI) clientView(clientID string, status MCPClientStatus) runtimeClientsUIClientView {
	view := runtimeClientsUIClientView{
		ClientID:       clientID,
		Supported:      u.validSpec(clientID),
		Installed:      safeRuntimeClientsUIInstalled(status.Installed),
		Registration:   safeRuntimeClientsUIRegistration(status.Registration),
		Connection:     safeRuntimeClientsUIConnection(status.Connection),
		ToolCall:       safeRuntimeClientsUIToolCall(status.ToolCall),
		Backup:         safeRuntimeClientsUIBackup(status.Backup),
		EvidenceSource: safeRuntimeClientsUIEvidence(status.EvidenceSource),
	}
	if view.Installed == "installed" && safeRuntimeClientsUIVersion.MatchString(status.Version) {
		view.Version = status.Version
	}
	return view
}

func (u *runtimeClientsUI) unknownClient(clientID string) runtimeClientsUIClientView {
	return runtimeClientsUIClientView{
		ClientID: clientID, Supported: u.validSpec(clientID), Installed: "unknown",
		Registration: "unknown", Connection: "unverified", ToolCall: "unverified",
		Backup: "not_created", EvidenceSource: "unknown",
	}
}

func (u *runtimeClientsUI) validSpec(clientID string) bool {
	spec, ok := u.specs[clientID]
	return ok && spec.ClientID == clientID && spec.DisplayName == "local-agent-harness" &&
		spec.Transport == MCPTransportStdio && spec.Endpoint == "" &&
		filepath.IsAbs(spec.Command) && len(spec.Args) == 1 && spec.Args[0] == "--mcp" &&
		len(spec.Scope) == 1 && spec.Scope[0] == "user"
}

func (u *runtimeClientsUI) controllerErrorCode(err error) string {
	if err == nil {
		return "failed"
	}
	if u.classify != nil {
		if code := safeRuntimeClientsUIErrorCode(u.classify(err)); code != "failed" {
			return code
		}
	}
	return "failed"
}

func (u *runtimeClientsUI) writeControllerError(w http.ResponseWriter, err error) {
	code := u.controllerErrorCode(err)
	writeRuntimeClientsUIJSON(w, runtimeClientsUIErrorStatus(code), runtimeClientsUIResponse{ErrorCode: code})
}

func (u *runtimeClientsUI) prunePlansLocked() {
	now := u.now()
	for token, plan := range u.plans {
		if now.Sub(plan.createdAt) > runtimeClientsUIPlanLifetime {
			delete(u.plans, token)
		}
	}
	for len(u.plans) >= runtimeClientsUIPlanLimit {
		var oldestToken string
		var oldest time.Time
		for token, plan := range u.plans {
			if oldestToken == "" || plan.createdAt.Before(oldest) {
				oldestToken, oldest = token, plan.createdAt
			}
		}
		if oldestToken == "" {
			return
		}
		delete(u.plans, oldestToken)
	}
}

func (u *runtimeClientsUI) resetPlanApplying(token string) {
	u.mu.Lock()
	if plan := u.plans[token]; plan != nil {
		plan.applying = false
	}
	u.mu.Unlock()
}

func runtimeClientsUINewID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func runtimeClientsUIRuntimeStatus(status MCPRuntimeStatus) *runtimeClientsUIRuntimeView {
	state := "unknown"
	switch status.State {
	case "running":
		state = "running"
	case "stopped":
		state = "stopped"
	case "error":
		state = "error"
	}
	view := &runtimeClientsUIRuntimeView{State: state}
	if !status.ChangedAt.IsZero() {
		view.ChangedAt = status.ChangedAt.UTC().Format(time.RFC3339)
	}
	return view
}

func safeRuntimeClientsUIInstalled(value string) string {
	switch value {
	case "installed", "installed_cli_not_observed":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeClientsUIRegistration(value string) string {
	switch value {
	case "registered", "not_registered", "conflict", "not_observed":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeClientsUIConnection(value string) string {
	if value == "unverified" {
		return value
	}
	return "unknown"
}

func safeRuntimeClientsUIToolCall(value string) string {
	if value == "unverified" {
		return value
	}
	return "unknown"
}

func safeRuntimeClientsUIBackup(value string) string {
	switch value {
	case "not_created", "created", "applying", "applied", "restored", "stale":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeClientsUIEvidence(value string) string {
	if value == "config_observed" || value == "not_observed" {
		return value
	}
	return "unknown"
}

func safeRuntimeClientsUIErrorCode(value string) string {
	switch value {
	case "client_unavailable", "stale_settings", "plan_expired", "backup_required", "backup_not_found", "backup_stale",
		"name_conflict", "invalid_spec", "unsupported_transport", "config_too_large":
		return value
	default:
		return "failed"
	}
}

func runtimeClientsUIErrorStatus(code string) int {
	switch code {
	case "stale_settings", "plan_expired", "backup_required", "backup_not_found", "backup_stale", "name_conflict", "client_not_installed", "apply_in_progress", "already_applied":
		return http.StatusConflict
	case "unsupported_transport", "invalid_spec", "unsupported_client", "invalid_request", "confirmation_required":
		return http.StatusBadRequest
	case "registration_unavailable":
		return http.StatusServiceUnavailable
	case "client_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func runtimeClientsUIHasField(fields []string, target string) bool {
	for _, field := range fields {
		if field == target {
			return true
		}
	}
	return false
}

func writeRuntimeClientsUIJSON(w http.ResponseWriter, status int, value runtimeClientsUIResponse) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > maxRuntimeClientsUIResponseBytes {
		status = http.StatusInternalServerError
		data = []byte(`{"error_code":"failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
