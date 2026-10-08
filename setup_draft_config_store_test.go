package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSetupDraftConfigStoreRoundTripsMetadataAndAppliesOnlyReviewedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := setupDraftStoreTestConfig()
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store := newSetupDraftConfigStore(path)
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !validSetupDraftRevision(snapshot.Revision) || len(snapshot.State.Connections) != 1 || snapshot.State.Connections[0].CredentialName != "Build token" {
		t.Fatalf("snapshot did not expose the safe named credential metadata: %#v", snapshot)
	}

	next := cloneSetupDraftState(snapshot.State)
	next.SSHTargets = append(next.SSHTargets, setupDraftSSHTargetRecord{
		ID:         "ssh_target_1",
		Definition: SetupDraftSSHTarget{Name: "Build host", Alias: "build-host", Operations: []SSHOperationDefinition{readOnlySSHOperation()}},
	})
	next.Bundles = append(next.Bundles, setupDraftBundleRecord{
		ID:         "service:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Definition: SetupDraftBundle{Name: "Delivery", RepositoryIDs: []string{"github:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Environments: []SetupDraftEnvironment{}},
	})
	newRevision, err := store.CompareAndSwap(snapshot.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	if !validSetupDraftRevision(newRevision) || newRevision == snapshot.Revision {
		t.Fatalf("CAS returned invalid revision %q after %q", newRevision, snapshot.Revision)
	}

	saved, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.GitHubTargets) != 1 || saved.GitHubTargets[0].SecretRef != "cred:11111111111111111111111111111111" || !saved.GitHubTargets[0].Disabled {
		t.Fatalf("unrelated existing target credential or enabled state changed: %#v", saved.GitHubTargets)
	}
	if !reflect.DeepEqual(saved.NamedSecrets, cfg.NamedSecrets) || len(saved.ServiceBundles) != 2 || len(saved.SSHTargets) != 1 {
		t.Fatalf("unrelated metadata or reviewed state was not preserved/applied: %#v", saved)
	}
	if _, err := store.CompareAndSwap(snapshot.Revision, next); !errors.Is(err, errSetupDraftStale) {
		t.Fatalf("old base revision was accepted: %v", err)
	}
}

func TestSetupDraftConfigStoreResolvesSelectedSecretAndClearsStaleConnectionTest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := setupDraftStoreTestConfig()
	cfg.NamedSecrets = append(cfg.NamedSecrets, namedSecretMetadata{
		ID: "secret:22222222222222222222222222222222", Name: "Release token", Purpose: "deploy",
		CredentialRef: "cred:22222222222222222222222222222222",
	})
	cfg.ConnectionTests = map[string]connectionTest{"github:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {Result: "success", CompletedAt: "2026-10-01T00:00:00Z"}}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store := newSetupDraftConfigStore(path)
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	next := cloneSetupDraftState(snapshot.State)
	next.Connections[0].CredentialName = "Release token"
	if _, err := store.CompareAndSwap(snapshot.Revision, next); err != nil {
		t.Fatal(err)
	}
	saved, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.GitHubTargets[0].SecretRef != "cred:22222222222222222222222222222222" {
		t.Fatalf("selected named secret did not resolve to its opaque ref: %#v", saved.GitHubTargets[0])
	}
	if _, exists := saved.ConnectionTests["github:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]; exists {
		t.Fatal("changing the credential mapping kept a stale successful connection test")
	}
}

func setupDraftStoreTestConfig() config {
	return config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID: "secret:11111111111111111111111111111111", Name: "Build token", Purpose: "build",
			CredentialRef: "cred:11111111111111111111111111111111",
		}},
		GitHubTargets: []target{{
			ID: "github:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "Repository", Origin: "https://github.com", Repository: "team/project",
			SecretRef: "cred:11111111111111111111111111111111", Disabled: true,
		}},
		ServiceBundles: []serviceBundle{{ID: "service:11111111111111111111111111111111", Name: "Existing", GitHubTargetIDs: []string{"github:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}},
	}
}
