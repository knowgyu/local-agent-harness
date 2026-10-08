package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxSettingsImportFileBytes     = maxConfigSize
	maxSettingsImportRequestBytes  = maxSettingsImportFileBytes + (8 << 10)
	maxSettingsImportResponseBytes = 8 << 10
)

const settingsImportSource = "selected Local Agent Harness settings JSON; actual path unverified"

type settingsImportPreview struct {
	Source              string                    `json:"source"`
	Version             int                       `json:"version"`
	Targets             settingsImportTargetCount `json:"targets"`
	ServiceBundles      int                       `json:"service_bundles"`
	Environments        int                       `json:"environments"`
	DisabledTargets     int                       `json:"disabled_targets"`
	PythonTasks         int                       `json:"python_tasks"`
	DisabledPythonTasks int                       `json:"disabled_python_tasks"`
	SSHTargets          int                       `json:"ssh_targets"`
	Error               string                    `json:"error,omitempty"`
}

type settingsImportTargetCount struct {
	GitHub    int `json:"github"`
	Jenkins   int `json:"jenkins"`
	Harbor    int `json:"harbor"`
	Dashboard int `json:"dashboard"`
}

// handleSettingsImportPreview accepts only a user-selected settings JSON file.
// It never reads or writes the app's active config, credential store, or network.
func (a *app) handleSettingsImportPreview(w http.ResponseWriter, r *http.Request) {
	checkResponse := newPostResponseCapture()
	postValid := a.checkPostLimit(checkResponse, r, maxSettingsImportRequestBytes)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !postValid {
		status := checkResponse.status
		if status == 0 {
			status = http.StatusForbidden
		}
		writeSettingsImportJSON(w, status, settingsImportPreview{Error: "Request rejected."})
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 ||
		len(r.MultipartForm.File["settings_json"]) != 1 ||
		len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["csrf"]) != 1 {
		writeSettingsImportJSON(w, http.StatusBadRequest, settingsImportPreview{Error: "Choose one Local Agent Harness settings JSON file."})
		return
	}

	fileHeader := r.MultipartForm.File["settings_json"][0]
	filename := strings.ReplaceAll(fileHeader.Filename, "\\", "/")
	if !isExactSettingsJSONFilePart(fileHeader.Header.Values("Content-Disposition"), fileHeader.Filename) ||
		!strings.EqualFold(path.Ext(filename), ".json") ||
		fileHeader.Size < 1 || fileHeader.Size > maxSettingsImportFileBytes {
		writeSettingsImportJSON(w, http.StatusBadRequest, settingsImportPreview{Error: "Choose one nonempty .json file no larger than 1 MiB."})
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeSettingsImportJSON(w, http.StatusBadRequest, settingsImportPreview{Error: "The selected settings file could not be read."})
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSettingsImportFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxSettingsImportFileBytes {
		writeSettingsImportJSON(w, http.StatusBadRequest, settingsImportPreview{Error: "The selected settings file could not be read within the 1 MiB limit."})
		return
	}

	cfg, sourceVersion, err := parseSelectedSettingsJSONWithVersion(data)
	if err != nil {
		writeSettingsImportJSON(w, http.StatusBadRequest, settingsImportPreview{Error: "The selected file is not a valid supported settings JSON version."})
		return
	}
	preview := settingsImportPreview{
		Source:              settingsImportSource,
		Version:             sourceVersion,
		Targets:             settingsImportTargetCount{GitHub: len(cfg.GitHubTargets), Jenkins: len(cfg.JenkinsTargets), Harbor: len(cfg.HarborTargets), Dashboard: len(cfg.DashboardTargets)},
		ServiceBundles:      len(cfg.ServiceBundles),
		DisabledTargets:     countDisabledSettingsTargets(cfg),
		PythonTasks:         len(cfg.PythonTasks),
		DisabledPythonTasks: countDisabledPythonTasks(cfg),
		SSHTargets:          len(cfg.SSHTargets),
	}
	for _, bundle := range cfg.ServiceBundles {
		preview.Environments += len(bundle.Environments)
	}
	writeSettingsImportJSON(w, http.StatusOK, preview)
}

func isExactSettingsJSONFilePart(dispositions []string, normalizedFilename string) bool {
	if len(dispositions) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(dispositions[0])
	return err == nil && strings.EqualFold(mediaType, "form-data") && len(params) == 2 &&
		params["name"] == "settings_json" && params["filename"] == normalizedFilename
}

func countDisabledSettingsTargets(cfg config) int {
	count := 0
	for _, target := range cfg.GitHubTargets {
		if target.Disabled {
			count++
		}
	}
	for _, target := range cfg.JenkinsTargets {
		if target.Disabled {
			count++
		}
	}
	for _, target := range cfg.HarborTargets {
		if target.Disabled {
			count++
		}
	}
	for _, target := range cfg.DashboardTargets {
		if target.Disabled {
			count++
		}
	}
	return count
}

func countDisabledPythonTasks(cfg config) int {
	count := 0
	for _, task := range cfg.PythonTasks {
		if task.Disabled {
			count++
		}
	}
	return count
}

func parseSelectedSettingsJSON(data []byte) (config, error) {
	cfg, _, err := parseSelectedSettingsJSONWithVersion(data)
	return cfg, err
}

func parseSelectedSettingsJSONWithVersion(data []byte) (config, int, error) {
	if len(data) == 0 || len(data) > maxSettingsImportFileBytes || !utf8.Valid(data) {
		return config{}, 0, errors.New("invalid settings JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := parseSettingsJSONValue(decoder, 0)
	if err != nil {
		return config{}, 0, errors.New("invalid settings JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return config{}, 0, errors.New("trailing settings JSON")
	}
	if validateSettingsJSONSchema(value) != nil {
		return config{}, 0, errors.New("unsupported settings JSON fields")
	}
	root := value.(map[string]any)
	versionNumber, ok := root["version"].(json.Number)
	if !ok {
		return config{}, 0, errors.New("invalid settings JSON version")
	}
	sourceVersion, err := versionNumber.Int64()
	if err != nil {
		return config{}, 0, errors.New("invalid settings JSON version")
	}
	var cfg config
	switch sourceVersion {
	case 4:
		var old configV4
		if decodeConfig(data, &old) != nil || old.Version != 4 {
			return config{}, 0, errors.New("invalid settings JSON")
		}
		cfg = config{
			Version: configVersion, ConnectionTests: old.ConnectionTests,
			GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
			HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
			ServiceBundles: old.ServiceBundles,
		}
	case 5:
		var old configV5
		if decodeConfig(data, &old) != nil || old.Version != 5 {
			return config{}, 0, errors.New("invalid settings JSON")
		}
		cfg = config{
			Version: configVersion, ConnectionTests: old.ConnectionTests,
			GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
			HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
			ServiceBundles: old.ServiceBundles,
		}
		for _, task := range old.PythonTasks {
			cfg.PythonTasks = append(cfg.PythonTasks, pythonTask{
				ID: task.ID, Name: task.Name, InterpreterPath: task.InterpreterPath,
				ScriptPath: task.ScriptPath, Disabled: task.Disabled,
			})
		}
	case 6:
		var old configV6
		if decodeConfig(data, &old) != nil || old.Version != 6 {
			return config{}, 0, errors.New("invalid settings JSON")
		}
		cfg = config{
			Version: configVersion, ConnectionTests: old.ConnectionTests,
			GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
			HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
			ServiceBundles: old.ServiceBundles,
		}
		for _, task := range old.PythonTasks {
			cfg.PythonTasks = append(cfg.PythonTasks, pythonTask{
				ID: task.ID, Name: task.Name, InterpreterPath: task.InterpreterPath,
				ScriptPath: task.ScriptPath, Disabled: task.Disabled,
				SecretEnvName: task.SecretEnvName, SecretRef: task.SecretRef,
			})
		}
	case 7:
		var old configV7
		if decodeConfig(data, &old) != nil || old.Version != 7 {
			return config{}, 0, errors.New("invalid settings JSON")
		}
		cfg = config{
			Version: configVersion, ConnectionTests: old.ConnectionTests,
			GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
			HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
			ServiceBundles: old.ServiceBundles, PythonTasks: old.PythonTasks,
		}
	case 8:
		var err error
		cfg, err = migrateConfigV8(data)
		if err != nil {
			return config{}, 0, errors.New("invalid settings JSON")
		}
	case configVersion:
		if decodeConfig(data, &cfg) != nil {
			return config{}, 0, errors.New("invalid settings JSON")
		}
	default:
		return config{}, 0, errors.New("unsupported settings JSON version")
	}
	if sourceVersion < configVersion && ensureNamedSecretMetadata(&cfg) != nil {
		return config{}, 0, errors.New("invalid settings JSON")
	}
	if validateConfig(cfg) != nil || validateNamedSecretReferenceCoverage(cfg) != nil {
		return config{}, 0, errors.New("invalid settings JSON")
	}
	return cfg, int(sourceVersion), nil
}

// parseSettingsJSONValue preserves object keys long enough to reject duplicate
// keys before encoding/json's struct decoder can silently accept the last one.
func parseSettingsJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("settings JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch delimiter, ok := token.(json.Delim); {
	case ok && delimiter == '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid settings JSON object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate settings JSON object key")
			}
			value, err := parseSettingsJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("invalid settings JSON object")
		}
		return object, nil
	case ok && delimiter == '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := parseSettingsJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("invalid settings JSON array")
		}
		return array, nil
	default:
		if token == nil {
			return nil, errors.New("null settings JSON value")
		}
		return token, nil
	}
}

func validateSettingsJSONSchema(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return errors.New("unsupported settings JSON root")
	}
	version, ok := root["version"].(json.Number)
	if !ok {
		return errors.New("unsupported settings JSON version")
	}
	versionNumber, err := version.Int64()
	if err != nil {
		return errors.New("unsupported settings JSON version")
	}
	allowedRootKeys := []string{"version", "github_targets", "jenkins_targets", "harbor_targets", "dashboard_targets", "service_bundles", "connection_tests"}
	if versionNumber == 5 || versionNumber == 6 || versionNumber == 7 || versionNumber == 8 || versionNumber == int64(configVersion) {
		allowedRootKeys = append(allowedRootKeys, "python_tasks")
	} else if versionNumber != 4 {
		return errors.New("unsupported settings JSON version")
	}
	if versionNumber == 8 || versionNumber == int64(configVersion) {
		allowedRootKeys = append(allowedRootKeys, "named_secrets")
	}
	if versionNumber == int64(configVersion) {
		allowedRootKeys = append(allowedRootKeys, "ssh_targets")
	}
	if !hasOnlySettingsJSONKeys(root, allowedRootKeys...) {
		return errors.New("unsupported settings JSON root")
	}
	if err := validateSettingsTargetArray(root, "github_targets", "id", "name", "origin", "repository", "secret_ref", "disabled"); err != nil {
		return err
	}
	if err := validateSettingsTargetArray(root, "jenkins_targets", "id", "name", "base_url", "username", "job_path", "environment", "secret_ref", "nonproduction_preapproved", "nonproduction_approval_scope", "disabled"); err != nil {
		return err
	}
	if err := validateSettingsTargetArray(root, "harbor_targets", "id", "name", "base_url", "username", "project", "repository", "secret_ref", "disabled"); err != nil {
		return err
	}
	if err := validateSettingsTargetArray(root, "dashboard_targets", "id", "name", "base_url", "secret_ref", "disabled"); err != nil {
		return err
	}
	if bundles, ok := root["service_bundles"]; ok {
		bundleArray, ok := bundles.([]any)
		if !ok {
			return errors.New("invalid service bundle list")
		}
		for _, item := range bundleArray {
			bundle, ok := item.(map[string]any)
			if !ok || !hasOnlySettingsJSONKeys(bundle, "id", "name", "github_target_ids", "environments") {
				return errors.New("invalid service bundle")
			}
			if environments, ok := bundle["environments"]; ok {
				environmentArray, ok := environments.([]any)
				if !ok {
					return errors.New("invalid service environment list")
				}
				for _, environmentItem := range environmentArray {
					environment, ok := environmentItem.(map[string]any)
					if !ok || !hasOnlySettingsJSONKeys(environment, "name", "jenkins_target_id", "harbor_target_id", "dashboard_target_id", "dashboard_namespace", "dashboard_deployment") {
						return errors.New("invalid service environment")
					}
				}
			}
		}
	}
	if history, ok := root["connection_tests"]; ok {
		historyMap, ok := history.(map[string]any)
		if !ok {
			return errors.New("invalid connection test history")
		}
		for _, value := range historyMap {
			entry, ok := value.(map[string]any)
			if !ok || !hasOnlySettingsJSONKeys(entry, "result", "completed_at") {
				return errors.New("invalid connection test record")
			}
		}
	}
	if tasks, ok := root["python_tasks"]; ok {
		taskArray, ok := tasks.([]any)
		if !ok {
			return errors.New("invalid Python task list")
		}
		for _, item := range taskArray {
			task, ok := item.(map[string]any)
			allowedTaskKeys := []string{"id", "name", "interpreter_path", "script_path", "disabled"}
			if versionNumber == 6 || versionNumber == 7 || versionNumber == 8 || versionNumber == int64(configVersion) {
				allowedTaskKeys = append(allowedTaskKeys, "secret_env_name", "secret_ref")
			}
			if versionNumber == 7 || versionNumber == 8 || versionNumber == int64(configVersion) {
				allowedTaskKeys = append(allowedTaskKeys,
					"interpreter_sha256", "interpreter_size", "script_sha256", "script_size")
			}
			if !ok || !hasOnlySettingsJSONKeys(task, allowedTaskKeys...) {
				return errors.New("invalid Python task")
			}
		}
	}
	if versionNumber == int64(configVersion) {
		if err := validateSettingsSSHTargetArray(root); err != nil {
			return err
		}
	}
	return nil
}

func validateSettingsSSHTargetArray(root map[string]any) error {
	value, present := root["ssh_targets"]
	if !present {
		return nil
	}
	targets, ok := value.([]any)
	if !ok {
		return errors.New("invalid SSH target list")
	}
	for _, item := range targets {
		target, ok := item.(map[string]any)
		if !ok || !hasOnlySettingsJSONKeys(target, "id", "name", "alias", "operations") {
			return errors.New("invalid SSH target")
		}
		operations, present := target["operations"]
		if !present {
			continue
		}
		operationArray, ok := operations.([]any)
		if !ok {
			return errors.New("invalid SSH operation list")
		}
		for _, operationItem := range operationArray {
			operation, ok := operationItem.(map[string]any)
			if !ok || !hasOnlySettingsJSONKeys(operation, "id", "name", "summary", "program", "fixed_args", "parameters", "risk", "approval", "revision") {
				return errors.New("invalid SSH operation")
			}
			if approvalValue, present := operation["approval"]; present {
				approval, ok := approvalValue.(map[string]any)
				if !ok || !hasOnlySettingsJSONKeys(approval, "mode", "revision", "scope") {
					return errors.New("invalid SSH approval policy")
				}
			}
			if parameterValue, present := operation["parameters"]; present {
				parameters, ok := parameterValue.([]any)
				if !ok {
					return errors.New("invalid SSH parameter list")
				}
				for _, parameterValue := range parameters {
					parameter, ok := parameterValue.(map[string]any)
					if !ok || !hasOnlySettingsJSONKeys(parameter, "name", "type", "required", "description") {
						return errors.New("invalid SSH parameter")
					}
				}
			}
		}
	}
	return nil
}

func validateSettingsTargetArray(root map[string]any, field string, keys ...string) error {
	value, present := root[field]
	if !present {
		return nil
	}
	targets, ok := value.([]any)
	if !ok {
		return errors.New("invalid target list")
	}
	for _, item := range targets {
		target, ok := item.(map[string]any)
		if !ok || !hasOnlySettingsJSONKeys(target, keys...) {
			return errors.New("invalid target record")
		}
	}
	return nil
}

func hasOnlySettingsJSONKeys(object map[string]any, allowed ...string) bool {
	keys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		keys[key] = struct{}{}
	}
	for key := range object {
		if _, ok := keys[key]; !ok {
			return false
		}
	}
	return true
}

func writeSettingsImportJSON(w http.ResponseWriter, status int, value settingsImportPreview) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Preview could not be encoded."}`)
	}
	if len(data)+1 > maxSettingsImportResponseBytes {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Preview exceeds the 8 KiB response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
