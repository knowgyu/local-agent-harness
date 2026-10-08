package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSSHConfigStoreCASPreservesEveryOtherSettingsField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	initial := sshConfigStoreTestConfig(t)
	if err := writeConfig(path, initial); err != nil {
		t.Fatal(err)
	}
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	store := newSSHConfigStore(path)

	var snapshot sshOperationConfigSnapshot
	if err := withConfigLock(func() error {
		var snapshotErr error
		snapshot, snapshotErr = store.Snapshot()
		return snapshotErr
	}); err != nil {
		t.Fatal(err)
	}
	wantBaseRevision, err := sshConfigTargetsRevision(before.SSHTargets)
	if err != nil || snapshot.Revision != wantBaseRevision || !reflect.DeepEqual(snapshot.Targets, cloneSSHTargets(before.SSHTargets)) {
		t.Fatalf("snapshot did not reflect the saved SSH targets: snapshot=%#v wantRevision=%q err=%v", snapshot, wantBaseRevision, err)
	}

	replacement := []sshTargetDefinition{{
		ID: "target_one", Name: "QA updated", Alias: "qa-host", Operations: []SSHOperationDefinition{readOnlySSHOperation()},
	}}
	var newRevision string
	if err := withConfigLock(func() error {
		var saveErr error
		newRevision, saveErr = store.CompareAndSwap(snapshot.Revision, replacement)
		return saveErr
	}); err != nil {
		t.Fatal(err)
	}
	after, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.SSHTargets, replacement) || newRevision == snapshot.Revision {
		t.Fatalf("CAS did not persist the replacement targets: revision=%q targets=%#v", newRevision, after.SSHTargets)
	}
	before.SSHTargets, after.SSHTargets = nil, nil
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("SSH CAS changed unrelated settings fields:\nbefore=%#v\nafter=%#v", before, after)
	}

	if err := withConfigLock(func() error {
		_, staleErr := store.CompareAndSwap(snapshot.Revision, nil)
		if !errors.Is(staleErr, errSSHConfigurationStale) {
			return errors.New("stale SSH settings revision was not rejected")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := store.Snapshot()
	if err != nil || finalSnapshot.Revision != newRevision || !reflect.DeepEqual(finalSnapshot.Targets, replacement) {
		t.Fatalf("stale CAS changed persisted SSH settings: snapshot=%#v err=%v", finalSnapshot, err)
	}
}

func TestSSHConfigStoreUsesStableRevisionForNoTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := newSSHConfigStore(path)
	first, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Snapshot()
	if err != nil || first.Revision == "" || first.Revision != second.Revision || len(first.Targets) != 0 {
		t.Fatalf("empty target set revision was not stable: first=%#v second=%#v err=%v", first, second, err)
	}
}

func TestSSHConfigStoreRejectsStaleStoredOperationRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	operation := readOnlySSHOperation()
	operation.Revision = "stale"
	if err := writeConfig(path, config{
		Version: configVersion,
		SSHTargets: []sshTargetDefinition{{
			ID: "target_one", Name: "QA", Alias: "qa-host", Operations: []SSHOperationDefinition{operation},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := newSSHConfigStore(path).Snapshot(); !errors.Is(err, errSSHConfigStoreInvalid) {
		t.Fatalf("stale stored operation revision was not rejected: %v", err)
	}
}

func sshConfigStoreTestConfig(t *testing.T) config {
	t.Helper()
	credentialRef := "cred:" + strings.Repeat("1", 32)
	github := target{
		ID: "github:" + strings.Repeat("a", 32), Name: "GitHub fixture",
		Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: credentialRef,
	}
	jenkins := jenkinsTarget{
		ID: "jenkins:" + strings.Repeat("b", 32), Name: "Jenkins fixture",
		BaseURL: "https://jenkins.example.invalid", Username: "fixture", JobPath: "team/job",
		Environment: "qa", SecretRef: credentialRef,
	}
	harbor := harborTarget{
		ID: "harbor:" + strings.Repeat("c", 32), Name: "Harbor fixture",
		BaseURL: "https://harbor.example.invalid", Username: "fixture", Project: "project",
		Repository: "images/app", SecretRef: credentialRef,
	}
	dashboard := dashboardTarget{
		ID: "dashboard:" + strings.Repeat("d", 32), Name: "Dashboard fixture",
		BaseURL: "https://dashboard.example.invalid", SecretRef: credentialRef,
	}
	return config{
		Version: configVersion,
		ConnectionTests: map[string]connectionTest{
			github.ID: {Result: "success", CompletedAt: "2026-10-04T00:00:00Z"},
		},
		NamedSecrets: []namedSecretMetadata{{
			ID: "secret:" + strings.Repeat("e", 32), Name: "Fixture credential", Purpose: "test fixture", CredentialRef: credentialRef,
		}},
		GitHubTargets:    []target{github},
		JenkinsTargets:   []jenkinsTarget{jenkins},
		HarborTargets:    []harborTarget{harbor},
		DashboardTargets: []dashboardTarget{dashboard},
		ServiceBundles: []serviceBundle{{
			ID: "service:" + strings.Repeat("f", 32), Name: "Fixture bundle",
			GitHubTargetIDs: []string{github.ID},
			Environments: []serviceEnvironment{{
				Name: "qa", JenkinsTargetID: jenkins.ID, HarborTargetID: harbor.ID, DashboardTargetID: dashboard.ID,
			}},
		}},
		PythonTasks: []pythonTask{{
			ID: "python:" + strings.Repeat("a", 32), Name: "Fixture task",
			InterpreterPath: filepath.Join(t.TempDir(), "python.exe"), ScriptPath: filepath.Join(t.TempDir(), "script.py"),
			Disabled: true,
		}},
		SSHTargets: []sshTargetDefinition{{ID: "target_one", Name: "QA", Alias: "qa-host"}},
	}
}
