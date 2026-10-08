package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

var errSetupDraftConfigStore = errors.New("setup draft settings are unavailable")

// setupDraftConfigStore adapts the canonical settings file to setup-draft
// snapshots. The controller owns the cross-process config lock around each
// Snapshot/CompareAndSwap pair, so this adapter must remain lock-neutral.
type setupDraftConfigStore struct {
	configPath string
}

func newSetupDraftConfigStore(configPath string) *setupDraftConfigStore {
	return &setupDraftConfigStore{configPath: configPath}
}

func (s *setupDraftConfigStore) Snapshot() (setupDraftConfigSnapshot, error) {
	cfg, err := s.readConfig()
	if err != nil {
		return setupDraftConfigSnapshot{}, errSetupDraftConfigStore
	}
	revision, err := setupDraftConfigRevision(cfg)
	if err != nil {
		return setupDraftConfigSnapshot{}, errSetupDraftConfigStore
	}
	snapshot := setupDraftConfigSnapshot{
		Revision:             revision,
		State:                setupDraftStateFromConfig(cfg),
		AvailableCredentials: namedSecretViews(cfg),
	}
	if !validSetupDraftSnapshot(snapshot) {
		return setupDraftConfigSnapshot{}, errSetupDraftConfigStore
	}
	return cloneSetupDraftSnapshot(snapshot), nil
}

func (s *setupDraftConfigStore) CompareAndSwap(expectedRevision string, state setupDraftState) (string, error) {
	cfg, err := s.readConfig()
	if err != nil {
		return "", errSetupDraftConfigStore
	}
	currentRevision, err := setupDraftConfigRevision(cfg)
	if err != nil || expectedRevision == "" || currentRevision != expectedRevision {
		return "", errSetupDraftStale
	}
	if !validSetupDraftState(state) {
		return "", errSetupDraftConfigStore
	}

	previousState := setupDraftStateFromConfig(cfg)
	updated, err := configWithSetupDraftState(cfg, state)
	if err != nil {
		return "", errSetupDraftConfigStore
	}
	for _, record := range state.Connections {
		if previous, ok := setupDraftConnectionByID(previousState.Connections, record.Kind, record.ID); !ok || !equalSetupDraftConnectionRecord(previous, record) {
			delete(updated.ConnectionTests, record.ID)
		}
	}
	updated.Version = configVersion
	setConfigAliases(&updated)
	if validateConfig(updated) != nil || validateNamedSecretReferenceCoverage(updated) != nil {
		return "", errSetupDraftConfigStore
	}
	if err := writeConfig(s.configPath, updated); err != nil {
		return "", errSetupDraftConfigStore
	}
	revision, err := setupDraftConfigRevision(updated)
	if err != nil || revision == currentRevision {
		return "", errSetupDraftConfigStore
	}
	return revision, nil
}

func (s *setupDraftConfigStore) readConfig() (config, error) {
	if s == nil || strings.TrimSpace(s.configPath) == "" {
		return config{}, errSetupDraftConfigStore
	}
	cfg, err := readConfig(s.configPath)
	if err != nil {
		return config{}, errSetupDraftConfigStore
	}
	return cfg, nil
}

func setupDraftConfigRevision(cfg config) (string, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return "", errSetupDraftConfigStore
	}
	digest := sha256.Sum256(encoded)
	return "config:sha256:" + hex.EncodeToString(digest[:]), nil
}

func setupDraftStateFromConfig(cfg config) setupDraftState {
	state := setupDraftState{
		Connections: make([]setupDraftConnectionRecord, 0,
			len(cfg.GitHubTargets)+len(cfg.JenkinsTargets)+len(cfg.HarborTargets)+len(cfg.DashboardTargets)),
		Bundles:    make([]setupDraftBundleRecord, 0, len(cfg.ServiceBundles)),
		SSHTargets: make([]setupDraftSSHTargetRecord, 0, len(cfg.SSHTargets)),
	}
	for _, target := range cfg.GitHubTargets {
		state.Connections = append(state.Connections, setupDraftConnectionRecord{
			ID: target.ID, Kind: SetupServiceGitHub, Name: target.Name, Origin: target.Origin,
			Repository: target.Repository, CredentialName: setupDraftCredentialName(cfg, target.SecretRef),
		})
	}
	for _, target := range cfg.JenkinsTargets {
		state.Connections = append(state.Connections, setupDraftConnectionRecord{
			ID: target.ID, Kind: SetupServiceJenkins, Name: target.Name, BaseURL: target.BaseURL,
			Username: target.Username, JobPath: target.JobPath, Environment: target.Environment,
			CredentialName: setupDraftCredentialName(cfg, target.SecretRef),
		})
	}
	for _, target := range cfg.HarborTargets {
		state.Connections = append(state.Connections, setupDraftConnectionRecord{
			ID: target.ID, Kind: SetupServiceHarbor, Name: target.Name, BaseURL: target.BaseURL,
			Username: target.Username, Project: target.Project, Repository: target.Repository,
			CredentialName: setupDraftCredentialName(cfg, target.SecretRef),
		})
	}
	for _, target := range cfg.DashboardTargets {
		state.Connections = append(state.Connections, setupDraftConnectionRecord{
			ID: target.ID, Kind: SetupServiceDashboard, Name: target.Name, BaseURL: target.BaseURL,
			CredentialName: setupDraftCredentialName(cfg, target.SecretRef),
		})
	}
	for _, bundle := range cfg.ServiceBundles {
		environments := make([]SetupDraftEnvironment, len(bundle.Environments))
		for index, environment := range bundle.Environments {
			environments[index] = SetupDraftEnvironment{
				Name: environment.Name, JenkinsTargetID: environment.JenkinsTargetID,
				HarborTargetID: environment.HarborTargetID, DashboardTargetID: environment.DashboardTargetID,
				DashboardNamespace: environment.DashboardNamespace, DashboardDeployment: environment.DashboardDeployment,
			}
		}
		state.Bundles = append(state.Bundles, setupDraftBundleRecord{
			ID: bundle.ID,
			Definition: SetupDraftBundle{
				Name: bundle.Name, RepositoryIDs: append([]string(nil), bundle.GitHubTargetIDs...), Environments: environments,
			},
		})
	}
	for _, target := range cfg.SSHTargets {
		state.SSHTargets = append(state.SSHTargets, setupDraftSSHTargetRecord{
			ID: target.ID,
			Definition: SetupDraftSSHTarget{
				Name: target.Name, Alias: target.Alias, Operations: cloneSSHOperations(target.Operations),
			},
		})
	}
	return state
}

func setupDraftCredentialName(cfg config, ref string) string {
	for _, secret := range cfg.NamedSecrets {
		if secret.CredentialRef == ref {
			return secret.Name
		}
	}
	return ""
}

func setupDraftConnectionByID(records []setupDraftConnectionRecord, kind SetupServiceKind, id string) (setupDraftConnectionRecord, bool) {
	for _, record := range records {
		if record.Kind == kind && record.ID == id {
			return record, true
		}
	}
	return setupDraftConnectionRecord{}, false
}

func configWithSetupDraftState(cfg config, state setupDraftState) (config, error) {
	github := make([]target, 0)
	jenkins := make([]jenkinsTarget, 0)
	harbor := make([]harborTarget, 0)
	dashboard := make([]dashboardTarget, 0)
	for _, record := range state.Connections {
		ref, disabled, existingJenkins := setupDraftConnectionCredential(cfg, record)
		if ref == "" {
			return config{}, errSetupDraftConfigStore
		}
		switch record.Kind {
		case SetupServiceGitHub:
			github = append(github, target{ID: record.ID, Name: record.Name, Origin: record.Origin, Repository: record.Repository, SecretRef: ref, Disabled: disabled})
		case SetupServiceJenkins:
			item := jenkinsTarget{
				ID: record.ID, Name: record.Name, BaseURL: record.BaseURL, Username: record.Username,
				JobPath: record.JobPath, Environment: record.Environment, SecretRef: ref, Disabled: disabled,
			}
			if existingJenkins.ID != "" && existingJenkins.NonProductionApprovalScope == jenkinsTriggerApprovalScope(item) {
				item.NonProductionPreapproved = existingJenkins.NonProductionPreapproved
				item.NonProductionApprovalScope = existingJenkins.NonProductionApprovalScope
			}
			jenkins = append(jenkins, item)
		case SetupServiceHarbor:
			harbor = append(harbor, harborTarget{
				ID: record.ID, Name: record.Name, BaseURL: record.BaseURL, Username: record.Username,
				Project: record.Project, Repository: record.Repository, SecretRef: ref, Disabled: disabled,
			})
		case SetupServiceDashboard:
			dashboard = append(dashboard, dashboardTarget{ID: record.ID, Name: record.Name, BaseURL: record.BaseURL, SecretRef: ref, Disabled: disabled})
		default:
			return config{}, errSetupDraftConfigStore
		}
	}

	bundles := make([]serviceBundle, 0, len(state.Bundles))
	for _, record := range state.Bundles {
		environments := make([]serviceEnvironment, len(record.Definition.Environments))
		for index, environment := range record.Definition.Environments {
			environments[index] = serviceEnvironment{
				Name: environment.Name, JenkinsTargetID: environment.JenkinsTargetID,
				HarborTargetID: environment.HarborTargetID, DashboardTargetID: environment.DashboardTargetID,
				DashboardNamespace: environment.DashboardNamespace, DashboardDeployment: environment.DashboardDeployment,
			}
		}
		bundles = append(bundles, serviceBundle{
			ID: record.ID, Name: record.Definition.Name,
			GitHubTargetIDs: append([]string(nil), record.Definition.RepositoryIDs...), Environments: environments,
		})
	}

	sshTargets := make([]sshTargetDefinition, 0, len(state.SSHTargets))
	for _, record := range state.SSHTargets {
		sshTargets = append(sshTargets, sshTargetDefinition{
			ID: record.ID, Name: record.Definition.Name, Alias: record.Definition.Alias,
			Operations: cloneSSHOperations(record.Definition.Operations),
		})
	}
	cfg.GitHubTargets, cfg.JenkinsTargets = github, jenkins
	cfg.HarborTargets, cfg.DashboardTargets = harbor, dashboard
	cfg.ServiceBundles, cfg.SSHTargets = bundles, sshTargets
	return cfg, nil
}

func setupDraftConnectionCredential(cfg config, record setupDraftConnectionRecord) (string, bool, jenkinsTarget) {
	var existingRef string
	var disabled bool
	var existingJenkins jenkinsTarget
	switch record.Kind {
	case SetupServiceGitHub:
		if index := findGitHubTargetIndex(cfg.GitHubTargets, record.ID); index >= 0 {
			existingRef, disabled = cfg.GitHubTargets[index].SecretRef, cfg.GitHubTargets[index].Disabled
		}
	case SetupServiceJenkins:
		if index := findJenkinsTargetIndex(cfg.JenkinsTargets, record.ID); index >= 0 {
			existingJenkins = cfg.JenkinsTargets[index]
			existingRef, disabled = existingJenkins.SecretRef, existingJenkins.Disabled
		}
	case SetupServiceHarbor:
		if index := findHarborTargetIndex(cfg.HarborTargets, record.ID); index >= 0 {
			existingRef, disabled = cfg.HarborTargets[index].SecretRef, cfg.HarborTargets[index].Disabled
		}
	case SetupServiceDashboard:
		if index := findDashboardTargetIndex(cfg.DashboardTargets, record.ID); index >= 0 {
			existingRef, disabled = cfg.DashboardTargets[index].SecretRef, cfg.DashboardTargets[index].Disabled
		}
	default:
		return "", false, jenkinsTarget{}
	}
	if record.CredentialName == "" {
		return existingRef, disabled, existingJenkins
	}
	for _, secret := range cfg.NamedSecrets {
		if strings.EqualFold(secret.Name, record.CredentialName) {
			return secret.CredentialRef, disabled, existingJenkins
		}
	}
	return "", disabled, existingJenkins
}

func cloneSSHOperations(operations []SSHOperationDefinition) []SSHOperationDefinition {
	result := make([]SSHOperationDefinition, len(operations))
	for index, operation := range operations {
		result[index] = cloneSSHOperation(operation)
	}
	return result
}
