package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSetupDraftPayloadBytes = 256 << 10
	maxSetupDraftSources      = 16
	maxSetupDraftMissing      = 64
	maxSetupDraftConnections  = 32
	maxSetupDraftBundles      = 32
	maxSetupDraftSSHTargets   = 16
	maxSetupDraftJSONDepth    = 64
)

var (
	errSetupDraftPayload          = errors.New("setup draft is malformed or contains unsupported data")
	setupDraftCredentialRefInText = regexp.MustCompile(`(?i)\b(?:cred|secret):[a-f0-9]{32}\b`)
)

// decodeSetupDraftSubmissionJSON is the strict boundary for MCP adapters. The
// typed controller cannot detect unknown fields after a caller has decoded
// JSON with encoding/json's permissive defaults.
func decodeSetupDraftSubmissionJSON(payload []byte) (SetupDraftSubmission, error) {
	if len(payload) == 0 || len(payload) > maxSetupDraftPayloadBytes || rejectDuplicateSetupDraftJSONKeys(payload) != nil {
		return SetupDraftSubmission{}, errSetupDraftPayload
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var submission SetupDraftSubmission
	if err := decoder.Decode(&submission); err != nil {
		return SetupDraftSubmission{}, errSetupDraftPayload
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return SetupDraftSubmission{}, errSetupDraftPayload
	}
	normalized, err := normalizeSetupDraftSubmission(submission)
	if err != nil {
		return SetupDraftSubmission{}, errSetupDraftPayload
	}
	return normalized, nil
}

func rejectDuplicateSetupDraftJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := consumeSetupDraftJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errSetupDraftPayload
	}
	return nil
}

func consumeSetupDraftJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxSetupDraftJSONDepth {
		return errSetupDraftPayload
	}
	token, err := decoder.Token()
	if err != nil {
		return errSetupDraftPayload
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return errSetupDraftPayload
			}
			key = strings.ToLower(key)
			if _, exists := seen[key]; exists {
				return errSetupDraftPayload
			}
			seen[key] = struct{}{}
			if err := consumeSetupDraftJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errSetupDraftPayload
		}
	case '[':
		for decoder.More() {
			if err := consumeSetupDraftJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errSetupDraftPayload
		}
	default:
		return errSetupDraftPayload
	}
	return nil
}

func normalizeSetupDraftSubmission(submission SetupDraftSubmission) (SetupDraftSubmission, error) {
	if !validSetupDraftRevision(submission.BaseRevision) || len(submission.Sources) == 0 || len(submission.Sources) > maxSetupDraftSources ||
		len(submission.Missing) > maxSetupDraftMissing || len(submission.Connections) > maxSetupDraftConnections ||
		len(submission.Bundles) > maxSetupDraftBundles || len(submission.SSHTargets) > maxSetupDraftSSHTargets {
		return SetupDraftSubmission{}, errSetupDraftInvalid
	}
	encoded, err := json.Marshal(submission)
	if err != nil || len(encoded) > maxSetupDraftPayloadBytes {
		return SetupDraftSubmission{}, errSetupDraftInvalid
	}
	result := cloneSetupDraftSubmission(submission)
	if err := normalizeSetupDraftSources(result.Sources); err != nil {
		return SetupDraftSubmission{}, err
	}
	if err := normalizeSetupDraftMissing(result.Missing); err != nil {
		return SetupDraftSubmission{}, err
	}
	connectionKeys := make(map[string]bool)
	for index := range result.Connections {
		connection, err := normalizeSetupDraftConnection(result.Connections[index])
		if err != nil {
			return SetupDraftSubmission{}, err
		}
		key := string(connection.Kind) + "\x00" + strings.ToLower(connection.Name)
		if connectionKeys[key] {
			return SetupDraftSubmission{}, errSetupDraftInvalid
		}
		connectionKeys[key] = true
		result.Connections[index] = connection
	}
	bundleNames := make(map[string]bool)
	for index := range result.Bundles {
		bundle, err := normalizeSetupDraftBundle(result.Bundles[index])
		if err != nil {
			return SetupDraftSubmission{}, err
		}
		key := strings.ToLower(bundle.Name)
		if bundleNames[key] {
			return SetupDraftSubmission{}, errSetupDraftInvalid
		}
		bundleNames[key] = true
		result.Bundles[index] = bundle
	}
	sshNames, sshAliases := make(map[string]bool), make(map[string]bool)
	for index := range result.SSHTargets {
		target, err := normalizeSetupDraftSSHTarget(result.SSHTargets[index])
		if err != nil {
			return SetupDraftSubmission{}, err
		}
		nameKey, aliasKey := strings.ToLower(target.Name), strings.ToLower(target.Alias)
		if sshNames[nameKey] || sshAliases[aliasKey] {
			return SetupDraftSubmission{}, errSetupDraftInvalid
		}
		sshNames[nameKey], sshAliases[aliasKey] = true, true
		result.SSHTargets[index] = target
	}
	if len(result.Connections)+len(result.Bundles)+len(result.SSHTargets) == 0 {
		return SetupDraftSubmission{}, errSetupDraftInvalid
	}
	return result, nil
}

func normalizeSetupDraftSources(sources []SetupDraftSource) error {
	seen := make(map[string]bool)
	for _, source := range sources {
		if source.Kind != "conversation" && source.Kind != "document" && source.Kind != "git_remote" && source.Kind != "runbook" && source.Kind != "ssh_config" && source.Kind != "settings" && source.Kind != "user_provided" {
			return errSetupDraftInvalid
		}
		if source.Confidence != "high" && source.Confidence != "medium" && source.Confidence != "low" && source.Confidence != "unknown" {
			return errSetupDraftInvalid
		}
		if !validSetupDraftText(source.Label, 120, false) {
			return errSetupDraftInvalid
		}
		key := source.Kind + "\x00" + strings.ToLower(source.Label)
		if seen[key] {
			return errSetupDraftInvalid
		}
		seen[key] = true
	}
	return nil
}

func normalizeSetupDraftMissing(missing []string) error {
	seen := make(map[string]bool)
	for _, item := range missing {
		if !validSetupDraftText(item, 160, false) || seen[strings.ToLower(item)] {
			return errSetupDraftInvalid
		}
		seen[strings.ToLower(item)] = true
	}
	return nil
}

func normalizeSetupDraftConnection(connection SetupDraftConnection) (SetupDraftConnection, error) {
	if connection.Kind != SetupServiceGitHub && connection.Kind != SetupServiceJenkins && connection.Kind != SetupServiceHarbor && connection.Kind != SetupServiceDashboard {
		return SetupDraftConnection{}, errSetupDraftInvalid
	}
	if connection.Action != SetupDraftAdd && connection.Action != SetupDraftUpdate && connection.Action != SetupDraftReuse {
		return SetupDraftConnection{}, errSetupDraftInvalid
	}
	if !validSetupDraftText(connection.Name, 80, false) || !validSetupDraftText(connection.CredentialName, maxNamedSecretName, true) {
		return SetupDraftConnection{}, errSetupDraftInvalid
	}
	for _, value := range []string{connection.Origin, connection.Repository, connection.Username, connection.Project, connection.JobPath, connection.Environment, connection.Namespace, connection.BaseURL, connection.CredentialName} {
		if value != "" && !validSetupDraftText(value, 2048, false) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
	}
	if connection.Action == SetupDraftAdd && connection.ExistingID != "" || connection.Action != SetupDraftAdd && connection.ExistingID == "" {
		return SetupDraftConnection{}, errSetupDraftInvalid
	}
	result := connection
	switch connection.Kind {
	case SetupServiceGitHub:
		if connection.Origin == "" || connection.Repository == "" || connection.BaseURL != "" || connection.Username != "" || connection.Project != "" || connection.JobPath != "" || connection.Environment != "" || connection.Namespace != "" {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		origin, err := validateOrigin(connection.Origin)
		if err != nil || origin != connection.Origin || !validRepository(connection.Repository) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		if connection.Action != SetupDraftAdd && !githubIDPattern.MatchString(connection.ExistingID) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
	case SetupServiceJenkins:
		if connection.Origin != "" || connection.Repository != "" || connection.Project != "" || connection.Namespace != "" ||
			connection.BaseURL == "" || connection.Username == "" || connection.JobPath == "" ||
			!validJenkinsUsername(connection.Username) || !validJenkinsJobPath(connection.JobPath) || !validJenkinsEnvironmentLabel(connection.Environment) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		baseURL, err := validateJenkinsBaseURL(connection.BaseURL)
		if err != nil || baseURL != connection.BaseURL {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		if connection.Action != SetupDraftAdd && !jenkinsIDPattern.MatchString(connection.ExistingID) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
	case SetupServiceHarbor:
		if connection.Origin != "" || connection.JobPath != "" || connection.Environment != "" || connection.Namespace != "" ||
			connection.BaseURL == "" || connection.Username == "" || connection.Project == "" || connection.Repository == "" ||
			!validJenkinsUsername(connection.Username) || !validHarborProject(connection.Project) || !validHarborRepository(connection.Repository) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		baseURL, err := validateJenkinsBaseURL(connection.BaseURL)
		if err != nil || baseURL != connection.BaseURL {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		if connection.Action != SetupDraftAdd && !harborIDPattern.MatchString(connection.ExistingID) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
	case SetupServiceDashboard:
		if connection.Origin != "" || connection.Repository != "" || connection.Username != "" || connection.Project != "" || connection.JobPath != "" || connection.Environment != "" || connection.Namespace != "" || connection.BaseURL == "" {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		baseURL, err := validateJenkinsBaseURL(connection.BaseURL)
		if err != nil || baseURL != connection.BaseURL {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
		if connection.Action != SetupDraftAdd && !dashboardIDPattern.MatchString(connection.ExistingID) {
			return SetupDraftConnection{}, errSetupDraftInvalid
		}
	}
	return result, nil
}

func normalizeSetupDraftBundle(bundle SetupDraftBundle) (SetupDraftBundle, error) {
	if !validBundleName(bundle.Name) || !validSetupDraftText(bundle.Name, 80, false) || len(bundle.RepositoryIDs) == 0 || len(bundle.RepositoryIDs) > maxSetupDraftConnections || len(bundle.Environments) > maxSetupDraftBundles {
		return SetupDraftBundle{}, errSetupDraftInvalid
	}
	result := cloneSetupDraftBundle(bundle)
	seenRepositories := make(map[string]bool)
	for _, id := range result.RepositoryIDs {
		if !githubIDPattern.MatchString(id) || seenRepositories[id] {
			return SetupDraftBundle{}, errSetupDraftInvalid
		}
		seenRepositories[id] = true
	}
	seenEnvironments := make(map[string]bool)
	for _, environment := range result.Environments {
		if !validBundleName(environment.Name) || !validSetupDraftText(environment.Name, 80, false) || seenEnvironments[strings.ToLower(environment.Name)] {
			return SetupDraftBundle{}, errSetupDraftInvalid
		}
		seenEnvironments[strings.ToLower(environment.Name)] = true
		if environment.JenkinsTargetID != "" && !jenkinsIDPattern.MatchString(environment.JenkinsTargetID) ||
			environment.HarborTargetID != "" && !harborIDPattern.MatchString(environment.HarborTargetID) ||
			environment.DashboardTargetID != "" && !dashboardIDPattern.MatchString(environment.DashboardTargetID) {
			return SetupDraftBundle{}, errSetupDraftInvalid
		}
		hasNamespace, hasDeployment := environment.DashboardNamespace != "", environment.DashboardDeployment != ""
		if hasNamespace != hasDeployment || hasNamespace && environment.DashboardTargetID == "" ||
			hasNamespace && (!validServiceDashboardNamespace(environment.DashboardNamespace) || !validServiceDashboardDeployment(environment.DashboardDeployment)) {
			return SetupDraftBundle{}, errSetupDraftInvalid
		}
		for _, value := range []string{environment.JenkinsTargetID, environment.HarborTargetID, environment.DashboardTargetID, environment.DashboardNamespace, environment.DashboardDeployment} {
			if value != "" && !validSetupDraftText(value, 253, false) {
				return SetupDraftBundle{}, errSetupDraftInvalid
			}
		}
	}
	return result, nil
}

func normalizeSetupDraftSSHTarget(target SetupDraftSSHTarget) (SetupDraftSSHTarget, error) {
	if !validSetupDraftText(target.Name, 80, false) || !sshAliasPattern.MatchString(target.Alias) || len(target.Operations) == 0 || len(target.Operations) > maxSSHTargetOperations {
		return SetupDraftSSHTarget{}, errSetupDraftInvalid
	}
	result := cloneSetupDraftSSHTarget(target)
	seenIDs, seenNames := make(map[string]bool), make(map[string]bool)
	for index, operation := range result.Operations {
		if !validSetupDraftText(operation.Name, 80, false) || !validSetupDraftText(operation.Summary, 500, false) ||
			!validSetupDraftText(operation.Program, 256, false) {
			return SetupDraftSSHTarget{}, errSetupDraftInvalid
		}
		for _, argument := range operation.FixedArgs {
			if !validSetupDraftText(argument, 256, false) {
				return SetupDraftSSHTarget{}, errSetupDraftInvalid
			}
		}
		for _, parameter := range operation.Parameters {
			if !validSetupDraftText(parameter.Name, 32, false) || !validSetupDraftText(parameter.Description, 200, true) {
				return SetupDraftSSHTarget{}, errSetupDraftInvalid
			}
		}
		if operation.Revision != "" || operation.Approval.Revision != "" || len(operation.Approval.Scope) != 0 ||
			(operation.Risk != SSHRiskReadOnly && operation.Approval.Mode != OperationApprovalPerCall) {
			return SetupDraftSSHTarget{}, errSetupDraftInvalid
		}
		if operation.Risk == SSHRiskStateChanging && operation.Approval.Mode != OperationApprovalPerCall ||
			operation.Risk == SSHRiskDestructive && operation.Approval.Mode != OperationApprovalPerCall {
			return SetupDraftSSHTarget{}, errSetupDraftInvalid
		}
		normalized, err := normalizeSSHOperationDefinition(operation, "draft_target")
		if err != nil || normalized.Approval.Mode == OperationApprovalPreapproved || seenIDs[normalized.ID] || seenNames[strings.ToLower(normalized.Name)] {
			return SetupDraftSSHTarget{}, errSetupDraftInvalid
		}
		seenIDs[normalized.ID], seenNames[strings.ToLower(normalized.Name)] = true, true
		result.Operations[index] = normalized
	}
	return result, nil
}

func validSetupDraftRevision(revision string) bool {
	if revision == "" || len(revision) > 128 || containsSetupDraftSecret(revision) {
		return false
	}
	for _, char := range revision {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._:-", char)) {
			return false
		}
	}
	return true
}

func validSetupDraftText(value string, maxBytes int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		strings.ContainsAny(value, "\x00\r\n") || containsSetupDraftSecret(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func containsSetupDraftSecret(value string) bool {
	return privateKeyPEM.MatchString(value) || githubTokenPattern.MatchString(value) || credentialHeader.MatchString(value) ||
		credentialPattern.MatchString(value) || setupDraftCredentialRefInText.MatchString(value)
}
