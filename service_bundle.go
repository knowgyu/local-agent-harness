package main

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type serviceBundle struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	GitHubTargetIDs []string             `json:"github_target_ids"`
	Environments    []serviceEnvironment `json:"environments,omitempty"`
}

type serviceEnvironment struct {
	Name                string `json:"name"`
	JenkinsTargetID     string `json:"jenkins_target_id,omitempty"`
	HarborTargetID      string `json:"harbor_target_id,omitempty"`
	DashboardTargetID   string `json:"dashboard_target_id,omitempty"`
	DashboardNamespace  string `json:"dashboard_namespace,omitempty"`
	DashboardDeployment string `json:"dashboard_deployment,omitempty"`
}

// configV2 keeps the old wire schema strict while migrating it to v3.
type configV2 struct {
	Version          int               `json:"version"`
	GitHubTargets    []target          `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget   `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget    `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget `json:"dashboard_targets,omitempty"`
}

func validateServiceBundles(cfg config) error {
	githubIDs := make(map[string]bool, len(cfg.GitHubTargets))
	for _, target := range cfg.GitHubTargets {
		githubIDs[target.ID] = true
	}
	jenkinsIDs := make(map[string]bool, len(cfg.JenkinsTargets))
	for _, target := range cfg.JenkinsTargets {
		jenkinsIDs[target.ID] = true
	}
	harborIDs := make(map[string]bool, len(cfg.HarborTargets))
	for _, target := range cfg.HarborTargets {
		harborIDs[target.ID] = true
	}
	dashboardIDs := make(map[string]bool, len(cfg.DashboardTargets))
	for _, target := range cfg.DashboardTargets {
		dashboardIDs[target.ID] = true
	}

	bundleIDs, bundleNames := map[string]bool{}, map[string]bool{}
	for _, bundle := range cfg.ServiceBundles {
		if !serviceBundleIDPattern.MatchString(bundle.ID) || !validBundleName(bundle.Name) || bundleIDs[bundle.ID] || bundleNames[bundle.Name] || len(bundle.GitHubTargetIDs) == 0 {
			return errors.New("invalid or duplicate service bundle")
		}
		bundleIDs[bundle.ID], bundleNames[bundle.Name] = true, true

		seenGitHub := map[string]bool{}
		for _, id := range bundle.GitHubTargetIDs {
			if !githubIDPattern.MatchString(id) || !githubIDs[id] || seenGitHub[id] {
				return errors.New("invalid service bundle GitHub reference")
			}
			seenGitHub[id] = true
		}

		environmentNames := map[string]bool{}
		for _, environment := range bundle.Environments {
			if !validBundleName(environment.Name) || environmentNames[environment.Name] {
				return errors.New("invalid or duplicate service environment")
			}
			environmentNames[environment.Name] = true
			if environment.JenkinsTargetID != "" && (!jenkinsIDPattern.MatchString(environment.JenkinsTargetID) || !jenkinsIDs[environment.JenkinsTargetID]) {
				return errors.New("invalid service bundle Jenkins reference")
			}
			if environment.HarborTargetID != "" && (!harborIDPattern.MatchString(environment.HarborTargetID) || !harborIDs[environment.HarborTargetID]) {
				return errors.New("invalid service bundle Harbor reference")
			}
			if environment.DashboardTargetID != "" && (!dashboardIDPattern.MatchString(environment.DashboardTargetID) || !dashboardIDs[environment.DashboardTargetID]) {
				return errors.New("invalid service bundle Dashboard reference")
			}
			hasNamespace := environment.DashboardNamespace != ""
			hasDeployment := environment.DashboardDeployment != ""
			if hasNamespace != hasDeployment || (hasNamespace && environment.DashboardTargetID == "") || (hasNamespace && (!validServiceDashboardNamespace(environment.DashboardNamespace) || !validServiceDashboardDeployment(environment.DashboardDeployment))) {
				return errors.New("invalid service bundle Dashboard resource")
			}
		}
	}
	return nil
}

func validBundleName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && len(name) <= 80 && !strings.ContainsAny(name, "\r\n\x00")
}

func validServiceDashboardNamespace(name string) bool {
	return validKubernetesDNSName(name, 63, false)
}

func validServiceDashboardDeployment(name string) bool {
	return validKubernetesDNSName(name, 253, true)
}

func validKubernetesDNSName(name string, maxLength int, allowDots bool) bool {
	if name == "" || len(name) > maxLength {
		return false
	}
	labels := []string{name}
	if allowDots {
		labels = strings.Split(name, ".")
	} else if strings.Contains(name, ".") {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || !kubernetesDNSAlphaNumeric(label[0]) || !kubernetesDNSAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !kubernetesDNSAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func kubernetesDNSAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

func (a *app) saveServiceBundle(id, name string, githubIDs []string, environments []serviceEnvironment) (string, error) {
	savedID := id
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Could not read current settings.")
		}
		index := -1
		if id != "" && id != "new" {
			for i := range cfg.ServiceBundles {
				if cfg.ServiceBundles[i].ID == id {
					index = i
					break
				}
			}
			if index < 0 {
				return errors.New("The selected service bundle is not registered.")
			}
		} else {
			savedID, err = newTargetID("service")
			if err != nil {
				return errors.New("Could not create service bundle ID.")
			}
		}
		bundle := serviceBundle{ID: savedID, Name: name, GitHubTargetIDs: append([]string(nil), githubIDs...), Environments: append([]serviceEnvironment(nil), environments...)}
		if index < 0 {
			cfg.ServiceBundles = append(cfg.ServiceBundles, bundle)
		} else {
			cfg.ServiceBundles[index] = bundle
		}
		cfg.Version = configVersion
		if err := validateConfig(cfg); err != nil {
			return errors.New("The service bundle is invalid or references unavailable targets. Settings were not changed.")
		}
		if err := writeConfig(a.configPath, cfg); err != nil {
			return errors.New("Could not save service bundle. Settings remain unchanged.")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return savedID, nil
}

func (a *app) deleteServiceBundle(id string) error {
	return withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Could not read current settings.")
		}
		for i := range cfg.ServiceBundles {
			if cfg.ServiceBundles[i].ID == id {
				cfg.ServiceBundles = append(cfg.ServiceBundles[:i], cfg.ServiceBundles[i+1:]...)
				cfg.Version = configVersion
				if err := writeConfig(a.configPath, cfg); err != nil {
					return errors.New("Could not delete service bundle. Settings remain unchanged.")
				}
				return nil
			}
		}
		return errors.New("The selected service bundle is not registered.")
	})
}

func (a *app) handleServiceBundleSave(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	form := r.Form
	names := form["environment_name"]
	jenkinsIDs := form["environment_jenkins_id"]
	harborIDs := form["environment_harbor_id"]
	dashboardIDs := form["environment_dashboard_id"]
	namespaces := form["environment_dashboard_namespace"]
	deployments := form["environment_dashboard_deployment"]
	count := len(names)
	for _, values := range [][]string{jenkinsIDs, harborIDs, dashboardIDs, namespaces, deployments} {
		if len(values) > count {
			count = len(values)
		}
	}
	environments := make([]serviceEnvironment, 0, count)
	for i := 0; i < count; i++ {
		valueAt := func(values []string) string {
			if i >= len(values) {
				return ""
			}
			return values[i]
		}
		environment := serviceEnvironment{
			Name:                valueAt(names),
			JenkinsTargetID:     valueAt(jenkinsIDs),
			HarborTargetID:      valueAt(harborIDs),
			DashboardTargetID:   valueAt(dashboardIDs),
			DashboardNamespace:  valueAt(namespaces),
			DashboardDeployment: valueAt(deployments),
		}
		if environment.Name == "" && environment.JenkinsTargetID == "" && environment.HarborTargetID == "" && environment.DashboardTargetID == "" && environment.DashboardNamespace == "" && environment.DashboardDeployment == "" {
			continue
		}
		environments = append(environments, environment)
	}
	id, err := a.saveServiceBundle(r.FormValue("bundle_id"), r.FormValue("bundle_name"), form["github_target_ids"], environments)
	if err != nil {
		a.setStatus(err.Error(), true)
		redirectID := r.FormValue("bundle_id")
		if redirectID == "" {
			redirectID = "new"
		}
		http.Redirect(w, r, "/?bundle_id="+url.QueryEscape(redirectID), http.StatusSeeOther)
		return
	}
	a.setStatus("Service bundle saved.", false)
	http.Redirect(w, r, "/?bundle_id="+url.QueryEscape(id), http.StatusSeeOther)
}

func (a *app) handleServiceBundleDelete(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	id := r.FormValue("bundle_id")
	if r.FormValue("confirm_delete") != "yes" {
		a.setStatus("Confirm service bundle deletion before removing it.", true)
		http.Redirect(w, r, "/?bundle_id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	if err := a.deleteServiceBundle(id); err != nil {
		a.setStatus(err.Error(), true)
		http.Redirect(w, r, "/?bundle_id="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	a.setStatus("Service bundle deleted.", false)
	http.Redirect(w, r, "/?bundle_id=new", http.StatusSeeOther)
}

func (a *app) populateServiceBundlePage(data *pageData, cfg config, selectedID string) {
	data.ServiceBundleID = "new"
	for _, bundle := range cfg.ServiceBundles {
		data.ServiceBundleChoices = append(data.ServiceBundleChoices, serviceBundleChoice{ID: bundle.ID, Name: bundle.Name})
		for _, environment := range bundle.Environments {
			if !validServiceDashboardDeploymentMapping(environment) {
				continue
			}
			targetIndex := findDashboardTargetIndex(cfg.DashboardTargets, environment.DashboardTargetID)
			if targetIndex < 0 {
				continue
			}
			target := cfg.DashboardTargets[targetIndex]
			if target.Disabled {
				data.DisabledDashboardMappings = true
				continue
			}
			data.DashboardDiagnosisChoices = append(data.DashboardDiagnosisChoices, dashboardDiagnosisPageChoice{
				ServiceBundle: bundle.Name, Environment: environment.Name, Target: target.Name,
				Namespace: environment.DashboardNamespace, Deployment: environment.DashboardDeployment,
			})
		}
		if bundle.ID != selectedID {
			continue
		}
		data.ServiceBundleID, data.ServiceBundleName = bundle.ID, bundle.Name
		data.ServiceBundleGitHubChoices = serviceBundleTargetChoices(cfg.GitHubTargets, bundle.GitHubTargetIDs)
		for _, environment := range bundle.Environments {
			data.ServiceBundleEnvironments = append(data.ServiceBundleEnvironments, serviceEnvironmentPage{
				Name: environment.Name, JenkinsID: environment.JenkinsTargetID, HarborID: environment.HarborTargetID,
				DashboardID: environment.DashboardTargetID, DashboardNamespace: environment.DashboardNamespace,
				DashboardDeployment: environment.DashboardDeployment,
				JenkinsChoices:      serviceEnvironmentChoicesJenkins(cfg.JenkinsTargets, environment.JenkinsTargetID),
				HarborChoices:       serviceEnvironmentChoicesHarbor(cfg.HarborTargets, environment.HarborTargetID),
				DashboardChoices:    serviceEnvironmentChoicesDashboard(cfg.DashboardTargets, environment.DashboardTargetID),
			})
		}
	}
	if data.ServiceBundleGitHubChoices == nil {
		data.ServiceBundleGitHubChoices = serviceBundleTargetChoices(cfg.GitHubTargets, nil)
	}
}

func serviceBundleReferencesTarget(cfg config, kind, id string) bool {
	for _, bundle := range cfg.ServiceBundles {
		if kind == "github" {
			for _, ref := range bundle.GitHubTargetIDs {
				if ref == id {
					return true
				}
			}
		}
		for _, environment := range bundle.Environments {
			if (kind == "jenkins" && environment.JenkinsTargetID == id) || (kind == "harbor" && environment.HarborTargetID == id) || (kind == "dashboard" && environment.DashboardTargetID == id) {
				return true
			}
		}
	}
	return false
}

func (a *app) registeredServiceBundles() (registeredServiceBundlesResult, error) {
	result := registeredServiceBundlesResult{Bundles: []registeredServiceBundle{}}
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local target settings are invalid.")
		}
		for _, bundle := range cfg.ServiceBundles {
			entry := registeredServiceBundle{Name: bundle.Name, Repositories: []registeredServiceTarget{}, Environments: []registeredServiceEnvironment{}}
			for _, id := range bundle.GitHubTargetIDs {
				target, ok := serviceCatalogTarget(cfg, "github", id)
				if !ok {
					return errors.New("Local target settings are invalid.")
				}
				entry.Repositories = append(entry.Repositories, target)
			}
			for _, environment := range bundle.Environments {
				view := registeredServiceEnvironment{Name: environment.Name}
				if environment.JenkinsTargetID != "" {
					target, ok := serviceCatalogTarget(cfg, "jenkins", environment.JenkinsTargetID)
					if !ok {
						return errors.New("Local target settings are invalid.")
					}
					view.Jenkins = &target
				}
				if environment.HarborTargetID != "" {
					target, ok := serviceCatalogTarget(cfg, "harbor", environment.HarborTargetID)
					if !ok {
						return errors.New("Local target settings are invalid.")
					}
					view.Harbor = &target
				}
				if environment.DashboardTargetID != "" {
					target, ok := serviceCatalogTarget(cfg, "dashboard", environment.DashboardTargetID)
					if !ok {
						return errors.New("Local target settings are invalid.")
					}
					if len(target.Actions) > 0 && validServiceDashboardDeploymentMapping(environment) {
						target.Actions = append(target.Actions, "dashboard_deployment_status")
						target.Actions = append(target.Actions, "dashboard_deployment_diagnosis")
						target.Actions = append(target.Actions, "dashboard_deployment_events")
						target.Actions = append(target.Actions, "dashboard_deployment_pods")
						target.Actions = append(target.Actions, "dashboard_deployment_pod_logs")
					}
					view.Dashboard = &target
				}
				entry.Environments = append(entry.Environments, view)
			}
			result.Bundles = append(result.Bundles, entry)
		}
		sort.Slice(result.Bundles, func(i, j int) bool { return result.Bundles[i].Name < result.Bundles[j].Name })
		return nil
	})
	return result, err
}

func serviceCatalogTarget(cfg config, kind, id string) (registeredServiceTarget, bool) {
	target := registeredServiceTarget{Actions: []string{}}
	switch kind {
	case "github":
		for _, item := range cfg.GitHubTargets {
			if item.ID == id {
				target.Name = item.Name
				target.ConnectionTest = catalogConnectionTestFor(cfg, item.ID)
				if !item.Disabled && validateTarget(item) == nil {
					target.Actions = []string{"github_repository", "github_pull_request", "registered_target_connection_test"}
				}
				return target, true
			}
		}
	case "jenkins":
		for _, item := range cfg.JenkinsTargets {
			if item.ID == id {
				target.Name = item.Name
				target.ConnectionTest = catalogConnectionTestFor(cfg, item.ID)
				if !item.Disabled && validateJenkinsTarget(item) == nil {
					target.Actions = []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log", "jenkins_run_registered_job", "registered_target_connection_test"}
				}
				return target, true
			}
		}
	case "harbor":
		for _, item := range cfg.HarborTargets {
			if item.ID == id {
				target.Name = item.Name
				target.ConnectionTest = catalogConnectionTestFor(cfg, item.ID)
				if !item.Disabled && validateHarborTarget(item) == nil {
					target.Actions = []string{"harbor_repository_artifacts", "harbor_project_quota", "registered_target_connection_test"}
				}
				return target, true
			}
		}
	case "dashboard":
		for _, item := range cfg.DashboardTargets {
			if item.ID == id {
				target.Name = item.Name
				target.ConnectionTest = catalogConnectionTestFor(cfg, item.ID)
				if !item.Disabled && validateDashboardTarget(item) == nil {
					target.Actions = []string{"dashboard_namespaces", "registered_target_connection_test"}
				}
				return target, true
			}
		}
	}
	return registeredServiceTarget{}, false
}

func validServiceDashboardDeploymentMapping(environment serviceEnvironment) bool {
	return environment.DashboardTargetID != "" &&
		environment.DashboardNamespace != "" &&
		environment.DashboardDeployment != "" &&
		validServiceDashboardNamespace(environment.DashboardNamespace) &&
		validServiceDashboardDeployment(environment.DashboardDeployment)
}

func serviceBundleTargetChoices(targets []target, selected []string) []targetChoice {
	selectedIDs := make(map[string]bool, len(selected))
	for _, id := range selected {
		selectedIDs[id] = true
	}
	choices := make([]targetChoice, 0, len(targets))
	for _, item := range targets {
		choices = append(choices, targetChoice{ID: item.ID, Name: item.Name, Disabled: item.Disabled, Selected: selectedIDs[item.ID]})
	}
	return choices
}

func serviceEnvironmentChoicesHarbor(targets []harborTarget, selectedID string) []targetChoice {
	choices := make([]targetChoice, 0, len(targets))
	for _, item := range targets {
		choices = append(choices, targetChoice{ID: item.ID, Name: item.Name, Disabled: item.Disabled, Selected: item.ID == selectedID})
	}
	return choices
}

func serviceEnvironmentChoicesDashboard(targets []dashboardTarget, selectedID string) []targetChoice {
	choices := make([]targetChoice, 0, len(targets))
	for _, item := range targets {
		choices = append(choices, targetChoice{ID: item.ID, Name: item.Name, Disabled: item.Disabled, Selected: item.ID == selectedID})
	}
	return choices
}

func serviceEnvironmentChoicesJenkins(targets []jenkinsTarget, selectedID string) []targetChoice {
	choices := make([]targetChoice, 0, len(targets))
	for _, item := range targets {
		choices = append(choices, targetChoice{ID: item.ID, Name: item.Name, Disabled: item.Disabled, Selected: item.ID == selectedID})
	}
	return choices
}
