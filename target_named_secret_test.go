package main

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

type targetNamedSecretStore struct {
	saveCalls   int
	loadCalls   int
	deleteCalls int
}

func (s *targetNamedSecretStore) Save(string, []byte) error {
	s.saveCalls++
	return nil
}

func (s *targetNamedSecretStore) Load(string) ([]byte, error) {
	s.loadCalls++
	return nil, nil
}

func (s *targetNamedSecretStore) Delete(string) error {
	s.deleteCalls++
	return nil
}

func TestTargetSaveUsesNamedSecretReferenceForEveryService(t *testing.T) {
	const (
		secretID  = "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		secretRef = "cred:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID: secretID, Name: "Shared synthetic credential", Purpose: "test only", CredentialRef: secretRef,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	store := &targetNamedSecretStore{}
	a := &app{configPath: configPath, secrets: store}

	saves := []struct {
		name string
		run  func() (string, error)
	}{
		{name: "GitHub", run: func() (string, error) {
			return a.saveTargetWithNamedSecret("", "GitHub test", "https://github.example.invalid", "owner/repo", "", secretID)
		}},
		{name: "Jenkins", run: func() (string, error) {
			return a.saveJenkinsTargetWithNamedSecret("", "Jenkins test", "https://jenkins.example.invalid", "build-user", "team/job", "qa", "", false, secretID)
		}},
		{name: "Harbor", run: func() (string, error) {
			return a.saveHarborTargetWithNamedSecret("", "Harbor test", "https://harbor.example.invalid", "robot", "ops", "agent", "", secretID)
		}},
		{name: "Dashboard", run: func() (string, error) {
			return a.saveDashboardTargetWithNamedSecret("", "Dashboard test", "https://dashboard.example.invalid/proxy", "", secretID)
		}},
	}
	for _, save := range saves {
		t.Run(save.name, func(t *testing.T) {
			if _, err := withConfigLockResult(save.run); err != nil {
				t.Fatal(err)
			}
		})
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range cfg.GitHubTargets {
		if target.SecretRef != secretRef {
			t.Errorf("GitHub target stored %q, want selected named reference", target.SecretRef)
		}
	}
	for _, target := range cfg.JenkinsTargets {
		if target.SecretRef != secretRef {
			t.Errorf("Jenkins target stored %q, want selected named reference", target.SecretRef)
		}
	}
	for _, target := range cfg.HarborTargets {
		if target.SecretRef != secretRef {
			t.Errorf("Harbor target stored %q, want selected named reference", target.SecretRef)
		}
	}
	for _, target := range cfg.DashboardTargets {
		if target.SecretRef != secretRef {
			t.Errorf("Dashboard target stored %q, want selected named reference", target.SecretRef)
		}
	}
	if store.saveCalls != 0 || store.loadCalls != 0 || store.deleteCalls != 0 {
		t.Fatalf("selecting a saved name accessed the secret store: saves=%d loads=%d deletes=%d", store.saveCalls, store.loadCalls, store.deleteCalls)
	}
}

func withConfigLockResult(run func() (string, error)) (string, error) {
	var result string
	err := withConfigLock(func() error {
		var err error
		result, err = run()
		return err
	})
	return result, err
}

func TestTargetSaveRejectsNamedSecretAndRawCredentialTogether(t *testing.T) {
	const (
		secretID  = "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		secretRef = "cred:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{
		Version:      configVersion,
		NamedSecrets: []namedSecretMetadata{{ID: secretID, Name: "Named", CredentialRef: secretRef}},
	}); err != nil {
		t.Fatal(err)
	}
	store := &targetNamedSecretStore{}
	a := &app{configPath: configPath, secrets: store}
	attempts := []func() (string, error){
		func() (string, error) {
			return a.saveTargetWithNamedSecret("", "GitHub", "https://github.example.invalid", "owner/repo", "raw-canary", secretID)
		},
		func() (string, error) {
			return a.saveJenkinsTargetWithNamedSecret("", "Jenkins", "https://jenkins.example.invalid", "user", "team/job", "qa", "raw-canary", false, secretID)
		},
		func() (string, error) {
			return a.saveHarborTargetWithNamedSecret("", "Harbor", "https://harbor.example.invalid", "robot", "ops", "agent", "raw-canary", secretID)
		},
		func() (string, error) {
			return a.saveDashboardTargetWithNamedSecret("", "Dashboard", "https://dashboard.example.invalid", "raw-canary", secretID)
		},
	}
	for index, attempt := range attempts {
		if _, err := attempt(); err == nil || !strings.Contains(err.Error(), "saved named secret") {
			t.Errorf("save %d did not reject two credential sources: %v", index, err)
		}
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GitHubTargets)+len(cfg.JenkinsTargets)+len(cfg.HarborTargets)+len(cfg.DashboardTargets) != 0 {
		t.Fatal("a target was saved despite conflicting credential inputs")
	}
	if store.saveCalls != 0 {
		t.Fatalf("conflicting input wrote a raw credential %d times", store.saveCalls)
	}
}

func TestTargetNamedSecretSelectorReadsOnlyUniquePostedValue(t *testing.T) {
	const secretID = "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	request := httptest.NewRequest("POST", "http://127.0.0.1/save?named_secret_id="+url.QueryEscape(secretID), strings.NewReader("csrf=ok"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if got, err := targetNamedSecretIDFromPost(request); err != nil || got != "" {
		t.Fatalf("query selector influenced a target save: got=%q err=%v", got, err)
	}

	request = httptest.NewRequest("POST", "http://127.0.0.1/save", strings.NewReader("named_secret_id="+secretID+"&named_secret_id="+secretID))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	if _, err := targetNamedSecretIDFromPost(request); err == nil {
		t.Fatal("duplicate selector values were accepted")
	}
}

func TestTargetSavePageExposesOnlyNamedSecretMetadata(t *testing.T) {
	const (
		secretID    = "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		secretRef   = "cred:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		secretValue = "credential-page-canary-value"
	)
	namedJSON := `[{"id":"` + secretID + `","name":"Shared test secret","purpose":"test","configured":true,"in_use":true}]`
	data := pageData{
		Language: "en", StorageReady: true, CSRF: "csrf", CSPNonce: "nonce",
		NamedSecretsJSON: namedJSON, UserEnvironmentJSON: "[]", SecretsEnvironmentAvailable: true,
		GitHubSecretID: secretID, JenkinsSecretID: secretID, HarborSecretID: secretID, DashboardSecretID: secretID,
	}
	var output bytes.Buffer
	if err := page.ExecuteTemplate(&output, "ui.html", data); err != nil {
		t.Fatal(err)
	}
	body := output.String()
	if !strings.Contains(body, `data-selected-secret-id="`+secretID+`"`) || !strings.Contains(body, "Shared test secret") {
		t.Fatal("page did not render the selected secret ID and safe display name")
	}
	if strings.Contains(body, secretRef) || strings.Contains(body, secretValue) {
		t.Fatal("page exposed a Credential Manager reference or secret value")
	}
	if strings.Contains(body, ` id="`+secretID+`"`) {
		t.Fatal("page used a saved secret ID as an HTML element ID")
	}
}

func TestLocalUIMuxWiresNamedSecretSaveWithoutReturningReferenceOrValue(t *testing.T) {
	const (
		host  = "127.0.0.1:49321"
		csrf  = "named-secret-mux-csrf"
		value = "named-secret-mux-value-canary"
	)
	configPath := filepath.Join(t.TempDir(), "config.json")
	store := &targetNamedSecretStore{}
	a := &app{
		configPath: configPath, secrets: store, namedSecrets: newNamedSecretController(configPath, store),
		host: host, csrf: csrf,
	}
	values := url.Values{"csrf": {csrf}, "name": {"Mux secret"}, "purpose": {"synthetic test"}, "value": {value}}
	request := httptest.NewRequest("POST", "http://"+host+uiRouteSaveNamedSecret, strings.NewReader(values.Encode()))
	request.Host = host
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	request.Header.Set("Origin", "http://"+host)
	response := httptest.NewRecorder()
	newLocalUIHandler(a).ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"name":"Mux secret"`) || strings.Contains(response.Body.String(), value) {
		t.Fatalf("named secret route did not return safe metadata: status=%d body=%s", response.Code, response.Body.String())
	}
	cfg, err := readConfig(configPath)
	if err != nil || len(cfg.NamedSecrets) != 1 {
		t.Fatalf("named secret metadata was not saved: cfg=%#v err=%v", cfg, err)
	}
	secretRef := cfg.NamedSecrets[0].CredentialRef
	if secretRef == "" || strings.Contains(response.Body.String(), secretRef) || store.saveCalls != 1 || store.loadCalls != 0 {
		t.Fatalf("route leaked or read credential data: ref=%q saves=%d loads=%d body=%s", secretRef, store.saveCalls, store.loadCalls, response.Body.String())
	}

	request = httptest.NewRequest("GET", "http://"+host+"/", nil)
	request.Host = host
	response = httptest.NewRecorder()
	newLocalUIHandler(a).ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Mux secret") || strings.Contains(response.Body.String(), secretRef) || strings.Contains(response.Body.String(), value) {
		t.Fatalf("root page exposed a reference/value or omitted safe name: status=%d", response.Code)
	}
}
