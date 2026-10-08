package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"time"
)

const (
	readinessStatusPath          = "/readiness-status"
	readinessClientInspectionKey = "inspect_clients"
	readinessClientInspectionArg = "1"
	readinessConfigTimeout       = 2 * time.Second
	readinessClientTimeout       = 750 * time.Millisecond
	readinessObserverTimeout     = 1500 * time.Millisecond
	readinessMaximumResponseSize = 128 << 10
)

var (
	readinessVersionPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	readinessCredentialRefPattern = regexp.MustCompile(`(?:cred|secret):[a-f0-9]{32}`)
	readinessURLPattern           = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	readinessWindowsPathPattern   = regexp.MustCompile(`(?i)[a-z]:\\[^\s<>"']+`)
	readinessUnixPathPattern      = regexp.MustCompile(`(?:^|\s)/[^\s<>"']+`)
)

type readinessStatusHandler struct {
	configPath    string
	registrations MCPClientRegistrationController
	observer      ResidentEndpointObservationProvider
}

type readinessStatusDTO struct {
	State            string                     `json:"state"`
	ObservedAt       time.Time                  `json:"observed_at"`
	ConfigurationRev string                     `json:"configuration_revision,omitempty"`
	Targets          []readinessTargetDTO       `json:"targets"`
	ServiceBundles   []readinessBundleDTO       `json:"service_bundles"`
	ResidentEndpoint readinessEndpointDTO       `json:"resident_endpoint"`
	Clients          []readinessClientDTO       `json:"clients"`
	MCPFeatures      []readinessFeatureDTO      `json:"mcp_features"`
	NamedSecrets     readinessNamedSecretCounts `json:"named_secrets"`
	UserEnvironment  readinessUnobservedCount   `json:"user_environment"`
	SSH              readinessSSHCounts         `json:"ssh"`
	PythonTasks      readinessPythonTaskCounts  `json:"python_tasks"`
	ErrorCode        string                     `json:"error_code,omitempty"`
}

type readinessTargetDTO struct {
	Type            string                     `json:"type"`
	Name            string                     `json:"name"`
	Enabled         bool                       `json:"enabled"`
	CredentialState string                     `json:"credential_state"`
	Actions         []string                   `json:"actions"`
	ConnectionTest  readinessConnectionTestDTO `json:"connection_test"`
}

type readinessConnectionTestDTO struct {
	State       string `json:"state"`
	CompletedAt string `json:"completed_at,omitempty"`
	Source      string `json:"source"`
}

type readinessBundleDTO struct {
	Name            string                    `json:"name"`
	RepositoryCount int                       `json:"repository_count"`
	Environments    []readinessEnvironmentDTO `json:"environments"`
}

type readinessEnvironmentDTO struct {
	Name                       string   `json:"name"`
	JenkinsLinked              bool     `json:"jenkins_linked"`
	JenkinsActions             []string `json:"jenkins_actions"`
	HarborLinked               bool     `json:"harbor_linked"`
	HarborActions              []string `json:"harbor_actions"`
	DashboardLinked            bool     `json:"dashboard_linked"`
	DashboardActions           []string `json:"dashboard_actions"`
	DashboardDiagnosisEligible bool     `json:"dashboard_diagnosis_eligible"`
}

type readinessEndpointDTO struct {
	State      string     `json:"state"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

type readinessClientDTO struct {
	ClientID       string `json:"client_id"`
	Workflow       string `json:"workflow"`
	Inspection     string `json:"inspection"`
	Supported      bool   `json:"supported"`
	Installed      string `json:"installed"`
	Version        string `json:"version,omitempty"`
	Registration   string `json:"registration"`
	EvidenceSource string `json:"evidence_source"`
	Backup         string `json:"backup"`
	Connection     string `json:"connection"`
	ToolCall       string `json:"tool_call"`
}

type readinessFeatureDTO struct {
	Key   string   `json:"key"`
	Tools []string `json:"tools"`
}

type readinessNamedSecretCounts struct {
	ReferenceCount int `json:"reference_count"`
}

type readinessUnobservedCount struct {
	State string `json:"state"`
	Count *int   `json:"count"`
}

type readinessSSHCounts struct {
	TargetCount    int `json:"target_count"`
	OperationCount int `json:"operation_count"`
}

type readinessPythonTaskCounts struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
}

func newReadinessStatusHandler(
	configPath string,
	registrations MCPClientRegistrationController,
	observer ResidentEndpointObservationProvider,
) http.Handler {
	handler := &readinessStatusHandler{
		configPath:    configPath,
		registrations: registrations,
		observer:      observer,
	}
	return http.HandlerFunc(handler.serveHTTP)
}

func (h *readinessStatusHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	cfg, err := h.readConfigSnapshot(r.Context())
	if err != nil {
		writeReadinessUnavailable(w)
		return
	}

	response := readinessStatusFromConfig(cfg)
	response.ResidentEndpoint = h.observeResidentEndpoint(r.Context())
	if readinessClientInspectionRequested(r) {
		response.Clients = h.inspectClients(r.Context())
	}
	response.ObservedAt = time.Now().UTC()

	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > readinessMaximumResponseSize {
		writeReadinessUnavailable(w)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (h *readinessStatusHandler) readConfigSnapshot(ctx context.Context) (config, error) {
	requestCtx, cancel := context.WithTimeout(ctx, readinessConfigTimeout)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return config{}, err
	}

	var cfg config
	err := withConfigLock(func() error {
		if err := requestCtx.Err(); err != nil {
			return err
		}
		var readErr error
		cfg, readErr = readConfig(h.configPath)
		return readErr
	})
	if err != nil {
		return config{}, errors.New("settings unavailable")
	}
	return cfg, nil
}

func readinessStatusFromConfig(cfg config) readinessStatusDTO {
	response := readinessStatusDTO{
		State:            "needs_setup",
		ConfigurationRev: readinessConfigurationRevision(cfg),
		Targets:          make([]readinessTargetDTO, 0),
		ServiceBundles:   make([]readinessBundleDTO, 0, len(cfg.ServiceBundles)),
		ResidentEndpoint: readinessEndpointDTO{State: "not_checked"},
		Clients:          defaultReadinessClients(),
		MCPFeatures:      readinessFeatureCatalog(),
		NamedSecrets:     readinessNamedSecretCounts{ReferenceCount: len(cfg.NamedSecrets)},
		UserEnvironment:  readinessUnobservedCount{State: "not_checked"},
		SSH:              readinessSSHCounts{TargetCount: len(cfg.SSHTargets)},
		PythonTasks:      readinessPythonTaskCounts{Total: len(cfg.PythonTasks)},
	}
	for _, target := range cfg.SSHTargets {
		response.SSH.OperationCount += len(target.Operations)
	}

	addTarget := func(kind, name, secretRef string, enabled bool, actions []string, targetID string) {
		if enabled {
			response.State = "configured"
		}
		response.Targets = append(response.Targets, readinessTargetDTO{
			Type:            kind,
			Name:            readinessSafeName(name),
			Enabled:         enabled,
			CredentialState: readinessCredentialReferenceState(secretRef),
			Actions:         append([]string{}, actions...),
			ConnectionTest:  readinessHistoricalConnectionTest(cfg, targetID),
		})
	}

	for _, target := range cfg.GitHubTargets {
		actions := []string{}
		if !target.Disabled {
			actions = []string{"github_repository", "github_pull_request", "registered_target_connection_test"}
		}
		addTarget("github", target.Name, target.SecretRef, !target.Disabled, actions, target.ID)
	}
	for _, target := range cfg.JenkinsTargets {
		actions := []string{}
		if !target.Disabled {
			actions = []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log", "registered_target_connection_test"}
			if validateJenkinsTarget(target) == nil {
				actions = append(actions, "jenkins_run_registered_job")
			}
		}
		addTarget("jenkins", target.Name, target.SecretRef, !target.Disabled, actions, target.ID)
	}
	for _, target := range cfg.HarborTargets {
		actions := []string{}
		if !target.Disabled {
			actions = []string{"harbor_repository_artifacts", "harbor_project_quota", "registered_target_connection_test"}
		}
		addTarget("harbor", target.Name, target.SecretRef, !target.Disabled, actions, target.ID)
	}
	for _, target := range cfg.DashboardTargets {
		actions := []string{}
		if !target.Disabled {
			actions = []string{"dashboard_namespaces", "registered_target_connection_test"}
		}
		addTarget("dashboard", target.Name, target.SecretRef, !target.Disabled, actions, target.ID)
	}

	sort.Slice(response.Targets, func(i, j int) bool {
		if response.Targets[i].Type == response.Targets[j].Type {
			return response.Targets[i].Name < response.Targets[j].Name
		}
		return response.Targets[i].Type < response.Targets[j].Type
	})

	for _, bundle := range cfg.ServiceBundles {
		bundleDTO := readinessBundleDTO{
			Name:            readinessSafeName(bundle.Name),
			RepositoryCount: len(bundle.GitHubTargetIDs),
			Environments:    make([]readinessEnvironmentDTO, 0, len(bundle.Environments)),
		}
		for _, environment := range bundle.Environments {
			environmentDTO := readinessEnvironmentDTO{
				Name:             readinessSafeName(environment.Name),
				JenkinsActions:   []string{},
				HarborActions:    []string{},
				DashboardActions: []string{},
			}
			if environment.JenkinsTargetID != "" {
				environmentDTO.JenkinsLinked = true
				if target, ok := serviceCatalogTarget(cfg, "jenkins", environment.JenkinsTargetID); ok {
					environmentDTO.JenkinsActions = append([]string{}, target.Actions...)
				}
			}
			if environment.HarborTargetID != "" {
				environmentDTO.HarborLinked = true
				if target, ok := serviceCatalogTarget(cfg, "harbor", environment.HarborTargetID); ok {
					environmentDTO.HarborActions = append([]string{}, target.Actions...)
				}
			}
			if environment.DashboardTargetID != "" {
				environmentDTO.DashboardLinked = true
				if target, ok := serviceCatalogTarget(cfg, "dashboard", environment.DashboardTargetID); ok && len(target.Actions) > 0 && validServiceDashboardDeploymentMapping(environment) {
					environmentDTO.DashboardActions = append([]string{}, target.Actions...)
					environmentDTO.DashboardActions = append(environmentDTO.DashboardActions,
						"dashboard_deployment_status",
						"dashboard_deployment_diagnosis",
						"dashboard_deployment_events",
						"dashboard_deployment_pods",
						"dashboard_deployment_pod_logs",
					)
					environmentDTO.DashboardDiagnosisEligible = true
				}
			}
			bundleDTO.Environments = append(bundleDTO.Environments, environmentDTO)
		}
		response.ServiceBundles = append(response.ServiceBundles, bundleDTO)
	}
	sort.Slice(response.ServiceBundles, func(i, j int) bool {
		return response.ServiceBundles[i].Name < response.ServiceBundles[j].Name
	})
	for i := range response.ServiceBundles {
		sort.Slice(response.ServiceBundles[i].Environments, func(a, b int) bool {
			return response.ServiceBundles[i].Environments[a].Name < response.ServiceBundles[i].Environments[b].Name
		})
	}
	for _, task := range cfg.PythonTasks {
		if !task.Disabled {
			response.PythonTasks.Enabled++
		}
	}
	return response
}

func readinessCredentialReferenceState(secretRef string) string {
	if secretRef == "" {
		return "no_reference"
	}
	return "reference_saved"
}

func readinessSafeName(name string) string {
	const maximumReadinessNameLength = 128
	if readinessCredentialRefPattern.MatchString(name) || readinessURLPattern.MatchString(name) || readinessWindowsPathPattern.MatchString(name) || readinessUnixPathPattern.MatchString(name) {
		return "[REDACTED]"
	}
	if len(name) > maximumReadinessNameLength {
		name = name[:maximumReadinessNameLength]
	}
	return cleanOutput(name, "", maximumReadinessNameLength)
}

func readinessHistoricalConnectionTest(cfg config, targetID string) readinessConnectionTestDTO {
	test := connectionTestFor(cfg, targetID)
	if test == nil {
		return readinessConnectionTestDTO{State: "not_tested", Source: "none"}
	}
	return readinessConnectionTestDTO{
		State:       test.Result,
		CompletedAt: test.CompletedAt,
		Source:      "saved_history",
	}
}

func readinessConfigurationRevision(cfg config) string {
	revisionConfig := cfg
	revisionConfig.ConnectionTests = nil
	revisionConfig.Target = nil
	revisionConfig.Jenkins = nil
	// Only the opaque digest crosses the API boundary; config bytes do not.
	encoded, err := json.Marshal(revisionConfig)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (h *readinessStatusHandler) observeResidentEndpoint(parent context.Context) readinessEndpointDTO {
	if h.observer == nil {
		return readinessEndpointDTO{State: "not_checked"}
	}
	ctx, cancel := context.WithTimeout(parent, readinessObserverTimeout)
	defer cancel()
	observation := h.observer.ObserveResidentEndpoint(ctx)
	state := "unknown"
	switch observation.State {
	case ResidentEndpointResponding:
		state = "responding"
	case ResidentEndpointNotReachable:
		state = "not_reachable"
	case ResidentEndpointUnknown:
		state = "unknown"
	case ResidentEndpointUnavailable:
		state = "unavailable"
	}
	result := readinessEndpointDTO{State: state}
	if !observation.ObservedAt.IsZero() {
		observedAt := observation.ObservedAt.UTC()
		result.ObservedAt = &observedAt
	}
	return result
}

func defaultReadinessClients() []readinessClientDTO {
	clientIDs := readinessClientIDs()
	clients := make([]readinessClientDTO, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		clients = append(clients, readinessClientDTO{
			ClientID:       clientID,
			Workflow:       "available",
			Inspection:     "not_checked",
			Supported:      isSupportedMCPClientID(clientID),
			Installed:      "not_checked",
			Registration:   "not_checked",
			EvidenceSource: "not_checked",
			Backup:         "not_checked",
			Connection:     "unverified",
			ToolCall:       "unverified",
		})
	}
	return clients
}

func readinessClientInspectionRequested(r *http.Request) bool {
	return r.URL.RawQuery == readinessClientInspectionKey+"="+readinessClientInspectionArg
}

func (h *readinessStatusHandler) inspectClients(parent context.Context) []readinessClientDTO {
	clients := defaultReadinessClients()
	if h.registrations == nil {
		for i := range clients {
			clients[i] = unavailableReadinessClient(clients[i].ClientID)
		}
		return clients
	}
	for i := range clients {
		ctx, cancel := context.WithTimeout(parent, readinessClientTimeout)
		status, err := h.registrations.InspectClient(ctx, clients[i].ClientID)
		cancel()
		if err != nil || status.ClientID != clients[i].ClientID {
			clients[i] = unavailableReadinessClient(clients[i].ClientID)
			continue
		}
		clients[i] = safeReadinessClient(status)
	}
	return clients
}

func unavailableReadinessClient(clientID string) readinessClientDTO {
	return readinessClientDTO{
		ClientID:       clientID,
		Workflow:       "available",
		Inspection:     "unavailable",
		Supported:      isSupportedMCPClientID(clientID),
		Installed:      "not_checked",
		Registration:   "not_checked",
		EvidenceSource: "not_checked",
		Backup:         "not_checked",
		Connection:     "unverified",
		ToolCall:       "unverified",
	}
}

func safeReadinessClient(status MCPClientStatus) readinessClientDTO {
	result := unavailableReadinessClient(status.ClientID)
	result.Inspection = "observed"
	if status.Installed == mcpClientInstalled || status.Installed == mcpClientInstalledNotObserved {
		result.Installed = status.Installed
	}
	if readinessVersionPattern.MatchString(status.Version) {
		result.Version = status.Version
	}
	switch status.Registration {
	case mcpClientRegistrationRegistered, mcpClientRegistrationNotRegistered, mcpClientRegistrationConflict, mcpClientRegistrationNotObserved:
		result.Registration = status.Registration
	}
	switch status.EvidenceSource {
	case mcpClientEvidenceNotObserved, mcpClientEvidenceConfigObserved:
		result.EvidenceSource = status.EvidenceSource
	}
	switch status.Backup {
	case mcpClientBackupNotCreated, mcpClientBackupCreated, mcpClientBackupApplying, mcpClientBackupApplied, mcpClientBackupRestored, mcpClientBackupStale:
		result.Backup = status.Backup
	}
	return result
}

func readinessFeatureCatalog() []readinessFeatureDTO {
	return []readinessFeatureDTO{
		{Key: "inventory", Tools: []string{"registered_targets", "registered_service_bundles", "registered_target_connection_test"}},
		{Key: "setup", Tools: []string{mcpToolSubmitSetupDraft}},
		{Key: "github", Tools: []string{"github_repository", "github_pull_request"}},
		{Key: "jenkins", Tools: []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log", "jenkins_run_registered_job"}},
		{Key: "harbor", Tools: []string{"harbor_repository_artifacts", "harbor_project_quota"}},
		{Key: "dashboard", Tools: []string{"dashboard_namespaces", "dashboard_deployment_status", "dashboard_deployment_diagnosis", "dashboard_deployment_events", "dashboard_deployment_pods", "dashboard_deployment_pod_logs"}},
		{Key: "python", Tools: []string{"registered_python_tasks", "python_run_registered_task"}},
		{Key: "ssh", Tools: []string{mcpToolRegisteredSSHTargets, mcpToolRunSSHOperation}},
	}
}

func writeReadinessUnavailable(w http.ResponseWriter) {
	response := readinessStatusDTO{
		State:            "unavailable",
		ObservedAt:       time.Now().UTC(),
		Targets:          []readinessTargetDTO{},
		ServiceBundles:   []readinessBundleDTO{},
		ResidentEndpoint: readinessEndpointDTO{State: "not_checked"},
		Clients:          defaultReadinessClients(),
		MCPFeatures:      readinessFeatureCatalog(),
		NamedSecrets:     readinessNamedSecretCounts{},
		UserEnvironment:  readinessUnobservedCount{State: "not_checked"},
		SSH:              readinessSSHCounts{},
		PythonTasks:      readinessPythonTaskCounts{},
		ErrorCode:        "settings_unavailable",
	}
	encoded, _ := json.Marshal(response)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(encoded)
}

func readinessFeatureToolNames() []string {
	tools := make([]string, 0)
	for _, feature := range readinessFeatureCatalog() {
		tools = append(tools, feature.Tools...)
	}
	sort.Strings(tools)
	return tools
}

func readinessClientIDs() []string {
	return []string{mcpClientIDCodex, mcpClientIDClaude, mcpClientIDGemini}
}

func readinessNormalizeTargetActions(actions []string) []string {
	if actions == nil {
		return []string{}
	}
	result := append([]string{}, actions...)
	sort.Strings(result)
	return result
}
