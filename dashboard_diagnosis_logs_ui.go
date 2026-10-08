package main

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
)

const (
	maxDashboardDiagnosisLogsUIRequestBytes  = 4 << 10
	maxDashboardDiagnosisLogsUIResponseBytes = 64 << 10
	dashboardDiagnosisLogsUIError            = "Dashboard logs could not be read."
)

type dashboardDiagnosisLogsUIResponse struct {
	Result *dashboardDeploymentLogsResult `json:"result,omitempty"`
	Error  string                         `json:"error,omitempty"`
}

// handleDashboardDiagnosisLogs revalidates the saved Deployment inventory for
// each log request, including requests for a previously displayed Pod.
func (a *app) handleDashboardDiagnosisLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	origins := r.Header.Values("Origin")
	if r.Host != a.host || len(origins) != 1 {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusForbidden, nil)
		return
	}
	if origins[0] != "http://"+a.host || a.csrf == "" {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusForbidden, nil)
		return
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || r.URL.RawQuery != "" {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusBadRequest, nil)
		return
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusBadRequest, nil)
		return
	}
	checkResponse := newPostResponseCapture()
	if !a.checkPostLimit(checkResponse, r, maxDashboardDiagnosisLogsUIRequestBytes) {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusForbidden, nil)
		return
	}
	if !isDashboardDiagnosisLogsUIForm(r) {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusBadRequest, nil)
		return
	}

	serviceBundle := r.PostForm.Get("service_bundle")
	environment := r.PostForm.Get("environment")
	pod := r.PostForm.Get("pod")
	container := r.PostForm.Get("container")
	validScope := validBundleName(serviceBundle) && validBundleName(environment)
	validSelection := validServiceDashboardDeployment(pod) && validKubernetesDNSName(container, 63, false)
	if !validScope || !validSelection {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusBadRequest, nil)
		return
	}

	result, err := a.registeredDashboardDeploymentLogs(
		r.Context(),
		serviceBundle,
		environment,
		pod,
		container,
	)
	if err != nil {
		writeDashboardDiagnosisLogsUIJSON(w, http.StatusInternalServerError, nil)
		return
	}
	writeDashboardDiagnosisLogsUIJSON(w, http.StatusOK, &result)
}

func isDashboardDiagnosisLogsUIForm(r *http.Request) bool {
	if len(r.PostForm) != 5 || r.MultipartForm != nil {
		return false
	}
	for _, field := range []string{"csrf", "service_bundle", "environment", "pod", "container"} {
		if len(r.PostForm[field]) != 1 {
			return false
		}
	}
	return true
}

func writeDashboardDiagnosisLogsUIJSON(w http.ResponseWriter, status int, result *dashboardDeploymentLogsResult) {
	response := dashboardDiagnosisLogsUIResponse{Result: result}
	if result == nil {
		response.Error = dashboardDiagnosisLogsUIError
	}
	var data []byte
	for {
		encoded, err := json.Marshal(response)
		if err != nil {
			status = http.StatusInternalServerError
			data = []byte(`{"error":"Dashboard logs could not be read."}`)
			break
		}
		if len(encoded)+1 <= maxDashboardDiagnosisLogsUIResponseBytes {
			data = encoded
			break
		}
		if result == nil || len(result.Lines) == 0 {
			status = http.StatusRequestEntityTooLarge
			data = []byte(`{"error":"Dashboard logs could not be read."}`)
			break
		}
		result.Lines = result.Lines[:len(result.Lines)-1]
		result.Truncated = true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if _, err := w.Write(append(data, '\n')); err != nil {
		return
	}
}
