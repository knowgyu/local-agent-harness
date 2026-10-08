package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxSSHSetupUIFormBytes = 128 << 10
	maxSSHSetupUIPageBytes = 512 << 10
)

var errSSHSetupUIRequest = errors.New("SSH settings request was rejected")

type sshSetupUIHandler struct {
	host      string
	csrf      string
	ssh       *sshOperationController
	drafts    SetupDraftController
	templates *template.Template
}

type sshSetupUIPageData struct {
	Language         string
	CSRF             string
	CSPNonce         string
	Revision         string
	Targets          []sshSetupUITarget
	Draft            *SetupDraftReview
	DraftHasMissing  bool
	Message          string
	MessageIsError   bool
	MessageIsWarning bool
}

type sshSetupUITarget struct {
	Definition sshTargetDefinition
	Operations []sshSetupUIOperation
	CSRF       string
	Revision   string
}

type sshSetupUIOperation struct {
	Definition       SSHOperationDefinition
	FixedArgsText    string
	ParametersJSON   string
	ScopeCount       int
	EditApprovalMode OperationApprovalMode
}

func newSSHSetupUIHandler(host, csrf string, ssh *sshOperationController, drafts SetupDraftController) http.Handler {
	templates, err := template.New("ssh_drafts.html").ParseFS(uiFiles, "ui_fragments/ssh_drafts.html")
	if err != nil {
		panic("SSH settings UI template could not be loaded")
	}
	handler := &sshSetupUIHandler{host: host, csrf: csrf, ssh: ssh, drafts: drafts, templates: templates}
	return http.HandlerFunc(handler.serveHTTP)
}

func (h *sshSetupUIHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h.setSecurityHeaders(w)
	if h == nil || h.host == "" || h.csrf == "" || r.Host != h.host {
		http.Error(w, localizeUIMessage(uiLocaleForRequest(r), "Not found"), http.StatusNotFound)
		return
	}
	switch r.URL.Path {
	case "/ssh-settings":
		if r.Method != http.MethodGet {
			h.writeMethodNotAllowed(w, r)
			return
		}
		h.handleSettings(w, r)
	case "/save-ssh-target":
		h.handlePOST(w, r, h.saveTarget)
	case "/delete-ssh-target":
		h.handlePOST(w, r, h.deleteTarget)
	case "/save-ssh-operation":
		h.handlePOST(w, r, h.saveOperation)
	case "/delete-ssh-operation":
		h.handlePOST(w, r, h.deleteOperation)
	case "/review-setup-draft":
		h.handlePOST(w, r, h.reviewDraft)
	case "/approve-setup-draft":
		h.handlePOST(w, r, h.approveDraft)
	case "/reject-setup-draft":
		h.handlePOST(w, r, h.rejectDraft)
	default:
		http.NotFound(w, r)
	}
}

func (h *sshSetupUIHandler) setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-ssh-settings'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", localUIReferrerPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

func (h *sshSetupUIHandler) handleSettings(w http.ResponseWriter, r *http.Request) {
	data := sshSetupUIPageData{Language: string(uiLocaleForRequest(r)), CSRF: h.csrf}
	switch r.URL.Query().Get("state") {
	case "target-saved":
		data.Message = sshUILocalized(r, "SSH target saved.", "SSH 대상을 저장했습니다.")
	case "target-deleted":
		data.Message = sshUILocalized(r, "SSH target deleted.", "SSH 대상을 삭제했습니다.")
	case "operation-saved":
		data.Message = sshUILocalized(r, "SSH operation saved.", "SSH 작업을 저장했습니다.")
	case "operation-deleted":
		data.Message = sshUILocalized(r, "SSH operation deleted.", "SSH 작업을 삭제했습니다.")
	case "draft-approved":
		data.Message = sshUILocalized(r, "The reviewed setup draft was saved.", "검토한 설정 초안을 저장했습니다.")
	case "draft-claim-uncertain":
		data.Message = sshUILocalized(r,
			"This request did not change settings. The draft may have been consumed; check current settings and submit a new draft before retrying.",
			"이 요청은 설정을 변경하지 않았습니다. 초안이 이미 소모됐을 수 있으니 현재 설정을 확인하고 새 초안을 제출한 뒤 다시 진행하세요.")
		data.MessageIsWarning = true
	case "draft-claimed-not-applied":
		data.Message = sshUILocalized(r,
			"The review was consumed, but approval could not be confirmed. Check current settings and submit a new draft if needed.",
			"검토 초안은 소모됐지만 승인을 확인하지 못했습니다. 현재 설정을 확인하고 필요하면 새 초안을 제출하세요.")
		data.MessageIsWarning = true
	case "draft-stale-after-claim":
		data.Message = sshUILocalized(r,
			"Settings changed after the draft was claimed. The draft was not applied; submit a new setup draft.",
			"초안을 소모한 뒤 설정이 변경됐습니다. 초안은 적용되지 않았으므로 새 설정 초안을 제출하세요.")
		data.MessageIsWarning = true
	case "draft-rejected":
		data.Message = sshUILocalized(r, "The setup draft was rejected and discarded.", "설정 초안을 거부하고 폐기했습니다.")
	}
	h.render(w, r, data, http.StatusOK)
}

type sshUIFormHandler func(http.ResponseWriter, *http.Request, url.Values)

func (h *sshSetupUIHandler) handlePOST(w http.ResponseWriter, r *http.Request, next sshUIFormHandler) {
	form, err := h.parsePOST(w, r)
	if err != nil {
		h.renderError(w, r, err, http.StatusForbidden)
		return
	}
	next(w, r, form)
}

func (h *sshSetupUIHandler) parsePOST(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if r.Method != http.MethodPost {
		return nil, errSSHSetupUIRequest
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || len(origins) == 1 && origins[0] != "http://"+h.host {
		return nil, errSSHSetupUIRequest
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return nil, errSSHSetupUIRequest
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSSHSetupUIFormBytes)
	if err := r.ParseForm(); err != nil {
		return nil, errSSHSetupUIRequest
	}
	form := r.PostForm
	for _, values := range form {
		if len(values) != 1 {
			return nil, errSSHSetupUIRequest
		}
	}
	token := form.Get("csrf")
	if len(token) != len(h.csrf) || subtle.ConstantTimeCompare([]byte(token), []byte(h.csrf)) != 1 {
		return nil, errSSHSetupUIRequest
	}
	return form, nil
}

func (h *sshSetupUIHandler) saveTarget(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "target_id", "expected_revision", "name", "alias", "reviewed_alias") || form.Get("reviewed_alias") != "yes" {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	snapshot, err := h.ssh.loadSnapshot(ctx)
	if err != nil || !sshUIValidRevision(snapshot.Revision, form.Get("expected_revision")) {
		h.renderError(w, r, errSSHConfigurationStale, http.StatusConflict)
		return
	}
	targetID := form.Get("target_id")
	target := sshTargetDefinition{ID: targetID, Name: form.Get("name"), Alias: form.Get("alias")}
	if targetID != "" {
		index := findSSHTargetIndex(snapshot.Targets, targetID)
		if index < 0 {
			h.renderError(w, r, errSSHTargetNotFound, http.StatusConflict)
			return
		}
		target.Operations = cloneSSHTarget(snapshot.Targets[index]).Operations
	}
	if _, _, err := h.ssh.SaveTarget(ctx, target, form.Get("expected_revision")); err != nil {
		h.renderError(w, r, err, http.StatusConflict)
		return
	}
	h.redirect(w, r, "target-saved")
}

func (h *sshSetupUIHandler) deleteTarget(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "target_id", "expected_revision", "confirm_delete") || form.Get("confirm_delete") != "yes" || !validSSHDefinitionID(form.Get("target_id")) {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	if _, err := h.ssh.DeleteTarget(r.Context(), form.Get("target_id"), form.Get("expected_revision")); err != nil {
		h.renderError(w, r, err, http.StatusConflict)
		return
	}
	h.redirect(w, r, "target-deleted")
}

func (h *sshSetupUIHandler) saveOperation(w http.ResponseWriter, r *http.Request, form url.Values) {
	fields := []string{"csrf", "target_id", "operation_id", "expected_revision", "name", "summary", "program", "fixed_args", "parameters_json", "risk", "approval_mode", "approved_parameter_sets", "reviewed_command", "confirm_preapproval"}
	if !sshUIHasOnlyFields(form, fields...) || form.Get("reviewed_command") != "yes" {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	snapshot, err := h.ssh.loadSnapshot(ctx)
	if err != nil || !sshUIValidRevision(snapshot.Revision, form.Get("expected_revision")) {
		h.renderError(w, r, errSSHConfigurationStale, http.StatusConflict)
		return
	}
	targetID := form.Get("target_id")
	targetIndex := findSSHTargetIndex(snapshot.Targets, targetID)
	if targetIndex < 0 {
		h.renderError(w, r, errSSHTargetNotFound, http.StatusConflict)
		return
	}
	targetAlias := snapshot.Targets[targetIndex].Alias
	op := SSHOperationDefinition{
		ID:         form.Get("operation_id"),
		Name:       form.Get("name"),
		Summary:    form.Get("summary"),
		Program:    form.Get("program"),
		FixedArgs:  sshUIParseFixedArgs(form.Get("fixed_args")),
		Risk:       SSHRiskClass(form.Get("risk")),
		Parameters: []SSHParameterSpec{},
	}
	if op.ID == "" {
		op.ID, err = newSSHDefinitionID("operation")
		if err != nil {
			h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
			return
		}
	}
	op.Parameters, err = sshUIParseParameters(form.Get("parameters_json"))
	if err != nil {
		h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
		return
	}
	mode := OperationApprovalMode(form.Get("approval_mode"))
	switch op.Risk {
	case SSHRiskReadOnly:
		if mode != OperationApprovalReadOnly {
			h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
			return
		}
	case SSHRiskStateChanging:
		if mode != OperationApprovalPerCall && mode != OperationApprovalPreapproved {
			h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
			return
		}
	case SSHRiskDestructive:
		if mode != OperationApprovalPerCall {
			h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
			return
		}
	default:
		h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
		return
	}
	op.Approval = OperationApprovalPolicy{Mode: mode, Scope: []string{}}
	if mode == OperationApprovalPreapproved {
		if form.Get("confirm_preapproval") != "yes" {
			h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
			return
		}
		op.Approval, err = sshUIBuildPreapproval(ctx, targetID, targetAlias, op, form.Get("approved_parameter_sets"))
		if err != nil {
			h.renderError(w, r, errSSHDefinitionInvalid, http.StatusBadRequest)
			return
		}
	} else if form.Get("approved_parameter_sets") != "" || form.Get("confirm_preapproval") != "" {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	if _, _, err := h.ssh.SaveOperation(ctx, targetID, op, form.Get("expected_revision")); err != nil {
		h.renderError(w, r, err, http.StatusConflict)
		return
	}
	h.redirect(w, r, "operation-saved")
}

func (h *sshSetupUIHandler) deleteOperation(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "target_id", "operation_id", "expected_revision", "confirm_delete") || form.Get("confirm_delete") != "yes" || !validSSHDefinitionID(form.Get("target_id")) || !validSSHDefinitionID(form.Get("operation_id")) {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	if _, err := h.ssh.DeleteOperation(r.Context(), form.Get("target_id"), form.Get("operation_id"), form.Get("expected_revision")); err != nil {
		h.renderError(w, r, err, http.StatusConflict)
		return
	}
	h.redirect(w, r, "operation-deleted")
}

func (h *sshSetupUIHandler) reviewDraft(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "draft_id", "base_revision", "digest") || h.drafts == nil {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	review, err := h.drafts.ReviewSetupDraft(r.Context(), form.Get("draft_id"), form.Get("base_revision"), form.Get("digest"))
	if err != nil {
		h.renderError(w, r, errSetupDraftInvalid, http.StatusConflict)
		return
	}
	data := sshSetupUIPageData{Language: string(uiLocaleForRequest(r)), CSRF: h.csrf, Draft: &review, DraftHasMissing: len(review.Missing) > 0}
	h.render(w, r, data, http.StatusOK)
}

func (h *sshSetupUIHandler) approveDraft(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "draft_id", "base_revision", "digest", "confirm_approve") || form.Get("confirm_approve") != "yes" || h.drafts == nil {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	result, err := h.drafts.ApproveSetupDraft(r.Context(), form.Get("draft_id"), form.Get("base_revision"), form.Get("digest"))
	switch result.NextAction {
	case setupDraftApprovalClaimUncertainNextAction:
		h.redirect(w, r, "draft-claim-uncertain")
		return
	case setupDraftApprovalClaimedWarningNextAction:
		if result.Stale {
			h.redirect(w, r, "draft-stale-after-claim")
		} else {
			h.redirect(w, r, "draft-claimed-not-applied")
		}
		return
	}
	if err != nil {
		h.renderError(w, r, errSetupDraftInvalid, http.StatusConflict)
		return
	}
	if result.Stale {
		h.renderError(w, r, errSetupDraftStale, http.StatusConflict)
		return
	}
	if !result.Applied {
		h.renderError(w, r, errSetupDraftMissing, http.StatusConflict)
		return
	}
	h.redirect(w, r, "draft-approved")
}

func (h *sshSetupUIHandler) rejectDraft(w http.ResponseWriter, r *http.Request, form url.Values) {
	if !sshUIHasOnlyFields(form, "csrf", "draft_id", "base_revision", "digest", "confirm_reject") || form.Get("confirm_reject") != "yes" || h.drafts == nil {
		h.renderError(w, r, errSSHSetupUIRequest, http.StatusBadRequest)
		return
	}
	if err := h.drafts.RejectSetupDraft(r.Context(), form.Get("draft_id"), form.Get("base_revision"), form.Get("digest")); err != nil {
		h.renderError(w, r, errSetupDraftInvalid, http.StatusConflict)
		return
	}
	h.redirect(w, r, "draft-rejected")
}

func (h *sshSetupUIHandler) renderError(w http.ResponseWriter, r *http.Request, cause error, status int) {
	message := sshUILocalized(r, "The request could not be completed. Review current settings and try again.", "요청을 처리하지 못했습니다. 현재 설정을 확인하고 다시 시도하세요.")
	switch {
	case errors.Is(cause, errSSHSetupUIRequest):
		message = sshUILocalized(r, "The request was rejected. Reload the local settings page and try again.", "요청이 거부되었습니다. 로컬 설정 화면을 새로고침한 뒤 다시 시도하세요.")
	case errors.Is(cause, errSSHConfigurationStale), errors.Is(cause, errSetupDraftStale):
		message = sshUILocalized(r, "Settings changed after review. Reload and review the current version.", "검토 후 설정이 바뀌었습니다. 새로고침해 현재 내용을 다시 확인하세요.")
	case errors.Is(cause, errSetupDraftInvalid), errors.Is(cause, errSetupDraftExpired):
		message = sshUILocalized(r, "The setup draft is unavailable, expired, or has already been used.", "설정 초안을 찾을 수 없거나 만료됐거나 이미 사용했습니다.")
	case errors.Is(cause, errSetupDraftMissing):
		message = sshUILocalized(r, "Complete the missing items and submit a new setup draft before approval.", "누락된 항목을 채운 뒤 새 설정 초안을 제출해야 승인할 수 있습니다.")
	case errors.Is(cause, errSSHDefinitionInvalid):
		message = sshUILocalized(r, "The SSH definition is invalid. Check the saved program, argument tokens, and parameter schema.", "SSH 정의가 올바르지 않습니다. 저장할 프로그램, 인자 토큰, 매개변수 스키마를 확인하세요.")
	}
	h.render(w, r, sshSetupUIPageData{Language: string(uiLocaleForRequest(r)), CSRF: h.csrf, Message: message, MessageIsError: true}, status)
}

func (h *sshSetupUIHandler) render(w http.ResponseWriter, r *http.Request, data sshSetupUIPageData, status int) {
	if data.Language == "" {
		data.Language = string(uiLocaleForRequest(r))
	}
	data.CSRF = h.csrf
	nonce, err := newSSHUINonce()
	if err != nil {
		http.Error(w, sshUILocalized(r, "Could not render local settings.", "로컬 설정을 표시할 수 없습니다."), http.StatusInternalServerError)
		return
	}
	data.CSPNonce = nonce
	if h.ssh != nil {
		snapshot, snapshotErr := h.ssh.loadSnapshot(r.Context())
		if snapshotErr != nil {
			if data.Message == "" {
				data.Message = sshUILocalized(r, "SSH settings are unavailable.", "SSH 설정을 사용할 수 없습니다.")
				data.MessageIsError = true
			}
		} else {
			data.Revision = snapshot.Revision
			data.Targets = make([]sshSetupUITarget, 0, len(snapshot.Targets))
			for _, target := range snapshot.Targets {
				view := sshSetupUITarget{Definition: cloneSSHTarget(target), Operations: make([]sshSetupUIOperation, 0, len(target.Operations)), CSRF: h.csrf, Revision: snapshot.Revision}
				for _, operation := range target.Operations {
					parameters, marshalErr := json.MarshalIndent(operation.Parameters, "", "  ")
					if marshalErr != nil {
						continue
					}
					editMode := operation.Approval.Mode
					if editMode == OperationApprovalPreapproved {
						editMode = OperationApprovalPerCall
					}
					view.Operations = append(view.Operations, sshSetupUIOperation{
						Definition: cloneSSHOperation(operation), FixedArgsText: strings.Join(operation.FixedArgs, "\n"),
						ParametersJSON: string(parameters), ScopeCount: len(operation.Approval.Scope), EditApprovalMode: editMode,
					})
				}
				data.Targets = append(data.Targets, view)
			}
		}
	}
	var body bytes.Buffer
	if err := h.templates.ExecuteTemplate(&body, "ssh-settings-page", data); err != nil || body.Len() > maxSSHSetupUIPageBytes {
		http.Error(w, sshUILocalized(r, "Could not render local settings.", "로컬 설정을 표시할 수 없습니다."), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'nonce-"+nonce+"'"))
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

func (h *sshSetupUIHandler) redirect(w http.ResponseWriter, r *http.Request, state string) {
	http.Redirect(w, r, "/ssh-settings?state="+state, http.StatusSeeOther)
}

func (h *sshSetupUIHandler) writeMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	http.Error(w, sshUILocalized(r, "Method not allowed.", "허용되지 않은 요청 방식입니다."), http.StatusMethodNotAllowed)
}

func sshUIHasOnlyFields(form url.Values, allowed ...string) bool {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	for name, values := range form {
		if _, ok := allow[name]; !ok || len(values) != 1 {
			return false
		}
	}
	_, hasCSRF := form["csrf"]
	return hasCSRF
}

func sshUIValidRevision(current, submitted string) bool {
	return current != "" && current == submitted && validSetupDraftRevision(submitted)
}

func sshUIParseFixedArgs(value string) []string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	arguments := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		arguments = append(arguments, line)
	}
	return arguments
}

func sshUIParseParameters(value string) ([]SSHParameterSpec, error) {
	if len(value) > 12<<10 || rejectDuplicateSetupDraftJSONKeys([]byte(value)) != nil {
		return nil, errSSHDefinitionInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var parameters []SSHParameterSpec
	if err := decoder.Decode(&parameters); err != nil {
		return nil, errSSHDefinitionInvalid
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errSSHDefinitionInvalid
	}
	if parameters == nil {
		return nil, errSSHDefinitionInvalid
	}
	return parameters, nil
}

func sshUIBuildPreapproval(ctx context.Context, targetID, targetAlias string, operation SSHOperationDefinition, raw string) (OperationApprovalPolicy, error) {
	if len(raw) == 0 || len(raw) > 48<<10 || rejectDuplicateSetupDraftJSONKeys([]byte(raw)) != nil {
		return OperationApprovalPolicy{}, errSSHDefinitionInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var parameterSets []map[string]json.RawMessage
	if err := decoder.Decode(&parameterSets); err != nil {
		return OperationApprovalPolicy{}, errSSHDefinitionInvalid
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF || len(parameterSets) == 0 || len(parameterSets) > maxSSHTargetOperations {
		return OperationApprovalPolicy{}, errSSHDefinitionInvalid
	}
	validated := cloneSSHOperation(operation)
	validated.Approval = OperationApprovalPolicy{Mode: OperationApprovalPerCall, Scope: []string{}}
	validated, err := normalizeSSHOperationDefinition(validated, targetID)
	if err != nil {
		return OperationApprovalPolicy{}, errSSHDefinitionInvalid
	}
	scopes := make([]string, 0, len(parameterSets))
	seen := make(map[string]struct{}, len(parameterSets))
	for _, values := range parameterSets {
		if err := checkSSHContext(ctx); err != nil {
			return OperationApprovalPolicy{}, errSSHDefinitionInvalid
		}
		prepared, err := prepareSSHInvocation(targetID, targetAlias, validated, values)
		if err != nil {
			return OperationApprovalPolicy{}, errSSHDefinitionInvalid
		}
		if _, exists := seen[prepared.scope]; exists {
			return OperationApprovalPolicy{}, errSSHDefinitionInvalid
		}
		seen[prepared.scope] = struct{}{}
		scopes = append(scopes, prepared.scope)
	}
	return OperationApprovalPolicy{Mode: OperationApprovalPreapproved, Scope: scopes}, nil
}

func sshUILocalized(r *http.Request, english, korean string) string {
	if uiLocaleForRequest(r) == uiLocaleKorean {
		return korean
	}
	return english
}

func newSSHUINonce() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
