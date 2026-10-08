package main

import "time"

type dashboardDiagnosisSafeCheck struct {
	Step      string `json:"step"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
}

// dashboardDiagnosisSafeSummary contains only fixed codes and validated
// Kubernetes object names. It deliberately excludes target details, raw
// upstream text, logs, credential references, and free-form next steps.
type dashboardDiagnosisSafeSummary struct {
	CapturedAt       string                        `json:"captured_at"`
	Namespace        string                        `json:"namespace,omitempty"`
	Deployment       string                        `json:"deployment,omitempty"`
	Checks           []dashboardDiagnosisSafeCheck `json:"checks"`
	Findings         []string                      `json:"findings"`
	NextAction       string                        `json:"next_action"`
	NotQueried       []string                      `json:"not_queried"`
	DetailsShortened bool                          `json:"details_shortened,omitempty"`
}

func newDashboardDiagnosisSafeSummary(result dashboardDeploymentDiagnosisResult, capturedAt time.Time) *dashboardDiagnosisSafeSummary {
	summary := &dashboardDiagnosisSafeSummary{
		CapturedAt: capturedAt.UTC().Format(time.RFC3339),
		Checks:     make([]dashboardDiagnosisSafeCheck, 0, len(result.Checks)),
		Findings:   make([]string, 0, 4),
		NextAction: "review_evidence",
		NotQueried: []string{
			"other_deployment_pods",
			"resource_requests_limits",
			"live_resource_usage",
			"container_logs",
		},
		DetailsShortened: result.OutputTruncated ||
			(result.Status != nil && result.Status.Truncated) ||
			(result.Events != nil && result.Events.Truncated) ||
			(result.Pods != nil && result.Pods.Truncated),
	}
	if validServiceDashboardNamespace(result.Namespace) {
		summary.Namespace = result.Namespace
	}
	if validServiceDashboardDeployment(result.Deployment) {
		summary.Deployment = result.Deployment
	}

	for _, check := range result.Checks {
		projected := dashboardDiagnosisSafeCheck{
			Step:   safeDashboardDiagnosisStep(check.Step),
			Status: safeDashboardDiagnosisStatus(check.Status),
		}
		if errorCode := safeDashboardDiagnosisErrorCode(check.ErrorCode); errorCode != "" {
			projected.ErrorCode = errorCode
		}
		summary.Checks = append(summary.Checks, projected)
	}
	if len(summary.Checks) == 0 {
		summary.Checks = append(summary.Checks, dashboardDiagnosisSafeCheck{Step: "unknown", Status: "unknown"})
	}

	findings := make(map[string]bool)
	if status := result.Status; status != nil {
		if status.Replicas > 0 && (status.Available < status.Replicas || status.Unavailable > 0) {
			findings["deployment_not_fully_available"] = true
		}
		for _, condition := range status.Conditions {
			if condition.Type == "Available" && condition.Status == "False" {
				findings["deployment_not_fully_available"] = true
			}
			if condition.Type == "Progressing" && condition.Status == "False" {
				findings["deployment_progress_stalled"] = true
			}
		}
	}
	if events := result.Events; events != nil {
		for _, event := range events.Events {
			switch event.Reason {
			case "FailedScheduling":
				findings["scheduling_constraint"] = true
			case "ImagePullBackOff", "ErrImagePull":
				findings["image_pull_issue"] = true
			case "FailedMount", "FailedAttachVolume":
				findings["volume_setup_issue"] = true
			case "BackOff":
				findings["container_backoff"] = true
			}
		}
	}
	if pods := result.Pods; pods != nil {
		for _, pod := range pods.Pods {
			if pod.Status != "" && pod.Status != "Running" {
				findings["pod_not_running"] = true
			}
			for _, container := range pod.Containers {
				if container.State == "Waiting" {
					findings["container_waiting"] = true
				}
			}
		}
	}
	for _, code := range []string{
		"deployment_not_fully_available",
		"deployment_progress_stalled",
		"scheduling_constraint",
		"image_pull_issue",
		"volume_setup_issue",
		"container_backoff",
		"pod_not_running",
		"container_waiting",
	} {
		if findings[code] {
			summary.Findings = append(summary.Findings, code)
		}
	}

	summary.NextAction = safeDashboardDiagnosisNextAction(summary.Checks)
	return summary
}

func safeDashboardDiagnosisStep(step string) string {
	switch step {
	case "binding", "status", "events", "pods", "output":
		return step
	default:
		return "unknown"
	}
}

func safeDashboardDiagnosisStatus(status string) string {
	switch status {
	case "succeeded", "failed", "truncated":
		return status
	default:
		return "unknown"
	}
}

func safeDashboardDiagnosisErrorCode(code string) string {
	switch code {
	case "request_failed", "timeout", "upstream_error", "invalid_input", "environment_not_found", "service_bundle_not_found",
		"settings_unavailable", "mapping_invalid", "credential_unavailable", "target_disabled", "target_unavailable",
		"redirected", "authentication_failed", "access_denied", "resource_or_path_not_found", "rate_limited",
		"connection_failed", "response_limited", "invalid_response", "output_limit":
		return code
	default:
		return ""
	}
}

func safeDashboardDiagnosisNextAction(checks []dashboardDiagnosisSafeCheck) string {
	for _, check := range checks {
		if check.Status != "failed" && check.Status != "truncated" {
			continue
		}
		switch check.ErrorCode {
		case "invalid_input", "environment_not_found", "service_bundle_not_found", "settings_unavailable", "mapping_invalid", "target_disabled", "target_unavailable", "resource_or_path_not_found":
			return "review_saved_mapping"
		case "credential_unavailable", "authentication_failed", "access_denied":
			return "review_read_access"
		case "timeout", "upstream_error", "connection_failed", "rate_limited":
			return "retry_dashboard"
		case "redirected", "response_limited", "invalid_response", "output_limit":
			return "review_dashboard_path"
		}
	}
	return "review_evidence"
}
