package main

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
)

const maxRegistrationURLPreviewRequestBytes = 16 << 10

type registrationURLPreviewCandidateDTO struct {
	Origin        string   `json:"origin"`
	Repository    string   `json:"repository"`
	BaseURL       string   `json:"base_url"`
	JobPath       string   `json:"job_path"`
	MissingFields []string `json:"missing_fields"`
	ReviewFields  []string `json:"review_fields"`
}

type registrationURLPreviewResponse struct {
	Candidate *registrationURLPreviewCandidateDTO `json:"candidate,omitempty"`
	Error     string                              `json:"error,omitempty"`
	ErrorCode string                              `json:"error_code,omitempty"`
}

func (a *app) handleRegistrationURLPreview(w http.ResponseWriter, r *http.Request) {
	checkResponse := newPostResponseCapture()
	postValid := a.checkPostLimit(checkResponse, r, maxRegistrationURLPreviewRequestBytes)
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
		writeRegistrationURLPreviewJSON(w, status, registrationURLPreviewResponse{
			Error:     "Request rejected.",
			ErrorCode: "request_rejected",
		})
		return
	}

	if r.Host != a.host || !mcpHTTPOriginAllowed(r, "http://"+a.host) {
		writeRegistrationURLPreviewJSON(w, http.StatusForbidden, registrationURLPreviewResponse{
			Error:     "Request rejected.",
			ErrorCode: "request_rejected",
		})
		return
	}
	if !isRegistrationURLPreviewForm(r) {
		writeRegistrationURLPreviewJSON(w, http.StatusBadRequest, registrationURLPreviewResponse{
			Error:     "Enter one service and URL.",
			ErrorCode: "invalid_request",
		})
		return
	}

	var service registrationService
	switch SetupServiceKind(r.PostForm.Get("service")) {
	case SetupServiceGitHub:
		service = registrationServiceGitHub
	case SetupServiceJenkins:
		service = registrationServiceJenkins
	case SetupServiceDashboard:
		service = registrationServiceDashboard
	default:
		writeRegistrationURLPreviewJSON(w, http.StatusBadRequest, registrationURLPreviewResponse{
			Error:     "Choose GitHub, Jenkins, or Dashboard.",
			ErrorCode: "invalid_request",
		})
		return
	}

	candidate, err := parseRegistrationURL(service, r.PostForm.Get("url"))
	if err != nil {
		writeRegistrationURLPreviewJSON(w, http.StatusBadRequest, registrationURLPreviewResponse{
			Error:     "Enter a valid HTTPS service URL without credentials, query, or fragment.",
			ErrorCode: "invalid_url",
		})
		return
	}

	writeRegistrationURLPreviewJSON(w, http.StatusOK, registrationURLPreviewResponse{
		Candidate: &registrationURLPreviewCandidateDTO{
			Origin:        candidate.Origin,
			Repository:    candidate.Repository,
			BaseURL:       candidate.BaseURL,
			JobPath:       candidate.JobPath,
			MissingFields: nonNilStringSlice(candidate.MissingFields),
			ReviewFields:  nonNilStringSlice(candidate.ReviewFields),
		},
	})
}

func isRegistrationURLPreviewForm(r *http.Request) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || r.URL.RawQuery != "" || r.URL.ForceQuery || r.MultipartForm != nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
		return false
	}
	if len(r.PostForm) != 3 || len(r.PostForm["csrf"]) != 1 ||
		len(r.PostForm["service"]) != 1 || len(r.PostForm["url"]) != 1 {
		return false
	}
	for field := range r.PostForm {
		if field != "csrf" && field != "service" && field != "url" {
			return false
		}
	}
	return true
}

func nonNilStringSlice(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func writeRegistrationURLPreviewJSON(w http.ResponseWriter, status int, value registrationURLPreviewResponse) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Request rejected.","error_code":"request_rejected"}`)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
