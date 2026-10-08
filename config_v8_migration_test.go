package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMigrateV7BackfillsNamedSecretMetadataAndPreservesLegacyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	githubOne, githubTwo, jenkins, harbor, dashboard := serviceBundleTargets()
	const sharedRef = "cred:" + "0123456789abcdef0123456789abcdef"
	const pythonRef = "cred:" + "fedcba9876543210fedcba9876543210"
	githubOne.SecretRef = sharedRef
	githubTwo.SecretRef = sharedRef
	jenkins.SecretRef = "cred:" + "2123456789abcdef0123456789abcdef"
	harbor.SecretRef = "cred:" + "3123456789abcdef0123456789abcdef"
	dashboard.SecretRef = "cred:" + "4123456789abcdef0123456789abcdef"
	bundle := serviceBundle{
		ID: "service:" + strings.Repeat("a", 32), Name: "Existing bundle",
		GitHubTargetIDs: []string{githubOne.ID, githubTwo.ID},
		Environments: []serviceEnvironment{{
			Name: "qa", JenkinsTargetID: jenkins.ID, HarborTargetID: harbor.ID,
			DashboardTargetID: dashboard.ID, DashboardNamespace: "apps", DashboardDeployment: "api",
		}},
	}
	python := pythonTask{
		ID: "python:" + strings.Repeat("b", 32), Name: "Existing Python mapping",
		InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\tasks\run.py`,
		SecretEnvName: "BUILD_TOKEN", SecretRef: pythonRef,
		InterpreterSHA256: strings.Repeat("c", 64), InterpreterSize: 1024,
		ScriptSHA256: strings.Repeat("d", 64), ScriptSize: 512,
	}
	legacy := configV7{
		Version:          7,
		ConnectionTests:  map[string]connectionTest{githubOne.ID: {Result: "success", CompletedAt: "2026-10-03T10:00:00Z"}},
		GitHubTargets:    []target{githubOne, githubTwo},
		JenkinsTargets:   []jenkinsTarget{jenkins},
		HarborTargets:    []harborTarget{harbor},
		DashboardTargets: []dashboardTarget{dashboard},
		ServiceBundles:   []serviceBundle{bundle},
		PythonTasks:      []pythonTask{python},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v7 settings: %v", err)
	}
	migrated, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != configVersion || len(migrated.NamedSecrets) != 4 {
		t.Fatalf("migration version/secret metadata = %d/%#v", migrated.Version, migrated.NamedSecrets)
	}
	if len(migrated.GitHubTargets) != 2 || migrated.GitHubTargets[0].SecretRef != sharedRef || migrated.GitHubTargets[1].SecretRef != sharedRef ||
		migrated.JenkinsTargets[0].SecretRef != jenkins.SecretRef || migrated.HarborTargets[0].SecretRef != harbor.SecretRef || migrated.DashboardTargets[0].SecretRef != dashboard.SecretRef {
		t.Fatal("legacy service credential references were not preserved")
	}
	if len(migrated.NamedSecrets) == 0 || migrated.NamedSecrets[0].CredentialRef == pythonRef {
		t.Fatal("Python task credential was incorrectly migrated into the service secret catalog")
	}
	if len(migrated.ServiceBundles) != 1 || migrated.ServiceBundles[0].Name != bundle.Name || len(migrated.ConnectionTests) != 1 {
		t.Fatal("bundle mapping or connection history was lost during migration")
	}
	if len(migrated.PythonTasks) != 1 || migrated.PythonTasks[0].SecretRef != pythonRef ||
		migrated.PythonTasks[0].SecretEnvName != python.SecretEnvName || migrated.PythonTasks[0].InterpreterSHA256 != python.InterpreterSHA256 {
		t.Fatal("Python task selection, credential reference, or file fingerprint was lost")
	}
	if err := validateNamedSecretReferenceCoverage(migrated); err != nil {
		t.Fatalf("migrated references do not resolve to metadata: %v", err)
	}
	firstSave, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}
	secondSave, err := os.ReadFile(path)
	if err != nil || string(firstSave) != string(secondSave) {
		t.Fatalf("second migration pass rewrote settings: err=%v", err)
	}
}

func TestMigrateV7RejectsNewFieldsAndPreservesOriginalBytes(t *testing.T) {
	for _, source := range []string{
		`{"version":7,"named_secrets":[{"id":"secret:11111111111111111111111111111111"}]}`,
		`{"version":7,"unknown_new_field":true}`,
		`{"version":7,"github_targets":[{"id":"github:11111111111111111111111111111111","name":"bad","origin":"https://github.example.invalid","repository":"ops/agent","secret_ref":"cred:invalid"}]}`,
	} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			before := []byte(source)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := migrateConfig(path); err == nil {
				t.Fatal("invalid v7 settings migrated successfully")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("failed migration changed original bytes: err=%v", err)
			}
		})
	}
}

func TestV8StrictReadRequiresNamedMetadataForServiceCredentialReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	value := `{"version":8,"github_targets":[{"id":"github:11111111111111111111111111111111","name":"Engineering","origin":"https://github.example.invalid","repository":"ops/agent","secret_ref":"cred:11111111111111111111111111111111"}]}`
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(path); err == nil {
		t.Fatal("v8 config accepted an unindexed service credential reference")
	}
}

func TestMigrateV8PreservesAllFieldsAndInitializesSSHTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := serviceBundleConfig()
	want.Version = 8
	want.PythonTasks = []pythonTask{{
		ID: "python:" + strings.Repeat("a", 32), Name: "Existing task",
		InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\tasks\run.py`,
		SecretEnvName: "BUILD_TOKEN", SecretRef: "cred:5123456789abcdef0123456789abcdef",
		InterpreterSHA256: strings.Repeat("c", 64), InterpreterSize: 1024,
		ScriptSHA256: strings.Repeat("d", 64), ScriptSize: 512,
	}}
	want.ConnectionTests = map[string]connectionTest{
		want.GitHubTargets[0].ID: {Result: "success", CompletedAt: "2026-10-03T10:00:00Z"},
	}
	if err := ensureNamedSecretMetadata(&want); err != nil {
		t.Fatal(err)
	}
	legacy := configV8{
		Version: want.Version, ConnectionTests: want.ConnectionTests, NamedSecrets: want.NamedSecrets,
		GitHubTargets: want.GitHubTargets, JenkinsTargets: want.JenkinsTargets,
		HarborTargets: want.HarborTargets, DashboardTargets: want.DashboardTargets,
		ServiceBundles: want.ServiceBundles, PythonTasks: want.PythonTasks,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v8 settings: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != configVersion || len(got.SSHTargets) != 0 ||
		!reflect.DeepEqual(got.ConnectionTests, want.ConnectionTests) ||
		!reflect.DeepEqual(got.NamedSecrets, want.NamedSecrets) ||
		!reflect.DeepEqual(got.GitHubTargets, want.GitHubTargets) ||
		!reflect.DeepEqual(got.JenkinsTargets, want.JenkinsTargets) ||
		!reflect.DeepEqual(got.HarborTargets, want.HarborTargets) ||
		!reflect.DeepEqual(got.DashboardTargets, want.DashboardTargets) ||
		!reflect.DeepEqual(got.ServiceBundles, want.ServiceBundles) ||
		!reflect.DeepEqual(got.PythonTasks, want.PythonTasks) {
		t.Fatalf("v8 migration lost saved data or failed to initialize SSH targets: %#v", got)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("repeat v8 migration: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("repeat migration rewrote settings: err=%v", err)
	}
}

func TestMigrateV8RejectsUnknownSSHFieldWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"version":8,"ssh_targets":[]}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err == nil {
		t.Fatal("v8 input with an unrecognized future SSH field was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected v8 input changed: err=%v", err)
	}
}
