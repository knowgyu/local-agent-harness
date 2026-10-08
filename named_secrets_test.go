package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type namedSecretStoreFake struct {
	values     map[string][]byte
	saves      []string
	loads      []string
	deletes    []string
	failSave   bool
	failLoad   bool
	failDelete bool
	beforeSave func(ref string)
}

func (s *namedSecretStoreFake) Save(ref string, value []byte) error {
	s.saves = append(s.saves, ref)
	if s.beforeSave != nil {
		s.beforeSave(ref)
	}
	if s.failSave {
		return errors.New("synthetic save failure")
	}
	if s.values == nil {
		s.values = make(map[string][]byte)
	}
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *namedSecretStoreFake) Load(ref string) ([]byte, error) {
	s.loads = append(s.loads, ref)
	if s.failLoad {
		return nil, errors.New("synthetic load failure")
	}
	value, ok := s.values[ref]
	if !ok {
		return nil, errors.New("synthetic missing credential")
	}
	return append([]byte(nil), value...), nil
}

func (s *namedSecretStoreFake) Delete(ref string) error {
	s.deletes = append(s.deletes, ref)
	if s.failDelete {
		return errors.New("synthetic delete failure")
	}
	delete(s.values, ref)
	return nil
}

func TestNamedSecretControllerCreateReuseRotateAndDelete(t *testing.T) {
	const firstCanary = "named-secret-canary-first-not-in-output"
	const rotatedCanary = "named-secret-canary-rotated-not-in-output"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	ctx := context.Background()

	created, err := controller.SaveNamedSecret(ctx, NamedSecretWrite{
		Name: "Release token", Purpose: "Jenkins automation", Value: []byte(firstCanary),
	})
	if err != nil || created.ID == "" || !created.Configured || created.InUse {
		t.Fatalf("create result = %#v, err=%v", created, err)
	}
	var publicJSON []byte
	publicJSON, err = json.Marshal(created)
	if err != nil || strings.Contains(string(publicJSON), firstCanary) || strings.Contains(string(publicJSON), "credential_ref") || strings.Contains(string(publicJSON), "cred:") {
		t.Fatalf("public named secret view leaked credential data: %s, err=%v", publicJSON, err)
	}
	settings, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(settings), firstCanary) {
		t.Fatalf("settings contain canary or are unavailable: err=%v", err)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.NamedSecrets) != 1 {
		t.Fatalf("saved named secret metadata = %#v, err=%v", cfg.NamedSecrets, err)
	}
	oldRef := cfg.NamedSecrets[0].CredentialRef
	if got := string(store.values[oldRef]); got != firstCanary {
		t.Fatalf("credential store value = %q, want canary", got)
	}
	newRefGenerator := controller.newRef
	controller.newRef = func() (string, error) { return oldRef, nil }
	savesBeforeCollision := len(store.saves)
	if _, err := controller.SaveNamedSecret(ctx, NamedSecretWrite{Name: "Second credential", Value: []byte(rotatedCanary)}); !errors.Is(err, errNamedSecretInvalid) {
		t.Fatalf("credential reference collision error = %v", err)
	}
	controller.newRef = newRefGenerator
	if len(store.saves) != savesBeforeCollision || string(store.values[oldRef]) != firstCanary {
		t.Fatal("credential reference collision overwrote an existing secret")
	}

	beforeSaves := len(store.saves)
	renamed, err := controller.SaveNamedSecret(ctx, NamedSecretWrite{ID: created.ID, Name: "Release token", Purpose: "Updated purpose"})
	if err != nil || renamed.Purpose != "Updated purpose" || len(store.saves) != beforeSaves {
		t.Fatalf("metadata-only update = %#v, saves=%d err=%v", renamed, len(store.saves), err)
	}

	registered := target{
		ID: "github:" + strings.Repeat("a", 32), Name: "Release repository",
		Origin: "https://github.example.invalid", Repository: "team/release", SecretRef: oldRef,
	}
	cfg.GitHubTargets = []target{registered}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	listed, err := controller.ListNamedSecrets(ctx)
	if err != nil || len(listed) != 1 || !listed[0].InUse || listed[0].ID != created.ID {
		t.Fatalf("reused named secret list = %#v, err=%v", listed, err)
	}
	beforeLoads, beforeDeletes := len(store.loads), len(store.deletes)
	if err := controller.DeleteNamedSecret(ctx, created.ID); !errors.Is(err, errNamedSecretInUse) {
		t.Fatalf("delete-in-use error = %v", err)
	}
	if len(store.loads) != beforeLoads || len(store.deletes) != beforeDeletes {
		t.Fatal("delete-in-use accessed the credential store")
	}

	rotated, err := controller.SaveNamedSecret(ctx, NamedSecretWrite{
		ID: created.ID, Name: "Release token", Purpose: "Updated purpose", Value: []byte(rotatedCanary),
	})
	if err != nil || rotated.ID != created.ID || !rotated.InUse {
		t.Fatalf("rotation result = %#v, err=%v", rotated, err)
	}
	cfg, err = readConfig(path)
	if err != nil || cfg.GitHubTargets[0].SecretRef == oldRef || cfg.NamedSecrets[0].CredentialRef != cfg.GitHubTargets[0].SecretRef {
		t.Fatalf("rotation did not preserve the reference link: cfg=%#v err=%v", cfg, err)
	}
	newRef := cfg.NamedSecrets[0].CredentialRef
	if _, exists := store.values[oldRef]; exists || string(store.values[newRef]) != rotatedCanary {
		t.Fatal("rotation did not replace the credential while preserving the named item")
	}
	settings, err = os.ReadFile(path)
	if err != nil || strings.Contains(string(settings), firstCanary) || strings.Contains(string(settings), rotatedCanary) {
		t.Fatal("rotated credential value appeared in settings")
	}

	cfg.GitHubTargets = nil
	cfg.Target = nil
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := controller.DeleteNamedSecret(ctx, created.ID); err != nil {
		t.Fatalf("delete unused named secret: %v", err)
	}
	cfg, err = readConfig(path)
	if err != nil || len(cfg.NamedSecrets) != 0 {
		t.Fatalf("deleted named secret metadata remains: %#v err=%v", cfg.NamedSecrets, err)
	}
	if _, exists := store.values[newRef]; exists {
		t.Fatal("deleted named secret credential remains in the fake store")
	}
}

func TestNamedSecretRotationRepointsEveryConsumerAndReportsCleanupFailure(t *testing.T) {
	const oldCanary = "shared-named-secret-old-canary"
	const newCanary = "shared-named-secret-new-canary"
	const oldRef = "cred:" + "0123456789abcdef0123456789abcdef"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	for i := range cfg.GitHubTargets {
		cfg.GitHubTargets[i].SecretRef = oldRef
	}
	cfg.JenkinsTargets[0].SecretRef = oldRef
	cfg.HarborTargets[0].SecretRef = oldRef
	cfg.DashboardTargets[0].SecretRef = oldRef
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.NamedSecrets) != 1 {
		t.Fatalf("shared credential was not indexed once: %#v err=%v", cfg.NamedSecrets, err)
	}
	item := cfg.NamedSecrets[0]
	store := &namedSecretStoreFake{values: map[string][]byte{oldRef: []byte(oldCanary)}, failDelete: true}
	controller := newNamedSecretCleanupTestController(t, path, store)
	view, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: item.ID, Name: item.Name, Purpose: item.Purpose, Value: []byte(newCanary),
	})
	if !errors.Is(err, errNamedSecretCleanup) || view.ID != item.ID || !view.InUse {
		t.Fatalf("cleanup-failure result = %#v, err=%v", view, err)
	}
	cfg, readErr := readConfig(path)
	if readErr != nil {
		t.Fatalf("read rotated config: %v", readErr)
	}
	newRef := cfg.NamedSecrets[0].CredentialRef
	for _, ref := range []string{
		cfg.GitHubTargets[0].SecretRef,
		cfg.GitHubTargets[1].SecretRef,
		cfg.JenkinsTargets[0].SecretRef,
		cfg.HarborTargets[0].SecretRef,
		cfg.DashboardTargets[0].SecretRef,
	} {
		if ref != newRef {
			t.Fatalf("consumer ref = %q, want the new named secret ref", ref)
		}
	}
	if string(store.values[newRef]) != newCanary || string(store.values[oldRef]) != oldCanary {
		t.Fatal("cleanup failure changed or lost a credential value")
	}
	encoded, marshalErr := json.Marshal(struct {
		View NamedSecretView `json:"view"`
		Err  string          `json:"error"`
	}{View: view, Err: err.Error()})
	if marshalErr != nil || strings.Contains(string(encoded), oldCanary) || strings.Contains(string(encoded), newCanary) || strings.Contains(string(encoded), oldRef) || strings.Contains(string(encoded), newRef) {
		t.Fatalf("cleanup response leaked credential material: %s", encoded)
	}
}

func TestNamedSecretValidationAndStoreFailuresDoNotPersistCanary(t *testing.T) {
	const canary = "named-secret-store-failure-canary"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{failSave: true}
	controller := newNamedSecretCleanupTestController(t, path, store)
	if _, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Token", Value: []byte(canary)}); !errors.Is(err, errNamedSecretStore) {
		t.Fatalf("store failure error = %v", err)
	}
	cfg, err := readConfig(path)
	if err != nil || len(cfg.NamedSecrets) != 0 || len(store.values) != 0 {
		t.Fatalf("store failure changed settings or credentials: cfg=%#v values=%#v err=%v", cfg.NamedSecrets, store.values, err)
	}
	if _, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "", Value: []byte(canary)}); !errors.Is(err, errNamedSecretInvalid) {
		t.Fatalf("invalid input error = %v", err)
	}
	if _, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: string([]byte{0xff}), Value: []byte(canary)}); !errors.Is(err, errNamedSecretInvalid) {
		t.Fatalf("invalid UTF-8 input error = %v", err)
	}
	store.failSave = false
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Build credential", Value: []byte(canary)})
	if err != nil {
		t.Fatalf("create first unique secret: %v", err)
	}
	savesBeforeDuplicate := len(store.saves)
	if _, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "build credential", Value: []byte(canary)}); !errors.Is(err, errNamedSecretInvalid) {
		t.Fatalf("case-insensitive duplicate name error = %v", err)
	}
	if len(store.saves) != savesBeforeDuplicate {
		t.Fatal("duplicate name reached the credential store")
	}
	if err := controller.DeleteNamedSecret(context.Background(), created.ID); err != nil {
		t.Fatalf("delete created secret: %v", err)
	}
}

func TestWriteConfigIndexesTargetCredentialAndRetainsNamedSecretWhenTargetIsDeleted(t *testing.T) {
	const ref = "cred:" + "abcdef0123456789abcdef0123456789"
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config{Version: configVersion, GitHubTargets: []target{{
		ID: "github:" + strings.Repeat("c", 32), Name: "Ops",
		Origin: "https://github.example.invalid", Repository: "ops/agent", SecretRef: ref,
	}}}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := readConfig(path)
	if err != nil || len(stored.NamedSecrets) != 1 || stored.NamedSecrets[0].CredentialRef != ref {
		t.Fatalf("target reference was not indexed: %#v err=%v", stored.NamedSecrets, err)
	}
	stored.GitHubTargets = nil
	stored.Target = nil
	if err := writeConfig(path, stored); err != nil {
		t.Fatal(err)
	}
	stored, err = readConfig(path)
	if err != nil || len(stored.NamedSecrets) != 1 || !configReferencesSecret(stored, ref) {
		t.Fatalf("unused named secret was not retained for reuse: %#v err=%v", stored.NamedSecrets, err)
	}
}
