package main

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"
)

const maxDashboardDiagnosisUIRequestBytes = 4 << 10

type dashboardDiagnosisPageChoice struct {
	ServiceBundle string
	Environment   string
	Target        string
	Namespace     string
	Deployment    string
}

type dashboardDiagnosisUIResponse struct {
	Result  *dashboardDeploymentDiagnosisResult `json:"result,omitempty"`
	Summary *dashboardDiagnosisSafeSummary      `json:"summary,omitempty"`
	Error   string                              `json:"error,omitempty"`
}

// handleDashboardDiagnosis runs the existing bounded, read-only Dashboard
// diagnosis against the exact saved service bundle and environment names.
func (a *app) handleDashboardDiagnosis(w http.ResponseWriter, r *http.Request) {
	checkResponse := newPostResponseCapture()
	postValid := a.checkPostLimit(checkResponse, r, maxDashboardDiagnosisUIRequestBytes)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !postValid {
		status := checkResponse.status
		if status == 0 {
			status = http.StatusForbidden
		}
		if allow := checkResponse.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeDashboardDiagnosisUIJSON(w, status, dashboardDiagnosisUIResponse{Error: "Request rejected."})
		return
	}
	if !isDashboardDiagnosisUIForm(r) {
		writeDashboardDiagnosisUIJSON(w, http.StatusBadRequest, dashboardDiagnosisUIResponse{Error: "Choose one registered Dashboard environment."})
		return
	}

	serviceBundle := r.PostForm.Get("service_bundle")
	environment := r.PostForm.Get("environment")
	if !validBundleName(serviceBundle) || !validBundleName(environment) {
		writeDashboardDiagnosisUIJSON(w, http.StatusBadRequest, dashboardDiagnosisUIResponse{Error: "Choose one registered Dashboard environment."})
		return
	}

	result, err := a.registeredDashboardDeploymentDiagnosis(r.Context(), serviceBundle, environment)
	if err != nil {
		writeDashboardDiagnosisUIJSON(w, http.StatusInternalServerError, dashboardDiagnosisUIResponse{Error: "Dashboard diagnosis could not be completed."})
		return
	}
	writeDashboardDiagnosisUIJSON(w, http.StatusOK, dashboardDiagnosisUIResponse{
		Result:  &result,
		Summary: newDashboardDiagnosisSafeSummary(result, time.Now()),
	})
}

func isDashboardDiagnosisUIForm(r *http.Request) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || r.URL.RawQuery != "" || r.MultipartForm != nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
		return false
	}
	if len(r.PostForm) != 3 || len(r.PostForm["csrf"]) != 1 ||
		len(r.PostForm["service_bundle"]) != 1 || len(r.PostForm["environment"]) != 1 {
		return false
	}
	for field := range r.PostForm {
		if field != "csrf" && field != "service_bundle" && field != "environment" {
			return false
		}
	}
	return true
}

func writeDashboardDiagnosisUIJSON(w http.ResponseWriter, status int, value dashboardDiagnosisUIResponse) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Dashboard diagnosis could not be completed."}`)
	}
	if len(data)+1 > dashboardDeploymentDiagnosisOutputLimit {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Dashboard diagnosis details exceeded the response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
