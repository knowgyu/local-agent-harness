package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type v1GitHubTarget struct {
	Name       string `json:"name"`
	Origin     string `json:"origin"`
	Repository string `json:"repository"`
	SecretRef  string `json:"secret_ref"`
}

type v1JenkinsTarget struct {
	Name                       string `json:"name"`
	BaseURL                    string `json:"base_url"`
	Username                   string `json:"username"`
	JobPath                    string `json:"job_path"`
	Environment                string `json:"environment"`
	SecretRef                  string `json:"secret_ref"`
	NonProductionPreapproved   bool   `json:"nonproduction_preapproved"`
	NonProductionApprovalScope string `json:"nonproduction_approval_scope,omitempty"`
}

type v1ConfigFile struct {
	Version int              `json:"version"`
	Target  *v1GitHubTarget  `json:"target,omitempty"`
	Jenkins *v1JenkinsTarget `json:"jenkins,omitempty"`
}

func TestMigrateV1ConfigKeepsTargetsCredentialsAndJenkinsApproval(t *testing.T) {
	const githubRef = "cred:0123456789abcdef0123456789abcdef"
	const jenkinsRef = "cred:fedcba9876543210fedcba9876543210"
	jenkins := jenkinsTarget{
		Name: "QA pipeline", BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user",
		JobPath: "folder/smoke", Environment: "qa", SecretRef: jenkinsRef,
	}
	validScope := jenkinsTriggerApprovalScope(jenkins)

	tests := []struct {
		name         string
		legacy       v1ConfigFile
		wantGitHub   int
		wantJenkins  int
		wantFlag     bool
		wantApproved bool
		wantScope    string
	}{
		{
			name: "GitHub only",
			legacy: v1ConfigFile{Version: legacyConfigVersion, Target: &v1GitHubTarget{
				Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: githubRef,
			}},
			wantGitHub: 1,
		},
		{
			name: "Jenkins only with approval",
			legacy: v1ConfigFile{Version: legacyConfigVersion, Jenkins: &v1JenkinsTarget{
				Name: jenkins.Name, BaseURL: jenkins.BaseURL, Username: jenkins.Username, JobPath: jenkins.JobPath,
				Environment: jenkins.Environment, SecretRef: jenkinsRef,
				NonProductionPreapproved: true, NonProductionApprovalScope: validScope,
			}},
			wantJenkins: 1, wantFlag: true, wantApproved: true, wantScope: validScope,
		},
		{
			name: "both targets",
			legacy: v1ConfigFile{Version: legacyConfigVersion,
				Target: &v1GitHubTarget{Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: githubRef},
				Jenkins: &v1JenkinsTarget{Name: jenkins.Name, BaseURL: jenkins.BaseURL, Username: jenkins.Username,
					JobPath: jenkins.JobPath, Environment: jenkins.Environment, SecretRef: jenkinsRef,
					NonProductionPreapproved: true, NonProductionApprovalScope: validScope},
			},
			wantGitHub: 1, wantJenkins: 1, wantFlag: true, wantApproved: true, wantScope: validScope,
		},
		{
			name: "mismatched approval remains unusable",
			legacy: v1ConfigFile{Version: legacyConfigVersion, Jenkins: &v1JenkinsTarget{
				Name: jenkins.Name, BaseURL: jenkins.BaseURL, Username: jenkins.Username, JobPath: jenkins.JobPath,
				Environment: jenkins.Environment, SecretRef: jenkinsRef,
				NonProductionPreapproved: true, NonProductionApprovalScope: "old-or-mismatched-scope",
			}},
			wantJenkins: 1, wantFlag: true, wantScope: "old-or-mismatched-scope",
		},
		{
			name: "approval without a scope fails closed",
			legacy: v1ConfigFile{Version: legacyConfigVersion, Jenkins: &v1JenkinsTarget{
				Name: jenkins.Name, BaseURL: jenkins.BaseURL, Username: jenkins.Username, JobPath: jenkins.JobPath,
				Environment: jenkins.Environment, SecretRef: jenkinsRef, NonProductionPreapproved: true,
			}},
			wantJenkins: 1, wantFlag: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(test.legacy)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := migrateConfig(path); err != nil {
				t.Fatalf("migrate v1 config: %v", err)
			}
			got, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != configVersion || len(got.GitHubTargets) != test.wantGitHub || len(got.JenkinsTargets) != test.wantJenkins {
				t.Fatalf("migrated config = version %d, %d GitHub, %d Jenkins", got.Version, len(got.GitHubTargets), len(got.JenkinsTargets))
			}
			if test.wantGitHub == 1 {
				if !githubIDPattern.MatchString(got.GitHubTargets[0].ID) || got.GitHubTargets[0].SecretRef != githubRef {
					t.Fatalf("migrated GitHub target = %+v", got.GitHubTargets[0])
				}
			}
			if test.wantJenkins == 1 {
				migrated := got.JenkinsTargets[0]
				if !jenkinsIDPattern.MatchString(migrated.ID) || migrated.SecretRef != jenkinsRef {
					t.Fatalf("migrated Jenkins target = %+v", migrated)
				}
				if migrated.NonProductionApprovalScope != test.wantScope {
					t.Fatalf("approval scope = %q, want %q", migrated.NonProductionApprovalScope, test.wantScope)
				}
				if migrated.NonProductionPreapproved != test.wantFlag {
					t.Fatalf("approval flag was not preserved: %+v", migrated)
				}
				if jenkinsTriggerApproved(migrated) != test.wantApproved {
					t.Fatalf("approval usable = %v, want %v", jenkinsTriggerApproved(migrated), test.wantApproved)
				}
			}

			firstIDs := make([]string, 0, 2)
			if test.wantGitHub == 1 {
				firstIDs = append(firstIDs, got.GitHubTargets[0].ID)
			}
			if test.wantJenkins == 1 {
				firstIDs = append(firstIDs, got.JenkinsTargets[0].ID)
			}
			if err := migrateConfig(path); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
			again, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			var secondIDs []string
			if test.wantGitHub == 1 {
				secondIDs = append(secondIDs, again.GitHubTargets[0].ID)
			}
			if test.wantJenkins == 1 {
				secondIDs = append(secondIDs, again.JenkinsTargets[0].ID)
			}
			if strings.Join(firstIDs, ",") != strings.Join(secondIDs, ",") {
				t.Fatalf("IDs changed after restart: %v -> %v", firstIDs, secondIDs)
			}
		})
	}
}

func TestEmptyConfigAndRegisteredTargetsCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatalf("write empty config: %v", err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("restart empty config: %v", err)
	}
	a := &app{configPath: path}
	result, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || string(encoded) != `{"targets":[]}` {
		t.Fatalf("empty catalog = %s, %v", encoded, err)
	}
	services, err := a.registeredServiceBundles()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(services)
	if err != nil || string(encoded) != `{"bundles":[]}` {
		t.Fatalf("empty service bundle catalog = %s, %v", encoded, err)
	}
}

func TestRootPageRendersTargetSelectorsAndAccessibleManagementForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	gh := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	jenkins := jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA", BaseURL: "https://jenkins.example.invalid", Username: "build-user", JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:1123456789abcdef0123456789abcdef"}
	if err := writeConfig(path, config{Version: configVersion, GitHubTargets: []target{gh}, JenkinsTargets: []jenkinsTarget{jenkins}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, csrf: "csrf-token"}
	request := httptest.NewRequest(http.MethodGet, "/?github_id="+url.QueryEscape(gh.ID)+"&jenkins_id="+url.QueryEscape(jenkins.ID), nil)
	response := httptest.NewRecorder()
	a.handleRoot(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("root page status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`id="github-selection"`, `name="github_id"`, `value="new"`,
		`id="jenkins-selection"`, `name="jenkins_id"`,
		`name="target_id" value="github:0123456789abcdef0123456789abcdef"`,
		`name="target_id" value="jenkins:0123456789abcdef0123456789abcdef"`,
		`name="confirm_delete" value="yes" required`,
		`name="disabled" value="yes"`,
		`for="github-name"`, `id="github-name"`,
		`for="jenkins-name"`, `id="jenkins-name"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("rendered page missing %q", expected)
		}
	}

	request = httptest.NewRequest(http.MethodGet, "/?github_id=new&jenkins_id=new", nil)
	response = httptest.NewRecorder()
	a.handleRoot(response, request)
	body = response.Body.String()
	if !strings.Contains(body, `<option value="new" selected>Add new target</option>`) || strings.Contains(body, `name="target_id" value="github:0123456789abcdef0123456789abcdef"`) {
		t.Fatal("new target selection did not render the add form")
	}
}

func TestSaveMultipleTargetsPreservesOtherIDsCredentialsAndApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	secrets := memorySecrets{}
	a := &app{configPath: path, secrets: secrets}

	githubID1, err := a.saveTarget("", "Engineering", "https://github.example.invalid", "ops/agent", "github-token-one")
	if err != nil {
		t.Fatal(err)
	}
	githubID2, err := a.saveTarget("", "Platform", "https://github.example.invalid", "platform/agent", "github-token-two")
	if err != nil {
		t.Fatal(err)
	}
	jenkinsID1, err := a.saveJenkinsTarget("", "QA", "https://jenkins.example.invalid/proxy", "build-user", "folder/qa", "qa", "jenkins-token-one", true)
	if err != nil {
		t.Fatal(err)
	}
	jenkinsID2, err := a.saveJenkinsTarget("", "Stage", "https://jenkins.example.invalid/proxy", "stage-user", "folder/stage", "stage", "jenkins-token-two", true)
	if err != nil {
		t.Fatal(err)
	}
	if githubID1 == githubID2 || jenkinsID1 == jenkinsID2 {
		t.Fatal("new targets did not receive distinct IDs")
	}

	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.JenkinsTargets[0].ID != jenkinsID1 || !jenkinsTriggerApproved(before.JenkinsTargets[0]) || !jenkinsTriggerApproved(before.JenkinsTargets[1]) {
		t.Fatalf("expected initial approvals: %+v", before.JenkinsTargets)
	}
	githubOther, jenkinsOther := before.GitHubTargets[1], before.JenkinsTargets[1]
	githubRef, jenkinsRef := before.GitHubTargets[0].SecretRef, before.JenkinsTargets[0].SecretRef

	updatedID, err := a.saveTarget(githubID1, "Engineering", "https://github.example.invalid", "ops/agent-renamed", "")
	if err != nil || updatedID != githubID1 {
		t.Fatalf("edit GitHub target ID=%q err=%v", updatedID, err)
	}
	updatedID, err = a.saveJenkinsTarget(jenkinsID1, "QA", "https://jenkins.example.invalid/proxy", "build-user", "folder/qa-renamed", "qa", "", false)
	if err != nil || updatedID != jenkinsID1 {
		t.Fatalf("edit Jenkins target ID=%q err=%v", updatedID, err)
	}
	if _, err := a.saveTarget("", "Engineering", "https://github.example.invalid", "ops/other", "duplicate-token"); err == nil {
		t.Fatal("duplicate GitHub display name was accepted")
	}
	if _, err := a.saveJenkinsTarget("", "QA", "https://jenkins.example.invalid/proxy", "build-user", "folder/other", "qa", "duplicate-token", true); err == nil {
		t.Fatal("duplicate Jenkins display name was accepted")
	}

	after, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.GitHubTargets[0].ID != githubID1 || after.GitHubTargets[0].SecretRef != githubRef || after.GitHubTargets[0].Repository != "ops/agent-renamed" {
		t.Fatalf("edited GitHub target = %+v", after.GitHubTargets[0])
	}
	if after.GitHubTargets[1] != githubOther {
		t.Fatalf("editing GitHub target changed its neighbor: before=%+v after=%+v", githubOther, after.GitHubTargets[1])
	}
	if after.JenkinsTargets[0].ID != jenkinsID1 || after.JenkinsTargets[0].SecretRef != jenkinsRef || after.JenkinsTargets[0].JobPath != "folder/qa-renamed" || jenkinsTriggerApproved(after.JenkinsTargets[0]) {
		t.Fatalf("edited Jenkins approval/identity = %+v", after.JenkinsTargets[0])
	}
	if after.JenkinsTargets[1] != jenkinsOther || !jenkinsTriggerApproved(after.JenkinsTargets[1]) {
		t.Fatalf("editing Jenkins target changed neighbor or approval: before=%+v after=%+v", jenkinsOther, after.JenkinsTargets[1])
	}
}

func TestDisabledAndUnregisteredTargetsDoNotLoadSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	ghEnabled := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}
	ghDisabled := target{ID: "github:1123456789abcdef0123456789abcdef", Name: "Disabled GitHub", Origin: "https://github.example.invalid", Repository: "ops/disabled", SecretRef: "cred:1123456789abcdef0123456789abcdef", Disabled: true}
	jenkinsEnabled := approvedJenkinsTarget(jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA", BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user", JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:2123456789abcdef0123456789abcdef"})
	jenkinsDisabled := approvedJenkinsTarget(jenkinsTarget{ID: "jenkins:1123456789abcdef0123456789abcdef", Name: "Disabled Jenkins", BaseURL: "https://jenkins.example.invalid/proxy", Username: "build-user", JobPath: "folder/disabled", Environment: "qa", SecretRef: "cred:3123456789abcdef0123456789abcdef", Disabled: true})
	if err := writeConfig(path, config{Version: configVersion, GitHubTargets: []target{ghEnabled, ghDisabled}, JenkinsTargets: []jenkinsTarget{jenkinsEnabled, jenkinsDisabled}}); err != nil {
		t.Fatal(err)
	}
	secrets := &recordingSecrets{}
	a := &app{configPath: path, secrets: secrets}
	if _, _, err := a.loadGitHubTarget("Disabled GitHub"); err == nil {
		t.Fatal("disabled GitHub target loaded")
	}
	if _, _, err := a.loadJenkinsTarget("Disabled Jenkins"); err == nil {
		t.Fatal("disabled Jenkins target loaded")
	}
	if _, _, err := a.loadGitHubTarget("Engineering "); err == nil {
		t.Fatal("GitHub lookup trimmed or relaxed target name")
	}
	if _, _, err := a.loadJenkinsTarget("QA "); err == nil {
		t.Fatal("Jenkins lookup trimmed or relaxed target name")
	}
	if secrets.loads != 0 {
		t.Fatalf("rejected target lookup loaded %d credentials", secrets.loads)
	}

	catalog, err := a.registeredTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Targets) != 2 || catalog.Targets[0].Name != "Engineering" || catalog.Targets[1].Name != "QA" {
		t.Fatalf("catalog = %+v", catalog)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{ghEnabled.ID, ghEnabled.Origin, ghEnabled.Repository, ghEnabled.SecretRef, jenkinsEnabled.ID, jenkinsEnabled.BaseURL, jenkinsEnabled.JobPath, jenkinsEnabled.SecretRef, "Disabled GitHub", "Disabled Jenkins"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("catalog leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestRegisteredTargetsAdvertisesTriggerForEnabledValidJenkinsTargets(t *testing.T) {
	tests := []struct {
		name        string
		mismatched  bool
		wantTrigger bool
	}{
		{name: "unapproved", wantTrigger: true},
		{name: "scope mismatch", mismatched: true, wantTrigger: true},
		{name: "approved", wantTrigger: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := jenkinsTarget{
				ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA",
				BaseURL: "https://jenkins.example.invalid", Username: "build-user",
				JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:0123456789abcdef0123456789abcdef",
			}
			if test.wantTrigger {
				target = approvedJenkinsTarget(target)
			} else if test.mismatched {
				target.NonProductionPreapproved = true
				target.NonProductionApprovalScope = "old-or-mismatched-scope"
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := writeConfig(path, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{target}}); err != nil {
				t.Fatal(err)
			}
			catalog, err := (&app{configPath: path}).registeredTargets()
			if err != nil || len(catalog.Targets) != 1 {
				t.Fatalf("catalog = %+v, err=%v", catalog, err)
			}
			hasTrigger := false
			for _, action := range catalog.Targets[0].Actions {
				if action == "jenkins_run_registered_job" {
					hasTrigger = true
				}
			}
			if hasTrigger != test.wantTrigger {
				t.Fatalf("trigger advertised = %v, want %v for %s", hasTrigger, test.wantTrigger, test.name)
			}
		})
	}
}

func TestInvalidTargetIDsFailBeforeCredentialLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	invalid := config{
		Version:        configVersion,
		GitHubTargets:  []target{{ID: "invalid", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: "cred:0123456789abcdef0123456789abcdef"}},
		JenkinsTargets: []jenkinsTarget{{ID: "invalid", Name: "QA", BaseURL: "https://jenkins.example.invalid", Username: "build-user", JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:1123456789abcdef0123456789abcdef"}},
	}
	data, err := json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := &recordingSecrets{}
	a := &app{configPath: path, secrets: secrets}
	if _, err := a.registeredRepository(context.Background(), "Engineering"); err == nil {
		t.Fatal("invalid GitHub target ID was accepted")
	}
	if _, err := a.registeredJenkinsJob(context.Background(), "QA"); err == nil {
		t.Fatal("invalid Jenkins target ID was accepted")
	}
	if secrets.loads != 0 {
		t.Fatalf("invalid IDs caused %d credential loads", secrets.loads)
	}
}

type deletionObserver struct {
	path        string
	deletedID   string
	called      bool
	returnError bool
}

func (*deletionObserver) Save(string, []byte) error   { return nil }
func (*deletionObserver) Load(string) ([]byte, error) { return nil, errors.New("unexpected load") }
func (s *deletionObserver) Delete(string) error {
	s.called = true
	cfg, err := readConfig(s.path)
	if err != nil {
		return err
	}
	if findGitHubTargetIndex(cfg.GitHubTargets, s.deletedID) >= 0 || findJenkinsTargetIndex(cfg.JenkinsTargets, s.deletedID) >= 0 {
		return errors.New("target deletion happened after credential cleanup")
	}
	if s.returnError {
		return errors.New("simulated credential cleanup failure")
	}
	return nil
}

func TestDeletePersistsFirstAndKeepsSharedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const sharedRef = "cred:0123456789abcdef0123456789abcdef"
	github := target{ID: "github:0123456789abcdef0123456789abcdef", Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: sharedRef}
	jenkins := jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA", BaseURL: "https://jenkins.example.invalid", Username: "build-user", JobPath: "folder/qa", Environment: "qa", SecretRef: sharedRef}
	if err := writeConfig(path, config{Version: configVersion, GitHubTargets: []target{github}, JenkinsTargets: []jenkinsTarget{jenkins}}); err != nil {
		t.Fatal(err)
	}
	observer := &deletionObserver{path: path, deletedID: github.ID, returnError: true}
	a := &app{configPath: path, secrets: observer}
	if err := a.deleteRegisteredTarget("github", github.ID); err != nil {
		t.Fatalf("delete target sharing credential: %v", err)
	}
	if observer.called {
		t.Fatal("shared credential was deleted while Jenkins still referenced it")
	}
	observer.deletedID = jenkins.ID
	if err := a.deleteRegisteredTarget("jenkins", jenkins.ID); err != nil {
		t.Fatalf("delete final target while retaining named secret: %v", err)
	}
	if observer.called {
		t.Fatal("target deletion removed a credential retained for named-secret reuse")
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.GitHubTargets)+len(cfg.JenkinsTargets) != 0 || len(cfg.NamedSecrets) != 1 || cfg.NamedSecrets[0].CredentialRef != sharedRef {
		t.Fatalf("deleted config = %+v, %v", cfg, err)
	}
}

func TestDisablingJenkinsClearsApprovalAndReenableDoesNotRestoreIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	jenkins := approvedJenkinsTarget(jenkinsTarget{ID: "jenkins:0123456789abcdef0123456789abcdef", Name: "QA", BaseURL: "https://jenkins.example.invalid", Username: "build-user", JobPath: "folder/qa", Environment: "qa", SecretRef: "cred:0123456789abcdef0123456789abcdef"})
	if err := writeConfig(path, config{Version: configVersion, JenkinsTargets: []jenkinsTarget{jenkins}}); err != nil {
		t.Fatal(err)
	}
	if err := aSetDisabled(path, "jenkins", jenkins.ID, true); err != nil {
		t.Fatal(err)
	}
	disabled, err := readConfig(path)
	if err != nil || disabled.JenkinsTargets[0].NonProductionPreapproved || disabled.JenkinsTargets[0].NonProductionApprovalScope != "" {
		t.Fatalf("disabled approval = %+v, %v", disabled.JenkinsTargets[0], err)
	}
	a := &app{configPath: path, secrets: memorySecrets{jenkins.SecretRef: []byte("token")}}
	if _, err := a.saveJenkinsTarget(jenkins.ID, jenkins.Name, jenkins.BaseURL, jenkins.Username, jenkins.JobPath, jenkins.Environment, "", true); err != nil {
		t.Fatalf("edit disabled target: %v", err)
	}
	disabled, err = readConfig(path)
	if err != nil || !disabled.JenkinsTargets[0].Disabled || jenkinsTriggerApproved(disabled.JenkinsTargets[0]) {
		t.Fatalf("editing disabled target enabled it or restored approval: %+v, %v", disabled.JenkinsTargets[0], err)
	}
	if err := aSetDisabled(path, "jenkins", jenkins.ID, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := readConfig(path)
	if err != nil || enabled.JenkinsTargets[0].Disabled || jenkinsTriggerApproved(enabled.JenkinsTargets[0]) {
		t.Fatalf("re-enabled target revived approval: %+v, %v", enabled.JenkinsTargets[0], err)
	}
}

func TestDeleteHandlerRequiresCheckedConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const id = "github:0123456789abcdef0123456789abcdef"
	const ref = "cred:0123456789abcdef0123456789abcdef"
	githubTarget := target{ID: id, Name: "Engineering", Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: ref}
	if err := writeConfig(path, config{Version: configVersion, GitHubTargets: []target{githubTarget}}); err != nil {
		t.Fatal(err)
	}
	secrets := memorySecrets{ref: []byte("canary")}
	a := &app{configPath: path, secrets: secrets, host: "127.0.0.1:43127", csrf: "valid-csrf"}
	form := url.Values{"csrf": {a.csrf}, "kind": {"github"}, "target_id": {id}}
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/delete-target", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	a.handleDeleteTarget(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("delete response = %d", response.Code)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.GitHubTargets) != 1 {
		t.Fatalf("unchecked delete changed config: %+v, %v", cfg, err)
	}
	if _, ok := secrets[ref]; !ok {
		t.Fatal("unchecked delete removed credential")
	}
}

func aSetDisabled(path, kind, id string, disabled bool) error {
	return withConfigLock(func() error { return (&app{configPath: path}).setTargetDisabled(kind, id, disabled) })
}
