package main

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const userEnvironmentUINameLimit = 255

type userEnvironmentUIResult struct {
	Name               string `json:"name"`
	Operation          string `json:"operation"`
	Applied            bool   `json:"applied"`
	RequiresNewSession bool   `json:"requires_new_session"`
	NextAction         string `json:"next_action,omitempty"`
}

type userEnvironmentUISuccess struct {
	OK     bool                    `json:"ok"`
	Result userEnvironmentUIResult `json:"result"`
}

// newUserEnvironmentUIHandler exposes only the reserved current-user environment
// mutation paths. Production registration must still wrap it in app.securityHeaders.
func newUserEnvironmentUIHandler(host, csrf string, controller UserEnvironmentController) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(uiRouteSaveUserEnvironment, func(w http.ResponseWriter, r *http.Request) {
		saveUserEnvironmentUI(w, r, host, csrf, controller)
	})
	mux.HandleFunc(uiRouteDeleteUserEnvironment, func(w http.ResponseWriter, r *http.Request) {
		deleteUserEnvironmentUI(w, r, host, csrf, controller)
	})
	return mux
}

func saveUserEnvironmentUI(w http.ResponseWriter, r *http.Request, host, csrf string, controller UserEnvironmentController) {
	form, ok := parseSecretsEnvironmentForm(w, r, host, csrf, uiRouteSaveUserEnvironment, "name", "value")
	if !ok {
		return
	}
	defer clearSecretsEnvironmentForm(form)
	name := form.Get("name")
	valueText := form.Get("value")
	if !validUserEnvironmentUIName(name) || !utf8.ValidString(valueText) || strings.ContainsRune(valueText, '\x00') {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	if controller == nil {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	value := []byte(valueText)
	valueText = ""
	defer clearBytes(value)
	result, err := controller.SetUserEnvironment(r.Context(), UserEnvironmentWrite{Name: name, Value: value})
	if err != nil {
		writeSecretsEnvironmentError(w, http.StatusInternalServerError, "operation_failed")
		return
	}
	writeUserEnvironmentResult(w, result, name, "set")
}

func deleteUserEnvironmentUI(w http.ResponseWriter, r *http.Request, host, csrf string, controller UserEnvironmentController) {
	form, ok := parseSecretsEnvironmentForm(w, r, host, csrf, uiRouteDeleteUserEnvironment, "name")
	if !ok {
		return
	}
	defer clearSecretsEnvironmentForm(form)
	name := form.Get("name")
	if !validUserEnvironmentUIName(name) {
		writeSecretsEnvironmentError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	if controller == nil {
		writeSecretsEnvironmentError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	result, err := controller.DeleteUserEnvironment(r.Context(), name)
	if err != nil {
		writeSecretsEnvironmentError(w, http.StatusInternalServerError, "operation_failed")
		return
	}
	writeUserEnvironmentResult(w, result, name, "delete")
}

func validUserEnvironmentUIName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || len(name) > userEnvironmentUINameLimit || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '=' {
			return false
		}
	}
	return true
}

func writeUserEnvironmentResult(w http.ResponseWriter, result UserEnvironmentResult, submittedName, operation string) {
	if result.Name != submittedName || result.Operation != operation {
		writeSecretsEnvironmentError(w, http.StatusInternalServerError, "unavailable")
		return
	}
	nextAction := ""
	if result.Applied && result.RequiresNewSession {
		nextAction = "new_session"
	}
	writeSecretsEnvironmentJSON(w, http.StatusOK, userEnvironmentUISuccess{OK: result.Applied, Result: userEnvironmentUIResult{
		Name: submittedName, Operation: operation, Applied: result.Applied,
		RequiresNewSession: result.RequiresNewSession, NextAction: nextAction,
	}})
}
