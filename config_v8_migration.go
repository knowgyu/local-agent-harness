package main

import "errors"

// configV8 preserves the strict settings shape used before SSH targets became
// part of the canonical config. A v8 migration deliberately initializes the
// new SSH collection as empty while retaining every existing v8 field.
type configV8 struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	NamedSecrets     []namedSecretMetadata     `json:"named_secrets,omitempty"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
	PythonTasks      []pythonTask              `json:"python_tasks,omitempty"`
}

func migrateConfigV8(data []byte) (config, error) {
	var old configV8
	if decodeConfig(data, &old) != nil || old.Version != 8 {
		return config{}, errors.New("version 8 settings invalid")
	}
	cfg := config{
		Version:          configVersion,
		ConnectionTests:  old.ConnectionTests,
		NamedSecrets:     old.NamedSecrets,
		GitHubTargets:    old.GitHubTargets,
		JenkinsTargets:   old.JenkinsTargets,
		HarborTargets:    old.HarborTargets,
		DashboardTargets: old.DashboardTargets,
		ServiceBundles:   old.ServiceBundles,
		PythonTasks:      old.PythonTasks,
	}
	if validateConfig(cfg) != nil || validateNamedSecretReferenceCoverage(cfg) != nil {
		return config{}, errors.New("version 8 settings invalid")
	}
	return cfg, nil
}

// configV7 preserves the exact strict input shape written before named secret
// metadata became part of the canonical settings schema.
type configV7 struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
	PythonTasks      []pythonTask              `json:"python_tasks,omitempty"`
}

func migrateConfigV7(data []byte) (config, error) {
	var old configV7
	if decodeConfig(data, &old) != nil || old.Version != 7 {
		return config{}, errors.New("version 7 settings invalid")
	}
	cfg := config{
		Version:          configVersion,
		ConnectionTests:  old.ConnectionTests,
		GitHubTargets:    old.GitHubTargets,
		JenkinsTargets:   old.JenkinsTargets,
		HarborTargets:    old.HarborTargets,
		DashboardTargets: old.DashboardTargets,
		ServiceBundles:   old.ServiceBundles,
		PythonTasks:      old.PythonTasks,
	}
	if validateConfig(cfg) != nil {
		return config{}, errors.New("version 7 settings invalid")
	}
	return cfg, nil
}
