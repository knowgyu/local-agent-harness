package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	onboardingDraftsPath       = "/onboarding/drafts"
	onboardingDraftItemPath    = "/onboarding/drafts/"
	onboardingReadReceiptsPath = "/onboarding/read-receipts"

	maxOnboardingUIRequestBytes  = 16 << 10
	maxOnboardingUIResponseBytes = 256 << 10
)

type onboardingDraftResponseDTO struct {
	Draft onboardingDraft `json:"draft"`
}

// onboardingDraftResponseDraftDTO is the allowlisted detail projection. The
// recovery flag is derived by the store from durable pending state and is not
// part of the sidecar draft record itself.
type onboardingDraftResponseDraftDTO struct {
	ID                         string                  `json:"id"`
	Revision                   uint64                  `json:"revision"`
	CheckGeneration            uint64                  `json:"check_generation"`
	Service                    onboardingServiceKind   `json:"service"`
	Fields                     onboardingDraftFields   `json:"fields"`
	CreatedAt                  time.Time               `json:"created_at"`
	UpdatedAt                  time.Time               `json:"updated_at"`
	Check                      *onboardingCheckReceipt `json:"check,omitempty"`
	ActivationRecoveryRequired bool                    `json:"activation_recovery_required"`
}

func (response onboardingDraftResponseDTO) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Draft onboardingDraftResponseDraftDTO `json:"draft"`
	}{Draft: onboardingDraftResponseDraft(response.Draft)})
}

type onboardingDraftDeleteResponseDTO struct {
	Deleted bool `json:"deleted"`
}

type onboardingPolicyReviewResponseDTO struct {
	Review onboardingPolicyReview `json:"review"`
}

type onboardingDraftActivateResponseDTO struct {
	Activation onboardingActivationRecord `json:"activation"`
}

type onboardingUIErrorDTO struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

type onboardingUIHandler struct {
	host       string
	csrf       string
	controller onboardingController
}

func newOnboardingUIHandler(host, csrf string, controller onboardingController) http.Handler {
	handler := &onboardingUIHandler{host: host, csrf: csrf, controller: controller}
	return http.HandlerFunc(handler.serveHTTP)
}

func (h *onboardingUIHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	setOnboardingUIHeaders(w)
	if h == nil || r == nil || h.host == "" || r.Host != h.host {
		h.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !onboardingUIReadOriginAllowed(r, h.host) {
		h.writeError(w, http.StatusForbidden, "request_rejected")
		return
	}

	switch {
	case r.URL.Path == onboardingDraftsPath:
		h.serveDraftCollection(w, r)
	case r.URL.Path == onboardingReadReceiptsPath:
		h.serveFirstReads(w, r)
	case strings.HasPrefix(r.URL.Path, onboardingDraftItemPath):
		h.serveDraftItem(w, r, strings.TrimPrefix(r.URL.Path, onboardingDraftItemPath))
	default:
		h.writeError(w, http.StatusNotFound, "not_found")
	}
}

func (h *onboardingUIHandler) serveDraftCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !onboardingUIReadRequestValid(r) {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if h.controller == nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		drafts, err := h.controller.ListDrafts(r.Context())
		if err != nil {
			h.writeControllerError(w, err)
			return
		}
		if len(drafts) > 50 {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if err := validateOnboardingDraftsForResponse(drafts); err != nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		h.writeJSON(w, http.StatusOK, onboardingDraftListResponse{Drafts: onboardingDraftListItems(drafts)})
	case http.MethodPost:
		data, ok := h.readMutationJSON(w, r)
		if !ok {
			return
		}
		defer clear(data)
		var request onboardingDraftCreateRequestDTO
		if !decodeOnboardingJSON(data, &request) {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if h.controller == nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		draft, err := h.controller.SaveDraft(r.Context(), onboardingDraftSaveRequest{
			Service: request.Service,
			Fields:  request.Fields,
		})
		if err != nil {
			h.writeControllerError(w, err)
			return
		}
		if err := validateOnboardingDraftForResponse(draft); err != nil {
			h.writeError(w, http.StatusInternalServerError, "unavailable")
			return
		}
		h.writeJSON(w, http.StatusCreated, onboardingDraftResponseDTO{Draft: draft})
	default:
		h.writeMethodNotAllowed(w, "GET, POST")
	}
}

func (h *onboardingUIHandler) serveDraftItem(w http.ResponseWriter, r *http.Request, remainder string) {
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || !validOnboardingDraftID(parts[0]) {
		h.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		h.serveDraft(w, r, id)
		return
	}
	if len(parts) != 2 {
		h.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	switch parts[1] {
	case "check":
		h.serveDraftCheck(w, r, id)
	case "policy-review":
		h.serveDraftPolicyReview(w, r, id)
	case "activate":
		h.serveDraftActivation(w, r, id)
	default:
		h.writeError(w, http.StatusNotFound, "not_found")
	}
}

func (h *onboardingUIHandler) serveDraft(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		if !onboardingUIReadRequestValid(r) {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if h.controller == nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		draft, err := h.controller.GetDraft(r.Context(), id)
		if err != nil {
			h.writeControllerError(w, err)
			return
		}
		if draft.ID != id || validateOnboardingDraftForResponse(draft) != nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		h.writeJSON(w, http.StatusOK, onboardingDraftResponseDTO{Draft: draft})
	case http.MethodPut:
		data, ok := h.readMutationJSON(w, r)
		if !ok {
			return
		}
		defer clear(data)
		var request onboardingDraftUpdateRequestDTO
		if !decodeOnboardingJSON(data, &request) {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if request.ExpectedRevision == 0 {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if h.controller == nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		existing, err := h.controller.GetDraft(r.Context(), id)
		if err != nil {
			h.writeControllerError(w, err)
			return
		}
		if existing.ID != id {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if existing.Revision != request.ExpectedRevision {
			h.writeError(w, http.StatusConflict, "stale_revision")
			return
		}
		draft, err := h.controller.SaveDraft(r.Context(), onboardingDraftSaveRequest{
			ID: id, ExpectedRevision: request.ExpectedRevision, Service: existing.Service, Fields: request.Fields,
		})
		if err != nil {
			h.writeControllerError(w, err)
			return
		}
		if draft.ID != id || validateOnboardingDraftForResponse(draft) != nil {
			h.writeError(w, http.StatusInternalServerError, "unavailable")
			return
		}
		h.writeJSON(w, http.StatusOK, onboardingDraftResponseDTO{Draft: draft})
	case http.MethodDelete:
		data, ok := h.readMutationJSON(w, r)
		if !ok {
			return
		}
		defer clear(data)
		var request onboardingDraftDeleteRequestDTO
		if !decodeOnboardingJSON(data, &request) || request.ExpectedRevision == 0 {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if h.controller == nil {
			h.writeError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if err := h.controller.DeleteDraft(r.Context(), id, request.ExpectedRevision); err != nil {
			h.writeControllerError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, onboardingDraftDeleteResponseDTO{Deleted: true})
	default:
		h.writeMethodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *onboardingUIHandler) serveDraftCheck(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	data, ok := h.readMutationJSON(w, r)
	if !ok {
		return
	}
	defer clear(data)
	var request onboardingDraftCheckRequestDTO
	if !decodeOnboardingJSON(data, &request) || request.ExpectedRevision == 0 || !validOnboardingCredentialChoice(request.Credential, request.NamedSecretID) {
		request.Credential = ""
		request.NamedSecretID = ""
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer func() {
		request.Credential = ""
		request.NamedSecretID = ""
	}()
	if h.controller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if h.rejectRecoveryRequiredDraft(w, r, id) {
		return
	}
	credential := []byte(request.Credential)
	defer clear(credential)
	credentialCanary := append([]byte(nil), credential...)
	defer clear(credentialCanary)
	result, err := h.controller.CheckDraft(r.Context(), onboardingCheckRequest{
		DraftID: id, ExpectedRevision: request.ExpectedRevision,
		Credential: credential, NamedSecretID: request.NamedSecretID,
	})
	if err != nil {
		h.writeControllerError(w, err)
		return
	}
	permitMatchesOutcome := result.Receipt.Outcome == onboardingCheckSucceeded && result.Permit != "" && result.Receipt.FailureKind == "" ||
		result.Receipt.Outcome == onboardingCheckFailed && result.Permit == "" && validOnboardingFailureKind(result.Receipt.FailureKind)
	if result.Receipt.DraftID != id || result.Receipt.DraftRevision != request.ExpectedRevision ||
		result.Receipt.CheckGeneration == 0 || result.Receipt.CandidateDigest == "" ||
		result.Receipt.PolicyDigest == "" || result.Receipt.ConfigRevision == "" ||
		result.Receipt.CheckedAt.IsZero() || !permitMatchesOutcome {
		h.writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	h.writeJSONGuarded(w, http.StatusOK, onboardingDraftCheckResponseDTO{Receipt: result.Receipt, Permit: result.Permit}, credentialCanary, []byte(request.NamedSecretID))
}

func (h *onboardingUIHandler) serveDraftPolicyReview(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	data, ok := h.readMutationJSON(w, r)
	if !ok {
		return
	}
	defer clear(data)
	var request onboardingPolicyReviewRequestDTO
	if !decodeOnboardingJSON(data, &request) || request.ExpectedRevision == 0 {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if h.controller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if h.rejectRecoveryRequiredDraft(w, r, id) {
		return
	}
	review, err := h.controller.ReviewDraft(r.Context(), id, request.ExpectedRevision)
	if err != nil {
		h.writeControllerError(w, err)
		return
	}
	if review.DraftID != id || review.DraftRevision != request.ExpectedRevision || review.CandidateDigest == "" ||
		review.PolicyDigest == "" || review.ConfigRevision == "" {
		h.writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	h.writeJSON(w, http.StatusOK, onboardingPolicyReviewResponseDTO{Review: review})
}

func (h *onboardingUIHandler) serveDraftActivation(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		h.writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	data, ok := h.readMutationJSON(w, r)
	if !ok {
		return
	}
	defer clear(data)
	var request onboardingDraftActivateRequestDTO
	if !decodeOnboardingJSON(data, &request) || request.ExpectedRevision == 0 || !request.PolicyConfirmed ||
		request.CheckGeneration == 0 ||
		request.CandidateDigest == "" || request.PolicyDigest == "" || request.ConfigRevision == "" ||
		!validOnboardingCredentialChoice(request.Credential, request.NamedSecretID) || request.CheckPermit == "" {
		request.Credential = ""
		request.NamedSecretID = ""
		request.CheckPermit = ""
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	defer func() {
		request.Credential = ""
		request.NamedSecretID = ""
		request.CheckPermit = ""
	}()
	if h.controller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	credential := []byte(request.Credential)
	defer clear(credential)
	credentialCanary := append([]byte(nil), credential...)
	defer clear(credentialCanary)
	activation, err := h.controller.ActivateDraft(r.Context(), onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: id, ExpectedRevision: request.ExpectedRevision, CheckGeneration: request.CheckGeneration,
			CandidateDigest: request.CandidateDigest, PolicyDigest: request.PolicyDigest, ConfigRevision: request.ConfigRevision,
		},
		PolicyConfirmed: request.PolicyConfirmed, Credential: credential,
		NamedSecretID: request.NamedSecretID, CheckPermit: request.CheckPermit,
	})
	if err != nil {
		h.writeControllerError(w, err)
		return
	}
	if activation.DraftID != id || activation.DraftRevision != request.ExpectedRevision ||
		activation.CheckGeneration != request.CheckGeneration || activation.CandidateDigest != request.CandidateDigest || activation.PolicyDigest != request.PolicyDigest ||
		activation.TargetID == "" || activation.ConfigRevision == "" || activation.ConfigRevision != request.ConfigRevision {
		h.writeError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	h.writeJSONGuarded(w, http.StatusOK, onboardingDraftActivateResponseDTO{Activation: activation}, credentialCanary,
		[]byte(request.NamedSecretID), []byte(request.CheckPermit))
}

func (h *onboardingUIHandler) serveFirstReads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	if !onboardingUIReadRequestValid(r) {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if h.controller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	receipts, err := h.controller.ListFirstReads(r.Context())
	if err != nil {
		h.writeControllerError(w, err)
		return
	}
	if len(receipts) > 256 {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	h.writeJSON(w, http.StatusOK, onboardingFirstReadListResponse{Receipts: nonNilOnboardingFirstReads(receipts)})
}

func (h *onboardingUIHandler) rejectRecoveryRequiredDraft(w http.ResponseWriter, r *http.Request, id string) bool {
	draft, err := h.controller.GetDraft(r.Context(), id)
	if err != nil {
		h.writeControllerError(w, err)
		return true
	}
	if draft.ID != id || validateOnboardingDraftForResponse(draft) != nil {
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
		return true
	}
	if draft.ActivationRecoveryRequired {
		h.writeControllerError(w, errOnboardingActivationRecoveryRequired)
		return true
	}
	return false
}

func (h *onboardingUIHandler) readMutationJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Host != h.host || h.host == "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return nil, false
	}
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != "http://"+h.host {
		h.writeError(w, http.StatusForbidden, "request_rejected")
		return nil, false
	}
	csrfValues := r.Header.Values("X-CSRF-Token")
	if h.csrf == "" || len(csrfValues) != 1 || len(csrfValues[0]) != len(h.csrf) ||
		subtle.ConstantTimeCompare([]byte(csrfValues[0]), []byte(h.csrf)) != 1 {
		h.writeError(w, http.StatusForbidden, "request_rejected")
		return nil, false
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		h.writeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		h.writeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOnboardingUIRequestBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			h.writeError(w, http.StatusBadRequest, "invalid_request")
		}
		clear(data)
		return nil, false
	}
	if len(data) == 0 || rejectDuplicateSetupDraftJSONKeys(data) != nil {
		clear(data)
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	return data, true
}

func decodeOnboardingJSON(data []byte, target any) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var extra json.RawMessage
	return decoder.Decode(&extra) == io.EOF
}

func validOnboardingCredentialChoice(credential, namedSecretID string) bool {
	if len(credential) > maxSecretSize || (credential != "" && namedSecretID != "") || (credential == "" && namedSecretID == "") {
		return false
	}
	return namedSecretID == "" || namedSecretIDPattern.MatchString(namedSecretID)
}

func onboardingUIReadRequestValid(r *http.Request) bool {
	return r.URL.RawQuery == "" && !r.URL.ForceQuery && r.URL.Fragment == "" && r.ContentLength == 0 && len(r.TransferEncoding) == 0
}

func onboardingUIReadOriginAllowed(r *http.Request, host string) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 0 || len(origins) == 1 && origins[0] == "http://"+host
}

func validateOnboardingDraftForResponse(draft onboardingDraft) error {
	if !validOnboardingDraftID(draft.ID) || draft.Revision == 0 ||
		(draft.Service != onboardingServiceGitHub && draft.Service != onboardingServiceJenkins && draft.Service != onboardingServiceDashboard) {
		return errOnboardingInvalid
	}
	if draft.Check != nil && (draft.Check.DraftID != draft.ID || draft.Check.DraftRevision == 0 ||
		draft.Check.CheckGeneration == 0 || draft.Check.CheckGeneration > draft.CheckGeneration ||
		draft.Check.CandidateDigest == "" || draft.Check.PolicyDigest == "") {
		return errOnboardingInvalid
	}
	return nil
}

func validateOnboardingDraftsForResponse(drafts []onboardingDraft) error {
	seen := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		if err := validateOnboardingDraftForResponse(draft); err != nil {
			return err
		}
		if _, exists := seen[draft.ID]; exists {
			return errOnboardingInvalid
		}
		seen[draft.ID] = struct{}{}
	}
	return nil
}

func onboardingDraftListItems(drafts []onboardingDraft) []onboardingDraftListItemDTO {
	result := make([]onboardingDraftListItemDTO, len(drafts))
	for index, draft := range drafts {
		item := onboardingDraftListItemDTO{
			ID: draft.ID, Revision: draft.Revision, Service: draft.Service,
			CreatedAt: draft.CreatedAt, UpdatedAt: draft.UpdatedAt,
			ActivationRecoveryRequired: draft.ActivationRecoveryRequired,
		}
		if draft.Check != nil {
			item.CheckOutcome = draft.Check.Outcome
		}
		result[index] = item
	}
	return result
}

func onboardingDraftResponseDraft(draft onboardingDraft) onboardingDraftResponseDraftDTO {
	return onboardingDraftResponseDraftDTO{
		ID: draft.ID, Revision: draft.Revision, CheckGeneration: draft.CheckGeneration,
		Service: draft.Service, Fields: draft.Fields, CreatedAt: draft.CreatedAt,
		UpdatedAt: draft.UpdatedAt, Check: draft.Check,
		ActivationRecoveryRequired: draft.ActivationRecoveryRequired,
	}
}

func nonNilOnboardingFirstReads(receipts []onboardingFirstReadReceipt) []onboardingFirstReadReceipt {
	result := make([]onboardingFirstReadReceipt, len(receipts))
	copy(result, receipts)
	return result
}

func (h *onboardingUIHandler) writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
}

func (h *onboardingUIHandler) writeControllerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOnboardingInvalid):
		h.writeError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, errOnboardingDraftNotFound):
		h.writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, errOnboardingDraftStale):
		h.writeError(w, http.StatusConflict, "stale_revision")
	case errors.Is(err, errOnboardingCheckRequired):
		h.writeError(w, http.StatusConflict, "check_required")
	case errors.Is(err, errOnboardingCheckPermit):
		h.writeError(w, http.StatusConflict, "check_permit_required")
	case errors.Is(err, errOnboardingReviewChanged):
		h.writeError(w, http.StatusConflict, "review_changed")
	case errors.Is(err, errOnboardingActivationRecoveryRequired):
		h.writeError(w, http.StatusConflict, "activation_recovery_required")
	case errors.Is(err, errOnboardingActivationPending):
		h.writeError(w, http.StatusConflict, "activation_pending")
	case errors.Is(err, errOnboardingUnavailable):
		h.writeError(w, http.StatusServiceUnavailable, "unavailable")
	default:
		h.writeError(w, http.StatusInternalServerError, "operation_failed")
	}
}

func (h *onboardingUIHandler) writeError(w http.ResponseWriter, status int, code string) {
	h.writeJSON(w, status, onboardingUIErrorDTO{Error: code, ErrorCode: code})
}

func (h *onboardingUIHandler) writeJSON(w http.ResponseWriter, status int, value any) {
	h.writeJSONGuarded(w, status, value)
}

func (h *onboardingUIHandler) writeJSONGuarded(w http.ResponseWriter, status int, value any, forbiddenValues ...[]byte) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > maxOnboardingUIResponseBytes {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"unavailable","error_code":"unavailable"}`)
	} else {
		for _, forbidden := range forbiddenValues {
			if onboardingJSONContainsValue(data, forbidden) {
				status = http.StatusInternalServerError
				data = []byte(`{"error":"unavailable","error_code":"unavailable"}`)
				break
			}
		}
	}
	setOnboardingUIHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func onboardingJSONContainsValue(data, value []byte) bool {
	if len(value) == 0 {
		return false
	}
	if bytes.Contains(data, value) {
		return true
	}
	escaped, err := json.Marshal(string(value))
	if err != nil || len(escaped) < 2 {
		clear(escaped)
		return false
	}
	contains := bytes.Contains(data, escaped[1:len(escaped)-1])
	clear(escaped)
	return contains
}

func setOnboardingUIHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
