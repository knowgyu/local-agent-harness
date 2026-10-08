package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClientRegistrationAdapter struct {
	id         string
	status     MCPClientStatus
	config     clientRegistrationConfig
	desired    clientRegistrationConfig
	inspectErr error
	planErr    error
	writeErr   error
	verifyErr  error
	planCalls  int
	writeCalls int
}

func (a *fakeClientRegistrationAdapter) clientID() string { return a.id }

func (a *fakeClientRegistrationAdapter) inspect(context.Context) (clientRegistrationSnapshot, error) {
	return clientRegistrationSnapshot{
		status: a.status,
		config: cloneClientRegistrationConfig(a.config),
	}, a.inspectErr
}

func (a *fakeClientRegistrationAdapter) plan(_ context.Context, _ MCPRegistrationSpec, current clientRegistrationConfig) (clientRegistrationMutation, error) {
	a.planCalls++
	if a.planErr != nil {
		return clientRegistrationMutation{}, a.planErr
	}
	if sameClientRegistrationConfig(current, a.desired) {
		return clientRegistrationMutation{config: cloneClientRegistrationConfig(a.desired), changes: []string{mcpRegistrationChangeUnchanged}}, nil
	}
	return clientRegistrationMutation{config: cloneClientRegistrationConfig(a.desired), changes: []string{mcpRegistrationChangeAdded}}, nil
}

func (a *fakeClientRegistrationAdapter) writeConfig(_ context.Context, expected, replacement clientRegistrationConfig) error {
	a.writeCalls++
	if a.writeErr != nil {
		return a.writeErr
	}
	if !sameClientRegistrationConfig(a.config, expected) {
		return errClientRegistrationStaleSettings
	}
	a.config = cloneClientRegistrationConfig(replacement)
	a.status.Registration = mcpClientRegistrationRegistered
	a.status.EvidenceSource = mcpClientEvidenceConfigObserved
	return nil
}

func (a *fakeClientRegistrationAdapter) verify(context.Context) (MCPClientStatus, error) {
	return a.status, a.verifyErr
}

func newFakeRegistrationController(t *testing.T, adapter *fakeClientRegistrationAdapter, now *time.Time, ttl time.Duration) *mcpClientRegistrationController {
	t.Helper()
	store, err := newClientRegistrationBackupStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("create isolated backup store: %v", err)
	}
	if now == nil {
		current := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
		now = &current
	}
	var idCounter int
	controller, err := newMCPClientRegistrationController(mcpClientRegistrationControllerOptions{
		adapters:    []clientRegistrationAdapter{adapter},
		backupStore: store,
		planTTL:     ttl,
		now:         func() time.Time { return *now },
		newID: func() (string, error) {
			idCounter++
			return fmt.Sprintf("plan_%015d", idCounter), nil
		},
	})
	if err != nil {
		t.Fatalf("create registration controller: %v", err)
	}
	return controller
}

func newReadyFakeAdapter(clientID string, original, desired []byte) *fakeClientRegistrationAdapter {
	return &fakeClientRegistrationAdapter{
		id: clientID,
		status: MCPClientStatus{
			Installed:      mcpClientInstalled,
			Version:        "1.2.3",
			Registration:   mcpClientRegistrationNotRegistered,
			EvidenceSource: mcpClientEvidenceConfigObserved,
		},
		config:  clientRegistrationConfig{exists: true, bytes: append([]byte(nil), original...)},
		desired: clientRegistrationConfig{exists: true, bytes: append([]byte(nil), desired...)},
	}
}

func validFakeRegistrationSpec(t *testing.T, clientID string) MCPRegistrationSpec {
	t.Helper()
	command, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return MCPRegistrationSpec{
		ClientID:    clientID,
		DisplayName: mcpClientRegistrationDisplayName,
		Transport:   MCPTransportStdio,
		Command:     command,
		Args:        []string{"--mcp"},
		Scope:       []string{"user"},
	}
}

func TestClientRegistrationApplyIsIdempotentAndRestoreUsesExactSnapshot(t *testing.T) {
	ctx := context.Background()
	original := []byte(`{"mcpServers":{"unmanaged":{"command":"existing"}}}`)
	desired := []byte(`{"mcpServers":{"unmanaged":{"command":"existing"},"local-agent-harness":{"command":"app","args":["--mcp"]}}}`)
	adapter := newReadyFakeAdapter(mcpClientIDCodex, original, desired)
	controller := newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)

	plan, err := controller.PlanClientRegistration(ctx, validFakeRegistrationSpec(t, mcpClientIDCodex))
	if err != nil {
		t.Fatalf("create registration plan: %v", err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0] != mcpRegistrationChangeAdded {
		t.Fatalf("unexpected safe change list: %#v", plan.Changes)
	}
	receipt, err := controller.BackupClientRegistration(ctx, mcpClientIDCodex)
	if err != nil {
		t.Fatalf("save backup: %v", err)
	}
	if receipt.Status != mcpClientBackupCreated {
		t.Fatalf("backup status = %q", receipt.Status)
	}

	status, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision)
	if err != nil {
		t.Fatalf("apply registration: %v", err)
	}
	if status.Registration != mcpClientRegistrationRegistered || status.Backup != mcpClientBackupApplied {
		t.Fatalf("unexpected applied status: %#v", status)
	}
	if adapter.writeCalls != 1 || string(adapter.config.bytes) != string(desired) {
		t.Fatalf("apply did not write the exact desired settings once")
	}

	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if adapter.writeCalls != 1 {
		t.Fatalf("idempotent apply rewrote settings %d times", adapter.writeCalls)
	}

	status, err = controller.RestoreClientRegistration(ctx, mcpClientIDCodex, receipt.ID)
	if err != nil {
		t.Fatalf("restore settings: %v", err)
	}
	if status.Backup != mcpClientBackupRestored || string(adapter.config.bytes) != string(original) {
		t.Fatalf("restore did not recover exact original settings")
	}
	status, err = controller.RestoreClientRegistration(ctx, mcpClientIDCodex, receipt.ID)
	if err != nil || status.Backup != mcpClientBackupRestored {
		t.Fatalf("repeat restore was not idempotent: status=%#v err=%v", status, err)
	}
}

func TestClientRegistrationRejectsStalePlanAndMissingBackup(t *testing.T) {
	ctx := context.Background()
	adapter := newReadyFakeAdapter(mcpClientIDCodex, []byte("original"), []byte("desired"))
	controller := newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)
	plan, err := controller.PlanClientRegistration(ctx, validFakeRegistrationSpec(t, mcpClientIDCodex))
	if err != nil {
		t.Fatalf("create registration plan: %v", err)
	}
	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); !errors.Is(err, errClientRegistrationBackupRequired) {
		t.Fatalf("apply without backup error = %v", err)
	}
	if _, err := controller.BackupClientRegistration(ctx, mcpClientIDCodex); err != nil {
		t.Fatalf("save backup: %v", err)
	}
	adapter.config = clientRegistrationConfig{exists: true, bytes: []byte("changed outside the plan")}
	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); !errors.Is(err, errClientRegistrationStaleSettings) {
		t.Fatalf("stale apply error = %v", err)
	}
	if adapter.writeCalls != 0 {
		t.Fatalf("stale plan wrote settings")
	}
}

func TestClientRegistrationPreservesInstalledButUnobservedScope(t *testing.T) {
	adapter := newReadyFakeAdapter(mcpClientIDClaude, []byte("unread"), []byte("desired"))
	adapter.status.Registration = mcpClientRegistrationNotObserved
	adapter.status.EvidenceSource = mcpClientEvidenceNotObserved
	controller := newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)

	status, err := controller.InspectClient(context.Background(), mcpClientIDClaude)
	if err != nil {
		t.Fatalf("inspect unobserved client: %v", err)
	}
	if status.Installed != mcpClientInstalled || status.Registration != mcpClientRegistrationNotObserved || status.EvidenceSource != mcpClientEvidenceNotObserved {
		t.Fatalf("installed-but-unobserved status was changed: %#v", status)
	}
	if _, err := controller.PlanClientRegistration(context.Background(), validFakeRegistrationSpec(t, mcpClientIDClaude)); !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("plan with unobserved settings error = %v", err)
	}
	if adapter.planCalls != 0 {
		t.Fatalf("adapter planned from an unobserved settings scope")
	}
}

func TestClientRegistrationExpiresPlansAndRejectsStaleRestore(t *testing.T) {
	ctx := context.Background()
	currentTime := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	adapter := newReadyFakeAdapter(mcpClientIDCodex, []byte("original"), []byte("desired"))
	controller := newFakeRegistrationController(t, adapter, &currentTime, time.Minute)
	plan, err := controller.PlanClientRegistration(ctx, validFakeRegistrationSpec(t, mcpClientIDCodex))
	if err != nil {
		t.Fatalf("create expiring plan: %v", err)
	}
	currentTime = currentTime.Add(2 * time.Minute)
	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); !errors.Is(err, errClientRegistrationPlanExpired) {
		t.Fatalf("expired apply error = %v", err)
	}

	controller = newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)
	plan, err = controller.PlanClientRegistration(ctx, validFakeRegistrationSpec(t, mcpClientIDCodex))
	if err != nil {
		t.Fatalf("create restore plan: %v", err)
	}
	receipt, err := controller.BackupClientRegistration(ctx, mcpClientIDCodex)
	if err != nil {
		t.Fatalf("save restore backup: %v", err)
	}
	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); err != nil {
		t.Fatalf("apply before stale restore: %v", err)
	}
	adapter.config = clientRegistrationConfig{exists: true, bytes: []byte("external settings change")}
	if _, err := controller.RestoreClientRegistration(ctx, mcpClientIDCodex, receipt.ID); !errors.Is(err, errClientRegistrationBackupStale) {
		t.Fatalf("stale restore error = %v", err)
	}
	if string(adapter.config.bytes) != "external settings change" {
		t.Fatalf("stale restore overwrote external changes")
	}
}

func TestClientRegistrationErrorAndStatusOutputAreFixedAndSecretFree(t *testing.T) {
	adapter := newReadyFakeAdapter(mcpClientIDCodex, []byte("fixture-only"), []byte("desired"))
	adapter.status.Version = "private token value"
	controller := newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)
	status, err := controller.InspectClient(context.Background(), mcpClientIDCodex)
	if err != nil {
		t.Fatalf("inspect test client: %v", err)
	}
	if status.Version != "" {
		t.Fatalf("unsafe version text reached public status: %#v", status)
	}
	adapter.inspectErr = errors.New("raw settings and private-token-value")
	if _, err := controller.InspectClient(context.Background(), mcpClientIDCodex); err != errClientRegistrationInspectionFailed {
		t.Fatalf("unexpected adapter error was not replaced with a fixed safe error: %v", err)
	}
	if code := clientRegistrationUIErrorCode(errClientRegistrationBackupStale); code != "backup_stale" {
		t.Fatalf("backup stale UI code = %q", code)
	}
}

func TestClientRegistrationBackupStoreValidatesPersistedStateAndDigest(t *testing.T) {
	storeInterface, err := newClientRegistrationBackupStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("create isolated backup store: %v", err)
	}
	store := storeInterface.(*clientRegistrationBackupStoreOnDisk)
	ctx := context.Background()
	original := clientRegistrationConfig{exists: true, bytes: []byte(`{"settings":"synthetic fixture"}`)}
	originalDigest := digestClientRegistrationConfig(original)
	receipt, err := store.save(ctx, mcpClientIDCodex, original, originalDigest)
	if err != nil {
		t.Fatalf("save synthetic backup: %v", err)
	}
	encodedReceipt, err := json.Marshal(receipt)
	if err != nil || strings.Contains(string(encodedReceipt), "synthetic fixture") {
		t.Fatalf("backup receipt contains settings content")
	}
	loaded, err := store.load(ctx, mcpClientIDCodex, receipt.ID)
	if err != nil || !sameClientRegistrationConfig(loaded.original, original) {
		t.Fatalf("backup round trip failed: err=%v", err)
	}
	desired := clientRegistrationConfig{exists: true, bytes: []byte(`{"settings":"registered fixture"}`)}
	desiredDigest := digestClientRegistrationConfig(desired)
	if err := store.prepareApply(ctx, mcpClientIDCodex, receipt.ID, desired, desiredDigest); err != nil {
		t.Fatalf("persist apply intent: %v", err)
	}
	if err := store.markApplied(ctx, mcpClientIDCodex, receipt.ID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	latest, err := store.findLatest(ctx, mcpClientIDCodex, originalDigest)
	if err != nil || latest.receipt.Status != mcpClientBackupApplied || !sameClientRegistrationConfig(latest.applied, desired) {
		t.Fatalf("apply state round trip failed: status=%q err=%v", latest.receipt.Status, err)
	}

	path := filepath.Join(store.path, mcpClientIDCodex+"-"+receipt.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synthetic record for tamper test: %v", err)
	}
	var envelope clientRegistrationBackupEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode synthetic record: %v", err)
	}
	envelope.OriginalBytes = []byte(`{"settings":"tampered fixture"}`)
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode tampered synthetic record: %v", err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("write tampered synthetic record: %v", err)
	}
	if _, err := store.load(ctx, mcpClientIDCodex, receipt.ID); !errors.Is(err, errClientRegistrationBackupNotFound) {
		t.Fatalf("tampered backup was accepted: %v", err)
	}
}

func TestClientRegistrationBackupRecoversDuringApplyingState(t *testing.T) {
	ctx := context.Background()
	original := []byte("synthetic original")
	desired := []byte("synthetic desired")
	adapter := newReadyFakeAdapter(mcpClientIDCodex, original, desired)
	controller := newFakeRegistrationController(t, adapter, nil, defaultClientRegistrationPlanTTL)
	store := controller.backupStore.(*clientRegistrationBackupStoreOnDisk)
	receipt, err := controller.BackupClientRegistration(ctx, mcpClientIDCodex)
	if err != nil {
		t.Fatalf("save synthetic backup: %v", err)
	}
	desiredConfig := clientRegistrationConfig{exists: true, bytes: desired}
	if err := store.prepareApply(ctx, mcpClientIDCodex, receipt.ID, desiredConfig, digestClientRegistrationConfig(desiredConfig)); err != nil {
		t.Fatalf("persist apply intent: %v", err)
	}
	// Model process interruption after the client replacement but before markApplied.
	adapter.config = cloneClientRegistrationConfig(desiredConfig)
	adapter.status.Registration = mcpClientRegistrationRegistered
	status, err := controller.RestoreClientRegistration(ctx, mcpClientIDCodex, receipt.ID)
	if err != nil {
		t.Fatalf("restore from applying state: %v", err)
	}
	if status.Backup != mcpClientBackupRestored || string(adapter.config.bytes) != string(original) {
		t.Fatalf("restore did not recover from persisted applying state")
	}
}

func TestClientRegistrationBackupRetentionPrunesOnlyRestoredRecords(t *testing.T) {
	storeInterface, err := newClientRegistrationBackupStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("create isolated backup store: %v", err)
	}
	store := storeInterface.(*clientRegistrationBackupStoreOnDisk)
	ctx := context.Background()
	original := clientRegistrationConfig{exists: true, bytes: []byte("synthetic old")}
	applied := clientRegistrationConfig{exists: true, bytes: []byte("synthetic new")}
	originalDigest := digestClientRegistrationConfig(original)
	appliedDigest := digestClientRegistrationConfig(applied)
	createdAt := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	for index := 0; index < maxClientRegistrationBackupRecords; index++ {
		id := fmt.Sprintf("receipt_%015d", index)
		record := clientRegistrationBackupRecord{
			clientID:       mcpClientIDCodex,
			receipt:        MCPClientBackupReceipt{ID: id, Status: mcpClientBackupRestored, CreatedAt: createdAt.Add(time.Duration(index) * time.Second)},
			original:       cloneClientRegistrationConfig(original),
			originalDigest: originalDigest,
			applied:        cloneClientRegistrationConfig(applied),
			appliedDigest:  appliedDigest,
			hasApplied:     true,
			restored:       true,
		}
		if err := store.writeRecord(record, true); err != nil {
			t.Fatalf("seed restored synthetic record %d: %v", index, err)
		}
	}
	activeID := "receipt_999999999999999"
	active := clientRegistrationBackupRecord{
		clientID:       mcpClientIDCodex,
		receipt:        MCPClientBackupReceipt{ID: activeID, Status: mcpClientBackupApplied, CreatedAt: createdAt.Add(time.Hour)},
		original:       cloneClientRegistrationConfig(original),
		originalDigest: originalDigest,
		applied:        cloneClientRegistrationConfig(applied),
		appliedDigest:  appliedDigest,
		hasApplied:     true,
	}
	if err := store.writeRecord(active, true); err != nil {
		t.Fatalf("seed active synthetic record: %v", err)
	}
	pendingPath := filepath.Join(store.path, ".pending-fixture.tmp")
	if err := os.WriteFile(pendingPath, []byte("abandoned synthetic write"), 0o600); err != nil {
		t.Fatalf("seed isolated pending file: %v", err)
	}
	if err := secureClientRegistrationBackupFile(pendingPath); err != nil {
		t.Fatalf("secure isolated pending file: %v", err)
	}
	oldTime := time.Now().Add(-2 * maxClientRegistrationPendingAge)
	if err := os.Chtimes(pendingPath, oldTime, oldTime); err != nil {
		t.Fatalf("age isolated pending file: %v", err)
	}
	newReceipt, err := store.save(ctx, mcpClientIDCodex, original, originalDigest)
	if err != nil {
		t.Fatalf("save after retention limit: %v", err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale pending file was retained: %v", err)
	}
	if _, err := store.load(ctx, mcpClientIDCodex, "receipt_000000000000000"); !errors.Is(err, errClientRegistrationBackupNotFound) {
		t.Fatalf("oldest restored record was retained: %v", err)
	}
	if _, err := store.load(ctx, mcpClientIDCodex, activeID); err != nil {
		t.Fatalf("active backup was pruned: %v", err)
	}
	if _, err := store.load(ctx, mcpClientIDCodex, newReceipt.ID); err != nil {
		t.Fatalf("new backup missing after pruning: %v", err)
	}
	entries, err := os.ReadDir(store.path)
	if err != nil {
		t.Fatalf("read isolated backup store: %v", err)
	}
	jsonCount := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			jsonCount++
		}
	}
	if jsonCount != maxClientRegistrationBackupRecords {
		t.Fatalf("backup record count = %d, want %d", jsonCount, maxClientRegistrationBackupRecords)
	}
}

func TestClientRegistrationBackupRejectsReparseStorePath(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	link := filepath.Join(parent, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("create isolated target directory: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create an isolated symlink on this host: %v", err)
	}
	if _, err := newClientRegistrationBackupStoreAt(link); err == nil {
		t.Fatalf("backup store accepted a symlink/reparse directory")
	}
}
