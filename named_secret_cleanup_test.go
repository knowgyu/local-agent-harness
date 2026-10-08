package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newNamedSecretCleanupTestController(t *testing.T, configPath string, store secretStore) *namedSecretController {
	t.Helper()
	controller := newNamedSecretController(configPath, store)
	controller.cleanup = newNamedSecretCleanupTestQueue(t)
	return controller
}

func newNamedSecretCleanupTestQueue(t *testing.T) *namedSecretCleanupFileQueue {
	t.Helper()
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, namedSecretCleanupQueueFileName)
	queue, err := newNamedSecretCleanupFileQueueAt(path)
	if err != nil {
		t.Fatalf("create isolated cleanup queue: %v", err)
	}
	return queue
}

func TestNamedSecretRotationPersistsCleanupIntentAndRetrySurvivesControllerRestart(t *testing.T) {
	const oldCanary = "cleanup-retry-old-value-canary"
	const newCanary = "cleanup-retry-new-value-canary"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Rotation token", Value: []byte(oldCanary)})
	if err != nil {
		t.Fatalf("create named secret: %v", err)
	}
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("a", 32)
	if newRef == oldRef {
		t.Fatal("test refs unexpectedly match")
	}
	controller.newRef = func() (string, error) { return newRef, nil }
	store.failDelete = true
	rotated, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Purpose: created.Purpose, Value: []byte(newCanary),
	})
	if !errors.Is(err, errNamedSecretCleanup) || rotated.ID != created.ID {
		t.Fatalf("rotation cleanup result = %#v, err=%v", rotated, err)
	}
	rotationErr := err
	ids, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []string{created.ID}) {
		t.Fatalf("pending IDs = %#v, err=%v", ids, err)
	}
	queueBytes, err := os.ReadFile(controller.cleanup.(*namedSecretCleanupFileQueue).path)
	if err != nil || !strings.Contains(string(queueBytes), oldRef) || strings.Contains(string(queueBytes), oldCanary) || strings.Contains(string(queueBytes), newCanary) {
		t.Fatalf("protected queue did not retain only opaque cleanup metadata: %q, err=%v", queueBytes, err)
	}
	if strings.Contains(rotationErr.Error(), oldRef) || strings.Contains(rotationErr.Error(), newRef) || strings.Contains(rotationErr.Error(), oldCanary) || strings.Contains(rotationErr.Error(), newCanary) {
		t.Fatal("rotation cleanup error exposed credential material")
	}
	loadsBefore := len(store.loads)
	if err := controller.DeleteNamedSecret(context.Background(), created.ID); !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("delete with pending old-ref cleanup = %v", err)
	}
	if len(store.loads) != loadsBefore || string(store.values[newRef]) != newCanary {
		t.Fatal("delete with pending cleanup removed or loaded the current credential")
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != newRef || configReferencesSecret(stored, oldRef) {
		t.Fatalf("committed rotation snapshot = %#v, err=%v", stored.NamedSecrets, err)
	}

	store.failDelete = false
	reopened := newNamedSecretController(path, store)
	reopened.cleanup = controller.cleanup
	pending, err := reopened.RetryNamedSecretCleanup(context.Background(), created.ID)
	if err != nil || pending {
		t.Fatalf("restart retry = pending %v, err=%v", pending, err)
	}
	if _, exists := store.values[oldRef]; exists {
		t.Fatal("retry left the old credential in the fake store")
	}
	ids, err = reopened.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("cleanup queue after retry = %#v, err=%v", ids, err)
	}
}

func TestNamedSecretRotationFailedConfigWriteRemovesIntentAndNewCredential(t *testing.T) {
	const oldCanary = "config-failure-old-canary"
	const newCanary = "config-failure-new-canary"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Config failure", Value: []byte(oldCanary)})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("b", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	controller.persist = func(string, config) error { return errors.New("synthetic settings failure") }
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Purpose: created.Purpose, Value: []byte(newCanary),
	})
	if !errors.Is(err, errNamedSecretSettings) {
		t.Fatalf("failed settings write error = %v", err)
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != oldRef {
		t.Fatalf("failed settings write changed the canonical ref: %#v, err=%v", stored.NamedSecrets, err)
	}
	if _, exists := store.values[newRef]; exists || string(store.values[oldRef]) != oldCanary {
		t.Fatal("failed rotation did not roll back only the staged new credential")
	}
	ids, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("failed rotation was reported as committed cleanup work: %#v, err=%v", ids, err)
	}
}

func TestNamedSecretRotationConfigWriteErrorAfterCommitRemainsCleanupPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Committed rotation", Value: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("c", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	store.failDelete = true
	controller.persist = func(configPath string, cfg config) error {
		if err := writeConfig(configPath, cfg); err != nil {
			return err
		}
		return errors.New("synthetic response lost after commit")
	}
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Purpose: created.Purpose, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("committed settings with failed cleanup error = %v", err)
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != newRef {
		t.Fatalf("post-error canonical settings = %#v, err=%v", stored.NamedSecrets, err)
	}
	if _, exists := store.values[oldRef]; !exists {
		t.Fatal("failed cleanup unexpectedly removed the old credential")
	}
}

func TestNamedSecretCleanupQueueWriteFailurePreventsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Queue failure", Value: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("d", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	controller.cleanup = failingNamedSecretCleanupQueue{namedSecretCleanupQueue: controller.cleanup, failPut: true}
	savesBefore := len(store.saves)
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Purpose: created.Purpose, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretSettings) {
		t.Fatalf("queue write failure = %v", err)
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != oldRef {
		t.Fatalf("queue failure committed a rotation: %#v err=%v", stored.NamedSecrets, err)
	}
	if _, exists := store.values[newRef]; exists {
		t.Fatal("queue failure left the staged new credential in the fake store")
	}
	if len(store.saves) != savesBefore {
		t.Fatal("credential save ran before durable cleanup intent reservation")
	}
}

func TestNamedSecretRotationReservesIntentBeforeCredentialSaveAndReconcilesSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Save failure", Value: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("8", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	reservedBeforeSave := false
	store.beforeSave = func(ref string) {
		intents, loadErr := controller.cleanup.Load()
		reservedBeforeSave = loadErr == nil && len(intents) == 1 && intents[0].OldRef == oldRef && intents[0].NewRef == ref && !intents[0].Committed
	}
	store.failSave = true
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretStore) || !reservedBeforeSave {
		t.Fatalf("save failure reservation order = %v, err=%v", reservedBeforeSave, err)
	}
	stored, readErr := readConfig(path)
	intents, queueErr := controller.cleanup.Load()
	if readErr != nil || stored.NamedSecrets[0].CredentialRef != oldRef || queueErr != nil || len(intents) != 0 || len(store.values) != 1 {
		t.Fatalf("failed credential save did not reconcile: cfg=%#v intents=%#v values=%#v read=%v queue=%v", stored.NamedSecrets, intents, store.values, readErr, queueErr)
	}
}

func TestNamedSecretCleanupRetryIsIdempotentWhenQueueRemovalFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Consume failure", Value: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	baseQueue := controller.cleanup
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	controller.newRef = func() (string, error) { return "cred:" + strings.Repeat("e", 32), nil }
	controller.cleanup = failingNamedSecretCleanupQueue{namedSecretCleanupQueue: baseQueue, failRemove: true}
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Purpose: created.Purpose, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("queue consume failure = %v", err)
	}
	if _, exists := store.values[oldRef]; exists {
		t.Fatal("credential deletion did not complete before synthetic queue removal failure")
	}
	if intents, err := baseQueue.Load(); err != nil || len(intents) != 1 || intents[0].OldRef != oldRef || !intents[0].Committed {
		t.Fatalf("durable intent lost after queue removal failure: %#v, err=%v", intents, err)
	}
	reopened := newNamedSecretController(path, store)
	reopened.cleanup = baseQueue
	pending, err := reopened.RetryNamedSecretCleanup(context.Background(), created.ID)
	if err != nil || pending || len(store.deletes) < 2 {
		t.Fatalf("idempotent retry after delete-before-consume = pending %v deletes %d err=%v", pending, len(store.deletes), err)
	}
}

func TestNamedSecretCleanupPreparedIntentRecoveryDeletesOnlyStagedNewRef(t *testing.T) {
	const (
		id     = "secret:" + "12121212121212121212121212121212"
		oldRef = "cred:" + "23232323232323232323232323232323"
		newRef = "cred:" + "34343434343434343434343434343434"
	)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, NamedSecrets: []namedSecretMetadata{{
		ID: id, Name: "Prepared intent", CredentialRef: oldRef,
	}}}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{values: map[string][]byte{oldRef: []byte("current"), newRef: []byte("staged")}}
	controller := newNamedSecretCleanupTestController(t, path, store)
	if err := controller.cleanup.Put(namedSecretCleanupIntent{ID: id, OldRef: oldRef, NewRef: newRef}); err != nil {
		t.Fatal(err)
	}
	ids, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("prepared intent IDs = %#v err=%v", ids, err)
	}
	pending, err := controller.RetryNamedSecretCleanup(context.Background(), id)
	if err != nil || pending {
		t.Fatalf("prepared intent recovery = pending %v err=%v", pending, err)
	}
	if string(store.values[oldRef]) != "current" {
		t.Fatal("pre-CAS recovery deleted the active old credential")
	}
	if _, exists := store.values[newRef]; exists || !reflect.DeepEqual(store.deletes, []string{newRef}) {
		t.Fatalf("pre-CAS recovery did not delete only the staged ref: deletes=%#v", store.deletes)
	}
}

func TestNamedSecretCleanupRequiresDurableCommitMarkerBeforeDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{Name: "Marker failure", Value: []byte("old")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readConfig(path)
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("9", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	baseQueue := controller.cleanup
	controller.cleanup = failingNamedSecretCleanupQueue{namedSecretCleanupQueue: baseQueue, failMark: true}
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: created.Name, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("commit-marker failure = %v", err)
	}
	current, err := readConfig(path)
	if err != nil || current.NamedSecrets[0].CredentialRef != newRef || len(store.deletes) != 0 {
		t.Fatalf("old credential was deleted before durable commit marker: config=%#v deletes=%#v err=%v", current.NamedSecrets, store.deletes, err)
	}
	controller.cleanup = baseQueue
	pending, err := controller.RetryNamedSecretCleanup(context.Background(), created.ID)
	if err != nil || pending || len(store.deletes) != 1 || store.deletes[0] != oldRef {
		t.Fatalf("exact current-ref proof did not recover marker: pending=%v deletes=%#v err=%v", pending, store.deletes, err)
	}
}

func TestNamedSecretSaveReturnsCommittedViewWhenCleanupMarkerWriteFails(t *testing.T) {
	const (
		oldValue = "marker-failure-old-value-canary"
		newValue = "marker-failure-new-value-canary"
	)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	created, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		Name: "Marker response", Purpose: "rotation", Value: []byte(oldValue),
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := readConfig(path)
	if err != nil || len(before.NamedSecrets) != 1 {
		t.Fatalf("read initial canonical settings: %#v err=%v", before.NamedSecrets, err)
	}
	oldRef := before.NamedSecrets[0].CredentialRef
	newRef := "cred:" + strings.Repeat("8", 32)
	controller.newRef = func() (string, error) { return newRef, nil }
	baseQueue := controller.cleanup
	controller.cleanup = failingNamedSecretCleanupQueue{namedSecretCleanupQueue: baseQueue, failMark: true}
	view, saveErr := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: created.ID, Name: "Marker response", Purpose: "rotation", Value: []byte(newValue),
	})
	if !errors.Is(saveErr, errNamedSecretCleanup) || view.ID != created.ID || view.Name != "Marker response" ||
		view.Purpose != "rotation" || !view.Configured {
		t.Fatalf("committed settings did not return a saved view with cleanup pending: view=%+v err=%v", view, saveErr)
	}
	current, err := readConfig(path)
	if err != nil || len(current.NamedSecrets) != 1 || current.NamedSecrets[0].CredentialRef != newRef {
		t.Fatalf("canonical settings did not commit new ref: %#v err=%v", current.NamedSecrets, err)
	}
	intents, err := baseQueue.Load()
	if err != nil || len(intents) != 1 || intents[0].OldRef != oldRef || intents[0].NewRef != newRef || intents[0].Committed {
		t.Fatalf("failed marker did not preserve retryable intent: %#v err=%v", intents, err)
	}
	for _, canary := range []string{oldValue, newValue, oldRef, newRef} {
		if strings.Contains(saveErr.Error(), canary) {
			t.Fatalf("cleanup error exposed secret material or an opaque reference (%q): %v", canary, saveErr)
		}
	}
	if len(store.deletes) != 0 || string(store.values[newRef]) != newValue || string(store.values[oldRef]) != oldValue {
		t.Fatalf("marker failure unexpectedly deleted credentials: deletes=%#v", store.deletes)
	}
}

func TestNamedSecretCleanupRetryDoesNotDeleteAReferenceThatBecameActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{}
	controller := newNamedSecretCleanupTestController(t, path, store)
	controller.newRef = func() (string, error) { return "cred:" + strings.Repeat("f", 32), nil }
	before, err := readConfig(path)
	if err != nil || len(before.NamedSecrets) == 0 {
		t.Fatalf("read pre-existing named secret: %#v err=%v", before.NamedSecrets, err)
	}
	owner := before.NamedSecrets[0]
	oldRef := owner.CredentialRef
	store.values = map[string][]byte{oldRef: []byte("old")}
	store.failDelete = true
	_, err = controller.SaveNamedSecret(context.Background(), NamedSecretWrite{
		ID: owner.ID, Name: owner.Name, Purpose: owner.Purpose, Value: []byte("new"),
	})
	if !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("expected committed rotation cleanup error, got %v", err)
	}
	store.failDelete = false
	current, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	current.GitHubTargets[0].SecretRef = oldRef
	if err := writeConfig(path, current); err != nil {
		t.Fatal(err)
	}
	deletesBefore := len(store.deletes)
	pending, err := controller.RetryNamedSecretCleanup(context.Background(), owner.ID)
	if !pending || !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("ambiguous active reference retry = pending %v, err=%v", pending, err)
	}
	if len(store.deletes) != deletesBefore || string(store.values[oldRef]) != "old" {
		t.Fatal("retry deleted an old reference that became active in current settings")
	}
}

func TestNamedSecretCleanupRetryDoesNotDeleteCommittedOldRefAfterOwnerRestoration(t *testing.T) {
	const (
		id     = "secret:" + "45454545454545454545454545454545"
		oldRef = "cred:" + "56565656565656565656565656565656"
		newRef = "cred:" + "67676767676767676767676767676767"
	)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, NamedSecrets: []namedSecretMetadata{{
		ID: id, Name: "Restored owner", CredentialRef: oldRef,
	}}}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{values: map[string][]byte{oldRef: []byte("old"), newRef: []byte("new")}}
	controller := newNamedSecretCleanupTestController(t, path, store)
	if err := controller.cleanup.Put(namedSecretCleanupIntent{ID: id, OldRef: oldRef, NewRef: newRef, DeleteOld: true}); err != nil {
		t.Fatal(err)
	}
	if err := controller.cleanup.MarkCommitted(id, oldRef, newRef); err != nil {
		t.Fatal(err)
	}

	pending, err := controller.RetryNamedSecretCleanup(context.Background(), id)
	if !pending || !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("committed intent with restored old owner = pending %v, err=%v", pending, err)
	}
	if len(store.deletes) != 0 || string(store.values[oldRef]) != "old" || string(store.values[newRef]) != "new" {
		t.Fatalf("restored owner reached credential deletion: deletes=%#v", store.deletes)
	}
	intents, err := controller.cleanup.Load()
	if err != nil || len(intents) != 1 || !intents[0].Committed {
		t.Fatalf("ambiguous committed intent was consumed: %#v, err=%v", intents, err)
	}
}

func TestNamedSecretCleanupQueueRejectsCorruptionAndQuotaOverflow(t *testing.T) {
	queue := newNamedSecretCleanupTestQueue(t)
	for index := 0; index < namedSecretCleanupQueueMaxItems; index++ {
		intent := namedSecretCleanupIntent{
			ID:     fmt.Sprintf("secret:%032x", index+1),
			OldRef: fmt.Sprintf("cred:%032x", index+1),
			NewRef: fmt.Sprintf("cred:%032x", index+namedSecretCleanupQueueMaxItems+1),
		}
		if err := queue.Put(intent); err != nil {
			t.Fatalf("put bounded queue entry %d: %v", index, err)
		}
	}
	if err := queue.Put(namedSecretCleanupIntent{ID: "secret:" + strings.Repeat("f", 32), OldRef: "cred:" + strings.Repeat("f", 32), NewRef: "cred:" + strings.Repeat("e", 32)}); !errors.Is(err, errNamedSecretCleanupQueue) {
		t.Fatalf("queue accepted an item beyond its bound: %v", err)
	}
	data, err := os.ReadFile(queue.path)
	if err != nil || len(data) > namedSecretCleanupQueueMaxBytes {
		t.Fatalf("queue exceeded byte bound: size=%d err=%v", len(data), err)
	}
	if err := writeSetupDraftQueueFileAtomically(queue.path, []byte(`{"version":1,"version":1,"entries":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Load(); !errors.Is(err, errNamedSecretCleanupQueue) {
		t.Fatalf("queue accepted duplicate JSON metadata: %v", err)
	}
}

func TestNamedSecretCleanupRetryInvalidOwnerFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretStoreFake{values: map[string][]byte{"cred:" + strings.Repeat("1", 32): []byte("synthetic")}}
	controller := newNamedSecretCleanupTestController(t, path, store)
	badID := "secret:" + strings.Repeat("2", 32)
	badRef := "cred:" + strings.Repeat("1", 32)
	if err := controller.cleanup.Put(namedSecretCleanupIntent{ID: badID, OldRef: badRef, NewRef: "cred:" + strings.Repeat("3", 32)}); err != nil {
		t.Fatal(err)
	}
	if pending, err := controller.RetryNamedSecretCleanup(context.Background(), badID); !pending ||
		(!errors.Is(err, errNamedSecretCleanupQueue) && !errors.Is(err, errNamedSecretCleanup)) {
		t.Fatalf("retry accepted an intent without one current metadata owner: %v", err)
	}
	if len(store.deletes) != 0 {
		t.Fatal("invalid intent ownership reached Credential Manager fake")
	}
}

func TestNamedSecretRotateAndRetrySerializeWithoutDeletingCurrentCredential(t *testing.T) {
	const (
		id        = "secret:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
		oldRef    = "cred:" + "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
		firstRef  = "cred:" + "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
		secondRef = "cred:" + "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
	)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(path, config{Version: configVersion, NamedSecrets: []namedSecretMetadata{{
		ID: id, Name: "Concurrent secret", CredentialRef: oldRef,
	}}}); err != nil {
		t.Fatal(err)
	}
	store := &namedSecretConcurrentStore{values: map[string][]byte{oldRef: []byte("old")}, failDelete: true}
	controller := newNamedSecretCleanupTestController(t, path, store)
	controller.newRef = func() (string, error) { return firstRef, nil }
	if _, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{ID: id, Name: "Concurrent secret", Value: []byte("first")}); !errors.Is(err, errNamedSecretCleanup) {
		t.Fatalf("create initial pending cleanup: %v", err)
	}
	store.setFailDelete(false)
	controller.newRef = func() (string, error) { return secondRef, nil }
	var group sync.WaitGroup
	errCh := make(chan error, 2)
	group.Add(2)
	go func() {
		defer group.Done()
		_, err := controller.SaveNamedSecret(context.Background(), NamedSecretWrite{ID: id, Name: "Concurrent secret", Value: []byte("second")})
		errCh <- err
	}()
	go func() {
		defer group.Done()
		_, err := controller.RetryNamedSecretCleanup(context.Background(), id)
		errCh <- err
	}()
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("serialized rotate/retry returned %v", err)
		}
	}
	stored, err := readConfig(path)
	if err != nil || stored.NamedSecrets[0].CredentialRef != secondRef {
		t.Fatalf("concurrent rotation canonical ref = %#v, err=%v", stored.NamedSecrets, err)
	}
	if _, exists := store.loadValue(secondRef); !exists {
		t.Fatal("concurrent cleanup deleted the current credential")
	}
	if _, exists := store.loadValue(oldRef); exists {
		t.Fatal("concurrent cleanup retained the obsolete original credential")
	}
	if _, exists := store.loadValue(firstRef); exists {
		t.Fatal("concurrent cleanup retained the intermediate credential")
	}
	ids, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("concurrent cleanup left pending intents: %#v err=%v", ids, err)
	}
}

type failingNamedSecretCleanupQueue struct {
	namedSecretCleanupQueue
	failPut    bool
	failMark   bool
	failRemove bool
}

func (q failingNamedSecretCleanupQueue) Put(intent namedSecretCleanupIntent) error {
	if q.failPut {
		return errNamedSecretCleanupQueue
	}
	return q.namedSecretCleanupQueue.Put(intent)
}

func (q failingNamedSecretCleanupQueue) Remove(id, ref string) error {
	if q.failRemove {
		return errNamedSecretCleanupQueue
	}
	return q.namedSecretCleanupQueue.Remove(id, ref)
}

func (q failingNamedSecretCleanupQueue) MarkCommitted(id, oldRef, newRef string) error {
	if q.failMark {
		return errNamedSecretCleanupQueue
	}
	return q.namedSecretCleanupQueue.MarkCommitted(id, oldRef, newRef)
}

type namedSecretConcurrentStore struct {
	mu         sync.Mutex
	values     map[string][]byte
	deleteHits int
	failDelete bool
}

func (s *namedSecretConcurrentStore) Save(ref string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string][]byte)
	}
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *namedSecretConcurrentStore) Load(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func (s *namedSecretConcurrentStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteHits++
	if s.failDelete {
		return errors.New("synthetic delete failure")
	}
	delete(s.values, ref)
	return nil
}

func (s *namedSecretConcurrentStore) setFailDelete(value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failDelete = value
}

func (s *namedSecretConcurrentStore) loadValue(ref string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	return append([]byte(nil), value...), ok
}
