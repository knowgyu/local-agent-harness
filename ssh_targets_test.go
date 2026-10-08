package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeSSHOperationStore struct {
	mu       sync.Mutex
	snapshot sshOperationConfigSnapshot
	loadErr  error
	saveErr  error
	loads    int
	writes   int
}

func newFakeSSHOperationStore() *fakeSSHOperationStore {
	return &fakeSSHOperationStore{snapshot: sshOperationConfigSnapshot{Revision: "initial"}}
}

func (s *fakeSSHOperationStore) Snapshot() (sshOperationConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return sshOperationConfigSnapshot{}, s.loadErr
	}
	return cloneSSHOperationSnapshot(s.snapshot), nil
}

func (s *fakeSSHOperationStore) CompareAndSwap(expectedRevision string, targets []sshTargetDefinition) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.saveErr != nil {
		return "", s.saveErr
	}
	if s.snapshot.Revision != expectedRevision {
		return "", errSSHConfigurationStale
	}
	encoded, err := json.Marshal(targets)
	if err != nil {
		return "", errors.New("synthetic store could not encode the snapshot")
	}
	digest := sha256.Sum256(encoded)
	s.snapshot = sshOperationConfigSnapshot{Revision: hex.EncodeToString(digest[:]), Targets: cloneSSHTargets(targets)}
	return s.snapshot.Revision, nil
}

func TestSSHOperationStoreCASAndSavedDefinitionRevision(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, &fakeSSHOperationRunner{}, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Dev cluster", Alias: "dev-cluster",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "target_one" || target.Alias != "dev-cluster" || revision == "initial" {
		t.Fatalf("target was not saved with the new revision: target=%#v revision=%q", target, revision)
	}

	operation, nextRevision, err := controller.SaveOperation(context.Background(), target.ID, readOnlySSHOperation(), revision)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Revision == "" || operation.Approval.Revision != "" || nextRevision == revision {
		t.Fatalf("operation revision was not generated: op=%#v next=%q", operation, nextRevision)
	}
	if !isStoredSSHOperationValid(operation, target.ID) {
		t.Fatal("controller saved an operation that fails its own revision check")
	}
	if _, _, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_two", Name: "Prod cluster", Alias: "prod-cluster",
	}, revision); !errors.Is(err, errSSHConfigurationStale) {
		t.Fatalf("stale config write was accepted: %v", err)
	}
	if store.writes != 2 {
		t.Fatalf("stale write reached CAS: writes=%d", store.writes)
	}
}

func TestSSHOperationStoreRejectsAliasAndNameConflicts(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, &fakeSSHOperationRunner{}, nil)
	_, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Dev", Alias: "shared-alias",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []sshTargetDefinition{
		{ID: "target_two", Name: "DEV", Alias: "another-alias"},
		{ID: "target_two", Name: "Production", Alias: "SHARED-ALIAS"},
		{ID: "target_two", Name: "Production", Alias: "bad*alias"},
	} {
		if _, _, err := controller.SaveTarget(context.Background(), duplicate, revision); !errors.Is(err, errSSHDefinitionInvalid) {
			t.Fatalf("duplicate or unsafe target was accepted: %#v err=%v", duplicate, err)
		}
	}
	if store.writes != 1 {
		t.Fatalf("invalid target was persisted: writes=%d", store.writes)
	}
}

func TestSSHOperationCatalogOmitsAliasAndReviewedCommand(t *testing.T) {
	store := newFakeSSHOperationStore()
	controller := newSSHOperationController(store, &fakeSSHOperationRunner{}, nil)
	target, revision, err := controller.SaveTarget(context.Background(), sshTargetDefinition{
		ID: "target_one", Name: "Visible target name", Alias: "private-alias",
	}, "initial")
	if err != nil {
		t.Fatal(err)
	}
	operation, _, err := controller.SaveOperation(context.Background(), target.ID, readOnlySSHOperation(), revision)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := controller.ListSSHOperationCatalog(context.Background())
	if err != nil || len(catalog) != 1 || len(catalog[0].Operations) != 1 {
		t.Fatalf("catalog failed: %#v err=%v", catalog, err)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-alias", operation.Program, strings.Join(operation.FixedArgs, " ")} {
		if forbidden != "" && strings.Contains(string(encoded), forbidden) {
			t.Fatalf("catalog exposed UI-only command data %q: %s", forbidden, encoded)
		}
	}
	if catalog[0].ID != target.ID || catalog[0].Name != target.Name || catalog[0].Operations[0].Name != operation.Name {
		t.Fatalf("catalog omitted its locked identifiers or task name: %#v", catalog)
	}
}

func TestSSHOperationControllerFailsClosedOnStoreErrors(t *testing.T) {
	store := newFakeSSHOperationStore()
	store.loadErr = errors.New("host details and private material")
	controller := newSSHOperationController(store, &fakeSSHOperationRunner{}, nil)
	if _, err := controller.ListSSHTargets(context.Background()); err == nil || strings.Contains(err.Error(), "private material") {
		t.Fatalf("store error was absent or leaked: %v", err)
	}
}

func readOnlySSHOperation() SSHOperationDefinition {
	operation, err := normalizeSSHOperationDefinition(SSHOperationDefinition{
		ID: "list_pods", Name: "List pods", Summary: "Read registered namespace workload names.",
		Program: "kubectl", FixedArgs: []string{"get", "pods", "--namespace=qa"},
		Risk: SSHRiskReadOnly, Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
	}, "target_one")
	if err != nil {
		panic(err)
	}
	return operation
}
