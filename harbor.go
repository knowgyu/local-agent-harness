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
)

const harborPageSize = 100

type harborTarget struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Username   string `json:"username"`
	Project    string `json:"project"`
	Repository string `json:"repository"`
	SecretRef  string `json:"secret_ref"`
	Disabled   bool   `json:"disabled,omitempty"`
}

type harborArtifactsInput struct {
	Target string `json:"target"`
}

type harborTag struct {
	Name string `json:"name"`
}

type harborArtifact struct {
	Digest string      `json:"digest"`
	Size   int64       `json:"size"`
	Tags   []harborTag `json:"tags,omitempty"`
}

type harborArtifactsResult struct {
	Target       string           `json:"target"`
	Project      string           `json:"project"`
	Repository   string           `json:"repository"`
	Artifacts    []harborArtifact `json:"artifacts"`
	Truncated    bool             `json:"truncated,omitempty"`
	MayBePartial bool             `json:"may_be_partial,omitempty"`
}

func (a *app) saveHarborTarget(targetID, name, baseURL, username, project, repository, secret string) (string, error) {
	return a.saveHarborTargetWithNamedSecret(targetID, name, baseURL, username, project, repository, secret, "")
}

func (a *app) saveHarborTargetWithNamedSecret(targetID, name, baseURL, username, project, repository, secret, namedSecretID string) (string, error) {
	current, err := readConfig(a.configPath)
	if err != nil {
		return "", errors.New("Could not read current settings. They were not changed.")
	}
	index := -1
	if targetID != "" {
		index = findHarborTargetIndex(current.HarborTargets, targetID)
		if index < 0 {
			return "", errors.New("The selected Harbor target is not registered.")
		}
	}
	for i, existing := range current.HarborTargets {
		if existing.Name == name && i != index {
			return "", &saveFieldError{field: "name", message: "A Harbor target name is already registered."}
		}
	}
	namedRef, selectedNamedSecret, err := resolveTargetNamedSecret(current, namedSecretID, secret)
	if err != nil {
		return "", err
	}
	if secret == "" && !selectedNamedSecret && index < 0 {
		return "", &saveFieldError{field: "secret", message: "Enter a Harbor credential for the new target."}
	}
	if secret == "" && !selectedNamedSecret && index >= 0 {
		old := current.HarborTargets[index]
		if old.BaseURL != baseURL || old.Username != username {
			return "", &saveFieldError{
				field:   "secret",
				message: "Changing the Harbor base URL or username requires entering the new credential.",
			}
		}
	}

	updated := harborTarget{Name: name, BaseURL: baseURL, Username: username, Project: project, Repository: repository}
	var old harborTarget
	if index >= 0 {
		old = current.HarborTargets[index]
		if validateHarborTarget(old) != nil {
			return "", errors.New("The saved Harbor target is invalid. It was not changed.")
		}
		updated.ID, updated.SecretRef, updated.Disabled = old.ID, old.SecretRef, old.Disabled
	} else {
		updated.ID, err = newTargetID("harbor")
		if err != nil {
			return "", errors.New("Could not create target ID. Settings were not changed.")
		}
	}
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") || !validHarborProject(project) || !validHarborRepository(repository) || !validJenkinsUsername(username) || len(secret) > maxSecretSize || strings.ContainsAny(secret, "\r\n\x00") {
		return "", errors.New("The Harbor target is invalid. Settings were not changed.")
	}

	newRef := ""
	createdCredential := false
	if secret != "" {
		refBytes := make([]byte, 16)
		if _, err := rand.Read(refBytes); err != nil {
			return "", errors.New("Could not create credential reference. Settings were not changed.")
		}
		newRef = "cred:" + hex.EncodeToString(refBytes)
		if err := a.secrets.Save(newRef, []byte(secret)); err != nil {
			return "", errors.New(secretStoreError())
		}
		updated.SecretRef = newRef
		createdCredential = true
	} else if selectedNamedSecret {
		newRef = namedRef
		updated.SecretRef = namedRef
	}
	if err := validateHarborTarget(updated); err != nil {
		if createdCredential {
			_ = a.secrets.Delete(newRef)
		}
		return "", errors.New("The Harbor target is invalid. Settings were not changed.")
	}

	next := current
	next.Version = configVersion
	if index < 0 {
		next.HarborTargets = append(next.HarborTargets, updated)
	} else {
		next.HarborTargets[index] = updated
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
			return updated.ID, errors.New("Harbor target saved, but its previous unused credential could not be removed.")
		}
	}
	return updated.ID, nil
}

func (a *app) handleHarborSave(w http.ResponseWriter, r *http.Request) {
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
	name := strings.TrimSpace(r.FormValue("harbor_name"))
	baseURL, err := validateJenkinsBaseURL(strings.TrimSpace(r.FormValue("harbor_url")))
	username := strings.TrimSpace(r.FormValue("harbor_username"))
	project := strings.TrimSpace(r.FormValue("harbor_project"))
	repository := strings.TrimSpace(r.FormValue("harbor_repository"))
	secret := r.FormValue("harbor_secret")
	field := ""
	switch {
	case name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00"):
		field = "name"
	case err != nil:
		field = "url"
	case !validJenkinsUsername(username):
		field = "username"
	case !validHarborProject(project):
		field = "project"
	case !validHarborRepository(repository):
		field = "repository"
	case len(secret) > maxSecretSize || strings.ContainsAny(secret, "\r\n\x00"):
		field = "secret"
	}
	if field != "" {
		a.renderHarborSaveFailure(
			w,
			r,
			harborSaveDraft{
				TargetID:   safeHarborSelectionID(selectionID),
				Name:       name,
				BaseURL:    strings.TrimSpace(r.FormValue("harbor_url")),
				Username:   username,
				Project:    project,
				Repository: repository,
				ErrorField: field,
			},
			"The Harbor "+field+" field is invalid.",
		)
		return
	}
	var savedID string
	err = withConfigLock(func() error {
		var saveErr error
		savedID, saveErr = a.saveHarborTargetWithNamedSecret(targetID, name, baseURL, username, project, repository, secret, namedSecretID)
		return saveErr
	})
	if err != nil {
		var fieldErr *saveFieldError
		if errors.As(err, &fieldErr) {
			a.renderHarborSaveFailure(
				w,
				r,
				harborSaveDraft{
					TargetID:   safeHarborSelectionID(selectionID),
					Name:       name,
					BaseURL:    baseURL,
					Username:   username,
					Project:    project,
					Repository: repository,
					ErrorField: fieldErr.field,
				},
				fieldErr.Error(),
			)
			return
		}
		field := ""
		switch {
		case strings.Contains(err.Error(), "name is already"):
			field = "name"
		case strings.HasPrefix(err.Error(), "Enter a Harbor credential"), strings.HasPrefix(err.Error(), "Changing the Harbor base URL"):
			field = "secret"
		}
		a.setStatus(err.Error(), true)
		if field != "" {
			http.Redirect(w, r, "/?harbor_id="+url.QueryEscape(targetID)+"&harbor_error="+field, http.StatusSeeOther)
			return
		}
	} else {
		a.setStatus("Harbor target saved. Test connection before using MCP.", false)
	}
	if savedID == "" {
		savedID = targetID
	}
	http.Redirect(w, r, "/?harbor_id="+url.QueryEscape(savedID), http.StatusSeeOther)
}

func (a *app) renderHarborSaveFailure(w http.ResponseWriter, r *http.Request, draft harborSaveDraft, message string) {
	a.renderRootPage(
		w,
		r,
		&savePageOverride{Status: message, StatusIsError: true, Harbor: &draft},
		http.StatusUnprocessableEntity,
	)
}

func safeHarborSelectionID(targetID string) string {
	if targetID == "new" || harborIDPattern.MatchString(targetID) {
		return targetID
	}
	return "new"
}

func (a *app) handleHarborTest(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	attempt := a.testConnection(r.Context(), "harbor", strings.TrimSpace(r.FormValue("target_id")), strings.TrimSpace(r.FormValue("target")))
	message, failed := connectionTestMessage(attempt)
	a.setStatus(message, failed)
	http.Redirect(w, r, "/?harbor_id="+url.QueryEscape(strings.TrimSpace(r.FormValue("target_id"))), http.StatusSeeOther)
}

func findHarborTargetIndex(targets []harborTarget, id string) int {
	for i := range targets {
		if targets[i].ID == id {
			return i
		}
	}
	return -1
}

func findHarborTargetNameIndex(targets []harborTarget, name string) int {
	for i := range targets {
		if targets[i].Name == name {
			return i
		}
	}
	return -1
}

func (a *app) registeredHarborArtifacts(ctx context.Context, requestedName string) (harborArtifactsResult, error) {
	target, secret, err := a.loadHarborTarget(requestedName)
	if err != nil {
		return harborArtifactsResult{}, err
	}
	defer clear(secret)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchHarborArtifacts(ctx, target, string(secret), a.client)
}

func (a *app) loadHarborTarget(requestedName string) (harborTarget, []byte, error) {
	var selected harborTarget
	var secret []byte
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local Harbor target settings are invalid.")
		}
		index := findHarborTargetNameIndex(cfg.HarborTargets, requestedName)
		if index < 0 {
			return errors.New("The requested Harbor target name is not registered.")
		}
		selected = cfg.HarborTargets[index]
		if selected.Disabled {
			return errors.New("The registered Harbor target is disabled.")
		}
		if validateHarborTarget(selected) != nil {
			return errors.New("The saved Harbor target is invalid. Review it in local settings.")
		}
		secret, err = a.secrets.Load(selected.SecretRef)
		if err != nil || len(secret) == 0 || len(secret) > maxSecretSize || strings.ContainsAny(string(secret), "\r\n\x00") {
			clear(secret)
			secret = nil
			return errors.New("The Harbor credential is unavailable. Re-enter it in local settings.")
		}
		return nil
	})
	if err != nil {
		clear(secret)
		return harborTarget{}, nil, err
	}
	return selected, secret, nil
}

func fetchHarborArtifacts(ctx context.Context, target harborTarget, secret string, client *http.Client) (harborArtifactsResult, error) {
	endpoint, err := harborArtifactsURL(target)
	if err != nil {
		return harborArtifactsResult{}, errors.New("The registered Harbor target is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return harborArtifactsResult{}, errors.New("Could not create Harbor request.")
	}
	req.SetBasicAuth(target.Username, secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "local-agent-harness")
	if client == nil {
		client = newGitHubClient()
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return harborArtifactsResult{}, connectionTransportFailure(
			err,
			"Could not reach the registered Harbor base URL. Check address, VPN, and TLS certificate.",
		)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Harbor redirected the request. Check the registered base URL."))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusRequestTimeout:
			return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureTimeout, errors.New("Harbor request timed out (408)."))
		case http.StatusUnauthorized:
			return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureAuthentication, errors.New("Harbor rejected the credential (401). Check the registered username and credential."))
		case http.StatusForbidden:
			return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureAccess, errors.New("Harbor denied access (403). Check read access to the registered project and repository."))
		case http.StatusNotFound:
			return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Harbor could not find the registered project or repository (404)."))
		case http.StatusTooManyRequests:
			return harborArtifactsResult{}, withConnectionDiagnostic(connectionFailureRateLimit, errors.New("Harbor rate limited the request (429)."))
		default:
			return harborArtifactsResult{}, fmt.Errorf("Harbor returned HTTP %d.", resp.StatusCode)
		}
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return harborArtifactsResult{}, errors.New("Harbor response exceeded 1 MiB or could not be read.")
	}
	var artifacts []harborArtifact
	if err := json.Unmarshal(body, &artifacts); err != nil {
		return harborArtifactsResult{}, errors.New("Harbor returned an invalid artifact response.")
	}
	result := harborArtifactsResult{
		Target:       cleanOutput(target.Name, secret, 80),
		Project:      cleanOutput(target.Project, secret, 128),
		Repository:   cleanOutput(target.Repository, secret, 512),
		Truncated:    len(artifacts) > harborPageSize,
		MayBePartial: len(artifacts) >= harborPageSize,
	}
	if len(artifacts) > harborPageSize {
		artifacts = artifacts[:harborPageSize]
	}
	result.Artifacts = make([]harborArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Size < 0 {
			return harborArtifactsResult{}, errors.New("Harbor returned an invalid artifact size.")
		}
		projected := harborArtifact{Digest: cleanOutput(artifact.Digest, secret, 256), Size: artifact.Size}
		if len(artifact.Tags) > 100 {
			artifact.Tags = artifact.Tags[:100]
			result.Truncated = true
		}
		for _, tag := range artifact.Tags {
			projected.Tags = append(projected.Tags, harborTag{Name: cleanOutput(tag.Name, secret, 128)})
		}
		result.Artifacts = append(result.Artifacts, projected)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return harborArtifactsResult{}, errors.New("Harbor result exceeded the 1 MiB output limit.")
	}
	return result, nil
}

func harborArtifactsURL(target harborTarget) (*url.URL, error) {
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v2.0/projects/" + target.Project + "/repositories/" + url.PathEscape(target.Repository) + "/artifacts"
	base.RawPath = ""
	query := url.Values{}
	query.Set("page", "1")
	query.Set("page_size", "100")
	query.Set("with_tag", "true")
	base.RawQuery = query.Encode()
	base.Fragment = ""
	return base, nil
}

func validateHarborTarget(target harborTarget) error {
	baseURL, err := validateJenkinsBaseURL(target.BaseURL)
	if err != nil || baseURL != target.BaseURL || !harborIDPattern.MatchString(target.ID) || target.Name == "" || len(target.Name) > 80 || strings.ContainsAny(target.Name, "\r\n\x00") || !validJenkinsUsername(target.Username) || !validHarborProject(target.Project) || !validHarborRepository(target.Repository) || !secretRefPattern.MatchString(target.SecretRef) {
		return errors.New("invalid Harbor target")
	}
	return nil
}

func validHarborProject(project string) bool {
	return project != "" && len(project) <= 128 && safeJenkinsSegment(project) && project != "." && project != ".."
}

func validHarborRepository(repository string) bool {
	return validJenkinsJobPath(repository) && len(repository) <= 512
}
