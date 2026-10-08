package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

type dashboardTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	SecretRef string `json:"secret_ref"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type dashboardTargetInput struct {
	Target string `json:"target"`
}

type dashboardDeploymentStatusInput struct {
	ServiceBundle string `json:"service_bundle"`
	Environment   string `json:"environment"`
}

type dashboardNamespacesResult struct {
	Target     string   `json:"target"`
	Namespaces []string `json:"namespaces"`
	Truncated  bool     `json:"truncated,omitempty"`
}

type dashboardDeploymentCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	LastTransitionTime string `json:"last_transition_time"`
}

type dashboardDeploymentStatusResult struct {
	ServiceBundle string                         `json:"service_bundle"`
	Environment   string                         `json:"environment"`
	Target        string                         `json:"target"`
	Namespace     string                         `json:"namespace"`
	Deployment    string                         `json:"deployment"`
	Replicas      int32                          `json:"replicas"`
	Updated       int32                          `json:"updated_replicas"`
	Available     int32                          `json:"available_replicas"`
	Unavailable   int32                          `json:"unavailable_replicas"`
	Conditions    []dashboardDeploymentCondition `json:"conditions"`
	Truncated     bool                           `json:"conditions_truncated,omitempty"`
}

type dashboardDeploymentEvent struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Reason     string `json:"reason"`
	Count      int32  `json:"count"`
	FirstSeen  string `json:"first_seen"`
	LastSeen   string `json:"last_seen"`
	ObjectName string `json:"object_name,omitempty"`
}

type dashboardDeploymentEventsResult struct {
	ServiceBundle string                     `json:"service_bundle"`
	Environment   string                     `json:"environment"`
	Target        string                     `json:"target"`
	Namespace     string                     `json:"namespace"`
	Deployment    string                     `json:"deployment"`
	Events        []dashboardDeploymentEvent `json:"events"`
	Truncated     bool                       `json:"events_truncated,omitempty"`
}

type dashboardPodContainer struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Ready bool   `json:"ready"`
}

type dashboardPodStatus struct {
	Name         string                  `json:"name"`
	Status       string                  `json:"status"`
	Node         string                  `json:"node,omitempty"`
	RestartCount int32                   `json:"restart_count"`
	Containers   []dashboardPodContainer `json:"containers"`
}

type dashboardDeploymentPodsResult struct {
	ServiceBundle string               `json:"service_bundle"`
	Environment   string               `json:"environment"`
	Target        string               `json:"target"`
	Namespace     string               `json:"namespace"`
	Deployment    string               `json:"deployment"`
	Pods          []dashboardPodStatus `json:"pods"`
	Truncated     bool                 `json:"pods_truncated,omitempty"`
}

type dashboardDeploymentDiagnosisCheck struct {
	Step      string `json:"step"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code,omitempty"`
	NextCheck string `json:"next_check,omitempty"`
}

type dashboardDeploymentDiagnosisResult struct {
	ServiceBundle   string                              `json:"service_bundle,omitempty"`
	Environment     string                              `json:"environment,omitempty"`
	Target          string                              `json:"target,omitempty"`
	Namespace       string                              `json:"namespace,omitempty"`
	Deployment      string                              `json:"deployment,omitempty"`
	Status          *dashboardDeploymentStatusResult    `json:"status,omitempty"`
	Events          *dashboardDeploymentEventsResult    `json:"events,omitempty"`
	Pods            *dashboardDeploymentPodsResult      `json:"pods,omitempty"`
	OutputTruncated bool                                `json:"output_truncated,omitempty"`
	Checks          []dashboardDeploymentDiagnosisCheck `json:"checks"`
}

type dashboardDeploymentLogsInput struct {
	ServiceBundle string `json:"service_bundle"`
	Environment   string `json:"environment"`
	Pod           string `json:"pod"`
	Container     string `json:"container"`
}

type dashboardLogLine struct {
	Timestamp string `json:"timestamp"`
	Content   string `json:"content"`
}

type dashboardDeploymentLogsResult struct {
	ServiceBundle string             `json:"service_bundle"`
	Environment   string             `json:"environment"`
	Pod           string             `json:"pod"`
	Container     string             `json:"container"`
	Lines         []dashboardLogLine `json:"lines"`
	Truncated     bool               `json:"truncated,omitempty"`
}

type dashboardServiceBinding struct {
	ServiceBundle string
	Environment   string
	Target        dashboardTarget
	Namespace     string
	Deployment    string
}

const dashboardNamespaceLimit = 100
const dashboardDeploymentConditionLimit = 16
const dashboardDeploymentEventLimit = 100
const dashboardDeploymentPodLimit = 100
const dashboardReplicaSetLimit = 16
const dashboardMCPResponseLimit = 64 * 1024
const dashboardDeploymentDiagnosisOutputLimit = dashboardMCPResponseLimit
const dashboardLogLineLimit = 100
const dashboardLogLineBytesLimit = 4096

func (a *app) saveDashboardTarget(targetID, name, baseURL, token string) (string, error) {
	return a.saveDashboardTargetWithNamedSecret(targetID, name, baseURL, token, "")
}

func (a *app) saveDashboardTargetWithNamedSecret(targetID, name, baseURL, token, namedSecretID string) (string, error) {
	current, err := readConfig(a.configPath)
	if err != nil {
		return "", errors.New("Could not read current settings. They were not changed.")
	}
	index := -1
	if targetID != "" {
		index = findDashboardTargetIndex(current.DashboardTargets, targetID)
		if index < 0 {
			return "", errors.New("The selected Dashboard target is not registered.")
		}
	}
	for i, existing := range current.DashboardTargets {
		if existing.Name == name && i != index {
			return "", &saveFieldError{field: "name", message: "A Dashboard target name is already registered."}
		}
	}
	namedRef, selectedNamedSecret, err := resolveTargetNamedSecret(current, namedSecretID, token)
	if err != nil {
		return "", err
	}
	if token == "" && !selectedNamedSecret && index < 0 {
		return "", &saveFieldError{
			field:   "token",
			message: "Enter the Dashboard API Bearer returned by its login flow for a new target.",
		}
	}
	if token == "" && !selectedNamedSecret && index >= 0 && current.DashboardTargets[index].BaseURL != baseURL {
		return "", &saveFieldError{
			field:   "token",
			message: "Changing the Dashboard URL requires entering a new bearer token.",
		}
	}
	updated := dashboardTarget{Name: name, BaseURL: baseURL}
	var old dashboardTarget
	if index >= 0 {
		old = current.DashboardTargets[index]
		if validateDashboardTarget(old) != nil {
			return "", errors.New("The saved Dashboard target is invalid. It was not changed.")
		}
		updated.ID, updated.SecretRef, updated.Disabled = old.ID, old.SecretRef, old.Disabled
	} else {
		updated.ID, err = newTargetID("dashboard")
		if err != nil {
			return "", errors.New("Could not create target ID. Settings were not changed.")
		}
	}
	if name == "" || strings.TrimSpace(name) != name || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") || len(token) > maxSecretSize || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("The Dashboard target name or bearer token is invalid.")
	}
	newRef := ""
	createdCredential := false
	if token != "" {
		refBytes := make([]byte, 16)
		if _, err := rand.Read(refBytes); err != nil {
			return "", errors.New("Could not create credential reference. Settings were not changed.")
		}
		newRef = "cred:" + hex.EncodeToString(refBytes)
		if err := a.secrets.Save(newRef, []byte(token)); err != nil {
			return "", errors.New(secretStoreError())
		}
		updated.SecretRef = newRef
		createdCredential = true
	} else if selectedNamedSecret {
		newRef = namedRef
		updated.SecretRef = namedRef
	}
	if err := validateDashboardTarget(updated); err != nil {
		if createdCredential {
			_ = a.secrets.Delete(newRef)
		}
		return "", errors.New("The Dashboard target is invalid. Settings were not changed.")
	}
	next := current
	next.Version = configVersion
	if index < 0 {
		next.DashboardTargets = append(next.DashboardTargets, updated)
	} else {
		next.DashboardTargets[index] = updated
	}
	clearConnectionTest(&next, updated.ID)
	if err := writeConfig(a.configPath, next); err != nil {
		if createdCredential && a.secrets.Delete(newRef) != nil {
			return updated.ID, errors.New("Could not save settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager.")
		}
		return "", errors.New("Could not save settings. Previous settings remain in place.")
	}
	if old.SecretRef != "" && old.SecretRef != newRef && !configReferencesSecret(next, old.SecretRef) {
		if err := a.secrets.Delete(old.SecretRef); err != nil {
			return updated.ID, errors.New("Dashboard target saved, but its previous unused credential could not be removed.")
		}
	}
	return updated.ID, nil
}

func (a *app) handleDashboardSave(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	namedSecretID, namedSecretErr := targetNamedSecretIDFromPost(r)
	if namedSecretErr != nil {
		a.setStatus(errTargetNamedSecret.Error(), true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	selectionID := strings.TrimSpace(r.FormValue("target_id"))
	targetID := selectionID
	if targetID == "new" {
		targetID = ""
	}
	name := strings.TrimSpace(r.FormValue("dashboard_name"))
	baseURL, err := validateJenkinsBaseURL(strings.TrimSpace(r.FormValue("dashboard_url")))
	token := r.FormValue("dashboard_token")
	field := ""
	switch {
	case name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00"):
		field = "name"
	case err != nil:
		field = "url"
	case len(token) > maxSecretSize || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\x00"):
		field = "token"
	}
	if field != "" {
		a.renderDashboardSaveFailure(
			w,
			r,
			dashboardSaveDraft{
				TargetID:   safeDashboardSelectionID(selectionID),
				Name:       name,
				BaseURL:    strings.TrimSpace(r.FormValue("dashboard_url")),
				ErrorField: field,
			},
			"The Dashboard "+field+" field is invalid. A connection test was not run.",
		)
		return
	}
	var savedID string
	err = withConfigLock(func() error {
		var saveErr error
		savedID, saveErr = a.saveDashboardTargetWithNamedSecret(targetID, name, baseURL, token, namedSecretID)
		return saveErr
	})
	if err != nil {
		var fieldErr *saveFieldError
		if errors.As(err, &fieldErr) {
			a.renderDashboardSaveFailure(
				w,
				r,
				dashboardSaveDraft{
					TargetID:   safeDashboardSelectionID(selectionID),
					Name:       name,
					BaseURL:    baseURL,
					ErrorField: fieldErr.field,
				},
				fieldErr.Error(),
			)
			return
		}
		a.setStatus(err.Error(), true)
		http.Redirect(w, r, "/?dashboard_id="+url.QueryEscape(targetID)+"&dashboard_error=target", http.StatusSeeOther)
		return
	}
	if savedID == "" {
		savedID = targetID
	}
	a.setStatus("Dashboard target saved. Test connection before using MCP.", false)
	http.Redirect(w, r, "/?dashboard_id="+url.QueryEscape(savedID), http.StatusSeeOther)
}

func (a *app) renderDashboardSaveFailure(w http.ResponseWriter, r *http.Request, draft dashboardSaveDraft, message string) {
	a.renderRootPage(
		w,
		r,
		&savePageOverride{Status: message, StatusIsError: true, Dashboard: &draft},
		http.StatusUnprocessableEntity,
	)
}

func safeDashboardSelectionID(targetID string) string {
	if targetID == "new" || dashboardIDPattern.MatchString(targetID) {
		return targetID
	}
	return "new"
}

func (a *app) handleDashboardTest(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("target"))
	attempt := a.testConnection(r.Context(), "dashboard", strings.TrimSpace(r.FormValue("target_id")), name)
	message, failed := connectionTestMessage(attempt)
	a.setStatus(message, failed)
	http.Redirect(w, r, "/?dashboard_id="+url.QueryEscape(strings.TrimSpace(r.FormValue("target_id"))), http.StatusSeeOther)
}

func findDashboardTargetIndex(targets []dashboardTarget, id string) int {
	for i := range targets {
		if targets[i].ID == id {
			return i
		}
	}
	return -1
}

func findDashboardTargetNameIndex(targets []dashboardTarget, name string) int {
	for i := range targets {
		if targets[i].Name == name {
			return i
		}
	}
	return -1
}

func selectDashboardTarget(cfg config, id string) *dashboardTarget {
	if id == "" && len(cfg.DashboardTargets) > 0 {
		return &cfg.DashboardTargets[0]
	}
	for i := range cfg.DashboardTargets {
		if cfg.DashboardTargets[i].ID == id {
			return &cfg.DashboardTargets[i]
		}
	}
	return nil
}

func (a *app) loadDashboardTarget(requestedName string) (dashboardTarget, []byte, error) {
	var selected dashboardTarget
	var token []byte
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local Dashboard target settings are invalid.")
		}
		index := findDashboardTargetNameIndex(cfg.DashboardTargets, requestedName)
		if index < 0 {
			return errors.New("The requested Dashboard target name is not registered.")
		}
		selected = cfg.DashboardTargets[index]
		if selected.Disabled {
			return errors.New("The registered Dashboard target is disabled.")
		}
		if validateDashboardTarget(selected) != nil {
			return errors.New("The saved Dashboard target is invalid. Review it in local settings.")
		}
		token, err = a.loadDashboardBearer(selected.SecretRef)
		return err
	})
	if err != nil {
		clear(token)
		return dashboardTarget{}, nil, err
	}
	return selected, token, nil
}

func (a *app) loadDashboardBearer(secretRef string) ([]byte, error) {
	token, err := a.secrets.Load(secretRef)
	if err != nil || len(token) == 0 || len(token) > maxSecretSize || strings.TrimSpace(string(token)) != string(token) || strings.ContainsAny(string(token), "\r\n\x00") {
		clear(token)
		return nil, errors.New("The Dashboard API Bearer is unavailable. Re-enter it in local settings.")
	}
	return token, nil
}

func (a *app) loadDashboardServiceBinding(serviceBundleName, environmentName string) (dashboardServiceBinding, []byte, error) {
	if !validBundleName(serviceBundleName) || !validBundleName(environmentName) {
		return dashboardServiceBinding{}, nil, errors.New("A registered service bundle and environment name are required.")
	}

	var binding dashboardServiceBinding
	var token []byte
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local service and Dashboard target settings are invalid.")
		}

		var bundle *serviceBundle
		for i := range cfg.ServiceBundles {
			if cfg.ServiceBundles[i].Name == serviceBundleName {
				bundle = &cfg.ServiceBundles[i]
				break
			}
		}
		if bundle == nil {
			return errors.New("The requested service bundle is not registered.")
		}

		var environment *serviceEnvironment
		for i := range bundle.Environments {
			if bundle.Environments[i].Name == environmentName {
				environment = &bundle.Environments[i]
				break
			}
		}
		if environment == nil {
			return errors.New("The requested service environment is not registered.")
		}
		if environment.DashboardTargetID == "" || environment.DashboardNamespace == "" || environment.DashboardDeployment == "" || !validServiceDashboardNamespace(environment.DashboardNamespace) || !validServiceDashboardDeployment(environment.DashboardDeployment) {
			return errors.New("The registered service environment has no valid Dashboard Deployment mapping.")
		}

		targetIndex := findDashboardTargetIndex(cfg.DashboardTargets, environment.DashboardTargetID)
		if targetIndex < 0 {
			return errors.New("The Dashboard target mapped to this environment is unavailable.")
		}
		target := cfg.DashboardTargets[targetIndex]
		if target.Disabled {
			return errors.New("The Dashboard target mapped to this environment is disabled.")
		}
		if validateDashboardTarget(target) != nil {
			return errors.New("The Dashboard target mapped to this environment is invalid.")
		}

		token, err = a.loadDashboardBearer(target.SecretRef)
		if err != nil {
			return err
		}
		binding = dashboardServiceBinding{
			ServiceBundle: bundle.Name,
			Environment:   environment.Name,
			Target:        target,
			Namespace:     environment.DashboardNamespace,
			Deployment:    environment.DashboardDeployment,
		}
		return nil
	})
	if err != nil {
		clear(token)
		return dashboardServiceBinding{}, nil, err
	}
	return binding, token, nil
}

func (a *app) registeredDashboardNamespaces(ctx context.Context, requestedName string) (dashboardNamespacesResult, error) {
	target, token, err := a.loadDashboardTarget(requestedName)
	if err != nil {
		return dashboardNamespacesResult{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchDashboardNamespaces(ctx, target, string(token), a.client)
}

func (a *app) registeredDashboardDeploymentStatus(ctx context.Context, serviceBundleName, environmentName string) (dashboardDeploymentStatusResult, error) {
	binding, token, err := a.loadDashboardServiceBinding(serviceBundleName, environmentName)
	if err != nil {
		return dashboardDeploymentStatusResult{}, err
	}
	defer clear(token)

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchDashboardDeploymentStatus(ctx, binding, string(token), a.client)
}

func (a *app) registeredDashboardDeploymentEvents(ctx context.Context, serviceBundleName, environmentName string) (dashboardDeploymentEventsResult, error) {
	binding, token, err := a.loadDashboardServiceBinding(serviceBundleName, environmentName)
	if err != nil {
		return dashboardDeploymentEventsResult{}, err
	}
	defer clear(token)

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchDashboardDeploymentEvents(ctx, binding, string(token), a.client)
}

func (a *app) registeredDashboardDeploymentPods(ctx context.Context, serviceBundleName, environmentName string) (dashboardDeploymentPodsResult, error) {
	binding, token, err := a.loadDashboardServiceBinding(serviceBundleName, environmentName)
	if err != nil {
		return dashboardDeploymentPodsResult{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchDashboardDeploymentPods(ctx, binding, string(token), a.client)
}

func (a *app) registeredDashboardDeploymentDiagnosis(ctx context.Context, serviceBundleName, environmentName string) (dashboardDeploymentDiagnosisResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	binding, token, err := a.loadDashboardServiceBinding(serviceBundleName, environmentName)
	if err != nil {
		return dashboardDeploymentDiagnosisResult{
			Checks: []dashboardDeploymentDiagnosisCheck{dashboardDeploymentDiagnosisFailure("binding", err, nil)},
		}, nil
	}
	defer clear(token)
	bearer := string(token)

	type fetchResult struct {
		step   string
		status dashboardDeploymentStatusResult
		events dashboardDeploymentEventsResult
		pods   dashboardDeploymentPodsResult
		err    error
	}
	results := make(chan fetchResult, 3)
	go func() {
		value, fetchErr := fetchDashboardDeploymentStatus(ctx, binding, bearer, a.client)
		results <- fetchResult{step: "status", status: value, err: fetchErr}
	}()
	go func() {
		value, fetchErr := fetchDashboardDeploymentEvents(ctx, binding, bearer, a.client)
		results <- fetchResult{step: "events", events: value, err: fetchErr}
	}()
	go func() {
		// Status, events, and the two ReplicaSet reads leave one Pod-list read
		// within the composite diagnosis's five-request budget.
		value, fetchErr := fetchDashboardDeploymentPodsWithLimit(
			ctx,
			binding,
			bearer,
			a.client,
			1,
		)
		results <- fetchResult{step: "pods", pods: value, err: fetchErr}
	}()

	result := dashboardDeploymentDiagnosisResult{
		ServiceBundle: cleanOutput(binding.ServiceBundle, bearer, 80),
		Environment:   cleanOutput(binding.Environment, bearer, 80),
		Target:        cleanOutput(binding.Target.Name, bearer, 80),
		Namespace:     cleanOutput(binding.Namespace, bearer, 63),
		Deployment:    cleanOutput(binding.Deployment, bearer, 253),
	}
	checks := make(map[string]dashboardDeploymentDiagnosisCheck, 3)
	completed := 0
	for completed < 3 {
		select {
		case fetched := <-results:
			completed++
			if fetched.err != nil {
				checks[fetched.step] = dashboardDeploymentDiagnosisFailure(fetched.step, fetched.err, ctx)
				continue
			}
			checks[fetched.step] = dashboardDeploymentDiagnosisCheck{Step: fetched.step, Status: "succeeded"}
			switch fetched.step {
			case "status":
				result.Status = &fetched.status
			case "events":
				result.Events = &fetched.events
			case "pods":
				result.Pods = &fetched.pods
			}
		case <-ctx.Done():
			for {
				select {
				case fetched := <-results:
					completed++
					if fetched.err != nil {
						checks[fetched.step] = dashboardDeploymentDiagnosisFailure(fetched.step, fetched.err, ctx)
						continue
					}
					checks[fetched.step] = dashboardDeploymentDiagnosisCheck{Step: fetched.step, Status: "succeeded"}
					switch fetched.step {
					case "status":
						result.Status = &fetched.status
					case "events":
						result.Events = &fetched.events
					case "pods":
						result.Pods = &fetched.pods
					}
				default:
					goto drained
				}
			}
		drained:
			for _, step := range []string{"status", "events", "pods"} {
				if _, ok := checks[step]; !ok {
					checks[step] = dashboardDeploymentDiagnosisFailure(step, ctx.Err(), ctx)
					completed++
				}
			}
			completed = 3
		}
	}
	for _, step := range []string{"status", "events", "pods"} {
		result.Checks = append(result.Checks, checks[step])
	}
	truncateDashboardDeploymentDiagnosisResult(&result)
	return result, nil
}

func truncateDashboardDeploymentDiagnosisResult(result *dashboardDeploymentDiagnosisResult) {
	if dashboardDeploymentDiagnosisSize(*result) <= dashboardDeploymentDiagnosisOutputLimit {
		return
	}
	result.OutputTruncated = true
	for dashboardDeploymentDiagnosisSize(*result) > dashboardDeploymentDiagnosisOutputLimit && result.Pods != nil {
		trimmed := false
		for i := range result.Pods.Pods {
			containers := result.Pods.Pods[i].Containers
			if len(containers) == 0 {
				continue
			}
			result.Pods.Pods[i].Containers = containers[:len(containers)/2]
			trimmed = true
		}
		if !trimmed {
			break
		}
		result.Pods.Truncated = true
	}
	for dashboardDeploymentDiagnosisSize(*result) > dashboardDeploymentDiagnosisOutputLimit && result.Pods != nil && len(result.Pods.Pods) > 0 {
		result.Pods.Pods = result.Pods.Pods[:len(result.Pods.Pods)/2]
		result.Pods.Truncated = true
	}
	for dashboardDeploymentDiagnosisSize(*result) > dashboardDeploymentDiagnosisOutputLimit && result.Events != nil && len(result.Events.Events) > 0 {
		result.Events.Events = result.Events.Events[:len(result.Events.Events)/2]
		result.Events.Truncated = true
	}
	for dashboardDeploymentDiagnosisSize(*result) > dashboardDeploymentDiagnosisOutputLimit && result.Status != nil && len(result.Status.Conditions) > 0 {
		result.Status.Conditions = result.Status.Conditions[:len(result.Status.Conditions)/2]
		result.Status.Truncated = true
	}
	if dashboardDeploymentDiagnosisSize(*result) > dashboardDeploymentDiagnosisOutputLimit {
		result.ServiceBundle = ""
		result.Environment = ""
		result.Target = ""
		result.Namespace = ""
		result.Deployment = ""
		result.Status = nil
		result.Events = nil
		result.Pods = nil
		result.Checks = []dashboardDeploymentDiagnosisCheck{{
			Step:      "output",
			Status:    "truncated",
			ErrorCode: "output_limit",
			NextCheck: "Diagnostic details were omitted to stay within the response size limit.",
		}}
	}
}

func dashboardDeploymentDiagnosisSize(result dashboardDeploymentDiagnosisResult) int {
	return dashboardMCPToolResultSize(result)
}

func dashboardMCPToolResultSize(result any) int {
	encoded, err := json.Marshal(result)
	if err != nil {
		return dashboardMCPResponseLimit + 1
	}
	envelope, err := json.Marshal(struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	}{
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: string(encoded)}},
		StructuredContent: encoded,
	})
	if err != nil {
		return dashboardMCPResponseLimit + 1
	}
	return len(envelope)
}

func dashboardDeploymentDiagnosisFailure(step string, err error, ctx context.Context) dashboardDeploymentDiagnosisCheck {
	check := dashboardDeploymentDiagnosisCheck{Step: step, Status: "failed"}
	if err == nil {
		check.ErrorCode = "request_failed"
		check.NextCheck = "Retry the read-only check after reviewing the registered Dashboard target and permissions."
		return check
	}
	message := strings.ToLower(err.Error())
	if (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) || strings.Contains(message, "timed out") || errors.Is(err, context.DeadlineExceeded) {
		check.ErrorCode = "timeout"
		check.NextCheck = "Check Dashboard responsiveness and the registered proxy path, then retry within the 12-second diagnostic window."
		return check
	}
	var statusErr *dashboardHTTPStatusError
	if errors.As(err, &statusErr) && statusErr.statusCode >= 500 && statusErr.statusCode <= 599 {
		check.ErrorCode = "upstream_error"
		check.NextCheck = "Dashboard returned a server error. Check its availability and retry the read-only diagnostic later."
		return check
	}
	if strings.Contains(message, "required") {
		check.ErrorCode = "invalid_input"
		check.NextCheck = "Choose a registered service bundle and one of its registered environment names."
		return check
	}
	if strings.Contains(message, "not registered") {
		if strings.Contains(message, "environment") {
			check.ErrorCode = "environment_not_found"
			check.NextCheck = "Choose an environment listed under the registered service bundle."
		} else {
			check.ErrorCode = "service_bundle_not_found"
			check.NextCheck = "Choose a service bundle registered in local settings."
		}
		return check
	}
	if strings.Contains(message, "settings are invalid") {
		check.ErrorCode = "settings_unavailable"
		check.NextCheck = "Review the saved service bundle and Dashboard target in local settings."
		return check
	}
	if strings.Contains(message, "no valid dashboard deployment mapping") {
		check.ErrorCode = "mapping_invalid"
		check.NextCheck = "Review the environment's registered Dashboard target, namespace, and Deployment mapping."
		return check
	}
	if strings.Contains(message, "credential") || strings.Contains(message, "bearer is unavailable") {
		check.ErrorCode = "credential_unavailable"
		check.NextCheck = "Re-enter the Dashboard login Bearer in local settings and confirm it is available to this Windows account."
		return check
	}
	if strings.Contains(message, "disabled") {
		check.ErrorCode = "target_disabled"
		check.NextCheck = "Enable the mapped Dashboard target in local settings if this diagnostic is authorized."
		return check
	}
	if strings.Contains(message, "mapped to this environment is unavailable") || strings.Contains(message, "mapped to this environment is invalid") {
		check.ErrorCode = "target_unavailable"
		check.NextCheck = "Review the mapped Dashboard target in local settings."
		return check
	}
	if strings.Contains(message, "redirect") {
		check.ErrorCode = "redirected"
		check.NextCheck = "Review the registered Dashboard base URL and reverse-proxy path."
		return check
	}
	if strings.Contains(message, "401") || strings.Contains(message, "rejected registered api bearer") {
		check.ErrorCode = "authentication_failed"
		check.NextCheck = "Re-enter the Dashboard login Bearer and confirm it belongs to the mapped Dashboard target."
		return check
	}
	if strings.Contains(message, "403") || strings.Contains(message, "denied") {
		check.ErrorCode = "access_denied"
		check.NextCheck = "Confirm the registered Bearer can read this Deployment, its events, and its ReplicaSet Pods."
		return check
	}
	if strings.Contains(message, "http 404") {
		check.ErrorCode = "resource_or_path_not_found"
		check.NextCheck = "A 404 alone cannot identify the cause; review both the registered namespace and Deployment mapping and the Dashboard base URL, proxy prefix, and REST path."
		return check
	}
	if strings.Contains(message, "http 429") {
		check.ErrorCode = "rate_limited"
		check.NextCheck = "Wait for the Dashboard rate limit to clear, then retry the read-only diagnostic."
		return check
	}
	if strings.Contains(message, "could not connect") {
		check.ErrorCode = "connection_failed"
		check.NextCheck = "Check Dashboard availability and the registered URL, proxy path, TLS, and network access."
		return check
	}
	if strings.Contains(message, "exceeded 1 mib") || strings.Contains(message, "response exceeded") {
		check.ErrorCode = "response_limited"
		check.NextCheck = "Review the Dashboard response size and server-side limits before retrying."
		return check
	}
	if strings.Contains(message, "invalid") || strings.Contains(message, "complete") || strings.Contains(message, "different deployment") {
		check.ErrorCode = "invalid_response"
		check.NextCheck = "Confirm the registered REST path returns a complete response for this Dashboard version."
		return check
	}
	check.ErrorCode = "request_failed"
	check.NextCheck = "Review the registered Dashboard REST path and read permissions, then retry the diagnostic."
	return check
}

type dashboardHTTPStatusError struct {
	statusCode int
}

func (e *dashboardHTTPStatusError) Error() string {
	return fmt.Sprintf("Dashboard returned HTTP %d.", e.statusCode)
}

func (a *app) registeredDashboardDeploymentLogs(ctx context.Context, serviceBundleName, environmentName, podName, containerName string) (dashboardDeploymentLogsResult, error) {
	binding, token, err := a.loadDashboardServiceBinding(serviceBundleName, environmentName)
	if err != nil {
		return dashboardDeploymentLogsResult{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchDashboardDeploymentLogs(ctx, binding, string(token), podName, containerName, a.client)
}

func fetchDashboardDeploymentLogs(ctx context.Context, binding dashboardServiceBinding, token, podName, containerName string, client *http.Client) (dashboardDeploymentLogsResult, error) {
	if token == "" || !validServiceDashboardDeployment(podName) || !validKubernetesDNSName(containerName, 63, false) {
		return dashboardDeploymentLogsResult{}, errors.New("The requested Pod or container is invalid.")
	}
	inventory, err := fetchDashboardDeploymentPods(ctx, binding, token, client)
	if err != nil {
		return dashboardDeploymentLogsResult{}, err
	}
	var podFound, containerFound bool
	for _, pod := range inventory.Pods {
		if pod.Name != cleanOutput(podName, token, 253) {
			continue
		}
		podFound = true
		for _, container := range pod.Containers {
			if container.Name == cleanOutput(containerName, token, 63) {
				containerFound = true
				break
			}
		}
		break
	}
	if !podFound || !containerFound {
		return dashboardDeploymentLogsResult{}, errors.New("The requested Pod and container are not present in the registered Deployment inventory.")
	}
	endpoint, err := dashboardPodLogsURL(binding.Target, binding.Namespace, podName, containerName)
	if err != nil {
		return dashboardDeploymentLogsResult{}, errors.New("The registered Dashboard Pod log mapping is invalid.")
	}
	body, err := dashboardGetJSON(ctx, client, endpoint, token, "Pod logs")
	if err != nil {
		return dashboardDeploymentLogsResult{}, err
	}
	var payload struct {
		Info *struct {
			PodName       *string `json:"podName"`
			ContainerName *string `json:"containerName"`
			Truncated     *bool   `json:"truncated"`
		} `json:"info"`
		Logs *[]struct {
			Timestamp *string `json:"timestamp"`
			Content   *string `json:"content"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Info == nil || payload.Info.PodName == nil || payload.Info.ContainerName == nil || payload.Info.Truncated == nil || payload.Logs == nil {
		return dashboardDeploymentLogsResult{}, errors.New("Dashboard returned an invalid Pod log response.")
	}
	if *payload.Info.PodName != podName || *payload.Info.ContainerName != containerName {
		return dashboardDeploymentLogsResult{}, errors.New("Dashboard returned logs for a different Pod or container.")
	}
	result := dashboardDeploymentLogsResult{
		ServiceBundle: cleanOutput(binding.ServiceBundle, token, 80),
		Environment:   cleanOutput(binding.Environment, token, 80),
		Pod:           cleanOutput(podName, token, 253),
		Container:     cleanOutput(containerName, token, 63),
		Lines:         make([]dashboardLogLine, 0, min(len(*payload.Logs), dashboardLogLineLimit)),
		Truncated:     *payload.Info.Truncated || len(*payload.Logs) > dashboardLogLineLimit,
	}
	var previousTimestamp, previousContent string
	havePrevious := false
	for i, line := range *payload.Logs {
		if i >= dashboardLogLineLimit {
			result.Truncated = true
			break
		}
		if line.Timestamp == nil || line.Content == nil || len(*line.Timestamp) > 64 || strings.ContainsAny(*line.Timestamp, "\r\n\x00") {
			return dashboardDeploymentLogsResult{}, errors.New("Dashboard returned an invalid Pod log line.")
		}
		if havePrevious && *line.Timestamp == previousTimestamp && *line.Content == previousContent {
			continue
		}
		previousTimestamp, previousContent, havePrevious = *line.Timestamp, *line.Content, true
		result.Lines = append(result.Lines, dashboardLogLine{
			Timestamp: cleanOutput(*line.Timestamp, token, 64),
			Content:   limitOutputBytes(cleanOutput(*line.Content, token, dashboardLogLineBytesLimit), dashboardLogLineBytesLimit),
		})
	}
	for dashboardMCPToolResultSize(result) > dashboardMCPResponseLimit {
		if len(result.Lines) == 0 {
			return dashboardDeploymentLogsResult{}, errors.New("Dashboard Pod log result exceeded the response size limit.")
		}
		result.Lines = result.Lines[:len(result.Lines)-1]
		result.Truncated = true
	}
	return result, nil
}

func dashboardPodLogsURL(target dashboardTarget, namespace, pod, container string) (*url.URL, error) {
	if !validServiceDashboardNamespace(namespace) || !validServiceDashboardDeployment(pod) || !validKubernetesDNSName(container, 63, false) {
		return nil, errors.New("invalid Pod log mapping")
	}
	endpoint, err := dashboardAPIURL(target, "log/"+namespace+"/"+pod+"/"+container)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("referenceTimestamp", "newest")
	query.Set("referenceLineNum", "0")
	query.Set("offsetFrom", fmt.Sprint(1-dashboardLogLineLimit))
	query.Set("offsetTo", "1")
	query.Set("logFilePosition", "end")
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

func limitOutputBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	maxPrefix := limit - len("…")
	if maxPrefix < 0 {
		return ""
	}
	end := 0
	for position := 0; position < len(value); {
		_, size := utf8.DecodeRuneInString(value[position:])
		if position+size > maxPrefix {
			break
		}
		position += size
		end = position
	}
	return value[:end] + "…"
}

func fetchDashboardDeploymentPods(ctx context.Context, binding dashboardServiceBinding, token string, client *http.Client) (dashboardDeploymentPodsResult, error) {
	return fetchDashboardDeploymentPodsWithLimit(
		ctx,
		binding,
		token,
		client,
		dashboardReplicaSetLimit+1,
	)
}

func fetchDashboardDeploymentPodsWithLimit(
	ctx context.Context,
	binding dashboardServiceBinding,
	token string,
	client *http.Client,
	maxPodLists int,
) (dashboardDeploymentPodsResult, error) {
	result := dashboardDeploymentPodsResult{
		ServiceBundle: cleanOutput(binding.ServiceBundle, token, 80),
		Environment:   cleanOutput(binding.Environment, token, 80),
		Target:        cleanOutput(binding.Target.Name, token, 80),
		Namespace:     cleanOutput(binding.Namespace, token, 63),
		Deployment:    cleanOutput(binding.Deployment, token, 253),
		Pods:          make([]dashboardPodStatus, 0),
	}

	newURL, err := dashboardDeploymentReplicaSetURL(binding.Target, binding.Namespace, binding.Deployment, false)
	if err != nil {
		return dashboardDeploymentPodsResult{}, errors.New("The registered Dashboard Deployment mapping is invalid.")
	}
	body, err := dashboardGetJSON(ctx, client, newURL, token, "new ReplicaSet")
	if err != nil {
		return dashboardDeploymentPodsResult{}, err
	}
	var current struct {
		ObjectMeta *struct {
			Name *string `json:"name"`
		} `json:"objectMeta"`
	}
	if err := json.Unmarshal(body, &current); err != nil || current.ObjectMeta == nil {
		return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid Deployment ReplicaSet response.")
	}
	rsNames := make([]string, 0, dashboardReplicaSetLimit+1)
	seenReplicaSets := make(map[string]bool, dashboardReplicaSetLimit+1)
	if current.ObjectMeta.Name != nil && *current.ObjectMeta.Name != "" {
		if !validServiceDashboardDeployment(*current.ObjectMeta.Name) {
			return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid ReplicaSet name.")
		}
		rsNames = append(rsNames, *current.ObjectMeta.Name)
		seenReplicaSets[*current.ObjectMeta.Name] = true
	}

	oldURL, err := dashboardDeploymentReplicaSetURL(binding.Target, binding.Namespace, binding.Deployment, true)
	if err != nil {
		return dashboardDeploymentPodsResult{}, errors.New("The registered Dashboard Deployment mapping is invalid.")
	}
	body, err = dashboardGetJSON(ctx, client, oldURL, token, "old ReplicaSet")
	if err != nil {
		return dashboardDeploymentPodsResult{}, err
	}
	var old struct {
		ListMeta *struct {
			TotalItems *int `json:"totalItems"`
		} `json:"listMeta"`
		ReplicaSets *[]struct {
			ObjectMeta *struct {
				Name *string `json:"name"`
			} `json:"objectMeta"`
		} `json:"replicaSets"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &old); err != nil || old.ListMeta == nil || old.ListMeta.TotalItems == nil || old.ReplicaSets == nil || *old.ListMeta.TotalItems < len(*old.ReplicaSets) || *old.ListMeta.TotalItems < 0 {
		return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid old ReplicaSet list.")
	}
	if len(old.Errors) != 0 {
		return dashboardDeploymentPodsResult{}, errors.New("Dashboard could not return complete old ReplicaSet list.")
	}
	if *old.ListMeta.TotalItems > len(*old.ReplicaSets) || len(*old.ReplicaSets) > dashboardReplicaSetLimit {
		result.Truncated = true
	}
	for i, replicaSet := range *old.ReplicaSets {
		if i >= dashboardReplicaSetLimit {
			break
		}
		if replicaSet.ObjectMeta == nil || replicaSet.ObjectMeta.Name == nil || !validServiceDashboardDeployment(*replicaSet.ObjectMeta.Name) {
			return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid old ReplicaSet.")
		}
		name := *replicaSet.ObjectMeta.Name
		if !seenReplicaSets[name] {
			rsNames = append(rsNames, name)
			seenReplicaSets[name] = true
		}
	}

	seen := make(map[string]bool, dashboardDeploymentPodLimit)
	for index, replicaSet := range rsNames {
		if index >= maxPodLists {
			result.Truncated = true
			break
		}
		endpoint, err := dashboardReplicaSetPodsURL(binding.Target, binding.Namespace, replicaSet)
		if err != nil {
			return dashboardDeploymentPodsResult{}, errors.New("The registered Dashboard ReplicaSet mapping is invalid.")
		}
		body, err := dashboardGetJSON(ctx, client, endpoint, token, "ReplicaSet Pods")
		if err != nil {
			return dashboardDeploymentPodsResult{}, err
		}
		var payload struct {
			ListMeta *struct {
				TotalItems *int `json:"totalItems"`
			} `json:"listMeta"`
			Pods *[]struct {
				ObjectMeta *struct {
					Name      *string `json:"name"`
					Namespace *string `json:"namespace"`
				} `json:"objectMeta"`
				Status         *string `json:"status"`
				RestartCount   *int32  `json:"restartCount"`
				NodeName       *string `json:"nodeName"`
				ContainerState *[]struct {
					Name  *string `json:"name"`
					State *string `json:"state"`
					Ready *bool   `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"pods"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.ListMeta == nil || payload.ListMeta.TotalItems == nil || payload.Pods == nil || *payload.ListMeta.TotalItems < len(*payload.Pods) || *payload.ListMeta.TotalItems < 0 {
			return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid ReplicaSet Pod list.")
		}
		if len(payload.Errors) != 0 {
			return dashboardDeploymentPodsResult{}, errors.New("Dashboard could not return complete ReplicaSet Pod list.")
		}
		if *payload.ListMeta.TotalItems > len(*payload.Pods) {
			result.Truncated = true
		}
		for _, pod := range *payload.Pods {
			if pod.ObjectMeta == nil || pod.ObjectMeta.Name == nil || pod.ObjectMeta.Namespace == nil || pod.Status == nil || pod.RestartCount == nil || pod.NodeName == nil || pod.ContainerState == nil || !validServiceDashboardDeployment(*pod.ObjectMeta.Name) || *pod.ObjectMeta.Namespace != binding.Namespace || *pod.RestartCount < 0 || len(*pod.Status) > 128 || strings.ContainsAny(*pod.Status, "\r\n\x00") {
				return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid Pod status.")
			}
			name := *pod.ObjectMeta.Name
			if seen[name] {
				continue
			}
			seen[name] = true
			if len(result.Pods) >= dashboardDeploymentPodLimit {
				result.Truncated = true
				break
			}
			projected := dashboardPodStatus{
				Name:         cleanOutput(name, token, 253),
				Status:       cleanOutput(*pod.Status, token, 128),
				Node:         cleanOutput(*pod.NodeName, token, 253),
				RestartCount: *pod.RestartCount,
				Containers:   make([]dashboardPodContainer, 0, min(len(*pod.ContainerState), 32)),
			}
			if len(*pod.ContainerState) > 32 {
				result.Truncated = true
			}
			for i, container := range *pod.ContainerState {
				if i >= 32 {
					break
				}
				if container.Name == nil || container.State == nil || container.Ready == nil || !validKubernetesDNSName(*container.Name, 63, false) || !validPodContainerState(*container.State) {
					return dashboardDeploymentPodsResult{}, errors.New("Dashboard returned an invalid Pod container status.")
				}
				projected.Containers = append(projected.Containers, dashboardPodContainer{
					Name:  cleanOutput(*container.Name, token, 63),
					State: cleanOutput(*container.State, token, 16),
					Ready: *container.Ready,
				})
			}
			result.Pods = append(result.Pods, projected)
			if len(result.Pods) >= dashboardDeploymentPodLimit {
				result.Truncated = true
				break
			}
		}
		if len(result.Pods) >= dashboardDeploymentPodLimit {
			break
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return dashboardDeploymentPodsResult{}, errors.New("Dashboard Deployment Pod result exceeded 1 MiB.")
	}
	return result, nil
}

func dashboardGetJSON(ctx context.Context, client *http.Client, endpoint *url.URL, token, resource string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("Could not create Dashboard " + resource + " request.")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = newGitHubClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("Dashboard " + resource + " request timed out.")
		}
		return nil, errors.New("Could not connect registered Dashboard target for " + resource + ".")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, errors.New("Dashboard redirected " + resource + " request. Check registered address proxy path.")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("Dashboard rejected registered API Bearer (401). Re-enter token returned by Dashboard login.")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, errors.New("Dashboard denied " + resource + " access (403). Check registered API Bearer read permission.")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &dashboardHTTPStatusError{statusCode: resp.StatusCode}
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return nil, errors.New("Dashboard " + resource + " response exceeded 1 MiB or could not be read.")
	}
	return body, nil
}

func dashboardDeploymentReplicaSetURL(target dashboardTarget, namespace, deployment string, old bool) (*url.URL, error) {
	if !validServiceDashboardNamespace(namespace) || !validServiceDashboardDeployment(deployment) {
		return nil, errors.New("invalid Deployment mapping")
	}
	route := "newreplicaset"
	if old {
		route = "oldreplicaset"
	}
	endpoint, err := dashboardAPIURL(target, "deployment/"+namespace+"/"+deployment+"/"+route)
	if err != nil {
		return nil, err
	}
	if old {
		query := url.Values{}
		query.Set("itemsPerPage", fmt.Sprint(dashboardReplicaSetLimit))
		query.Set("page", "1")
		endpoint.RawQuery = query.Encode()
	}
	return endpoint, nil
}

func dashboardReplicaSetPodsURL(target dashboardTarget, namespace, replicaSet string) (*url.URL, error) {
	if !validServiceDashboardNamespace(namespace) || !validServiceDashboardDeployment(replicaSet) {
		return nil, errors.New("invalid ReplicaSet mapping")
	}
	endpoint, err := dashboardAPIURL(target, "replicaset/"+namespace+"/"+replicaSet+"/pod")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("itemsPerPage", fmt.Sprint(dashboardDeploymentPodLimit))
	query.Set("page", "1")
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

func validPodContainerState(state string) bool {
	return state == "Waiting" || state == "Running" || state == "Terminated" || state == "Failed" || state == "Unknown"
}

func fetchDashboardDeploymentEvents(ctx context.Context, binding dashboardServiceBinding, token string, client *http.Client) (dashboardDeploymentEventsResult, error) {
	endpoint, err := dashboardDeploymentEventsURL(binding.Target, binding.Namespace, binding.Deployment)
	if err != nil {
		return dashboardDeploymentEventsResult{}, errors.New("The registered Dashboard Deployment mapping is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return dashboardDeploymentEventsResult{}, errors.New("Could not create the Dashboard Deployment events request.")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = newGitHubClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return dashboardDeploymentEventsResult{}, errors.New("Dashboard Deployment events request timed out.")
		}
		return dashboardDeploymentEventsResult{}, errors.New("Could not connect to the registered Dashboard target.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard redirected the Deployment events request. Check the registered address and proxy path.")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard rejected the registered API Bearer (401). Re-enter the token returned by Dashboard login.")
	}
	if resp.StatusCode == http.StatusForbidden {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard denied Deployment event access (403). Check the registered API Bearer's read permission.")
	}
	if resp.StatusCode != http.StatusOK {
		return dashboardDeploymentEventsResult{}, &dashboardHTTPStatusError{statusCode: resp.StatusCode}
	}

	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard Deployment events response exceeded 1 MiB or could not be read.")
	}
	var payload struct {
		ListMeta *struct {
			TotalItems *int `json:"totalItems"`
		} `json:"listMeta"`
		Events *[]struct {
			ObjectMeta *struct {
				Name *string `json:"name"`
			} `json:"objectMeta"`
			Type       *string    `json:"type"`
			Reason     *string    `json:"reason"`
			Count      *int32     `json:"count"`
			FirstSeen  *time.Time `json:"firstSeen"`
			LastSeen   *time.Time `json:"lastSeen"`
			ObjectName *string    `json:"objectName"`
		} `json:"events"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.ListMeta == nil || payload.ListMeta.TotalItems == nil || payload.Events == nil || *payload.ListMeta.TotalItems < 0 || *payload.ListMeta.TotalItems < len(*payload.Events) {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard returned an invalid Deployment events list.")
	}
	if len(payload.Errors) != 0 {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard could not return a complete Deployment events list.")
	}

	result := dashboardDeploymentEventsResult{
		ServiceBundle: cleanOutput(binding.ServiceBundle, token, 80),
		Environment:   cleanOutput(binding.Environment, token, 80),
		Target:        cleanOutput(binding.Target.Name, token, 80),
		Namespace:     cleanOutput(binding.Namespace, token, 63),
		Deployment:    cleanOutput(binding.Deployment, token, 253),
		Events:        make([]dashboardDeploymentEvent, 0, min(len(*payload.Events), dashboardDeploymentEventLimit)),
		Truncated:     *payload.ListMeta.TotalItems > len(*payload.Events) || len(*payload.Events) > dashboardDeploymentEventLimit,
	}
	for i, event := range *payload.Events {
		if event.ObjectMeta == nil || event.ObjectMeta.Name == nil || event.Type == nil || event.Reason == nil || event.Count == nil || event.FirstSeen == nil || event.LastSeen == nil ||
			!validServiceDashboardDeployment(*event.ObjectMeta.Name) || len(*event.Type) > 32 || strings.ContainsAny(*event.Type, "\r\n\x00") || len(*event.Reason) > 128 || strings.ContainsAny(*event.Reason, "\r\n\x00") || *event.Count < 0 {
			return dashboardDeploymentEventsResult{}, errors.New("Dashboard returned an invalid Deployment event.")
		}
		if event.ObjectName != nil && *event.ObjectName != "" && !validServiceDashboardDeployment(*event.ObjectName) {
			return dashboardDeploymentEventsResult{}, errors.New("Dashboard returned an invalid referenced object name.")
		}
		if i >= dashboardDeploymentEventLimit {
			continue
		}
		projected := dashboardDeploymentEvent{
			Name:      cleanOutput(*event.ObjectMeta.Name, token, 253),
			Type:      cleanOutput(*event.Type, token, 32),
			Reason:    cleanOutput(*event.Reason, token, 128),
			Count:     *event.Count,
			FirstSeen: event.FirstSeen.UTC().Format(time.RFC3339Nano),
			LastSeen:  event.LastSeen.UTC().Format(time.RFC3339Nano),
		}
		if event.ObjectName != nil && *event.ObjectName != "" {
			projected.ObjectName = cleanOutput(*event.ObjectName, token, 253)
		}
		result.Events = append(result.Events, projected)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return dashboardDeploymentEventsResult{}, errors.New("Dashboard Deployment events result exceeded 1 MiB.")
	}
	return result, nil
}

func fetchDashboardDeploymentStatus(ctx context.Context, binding dashboardServiceBinding, token string, client *http.Client) (dashboardDeploymentStatusResult, error) {
	endpoint, err := dashboardDeploymentStatusURL(binding.Target, binding.Namespace, binding.Deployment)
	if err != nil {
		return dashboardDeploymentStatusResult{}, errors.New("The registered Dashboard Deployment mapping is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return dashboardDeploymentStatusResult{}, errors.New("Could not create Dashboard Deployment request.")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = newGitHubClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return dashboardDeploymentStatusResult{}, errors.New("Dashboard Deployment request timed out.")
		}
		return dashboardDeploymentStatusResult{}, errors.New("Could not connect to the registered Dashboard target.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard redirected the Deployment request. Check the registered address and proxy path.")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard rejected the registered API Bearer (401). Re-enter the token returned by Dashboard login.")
	}
	if resp.StatusCode == http.StatusForbidden {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard denied Deployment access (403). Check the registered API Bearer's read permission.")
	}
	if resp.StatusCode != http.StatusOK {
		return dashboardDeploymentStatusResult{}, &dashboardHTTPStatusError{statusCode: resp.StatusCode}
	}

	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard Deployment response exceeded 1 MiB or could not be read.")
	}
	var payload struct {
		ObjectMeta *struct {
			Name      *string `json:"name"`
			Namespace *string `json:"namespace"`
		} `json:"objectMeta"`
		StatusInfo *struct {
			Replicas    *int32 `json:"replicas"`
			Updated     *int32 `json:"updated"`
			Available   *int32 `json:"available"`
			Unavailable *int32 `json:"unavailable"`
		} `json:"statusInfo"`
		Conditions *[]struct {
			Type               *string `json:"type"`
			Status             *string `json:"status"`
			LastTransitionTime *string `json:"lastTransitionTime"`
			Reason             *string `json:"reason"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.ObjectMeta == nil || payload.ObjectMeta.Name == nil || payload.ObjectMeta.Namespace == nil || payload.StatusInfo == nil || payload.StatusInfo.Replicas == nil || payload.StatusInfo.Updated == nil || payload.StatusInfo.Available == nil || payload.StatusInfo.Unavailable == nil || payload.Conditions == nil {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned an invalid Deployment response.")
	}
	if *payload.ObjectMeta.Name != binding.Deployment || *payload.ObjectMeta.Namespace != binding.Namespace {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned a different Deployment than the registered mapping.")
	}
	if *payload.StatusInfo.Replicas < 0 || *payload.StatusInfo.Updated < 0 || *payload.StatusInfo.Available < 0 || *payload.StatusInfo.Unavailable < 0 {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned invalid Deployment replica counts.")
	}

	result := dashboardDeploymentStatusResult{
		ServiceBundle: cleanOutput(binding.ServiceBundle, token, 80),
		Environment:   cleanOutput(binding.Environment, token, 80),
		Target:        cleanOutput(binding.Target.Name, token, 80),
		Namespace:     cleanOutput(binding.Namespace, token, 63),
		Deployment:    cleanOutput(binding.Deployment, token, 253),
		Replicas:      *payload.StatusInfo.Replicas,
		Updated:       *payload.StatusInfo.Updated,
		Available:     *payload.StatusInfo.Available,
		Unavailable:   *payload.StatusInfo.Unavailable,
		Conditions:    make([]dashboardDeploymentCondition, 0, min(len(*payload.Conditions), dashboardDeploymentConditionLimit)),
		Truncated:     len(*payload.Conditions) > dashboardDeploymentConditionLimit,
	}
	for i, condition := range *payload.Conditions {
		if condition.Type == nil || condition.Status == nil || condition.LastTransitionTime == nil || condition.Reason == nil || *condition.Type == "" || len(*condition.Type) > 64 || len(*condition.Reason) > 128 {
			return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned an invalid Deployment condition.")
		}
		if *condition.Status != "True" && *condition.Status != "False" && *condition.Status != "Unknown" {
			return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned an invalid Deployment condition status.")
		}
		transitionTime, err := time.Parse(time.RFC3339Nano, *condition.LastTransitionTime)
		if err != nil {
			return dashboardDeploymentStatusResult{}, errors.New("Dashboard returned an invalid Deployment condition time.")
		}
		if i >= dashboardDeploymentConditionLimit {
			continue
		}
		result.Conditions = append(result.Conditions, dashboardDeploymentCondition{
			Type:               cleanOutput(*condition.Type, token, 64),
			Status:             cleanOutput(*condition.Status, token, 16),
			Reason:             cleanOutput(*condition.Reason, token, 128),
			LastTransitionTime: transitionTime.UTC().Format(time.RFC3339Nano),
		})
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return dashboardDeploymentStatusResult{}, errors.New("Dashboard Deployment result exceeded 1 MiB.")
	}
	return result, nil
}

func fetchDashboardNamespaces(ctx context.Context, target dashboardTarget, token string, client *http.Client) (dashboardNamespacesResult, error) {
	endpoint, err := dashboardNamespacesURL(target)
	if err != nil {
		return dashboardNamespacesResult{}, errors.New("The registered Dashboard target is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return dashboardNamespacesResult{}, errors.New("Could not create Dashboard request.")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = newGitHubClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		return dashboardNamespacesResult{}, connectionTransportFailure(
			err,
			"Could not connect to the registered Dashboard target.",
		)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Dashboard redirected the request. Check the registered address and proxy path."))
	}
	if resp.StatusCode == http.StatusRequestTimeout {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureTimeout, errors.New("Dashboard request timed out (408)."))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureAuthentication, errors.New("Dashboard rejected the registered API Bearer (401). Re-enter the token returned by Dashboard login and verify namespace read access."))
	}
	if resp.StatusCode == http.StatusForbidden {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureAccess, errors.New("Dashboard denied namespace access (403). Check namespace read permissions for the registered API Bearer."))
	}
	if resp.StatusCode == http.StatusNotFound {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Dashboard could not find the namespace endpoint (404). Check the registered base URL and proxy path."))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return dashboardNamespacesResult{}, withConnectionDiagnostic(connectionFailureRateLimit, errors.New("Dashboard rate limited the request (429)."))
	}
	if resp.StatusCode != http.StatusOK {
		return dashboardNamespacesResult{}, fmt.Errorf("Dashboard returned HTTP %d.", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return dashboardNamespacesResult{}, errors.New("Dashboard response exceeded 1 MiB or could not be read.")
	}
	var payload struct {
		ListMeta *struct {
			TotalItems *int `json:"totalItems"`
		} `json:"listMeta"`
		Namespaces []struct {
			ObjectMeta struct {
				Name string `json:"name"`
			} `json:"objectMeta"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.ListMeta == nil || payload.ListMeta.TotalItems == nil || payload.Namespaces == nil || *payload.ListMeta.TotalItems < len(payload.Namespaces) {
		return dashboardNamespacesResult{}, errors.New("Dashboard returned an invalid namespace response.")
	}
	result := dashboardNamespacesResult{
		Target:     cleanOutput(target.Name, token, 80),
		Namespaces: make([]string, 0, min(len(payload.Namespaces), dashboardNamespaceLimit)),
		Truncated:  len(payload.Namespaces) > dashboardNamespaceLimit || *payload.ListMeta.TotalItems > len(payload.Namespaces),
	}
	for _, namespace := range payload.Namespaces[:min(len(payload.Namespaces), dashboardNamespaceLimit)] {
		if !validDashboardNamespace(namespace.ObjectMeta.Name) {
			return dashboardNamespacesResult{}, errors.New("Dashboard returned an invalid namespace name.")
		}
		result.Namespaces = append(result.Namespaces, cleanOutput(namespace.ObjectMeta.Name, token, 253))
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return dashboardNamespacesResult{}, errors.New("Dashboard namespace result exceeded 1 MiB.")
	}
	return result, nil
}

func dashboardNamespacesURL(target dashboardTarget) (*url.URL, error) {
	base, err := dashboardAPIURL(target, "namespace")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("itemsPerPage", fmt.Sprint(dashboardNamespaceLimit))
	base.RawQuery = query.Encode()
	return base, nil
}

func dashboardDeploymentStatusURL(target dashboardTarget, namespace, deployment string) (*url.URL, error) {
	if !validServiceDashboardNamespace(namespace) || !validServiceDashboardDeployment(deployment) {
		return nil, errors.New("invalid Dashboard Deployment scope")
	}
	return dashboardAPIURL(target, "deployment/"+namespace+"/"+deployment)
}

func dashboardDeploymentEventsURL(target dashboardTarget, namespace, deployment string) (*url.URL, error) {
	endpoint, err := dashboardDeploymentStatusURL(target, namespace, deployment)
	if err != nil {
		return nil, err
	}
	endpoint.Path += "/event"
	query := url.Values{}
	query.Set("itemsPerPage", fmt.Sprint(dashboardDeploymentEventLimit))
	query.Set("page", "1")
	endpoint.RawQuery = query.Encode()
	return endpoint, nil
}

func dashboardAPIURL(target dashboardTarget, route string) (*url.URL, error) {
	if validateDashboardTarget(target) != nil {
		return nil, errors.New("invalid Dashboard target")
	}
	if route == "" || strings.ContainsAny(route, "?#\\") || strings.HasPrefix(route, "/") {
		return nil, errors.New("invalid Dashboard API route")
	}
	for _, segment := range strings.Split(route, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, errors.New("invalid Dashboard API route")
		}
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/api/v1/" + route
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}

func validateDashboardTarget(target dashboardTarget) error {
	baseURL, err := validateJenkinsBaseURL(target.BaseURL)
	if err != nil || baseURL != target.BaseURL || !dashboardIDPattern.MatchString(target.ID) || target.Name == "" || strings.TrimSpace(target.Name) != target.Name || len(target.Name) > 80 || strings.ContainsAny(target.Name, "\r\n\x00") || !secretRefPattern.MatchString(target.SecretRef) {
		return errors.New("invalid Dashboard target")
	}
	return nil
}

func validDashboardNamespace(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !dashboardAlphaNumeric(label[0]) || !dashboardAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !dashboardAlphaNumeric(c) && c != '-' {
				return false
			}
		}
	}
	return true
}

func dashboardAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
