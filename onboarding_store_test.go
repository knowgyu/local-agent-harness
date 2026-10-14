package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOnboardingStoreDraftCRUDRestartAndCAS(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	first := newOnboardingStoreForTest(t, path, now)
	second := newOnboardingStoreForTest(t, path, now)
	ctx := context.Background()

	initial, err := first.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}
	if initial.Revision != 1 || !validOnboardingDraftID(initial.ID) {
		t.Fatalf("CreateDraft() = %#v, want revision 1 and generated onboarding ID", initial)
	}

	reopened := newOnboardingStoreForTest(t, path, now)
	got, err := reopened.GetDraft(ctx, initial.ID)
	if err != nil || got.ID != initial.ID || got.Revision != 1 || got.Fields != initial.Fields {
		t.Fatalf("GetDraft() after restart = %#v, %v", got, err)
	}

	updatedFields := onboardingStoreTestGitHubFields()
	updatedFields.Name = "Renamed repository"
	updated, err := second.UpdateDraft(ctx, initial.ID, 1, updatedFields)
	if err != nil || updated.Revision != 2 || updated.Check != nil {
		t.Fatalf("UpdateDraft() = %#v, %v; want revision 2 with no check", updated, err)
	}
	if _, err := first.UpdateDraft(ctx, initial.ID, 1, onboardingStoreTestGitHubFields()); !errors.Is(err, errOnboardingDraftStale) {
		t.Fatalf("stale UpdateDraft() error = %v, want errOnboardingDraftStale", err)
	}
	if err := reopened.DeleteDraft(ctx, initial.ID, 2); err != nil {
		t.Fatalf("DeleteDraft() = %v", err)
	}
	if _, err := first.GetDraft(ctx, initial.ID); !errors.Is(err, errOnboardingDraftNotFound) {
		t.Fatalf("GetDraft() after delete error = %v, want not found", err)
	}
}

func TestOnboardingStoreAcceptsNormalizedURLsAndIncompleteDrafts(t *testing.T) {
	services := []struct {
		name     string
		service  onboardingServiceKind
		complete onboardingDraftFields
	}{
		{
			name: "GitHub", service: onboardingServiceGitHub,
			complete: onboardingDraftFields{
				Name: "Repo", Origin: "https://github.example.invalid", Repository: "org/repo",
			},
		},
		{
			name: "Jenkins", service: onboardingServiceJenkins,
			complete: onboardingDraftFields{
				Name: "CI", BaseURL: "https://jenkins.example.invalid/ci", Username: "builder",
				JobPath: "team/build", Environment: "production",
			},
		},
		{
			name: "Dashboard", service: onboardingServiceDashboard,
			complete: onboardingDraftFields{Name: "Metrics", BaseURL: "https://metrics.example.invalid/dash"},
		},
	}
	for _, tc := range services {
		t.Run(tc.name, func(t *testing.T) {
			path := onboardingTestStorePath(t)
			store := newOnboardingStoreForTest(t, path, time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC))
			incomplete := onboardingDraftFields{}
			draft, err := store.CreateDraft(context.Background(), tc.service, tc.complete)
			if err != nil {
				t.Fatalf("CreateDraft(complete) error = %v", err)
			}
			updated, err := store.UpdateDraft(context.Background(), draft.ID, draft.Revision, incomplete)
			if err != nil || updated.Revision != 2 || updated.Fields != incomplete {
				t.Fatalf("UpdateDraft(incomplete) = %#v, %v", updated, err)
			}
			reopened := newOnboardingStoreForTest(t, path, time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC))
			persisted, err := reopened.GetDraft(context.Background(), draft.ID)
			if err != nil || persisted.Fields != incomplete {
				t.Fatalf("GetDraft(incomplete) = %#v, %v", persisted, err)
			}
			if tc.service == onboardingServiceJenkins && persisted.Fields.Username != "" {
				t.Fatalf("incomplete Jenkins draft username = %q, want empty", persisted.Fields.Username)
			}

			second, err := reopened.CreateDraft(context.Background(), tc.service, incomplete)
			if err != nil {
				t.Fatalf("CreateDraft(incomplete) error = %v", err)
			}
			if _, err := store.UpdateDraft(context.Background(), second.ID, second.Revision, tc.complete); err != nil {
				t.Fatalf("UpdateDraft(complete normalized URL) error = %v", err)
			}
		})
	}
}

func TestOnboardingStoreConcurrentInstancesUseRevisionCAS(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	first := newOnboardingStoreForTest(t, path, now)
	second := newOnboardingStoreForTest(t, path, now)
	ctx := context.Background()
	draft, err := first.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft() error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		fields := onboardingStoreTestGitHubFields()
		fields.Name = "Writer one"
		<-start
		_, updateErr := first.UpdateDraft(ctx, draft.ID, draft.Revision, fields)
		results <- updateErr
	}()
	go func() {
		defer workers.Done()
		fields := onboardingStoreTestGitHubFields()
		fields.Name = "Writer two"
		<-start
		_, updateErr := second.UpdateDraft(ctx, draft.ID, draft.Revision, fields)
		results <- updateErr
	}()
	close(start)
	workers.Wait()
	close(results)

	successes, stale := 0, 0
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, errOnboardingDraftStale):
			stale++
		default:
			t.Fatalf("concurrent update error = %v", result)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent updates: successes=%d stale=%d, want one each", successes, stale)
	}
	stored, err := first.GetDraft(ctx, draft.ID)
	if err != nil || stored.Revision != 2 || (stored.Fields.Name != "Writer one" && stored.Fields.Name != "Writer two") {
		t.Fatalf("final draft = %#v, %v", stored, err)
	}
}

func TestOnboardingStoreStartCheckSupersedesOlderConcurrentResults(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 15, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	first := newOnboardingStoreForTest(t, path, now)
	second := newOnboardingStoreForTest(t, path, now)
	draft, err := first.CreateDraft(context.Background(), onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft(): %v", err)
	}
	generation1, err := first.StartCheck(context.Background(), draft.ID, draft.Revision)
	if err != nil || generation1 != 1 {
		t.Fatalf("first StartCheck() = %d, %v; want generation 1", generation1, err)
	}
	generation2, err := second.StartCheck(context.Background(), draft.ID, draft.Revision)
	if err != nil || generation2 != 2 {
		t.Fatalf("second StartCheck() = %d, %v; want generation 2", generation2, err)
	}
	candidateDigest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	if err != nil {
		t.Fatalf("candidate digest: %v", err)
	}
	receipt := onboardingCheckReceipt{
		DraftID: draft.ID, DraftRevision: draft.Revision, CheckGeneration: generation1,
		CandidateDigest: candidateDigest, PolicyDigest: "policy:sha256:" + strings.Repeat("a", 64),
		ConfigRevision: "config:sha256:" + strings.Repeat("b", 64),
		Outcome:        onboardingCheckSucceeded, CheckedAt: now,
	}
	if err := first.SaveCheckReceipt(context.Background(), receipt); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("older check receipt save error = %v, want check required", err)
	}
	current, err := second.GetDraft(context.Background(), draft.ID)
	if err != nil || current.CheckGeneration != generation2 || current.Check != nil {
		t.Fatalf("draft after concurrent StartCheck = %#v, %v", current, err)
	}
	receipt.CheckGeneration = generation2
	if err := second.SaveCheckReceipt(context.Background(), receipt); err != nil {
		t.Fatalf("latest check receipt save: %v", err)
	}
	current, err = first.GetDraft(context.Background(), draft.ID)
	if err != nil || current.Check == nil || current.Check.CheckGeneration != generation2 {
		t.Fatalf("draft after latest check = %#v, %v", current, err)
	}
	generation3, err := first.StartCheck(context.Background(), draft.ID, draft.Revision)
	if err != nil || generation3 != 3 {
		t.Fatalf("third StartCheck() = %d, %v; want generation 3", generation3, err)
	}
	if err := second.SaveCheckReceipt(context.Background(), receipt); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("superseded receipt save error = %v, want check required", err)
	}
	current, err = second.GetDraft(context.Background(), draft.ID)
	if err != nil || current.CheckGeneration != generation3 || current.Check != nil {
		t.Fatalf("draft after superseding check = %#v, %v", current, err)
	}
}

func TestOnboardingStoreStartCheckRejectsGenerationOverflow(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 20, 0, 0, time.UTC)
	store := newOnboardingStoreForTest(t, onboardingTestStorePath(t), now)
	draft, err := store.CreateDraft(context.Background(), onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft(): %v", err)
	}
	err = withOnboardingStoreLock(store.path, func() error {
		envelope, readErr := store.readLocked()
		if readErr != nil {
			return readErr
		}
		envelope.Drafts[0].CheckGeneration = ^uint64(0)
		return store.writeLocked(envelope)
	})
	if err != nil {
		t.Fatalf("persist overflow fixture: %v", err)
	}
	if _, err := store.StartCheck(context.Background(), draft.ID, draft.Revision); !errors.Is(err, errOnboardingCapacity) {
		t.Fatalf("StartCheck() at max generation error = %v, want capacity", err)
	}
	stored, err := store.GetDraft(context.Background(), draft.ID)
	if err != nil || stored.CheckGeneration != ^uint64(0) {
		t.Fatalf("draft after overflow StartCheck = %#v, %v", stored, err)
	}
}

func TestOnboardingStoreRejectsCorruptSidecarWithoutReset(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	if _, err := store.ListDrafts(context.Background()); err != nil {
		t.Fatalf("initialize store directory: %v", err)
	}
	corrupt := []byte(`{"version":1,"version":1,"drafts":[],"pending_activations":[],"activations":[],"first_reads":[]}`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}
	if _, err := store.ListDrafts(context.Background()); !errors.Is(err, errOnboardingUnavailable) {
		t.Fatalf("ListDrafts() on corrupt sidecar error = %v, want unavailable", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(corrupt) {
		t.Fatalf("corrupt sidecar was changed or removed: read error=%v bytes=%q", err, after)
	}
}

func TestOnboardingStoreAtomicWriteFailureLeavesNoDraft(t *testing.T) {
	now := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	const privateFailure = "atomic-write-private-canary"
	store.writeFileAtomically = func(string, []byte) error { return errors.New(privateFailure) }
	_, err := store.CreateDraft(context.Background(), onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if !errors.Is(err, errOnboardingUnavailable) || strings.Contains(err.Error(), privateFailure) || strings.Contains(err.Error(), path) {
		t.Fatalf("CreateDraft() error = %v, want fixed safe unavailable error", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("atomic failure left sidecar file: %v", statErr)
	}
	reopened := newOnboardingStoreForTest(t, path, now)
	drafts, err := reopened.ListDrafts(context.Background())
	if err != nil || len(drafts) != 0 {
		t.Fatalf("ListDrafts() after failed write = %#v, %v", drafts, err)
	}
}

func TestOnboardingStoreRejectsSecretAndRawURLFields(t *testing.T) {
	now := time.Date(2026, 10, 11, 11, 30, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	secret := "github_pat_store_canary_0123456789ABCDEF"
	cases := []struct {
		name    string
		service onboardingServiceKind
		fields  onboardingDraftFields
	}{
		{name: "token in name", service: onboardingServiceGitHub, fields: onboardingDraftFields{Name: secret}},
		{name: "query in base URL", service: onboardingServiceDashboard, fields: onboardingDraftFields{BaseURL: "https://dashboard.example.invalid/proxy?token=" + secret}},
		{name: "credential reference in username", service: onboardingServiceJenkins, fields: onboardingDraftFields{Username: "cred:0123456789abcdef0123456789abcdef"}},
		{name: "pasted URL in repository", service: onboardingServiceGitHub, fields: onboardingDraftFields{Repository: "https://github.example.invalid/org/repo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.CreateDraft(context.Background(), tc.service, tc.fields)
			if !errors.Is(err, errOnboardingInvalid) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), path) {
				t.Fatalf("CreateDraft() error = %v, want fixed invalid error", err)
			}
		})
	}
	drafts, err := store.ListDrafts(context.Background())
	if err != nil || len(drafts) != 0 {
		t.Fatalf("rejected fields persisted: drafts=%#v error=%v", drafts, err)
	}
}

func TestOnboardingStoreConfigPathNamespaceAndConfigIsolation(t *testing.T) {
	now := time.Now().UTC()
	configDir := t.TempDir()
	firstConfig := filepath.Join(configDir, "config.json")
	secondConfig := filepath.Join(configDir, "alternate-config.json")
	firstBytes := []byte("active-config-one-canary")
	secondBytes := []byte("active-config-two-canary")
	if err := os.WriteFile(firstConfig, firstBytes, 0o600); err != nil {
		t.Fatalf("write first config fixture: %v", err)
	}
	if err := os.WriteFile(secondConfig, secondBytes, 0o600); err != nil {
		t.Fatalf("write second config fixture: %v", err)
	}

	first, err := newOnboardingDraftStoreForConfigPath(firstConfig)
	if err != nil {
		t.Fatalf("first config store: %v", err)
	}
	second, err := newOnboardingDraftStoreForConfigPath(secondConfig)
	if err != nil {
		t.Fatalf("second config store: %v", err)
	}
	firstStore := first.(*onboardingFileDraftStore)
	secondStore := second.(*onboardingFileDraftStore)
	if firstStore.path == secondStore.path || filepath.Base(firstStore.path) != onboardingStoreFileName || filepath.Base(secondStore.path) != onboardingStoreFileName {
		t.Fatalf("selected config paths did not get distinct sidecars: %q %q", firstStore.path, secondStore.path)
	}
	if len(filepath.Base(filepath.Dir(firstStore.path))) != 64 {
		t.Fatalf("sidecar path lacks bounded config namespace: %q", firstStore.path)
	}
	for _, r := range filepath.Base(filepath.Dir(firstStore.path)) {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("sidecar config namespace is not lowercase hex: %q", firstStore.path)
		}
	}

	ctx := context.Background()
	created, err := first.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft() on first selected config: %v", err)
	}
	secondDrafts, err := second.ListDrafts(ctx)
	if err != nil || len(secondDrafts) != 0 {
		t.Fatalf("second selected config saw first config drafts: %#v, %v", secondDrafts, err)
	}
	firstRead := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub,
		Tool: "github_repository", ObservedAt: now,
		DefinitionDigest: "sha256:" + strings.Repeat("d", 64),
	}
	if added, recordErr := first.RecordFirstRead(ctx, firstRead); recordErr != nil || !added {
		t.Fatalf("RecordFirstRead() = %v, %v", added, recordErr)
	}
	secondReceipts, err := second.ListFirstReads(ctx)
	if err != nil || len(secondReceipts) != 0 {
		t.Fatalf("second selected config saw first config receipts: %#v, %v", secondReceipts, err)
	}
	reopened, err := newOnboardingDraftStoreForConfigPath(firstConfig)
	if err != nil {
		t.Fatalf("reopen first selected config: %v", err)
	}
	reopenedDrafts, err := reopened.ListDrafts(ctx)
	if err != nil || len(reopenedDrafts) != 1 || reopenedDrafts[0].ID != created.ID {
		t.Fatalf("same selected config did not reopen its draft: %#v, %v", reopenedDrafts, err)
	}
	reopenedReceipts, err := reopened.ListFirstReads(ctx)
	if err != nil || len(reopenedReceipts) != 1 || reopenedReceipts[0] != firstRead {
		t.Fatalf("same selected config did not reopen its receipt: %#v, %v", reopenedReceipts, err)
	}
	if err := first.DeleteDraft(ctx, created.ID, created.Revision); err != nil {
		t.Fatalf("DeleteDraft(): %v", err)
	}
	afterFirst, err := os.ReadFile(firstConfig)
	if err != nil || string(afterFirst) != string(firstBytes) {
		t.Fatalf("draft CRUD changed active config one: %q, %v", afterFirst, err)
	}
	afterSecond, err := os.ReadFile(secondConfig)
	if err != nil || string(afterSecond) != string(secondBytes) {
		t.Fatalf("draft CRUD changed active config two: %q, %v", afterSecond, err)
	}
}

func TestOnboardingStoreFirstReadReceiptPersistsAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	reopened := newOnboardingStoreForTest(t, path, now)
	receipt := onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceHarbor,
		Tool: "harbor_repository_artifacts", ObservedAt: now,
		DefinitionDigest: "sha256:" + strings.Repeat("c", 64),
	}
	added, err := store.RecordFirstRead(context.Background(), receipt)
	if err != nil || !added {
		t.Fatalf("RecordFirstRead() = %v, %v; want inserted receipt", added, err)
	}
	duplicate := receipt
	duplicate.ObservedAt = now.Add(-time.Minute)
	added, err = reopened.RecordFirstRead(context.Background(), duplicate)
	if err != nil || added {
		t.Fatalf("duplicate RecordFirstRead() = %v, %v; want no change", added, err)
	}
	receipts, err := reopened.ListFirstReads(context.Background())
	if err != nil || len(receipts) != 1 || receipts[0] != receipt {
		t.Fatalf("ListFirstReads() = %#v, %v; want original persisted receipt", receipts, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	for _, forbidden := range []string{"target_id", "target_name", "response", "payload"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("sidecar contains forbidden first-read field %q: %s", forbidden, data)
		}
	}
}

func TestOnboardingStoreFirstReadRetentionKeepsNewestDistinctReceipts(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	now := base.Add(time.Hour)
	store := newOnboardingStoreForTest(t, onboardingTestStorePath(t), now)
	envelope := emptyOnboardingStoreEnvelope()
	for i := 0; i < onboardingStoreMaxFirstReads; i++ {
		envelope.FirstReads = append(envelope.FirstReads, onboardingStoreTestFirstReadReceipt(i, base.Add(time.Duration(i)*time.Second)))
	}
	if err := withOnboardingStoreLock(store.path, func() error { return store.writeLocked(envelope) }); err != nil {
		t.Fatalf("write full valid receipt history: %v", err)
	}

	latest := onboardingStoreTestFirstReadReceipt(onboardingStoreMaxFirstReads, base.Add(time.Duration(onboardingStoreMaxFirstReads)*time.Second))
	if added, err := store.RecordFirstRead(context.Background(), latest); err != nil || !added {
		t.Fatalf("RecordFirstRead() at capacity = %v, %v; want new receipt retained", added, err)
	}
	receipts, err := store.ListFirstReads(context.Background())
	if err != nil || len(receipts) != onboardingStoreMaxFirstReads {
		t.Fatalf("ListFirstReads() = %d receipts, %v; want bounded size %d", len(receipts), err, onboardingStoreMaxFirstReads)
	}
	oldestRetained := onboardingStoreTestFirstReadReceipt(1, base.Add(time.Second))
	if receipts[0] != oldestRetained || receipts[len(receipts)-1] != latest {
		t.Fatalf("retained receipts range = [%+v, %+v]; want oldest %+v and newest %+v", receipts[0], receipts[len(receipts)-1], oldestRetained, latest)
	}

	duplicate := oldestRetained
	duplicate.ObservedAt = now.Add(-time.Second)
	if added, err := store.RecordFirstRead(context.Background(), duplicate); err != nil || added {
		t.Fatalf("duplicate RecordFirstRead() = %v, %v; want no insertion or refresh", added, err)
	}
	receipts, err = store.ListFirstReads(context.Background())
	if err != nil || len(receipts) != onboardingStoreMaxFirstReads {
		t.Fatalf("duplicate changed retained history size: len=%d err=%v", len(receipts), err)
	}
	if receipts[0] != oldestRetained || receipts[len(receipts)-1] != latest {
		t.Fatalf("duplicate changed retained history: len=%d first=%+v last=%+v err=%v", len(receipts), receipts[0], receipts[len(receipts)-1], err)
	}
}

func TestOnboardingStoreRejectsOverLimitExistingFirstReadHistory(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	store := newOnboardingStoreForTest(t, onboardingTestStorePath(t), base.Add(time.Hour))
	envelope := emptyOnboardingStoreEnvelope()
	for i := 0; i <= onboardingStoreMaxFirstReads; i++ {
		envelope.FirstReads = append(envelope.FirstReads, onboardingStoreTestFirstReadReceipt(i, base.Add(time.Duration(i)*time.Second)))
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal over-limit sidecar: %v", err)
	}
	if err := withOnboardingStoreLock(store.path, func() error {
		return writeOnboardingStoreFileAtomically(store.path, data)
	}); err != nil {
		t.Fatalf("write over-limit sidecar: %v", err)
	}
	if receipts, err := store.ListFirstReads(context.Background()); err == nil || receipts != nil {
		t.Fatalf("ListFirstReads() on over-limit sidecar = %#v, %v; want unavailable", receipts, err)
	}
	after, err := os.ReadFile(store.path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("over-limit sidecar was changed: read error=%v", err)
	}
}

func onboardingStoreTestFirstReadReceipt(index int, observedAt time.Time) onboardingFirstReadReceipt {
	return onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceGitHub, Tool: "github_repository",
		ObservedAt: observedAt, DefinitionDigest: fmt.Sprintf("sha256:%064x", index),
	}
}

func TestOnboardingStoreActivationRecoveryReusesTargetID(t *testing.T) {
	now := time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	ctx := context.Background()
	draft, err := store.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft(): %v", err)
	}
	candidateDigest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	if err != nil {
		t.Fatalf("candidate digest: %v", err)
	}
	policyDigest := "policy:sha256:" + strings.Repeat("a", 64)
	configRevisionA := "config:sha256:" + strings.Repeat("b", 64)
	configRevisionB := "config:sha256:" + strings.Repeat("c", 64)
	generationA, err := store.StartCheck(ctx, draft.ID, draft.Revision)
	if err != nil {
		t.Fatalf("StartCheck(): %v", err)
	}
	receipt := onboardingCheckReceipt{
		DraftID: draft.ID, DraftRevision: draft.Revision, CheckGeneration: generationA, CandidateDigest: candidateDigest,
		PolicyDigest: policyDigest, ConfigRevision: configRevisionA,
		Outcome: onboardingCheckSucceeded, CheckedAt: now,
	}
	if err := store.SaveCheckReceipt(ctx, receipt); err != nil {
		t.Fatalf("SaveCheckReceipt(): %v", err)
	}

	writes := 0
	store.writeFileAtomically = func(path string, data []byte) error {
		writes++
		if writes == 2 {
			return errors.New("private atomic replace failure")
		}
		return writeOnboardingStoreFileAtomically(path, data)
	}
	casA := onboardingActivationCAS{
		DraftID: draft.ID, ExpectedRevision: draft.Revision, CheckGeneration: generationA, CandidateDigest: candidateDigest,
		PolicyDigest: policyDigest, ConfigRevision: configRevisionA,
	}
	var firstTargetID string
	applyCalls := 0
	_, err = store.WithActivation(ctx, casA, func(applyDraft onboardingDraft, targetID string) error {
		applyCalls++
		if applyDraft.ActivationRetry {
			t.Fatal("new reservation must not be marked as a retry")
		}
		firstTargetID = targetID
		return nil
	})
	if !errors.Is(err, errOnboardingUnavailable) || applyCalls != 1 || !validOnboardingTargetID(draft.Service, firstTargetID) {
		t.Fatalf("first WithActivation() error=%v calls=%d target=%q", err, applyCalls, firstTargetID)
	}

	store.writeFileAtomically = nil
	generationB, err := store.StartCheck(ctx, draft.ID, draft.Revision)
	if err != nil {
		t.Fatalf("StartCheck() for activation recovery: %v", err)
	}
	receipt.CheckGeneration = generationB
	receipt.ConfigRevision = configRevisionB
	if err := store.SaveCheckReceipt(ctx, receipt); err != nil {
		t.Fatalf("save current config receipt for recovery: %v", err)
	}
	casB := casA
	casB.CheckGeneration = generationB
	casB.ConfigRevision = configRevisionB
	record, err := store.WithActivation(ctx, casB, func(applyDraft onboardingDraft, targetID string) error {
		applyCalls++
		if !applyDraft.ActivationRetry {
			t.Fatal("reused pending reservation must be marked as a retry")
		}
		if targetID != firstTargetID {
			t.Fatalf("recovery target ID = %q, want reserved %q", targetID, firstTargetID)
		}
		return nil
	})
	if err != nil || record.TargetID != firstTargetID || applyCalls != 2 || record.ConfigRevision != configRevisionB {
		t.Fatalf("recovery WithActivation() = %#v, %v calls=%d", record, err, applyCalls)
	}
	replayed, err := store.WithActivation(ctx, casB, func(onboardingDraft, string) error {
		applyCalls++
		return nil
	})
	if err != nil || replayed != record || applyCalls != 2 {
		t.Fatalf("completed activation replay = %#v, %v calls=%d", replayed, err, applyCalls)
	}
	wrongRevisionCAS := casB
	wrongRevisionCAS.ExpectedRevision++
	if _, err := store.WithActivation(ctx, wrongRevisionCAS, func(onboardingDraft, string) error {
		t.Fatal("wrong-revision replay must not invoke activation callback")
		return nil
	}); !errors.Is(err, errOnboardingReviewChanged) {
		t.Fatalf("completed activation with wrong revision error = %v, want review changed", err)
	}
	if _, err := store.GetDraft(ctx, draft.ID); !errors.Is(err, errOnboardingDraftNotFound) {
		t.Fatalf("activated draft remains available: %v", err)
	}
}

func TestOnboardingStoreReleasesOnlyProvenUncommittedActivation(t *testing.T) {
	now := time.Date(2026, 10, 11, 13, 30, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	ctx := context.Background()
	draft, err := store.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft(): %v", err)
	}
	candidateDigest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	if err != nil {
		t.Fatalf("candidate digest: %v", err)
	}
	policyDigest := "policy:sha256:" + strings.Repeat("a", 64)
	configRevision := "config:sha256:" + strings.Repeat("b", 64)
	generation, err := store.StartCheck(ctx, draft.ID, draft.Revision)
	if err != nil {
		t.Fatalf("StartCheck(): %v", err)
	}
	if err := store.SaveCheckReceipt(ctx, onboardingCheckReceipt{
		DraftID: draft.ID, DraftRevision: draft.Revision, CheckGeneration: generation, CandidateDigest: candidateDigest,
		PolicyDigest: policyDigest, ConfigRevision: configRevision,
		Outcome: onboardingCheckSucceeded, CheckedAt: now,
	}); err != nil {
		t.Fatalf("SaveCheckReceipt(): %v", err)
	}
	cas := onboardingActivationCAS{
		DraftID: draft.ID, ExpectedRevision: draft.Revision, CheckGeneration: generation, CandidateDigest: candidateDigest,
		PolicyDigest: policyDigest, ConfigRevision: configRevision,
	}
	_, err = store.WithActivation(ctx, cas, func(onboardingDraft, string) error {
		return errors.Join(errOnboardingActivationNotCommitted, errOnboardingInvalid)
	})
	if !errors.Is(err, errOnboardingUnavailable) {
		t.Fatalf("WithActivation() error = %v, want fixed safe unavailable error", err)
	}
	if _, err := store.GetDraft(ctx, draft.ID); err != nil {
		t.Fatalf("GetDraft() after proven pre-commit failure: %v", err)
	}
	if err := store.DeleteDraft(ctx, draft.ID, draft.Revision); err != nil {
		t.Fatalf("DeleteDraft() after released reservation: %v", err)
	}
}

func TestOnboardingStoreAmbiguousActivationRequiresManualRecoveryAfterRestart(t *testing.T) {
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	path := onboardingTestStorePath(t)
	store := newOnboardingStoreForTest(t, path, now)
	ctx := context.Background()
	draft, err := store.CreateDraft(ctx, onboardingServiceGitHub, onboardingStoreTestGitHubFields())
	if err != nil {
		t.Fatalf("CreateDraft(): %v", err)
	}
	candidateDigest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	if err != nil {
		t.Fatalf("candidate digest: %v", err)
	}
	policyDigest := "policy:sha256:" + strings.Repeat("a", 64)
	configRevision := "config:sha256:" + strings.Repeat("b", 64)
	generation, err := store.StartCheck(ctx, draft.ID, draft.Revision)
	if err != nil {
		t.Fatalf("StartCheck(): %v", err)
	}
	receipt := onboardingCheckReceipt{
		DraftID: draft.ID, DraftRevision: draft.Revision, CheckGeneration: generation,
		CandidateDigest: candidateDigest, PolicyDigest: policyDigest, ConfigRevision: configRevision,
		Outcome: onboardingCheckSucceeded, CheckedAt: now,
	}
	if err := store.SaveCheckReceipt(ctx, receipt); err != nil {
		t.Fatalf("SaveCheckReceipt(): %v", err)
	}
	cas := onboardingActivationCAS{
		DraftID: draft.ID, ExpectedRevision: draft.Revision, CheckGeneration: generation,
		CandidateDigest: candidateDigest, PolicyDigest: policyDigest, ConfigRevision: configRevision,
	}

	// Model the save helper creating one vault entry before reporting the
	// explicit ambiguous outcome. Config is unchanged because its write failed.
	configTargets := []string{}
	vaultReferences := []string{}
	applyCalls := 0
	_, err = store.WithActivation(ctx, cas, func(applyDraft onboardingDraft, _ string) error {
		applyCalls++
		if applyDraft.ActivationRetry {
			t.Fatal("new reservation must not be marked as a retry")
		}
		vaultReferences = append(vaultReferences, "synthetic-secret-ref")
		return errors.Join(errOnboardingActivationAmbiguous, errOnboardingActivationPending)
	})
	if !errors.Is(err, errOnboardingActivationRecoveryRequired) || applyCalls != 1 {
		t.Fatalf("ambiguous WithActivation() error=%v calls=%d", err, applyCalls)
	}
	if len(configTargets) != 0 || len(vaultReferences) != 1 {
		t.Fatalf("after ambiguous save: config targets=%v vault references=%v", configTargets, vaultReferences)
	}

	listed, err := store.ListDrafts(ctx)
	if err != nil || len(listed) != 1 || !listed[0].ActivationRecoveryRequired {
		t.Fatalf("ListDrafts() recovery state = %#v, %v", listed, err)
	}
	loaded, err := store.GetDraft(ctx, draft.ID)
	if err != nil || !loaded.ActivationRecoveryRequired {
		t.Fatalf("GetDraft() recovery state = %#v, %v", loaded, err)
	}
	envelope, err := store.readForTest(ctx)
	if err != nil || len(envelope.PendingActivations) != 1 || !envelope.PendingActivations[0].RecoveryRequired {
		t.Fatalf("pending recovery state = %#v, %v", envelope.PendingActivations, err)
	}
	serialized, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if strings.Contains(string(serialized), "synthetic-secret-ref") {
		t.Fatalf("sidecar persisted a secret reference: %s", serialized)
	}
	if !strings.Contains(string(serialized), `"recovery_required":true`) ||
		strings.Contains(string(serialized), `"activation_recovery_required"`) ||
		strings.Contains(string(serialized), `"activation_retry"`) {
		t.Fatalf("sidecar recovery marker is missing or duplicated on draft: %s", serialized)
	}

	if _, err := store.UpdateDraft(ctx, draft.ID, draft.Revision, draft.Fields); !errors.Is(err, errOnboardingActivationPending) {
		t.Fatalf("UpdateDraft() while ambiguous = %v, want pending", err)
	}
	if err := store.DeleteDraft(ctx, draft.ID, draft.Revision); !errors.Is(err, errOnboardingActivationPending) {
		t.Fatalf("DeleteDraft() while ambiguous = %v, want pending", err)
	}

	// A newly observed check receipt may update the current config snapshot, but
	// it cannot erase the durable manual-recovery gate.
	configRevisionB := "config:sha256:" + strings.Repeat("c", 64)
	receipt.ConfigRevision = configRevisionB
	if err := store.SaveCheckReceipt(ctx, receipt); err != nil {
		t.Fatalf("SaveCheckReceipt() while ambiguous: %v", err)
	}

	restarted := newOnboardingStoreForTest(t, path, now)
	if generation, err := restarted.StartCheck(ctx, draft.ID, draft.Revision); !errors.Is(err, errOnboardingActivationRecoveryRequired) || generation != 0 {
		t.Fatalf("StartCheck() after restart = generation %d, error %v", generation, err)
	}
	restartedDraft, err := restarted.GetDraft(ctx, draft.ID)
	if err != nil || !restartedDraft.ActivationRecoveryRequired {
		t.Fatalf("GetDraft() after restart recovery state = %#v, %v", restartedDraft, err)
	}
	cas.ConfigRevision = configRevisionB
	_, err = restarted.WithActivation(ctx, cas, func(applyDraft onboardingDraft, _ string) error {
		if !applyDraft.ActivationRetry {
			t.Fatal("persisted pending reservation must be marked as a retry")
		}
		applyCalls++
		vaultReferences = append(vaultReferences, "second-synthetic-secret-ref")
		configTargets = append(configTargets, "should-not-commit")
		return nil
	})
	if !errors.Is(err, errOnboardingActivationRecoveryRequired) || applyCalls != 1 {
		t.Fatalf("WithActivation() after restart = %v calls=%d, want recovery-required and no retry", err, applyCalls)
	}
	if len(configTargets) != 0 || len(vaultReferences) != 1 {
		t.Fatalf("retry changed config/vault: config targets=%v vault references=%v", configTargets, vaultReferences)
	}
}

func newOnboardingStoreForTest(t *testing.T, path string, now time.Time) *onboardingFileDraftStore {
	t.Helper()
	store, err := newOnboardingFileDraftStoreForTest(path, func() time.Time { return now.UTC() })
	if err != nil {
		t.Fatalf("new onboarding test store: %v", err)
	}
	return store
}

func onboardingTestStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), onboardingStoreDirectoryName, strings.Repeat("a", 64), onboardingStoreFileName)
}

func (s *onboardingFileDraftStore) readForTest(ctx context.Context) (onboardingStoreEnvelope, error) {
	if err := s.validateCall(ctx); err != nil {
		return onboardingStoreEnvelope{}, err
	}
	var envelope onboardingStoreEnvelope
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		envelope, err = s.readLocked()
		return err
	})
	if err != nil {
		return onboardingStoreEnvelope{}, s.publicError(err)
	}
	return envelope, nil
}

func onboardingStoreTestGitHubFields() onboardingDraftFields {
	return onboardingDraftFields{
		Name: "Example repository", Origin: "https://github.example.invalid", Repository: "team/repository",
	}
}
