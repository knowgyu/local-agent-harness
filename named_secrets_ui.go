package main

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
)

type secretsEnvironmentUIError struct {
	Error string `json:"error"`
}

type namedSecretUISuccess struct {
	OK      bool            `json:"ok"`
	Secret  NamedSecretView `json:"secret"`
	Warning string          `json:"warning,omitempty"`
}

type namedSecretCleanupUISuccess struct {
	OK             bool `json:"ok"`
	CleanupPending bool `json:"cleanup_pending"`
}

// newNamedSecretUIHandler exposes only the reserved named-secret mutation paths.
// Production registration must still wrap it in app.securityHeaders.
func newNamedSecretUIHandler(host, csrf string, controller NamedSecretController) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(uiRouteSaveNamedSecret, func(w http.ResponseWriter, r *http.Request) {
		saveNamedSecretUI(w, r, host, csrf, controller)
	})
	mux.HandleFunc(uiRouteDeleteNamedSecret, func(w http.ResponseWriter, r *http.Request) {
		deleteNamedSecretUI(w, r, host, csrf, controller)
	})
	mux.HandleFunc(uiRouteRetryNamedSecretCleanup, func(w http.ResponseWriter, r *http.Request) {
		retryNamedSecretCleanupUI(w, r, host, csrf, controller)
	})
	return mux
}

func saveNamedSecretUI(w http.ResponseWriter, r *http.Request, host, csrf string, controller NamedSecretController) {
	form, ok := parseSecretsEnvironmentForm(w, r, host, csrf, uiRouteSaveNamedSecret, "id", "name", "purpose", "value")
	if !ok {
		return
	}
	defer clearSecretsEnvironmentForm(form)
	id := form.Get("id")
	valueText := form.Get("value")
	var value []byte
	if valueText != "" {
		value = []byte(valueText)
	}
	valueText = ""
	write := NamedSecretWrite{
		ID:      id,
		Name:    form.Get("name"),
		Purpose: form.Get("purpose"),
		Value:   value,
	}
	defer clearBytes(value)
	if !validNamedSecretInput(write) || (write.ID == "" && len(write.Value) == 0) || len(write.Value) > maxSecretSize {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	if controller == nil {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	view, err := controller.SaveNamedSecret(r.Context(), write)
	cleanupPending := errors.Is(err, errNamedSecretCleanup)
	if err != nil && !cleanupPending {
		code := namedSecretUIErrorCode(err)
		writeSecretsEnvironmentError(w, namedSecretUIErrorStatus(code), code)
		return
	}
	if !view.Configured || !namedSecretIDPattern.MatchString(view.ID) || view.Name != write.Name || view.Purpose != write.Purpose {
		writeSecretsEnvironmentError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	warning := ""
	if cleanupPending {
		warning = "cleanup_pending"
	}
	writeSecretsEnvironmentJSON(w, http.StatusOK, namedSecretUISuccess{OK: true, Warning: warning, Secret: NamedSecretView{
		ID: view.ID, Name: view.Name, Purpose: view.Purpose, Configured: true, InUse: view.InUse,
	}})
}

func deleteNamedSecretUI(w http.ResponseWriter, r *http.Request, host, csrf string, controller NamedSecretController) {
	form, ok := parseSecretsEnvironmentForm(w, r, host, csrf, uiRouteDeleteNamedSecret, "id")
	if !ok {
		return
	}
	id := form.Get("id")
	if !namedSecretIDPattern.MatchString(id) {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	if controller == nil {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err := controller.DeleteNamedSecret(r.Context(), id); err != nil {
		code := namedSecretUIErrorCode(err)
		writeSecretsEnvironmentError(w, namedSecretUIErrorStatus(code), code)
		return
	}
	writeSecretsEnvironmentJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func retryNamedSecretCleanupUI(w http.ResponseWriter, r *http.Request, host, csrf string, controller NamedSecretController) {
	form, ok := parseSecretsEnvironmentForm(w, r, host, csrf, uiRouteRetryNamedSecretCleanup, "id")
	if !ok {
		return
	}
	id := form.Get("id")
	if !namedSecretIDPattern.MatchString(id) {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	cleanup, ok := controller.(namedSecretCleanupUIController)
	if !ok {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	pendingIDs, err := cleanup.PendingNamedSecretCleanupIDs(r.Context())
	if err != nil || len(pendingIDs) > 256 {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	found := false
	seen := make(map[string]struct{}, len(pendingIDs))
	for _, pendingID := range pendingIDs {
		if !namedSecretIDPattern.MatchString(pendingID) {
			writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if _, duplicate := seen[pendingID]; duplicate {
			writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		seen[pendingID] = struct{}{}
		if pendingID == id {
			found = true
		}
	}
	if !found {
		writeSecretsEnvironmentError(w, http.StatusConflict, "not_found")
		return
	}
	pending, err := cleanup.RetryNamedSecretCleanup(r.Context(), id)
	if err != nil {
		code := namedSecretUIErrorCode(err)
		writeSecretsEnvironmentError(w, namedSecretUIErrorStatus(code), code)
		return
	}
	writeSecretsEnvironmentJSON(w, http.StatusOK, namedSecretCleanupUISuccess{OK: true, CleanupPending: pending})
}

func namedSecretUIErrorCode(err error) string {
	switch {
	case errors.Is(err, errNamedSecretInvalid):
		return "invalid_input"
	case errors.Is(err, errNamedSecretNotFound):
		return "not_found"
	case errors.Is(err, errNamedSecretInUse):
		return "in_use"
	case errors.Is(err, errNamedSecretCleanup):
		return "cleanup_failed"
	case errors.Is(err, errNamedSecretRollback):
		return "save_failed"
	case errors.Is(err, errNamedSecretStore), errors.Is(err, errNamedSecretSettings):
		return "unavailable"
	default:
		return "operation_failed"
	}
}

func namedSecretUIErrorStatus(code string) int {
	switch code {
	case "invalid_input":
		return http.StatusBadRequest
	case "not_found":
		return http.StatusNotFound
	case "in_use":
		return http.StatusConflict
	case "unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func parseSecretsEnvironmentForm(w http.ResponseWriter, r *http.Request, host, csrf, expectedPath string, fields ...string) (url.Values, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSecretsEnvironmentError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return nil, false
	}
	if host == "" || r.Host != host || r.URL.Path != expectedPath {
		writeSecretsEnvironmentError(w, http.StatusNotFound, "not_found")
		return nil, false
	}
	if csrf == "" {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return nil, false
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && origins[0] != "http://"+host) {
		writeSecretsEnvironmentError(w, http.StatusForbidden, "request_rejected")
		return nil, false
	}
	if r.URL.RawQuery != "" {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return nil, false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeSecretsEnvironmentError(w, http.StatusUnsupportedMediaType, "invalid_input")
		return nil, false
	}
	allowed := map[string]bool{"csrf": true}
	for _, field := range fields {
		allowed[field] = true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	if err := r.ParseForm(); err != nil {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return nil, false
	}
	for key, values := range r.PostForm {
		if !allowed[key] || len(values) != 1 {
			writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
			return nil, false
		}
	}
	csrfValues := r.PostForm["csrf"]
	if len(csrfValues) != 1 || csrfValues[0] != csrf {
		writeSecretsEnvironmentError(w, http.StatusForbidden, "request_rejected")
		return nil, false
	}
	return r.PostForm, true
}

func writeSecretsEnvironmentError(w http.ResponseWriter, status int, code string) {
	writeSecretsEnvironmentJSON(w, status, secretsEnvironmentUIError{Error: code})
}

func writeSecretsEnvironmentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func clearSecretsEnvironmentForm(form url.Values) {
	for _, values := range form {
		for i := range values {
			values[i] = ""
		}
	}
}
