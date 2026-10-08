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

type fakeSetupDraftStore struct {
	mu       sync.Mutex
	snapshot setupDraftConfigSnapshot
	loadErr  error
	saveErr  error
	loads    int
	writes   int
}

type fakeSetupDraftQueueStore struct {
	mu       sync.Mutex
	entries  map[string]SetupDraftReview
	claimErr error
}

func (q *fakeSetupDraftQueueStore) Load(now time.Time) ([]SetupDraftReview, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	reviews := make([]SetupDraftReview, 0, len(q.entries))
	for _, review := range q.entries {
		if now.Before(review.ExpiresAt) {
			reviews = append(reviews, cloneSetupDraftReview(review))
		}
	}
	return reviews, nil
}

func (q *fakeSetupDraftQueueStore) Put(review SetupDraftReview) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.entries == nil {
		q.entries = make(map[string]SetupDraftReview)
	}
	if _, exists := q.entries[review.ID]; exists {
		return errSetupDraftQueueInvalid
	}
	if len(q.entries) >= setupDraftMaximumPending {
		return errSetupDraftQueueCapacity
	}
	q.entries[review.ID] = cloneSetupDraftReview(review)
	return nil
}

func (q *fakeSetupDraftQueueStore) Delete(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.entries, id)
	return nil
}

func (q *fakeSetupDraftQueueStore) WithReview(id string, now time.Time, fn func(SetupDraftReview) (bool, error)) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	review, ok := q.entries[id]
	if !ok {
		return errSetupDraftQueueReviewMissing
	}
	if !now.Before(review.ExpiresAt) {
		return errSetupDraftQueueReviewExpired
	}
	consume, err := fn(cloneSetupDraftReview(review))
	if err != nil {
		return err
	}
	if consume {
		delete(q.entries, id)
	}
	return nil
}

func (q *fakeSetupDraftQueueStore) ClaimReview(id, baseRevision, digest string, now time.Time, validate func(SetupDraftReview) error) (SetupDraftReview, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	review, ok := q.entries[id]
	if !ok {
		return SetupDraftReview{}, errSetupDraftQueueReviewMissing
	}
	if !now.Before(review.ExpiresAt) {
		return SetupDraftReview{}, errSetupDraftQueueReviewExpired
	}
	if review.BaseRevision != baseRevision || review.Digest != digest {
		return SetupDraftReview{}, errSetupDraftQueueReviewMismatch
	}
	if q.claimErr != nil {
		return SetupDraftReview{}, q.claimErr
	}
	review = cloneSetupDraftReview(review)
	if err := validate(review); err != nil {
		return SetupDraftReview{}, err
	}
	delete(q.entries, id)
	return review, nil
}

func TestSetupDraftReviewDigestBindsIDAndExpiry(t *testing.T) {
	review := SetupDraftReview{
		ID:           "setupdraft:11111111111111111111111111111111",
		BaseRevision: "revision-one",
		ExpiresAt:    time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC),
		Sources:      []SetupDraftSource{{Kind: "note", Label: "reviewed source", Confidence: "high"}},
		Missing:      []string{},
		Changes:      []SetupDraftChange{{Kind: "ssh_target_add", Name: "Build host", Summary: "Add host", Scope: []string{}, PermissionIncrease: []string{}}},
		Proposal:     SetupDraftSubmission{BaseRevision: "revision-one", Sources: []SetupDraftSource{{Kind: "note", Label: "reviewed source", Confidence: "high"}}},
	}
	base := setupDraftReviewDigest(review)
	if base == "" {
		t.Fatal("review digest was empty")
	}
	expiryChanged := review
	expiryChanged.ExpiresAt = expiryChanged.ExpiresAt.Add(time.Second)
	if setupDraftReviewDigest(expiryChanged) == base {
		t.Fatal("changing expiry did not change the review digest")
	}
	idChanged := review
	idChanged.ID = "setupdraft:22222222222222222222222222222222"
	if setupDraftReviewDigest(idChanged) == base {
		t.Fatal("changing the draft ID did not change the review digest")
	}
}

func (s *fakeSetupDraftStore) Snapshot() (setupDraftConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return setupDraftConfigSnapshot{}, s.loadErr
	}
	return cloneSetupDraftSnapshot(s.snapshot), nil
}

func (s *fakeSetupDraftStore) CompareAndSwap(expectedRevision string, next setupDraftState) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.saveErr != nil {
		return "", s.saveErr
	}
	if s.snapshot.Revision != expectedRevision {
		return "", errSetupDraftStale
	}
	newRevision := setupDraftStateRevision(next)
	if !validSetupDraftRevision(newRevision) || newRevision == expectedRevision {
		return "", errSetupDraftStore
	}
	s.snapshot.State = cloneSetupDraftState(next)
	s.snapshot.Revision = newRevision
	return newRevision, nil
}

func newTestSetupDraftController(store *fakeSetupDraftStore) *setupDraftController {
	controller := newSetupDraftControllerWithQueue(store, &fakeSetupDraftQueueStore{})
	counter := 0
	controller.newDraftID = func() (string, error) {
		counter++
		return fmt.Sprintf("setupdraft:%032x", counter), nil
	}
	return controller
}

func TestSetupDraftControllerRestoresQueueAndReplansBeforeApproval(t *testing.T) {
	store := newTestSetupDraftStore()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	controller := newSetupDraftControllerWithQueue(store, queue)
	controller.now = func() time.Time { return now }
	controller.newDraftID = func() (string, error) { return "setupdraft:" + strings.Repeat("a", 32), nil }
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restarted := newSetupDraftControllerWithQueue(store, reopened)
	restarted.now = func() time.Time { return now }
	if got, err := restarted.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); err != nil || got.Digest != review.Digest {
		t.Fatalf("restart did not recover the exact reviewed proposal: digest=%q err=%v", got.Digest, err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildSetupDraftPlan(snapshot, review.Proposal, setupDraftResourceIDGenerator(review.ID))
	if err != nil || !sameSetupDraftReviewPlan(review, plan) {
		reviewChanges, _ := json.Marshal(review.Changes)
		planChanges, _ := json.Marshal(plan.changes)
		reviewMissing, _ := json.Marshal(review.Missing)
		planMissing, _ := json.Marshal(plan.missing)
		t.Fatalf("replanned review diverged: changes review=%s plan=%s missing review=%s plan=%s err=%v", reviewChanges, planChanges, reviewMissing, planMissing, err)
	}
	result, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !result.Applied || len(store.snapshot.State.Connections) != 1 {
		t.Fatalf("restarted controller did not replan and CAS the reviewed proposal: result=%#v state=%#v err=%v", result, store.snapshot.State, err)
	}
	if _, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("replayed persisted review was accepted: %v", err)
	}
	remaining, err := reopened.Load(now)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("approved queue entry remained after cleanup: count=%d err=%v", len(remaining), err)
	}
}

func TestSetupDraftDurableSSHOnlyProposalRoundTripsAndApproves(t *testing.T) {
	store := newTestSetupDraftStore()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	controller := newSetupDraftControllerWithQueue(store, queue)
	controller.now = func() time.Time { return now }
	controller.newDraftID = func() (string, error) {
		return "setupdraft:" + strings.Repeat("d", 32), nil
	}
	submission := SetupDraftSubmission{
		BaseRevision: store.snapshot.Revision,
		Sources:      []SetupDraftSource{{Kind: "user_provided", Label: "Q01 synthetic inventory scope", Confidence: "high"}},
		SSHTargets: []SetupDraftSSHTarget{{
			Name: "Q01 synthetic target", Alias: "q01-synthetic-alias",
			Operations: []SSHOperationDefinition{{
				ID: "synthetic_inventory", Name: "Synthetic inventory", Summary: "Read one synthetic inventory.",
				Program: "synthetic-tool", FixedArgs: []string{"inventory"}, Risk: SSHRiskReadOnly,
				Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
			}},
		}},
	}
	review, err := controller.SubmitSetupDraft(context.Background(), submission)
	if err != nil {
		t.Fatalf("SSH-only durable draft submission failed: %v", err)
	}
	if store.writes != 0 {
		t.Fatalf("submitting an SSH-only draft wrote settings: %d", store.writes)
	}

	reopened, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restarted := newSetupDraftControllerWithQueue(store, reopened)
	restarted.now = func() time.Time { return now }
	loaded, err := restarted.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || len(loaded.Proposal.SSHTargets) != 1 || loaded.Proposal.SSHTargets[0].Operations[0].Revision == "" {
		t.Fatalf("SSH-only durable draft did not round-trip canonically: proposal=%#v err=%v", loaded.Proposal, err)
	}
	result, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !result.Applied || store.writes != 1 || len(store.snapshot.State.SSHTargets) != 1 {
		t.Fatalf("SSH-only durable draft did not approve through CAS: result=%#v writes=%d targets=%#v err=%v", result, store.writes, store.snapshot.State.SSHTargets, err)
	}
	if _, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("SSH-only durable draft replay was accepted: %v", err)
	}
}

func TestSetupDraftApprovalRejectsPersistedDiffThatDoesNotMatchReplannedProposal(t *testing.T) {
	store := newTestSetupDraftStore()
	queue := &fakeSetupDraftQueueStore{}
	controller := newSetupDraftControllerWithQueue(store, queue)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	tampered := cloneSetupDraftReview(review)
	tampered.Changes[0].Summary = "Different text from the accepted proposal"
	tampered.Digest = setupDraftReviewDigest(tampered)
	queue.mu.Lock()
	queue.entries[review.ID] = tampered
	queue.mu.Unlock()
	if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, tampered.Digest); !errors.Is(err, errSetupDraftInvalid) || store.writes != 0 {
		t.Fatalf("approval trusted a digest-valid but mismatched public diff: writes=%d err=%v", store.writes, err)
	}
}

func TestSetupDraftQueueAcceptsControllerReview(t *testing.T) {
	store := newTestSetupDraftStore()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	queue := &fakeSetupDraftQueueStore{}
	controller := newSetupDraftControllerWithQueue(store, queue)
	controller.now = func() time.Time { return now }
	controller.newDraftID = func() (string, error) { return "setupdraft:" + strings.Repeat("a", 32), nil }
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if !validSetupDraftQueueReview(review, now, now, true) {
		t.Fatalf("generated review rejected: %#v; proposal=%#v", review, review.Proposal)
	}
	queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	fileQueue, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := fileQueue.Put(review); err != nil {
		t.Fatalf("file queue rejected generated review: %v", err)
	}
}

func newTestSetupDraftStore() *fakeSetupDraftStore {
	return &fakeSetupDraftStore{snapshot: setupDraftConfigSnapshot{
		Revision: "revision-one",
		AvailableCredentials: []NamedSecretView{{
			ID: "secret:" + strings.Repeat("a", 32), Name: "GitHub automation", Purpose: "Read-only API access", Configured: true,
		}},
	}}
}

func validSetupDraftSubmission(revision string) SetupDraftSubmission {
	return SetupDraftSubmission{
		BaseRevision: revision,
		Sources:      []SetupDraftSource{{Kind: "conversation", Label: "Current conversation", Confidence: "high"}},
		Connections: []SetupDraftConnection{{
			Kind: SetupServiceGitHub, Action: SetupDraftAdd, Name: "Example repository",
			Origin: "https://github.example.invalid", Repository: "team/repository", CredentialName: "GitHub automation",
		}},
	}
}

func TestSetupDraftSubmitAndReviewAreSideEffectFreeAndReturnSafeDiff(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if store.writes != 0 || store.loads != 1 {
		t.Fatalf("submission wrote settings or read the store an unexpected number of times: loads=%d writes=%d", store.loads, store.writes)
	}
	if review.BaseRevision != "revision-one" || review.Digest == "" || !review.ExpiresAt.After(time.Now()) || len(review.Changes) != 1 {
		t.Fatalf("incomplete setup draft review: %#v", review)
	}
	if review.Changes[0].Kind != "connection_add" || !containsSetupDraftItem(review.Changes[0].PermissionIncrease, "github_repository") || !containsSetupDraftItem(review.Changes[0].PermissionIncrease, "github_pull_request") {
		t.Fatalf("review omitted the connection scope or permissions: %#v", review.Changes)
	}
	encoded, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credential_ref", "secret:", "cred:", "token-value", "password-value"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("review exposed secret metadata %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), "GitHub automation") {
		t.Fatal("review omitted the logical credential name the user needs to verify")
	}

	review.Proposal.Connections[0].Name = "mutated by caller"
	second, err := controller.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || second.Proposal.Connections[0].Name != "Example repository" {
		t.Fatalf("review returned a mutable alias to stored draft data: proposal=%#v err=%v", second.Proposal, err)
	}
}

func TestSetupDraftApprovalCASPersistsOnlyAfterApprovalAndConsumesOnce(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	approved, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !approved.Applied || approved.Stale || store.writes != 1 {
		t.Fatalf("approved draft did not commit once: result=%#v writes=%d err=%v", approved, store.writes, err)
	}
	if len(store.snapshot.State.Connections) != 1 {
		t.Fatalf("approved settings not saved: %#v", store.snapshot.State)
	}
	saved := store.snapshot.State.Connections[0]
	if !githubIDPattern.MatchString(saved.ID) || saved.Name != "Example repository" || saved.CredentialName != "GitHub automation" {
		t.Fatalf("unexpected persisted metadata: %#v", saved)
	}
	encoded, _ := json.Marshal(store.snapshot.State)
	if strings.Contains(string(encoded), "secret:") || strings.Contains(string(encoded), "cred:") {
		t.Fatalf("setup draft store copied a Credential Manager reference: %s", encoded)
	}
	if _, err := controller.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("successful approval did not consume the draft: %v", err)
	}
	if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) || store.writes != 1 {
		t.Fatalf("successful draft was replayed: writes=%d err=%v", store.writes, err)
	}
}

func TestSetupDraftStalePreflightPreservesReviewAndCASFailureConsumesClaim(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, errSetupDraftInvalid) || store.writes != 0 {
		t.Fatalf("incorrect reviewed digest reached the store: writes=%d err=%v", store.writes, err)
	}
	store.snapshot.Revision = "revision-two"
	result, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !result.Stale || result.Applied || store.writes != 0 {
		t.Fatalf("stale settings were saved or misreported: result=%#v writes=%d err=%v", result, store.writes, err)
	}
	if err := controller.RejectSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftStale) {
		t.Fatalf("reject after a possible prior apply was not reported as stale: %v", err)
	}
	queued, err := controller.queue.Load(time.Now().UTC())
	if err != nil || len(queued) != 1 || queued[0].ID != review.ID {
		t.Fatalf("stale decision incorrectly consumed the possibly applied draft: reviews=%v err=%v", queued, err)
	}

	store = newTestSetupDraftStore()
	controller = newTestSetupDraftController(store)
	review, err = controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	store.saveErr = errors.New("private config content")
	result, err = controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if !errors.Is(err, errSetupDraftStore) || strings.Contains(err.Error(), "private config content") ||
		result.Applied || result.NextAction != setupDraftApprovalClaimedWarningNextAction {
		t.Fatalf("claimed store failure leaked detail or was misreported: result=%#v err=%v", result, err)
	}
	if _, err := controller.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("claimed draft remained reviewable after a failed CAS: %v", err)
	}
	store.saveErr = nil
	freshReview, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatalf("new draft could not be submitted after claimed failure: %v", err)
	}
	if result, err := controller.ApproveSetupDraft(context.Background(), freshReview.ID, freshReview.BaseRevision, freshReview.Digest); err != nil || !result.Applied {
		t.Fatalf("new draft could not be applied after a failed save: result=%#v err=%v", result, err)
	}
}

func TestSetupDraftClaimFailureNeverWritesSettings(t *testing.T) {
	now := time.Now().UTC()
	store := newTestSetupDraftStore()
	queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	controller := newSetupDraftControllerWithQueue(store, queue)
	controller.now = func() time.Time { return now }
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}

	queue.writeFileAtomically = func(string, []byte) error {
		return errors.New("synthetic ambiguous claim failure")
	}
	result, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if !errors.Is(err, errSetupDraftStore) || result.Applied || result.NextAction != setupDraftApprovalClaimUncertainNextAction || store.writes != 0 {
		t.Fatalf("failed claim authorized a settings write: result=%#v writes=%d err=%v", result, store.writes, err)
	}
}

func TestSetupDraftClaimedCASFailureConsumesReviewAndRequiresNewDraft(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}

	store.saveErr = errors.New("synthetic configuration write failure")
	result, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if !errors.Is(err, errSetupDraftStore) || result.Applied || result.NextAction != setupDraftApprovalClaimedWarningNextAction {
		t.Fatalf("claimed write failure was not reported safely: result=%#v err=%v", result, err)
	}
	store.saveErr = nil
	if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("claimed review was restored after CAS failure: %v", err)
	}
	if len(store.snapshot.State.Connections) != 0 {
		t.Fatal("failed config write changed settings")
	}
}

func TestSetupDraftDurableClaimPreventsReplayAfterConfigABA(t *testing.T) {
	now := time.Now().UTC()
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	initialConfig := config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID: "secret:" + strings.Repeat("b", 32), Name: "GitHub automation", Purpose: "Read-only API access",
			CredentialRef: "cred:" + strings.Repeat("b", 32),
		}},
	}
	if err := writeConfig(settingsPath, initialConfig); err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store := newSetupDraftConfigStore(settingsPath)
	initial, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	controller := newSetupDraftControllerWithQueue(store, queue)
	controller.now = func() time.Time { return now }
	controller.newDraftID = func() (string, error) {
		return "setupdraft:" + strings.Repeat("c", 32), nil
	}
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(initial.Revision))
	if err != nil || len(review.Missing) != 0 {
		t.Fatalf("submit complete fixture: missing=%v err=%v", review.Missing, err)
	}

	result, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !result.Applied {
		t.Fatalf("first approval failed: result=%#v err=%v", result, err)
	}
	approved, err := store.Snapshot()
	if err != nil || approved.Revision == initial.Revision || len(approved.State.Connections) != 1 {
		t.Fatalf("approval did not change config A to B: snapshot=%#v err=%v", approved, err)
	}
	if err := writeConfig(settingsPath, initialConfig); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Snapshot()
	if err != nil || restored.Revision != initial.Revision || len(restored.State.Connections) != 0 {
		t.Fatalf("fixture did not restore exact config A: snapshot=%#v err=%v", restored, err)
	}
	beforeRetry, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatalf("same draft was accepted after restoring exact base config A: %v", err)
	}
	afterRetry, err := os.ReadFile(settingsPath)
	if err != nil || string(afterRetry) != string(beforeRetry) {
		t.Fatalf("replay changed restored config A: err=%v", err)
	}
}

func TestSetupDraftExpiryAndContextCancellationHaveNoConfigSideEffects(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }
	controller.lifetime = time.Minute
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := controller.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) || store.writes != 0 {
		t.Fatalf("expired draft remained reviewable or wrote state: writes=%d err=%v", store.writes, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.SubmitSetupDraft(ctx, validSetupDraftSubmission(store.snapshot.Revision)); !errors.Is(err, errSetupDraftInvalid) || store.writes != 0 {
		t.Fatalf("canceled context had a configuration effect: writes=%d err=%v", store.writes, err)
	}
}

func TestSetupDraftMissingCredentialAndUnknownTargetMappingsBlockApproval(t *testing.T) {
	t.Run("missing credential", func(t *testing.T) {
		store := newTestSetupDraftStore()
		controller := newTestSetupDraftController(store)
		submission := validSetupDraftSubmission(store.snapshot.Revision)
		submission.Connections[0].CredentialName = ""
		review, err := controller.SubmitSetupDraft(context.Background(), submission)
		if err != nil || len(review.Missing) == 0 {
			t.Fatalf("missing credential was not displayed: review=%#v err=%v", review, err)
		}
		if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftMissing) || store.writes != 0 {
			t.Fatalf("draft without a secret mapping was saved: writes=%d err=%v", store.writes, err)
		}
	})

	t.Run("unregistered credential", func(t *testing.T) {
		store := newTestSetupDraftStore()
		controller := newTestSetupDraftController(store)
		submission := validSetupDraftSubmission(store.snapshot.Revision)
		submission.Connections[0].CredentialName = "Not configured"
		review, err := controller.SubmitSetupDraft(context.Background(), submission)
		if err != nil || len(review.Missing) == 0 {
			t.Fatalf("unknown named credential was not shown as missing: review=%#v err=%v", review, err)
		}
		if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftMissing) || store.writes != 0 {
			t.Fatalf("unknown named credential was persisted: writes=%d err=%v", store.writes, err)
		}
	})

	t.Run("unknown bundle target", func(t *testing.T) {
		store := newTestSetupDraftStore()
		controller := newTestSetupDraftController(store)
		submission := SetupDraftSubmission{
			BaseRevision: store.snapshot.Revision,
			Sources:      []SetupDraftSource{{Kind: "conversation", Label: "Current conversation", Confidence: "medium"}},
			Bundles:      []SetupDraftBundle{{Name: "Service set", RepositoryIDs: []string{"github:" + strings.Repeat("b", 32)}}},
		}
		review, err := controller.SubmitSetupDraft(context.Background(), submission)
		if err != nil || len(review.Missing) == 0 {
			t.Fatalf("unregistered bundle target was not shown: review=%#v err=%v", review, err)
		}
		plan, err := buildSetupDraftPlan(store.snapshot, review.Proposal, setupDraftResourceIDGenerator(review.ID))
		if err != nil || !sameSetupDraftReviewPlan(review, plan) {
			reviewChanges, _ := json.Marshal(review.Changes)
			planChanges, _ := json.Marshal(plan.changes)
			reviewMissing, _ := json.Marshal(review.Missing)
			planMissing, _ := json.Marshal(plan.missing)
			t.Fatalf("replanned missing bundle diverged: changes review=%s plan=%s missing review=%s plan=%s err=%v", reviewChanges, planChanges, reviewMissing, planMissing, err)
		}
		if _, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftMissing) || store.writes != 0 {
			t.Fatalf("bundle with unknown target was saved: writes=%d err=%v", store.writes, err)
		}
	})
}

func TestSetupDraftSupportsExistingBundleAndSSHScopeDiff(t *testing.T) {
	githubID := "github:" + strings.Repeat("a", 32)
	store := newTestSetupDraftStore()
	store.snapshot.State.Connections = []setupDraftConnectionRecord{{
		ID: githubID, Kind: SetupServiceGitHub, Name: "Example repository", Origin: "https://github.example.invalid",
		Repository: "team/repository", CredentialName: "GitHub automation",
	}}
	store.snapshot.State.Bundles = []setupDraftBundleRecord{{
		ID:         "service:" + strings.Repeat("c", 32),
		Definition: SetupDraftBundle{Name: "QA bundle", RepositoryIDs: []string{githubID}},
	}}
	store.snapshot.Revision = "revision-with-state"
	controller := newTestSetupDraftController(store)
	submission := SetupDraftSubmission{
		BaseRevision: store.snapshot.Revision,
		Sources:      []SetupDraftSource{{Kind: "document", Label: "Selected runbook.md", Confidence: "medium"}},
		Connections: []SetupDraftConnection{{
			Kind: SetupServiceGitHub, Action: SetupDraftReuse, ExistingID: githubID, Name: "Example repository",
			Origin: "https://github.example.invalid", Repository: "team/repository", CredentialName: "GitHub automation",
		}},
		Bundles: []SetupDraftBundle{{
			Name: "QA bundle", RepositoryIDs: []string{githubID},
			Environments: []SetupDraftEnvironment{{Name: "qa"}},
		}},
		SSHTargets: []SetupDraftSSHTarget{{
			Name: "QA host", Alias: "qa-host", Operations: []SSHOperationDefinition{{
				ID: "list_pods", Name: "List pods", Summary: "Read one namespace inventory.", Program: "kubectl",
				FixedArgs: []string{"get", "pods"}, Risk: SSHRiskReadOnly,
				Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly},
			}},
		}},
	}
	review, err := controller.SubmitSetupDraft(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Changes) != 2 || review.Changes[0].PermissionIncrease == nil && review.Changes[1].PermissionIncrease == nil {
		t.Fatalf("bundle and SSH review omitted scope changes: %#v", review.Changes)
	}
	if store.writes != 0 {
		t.Fatalf("draft submission saved proposed bundle or SSH settings: writes=%d", store.writes)
	}
	approved, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
	if err != nil || !approved.Applied || store.writes != 1 || len(store.snapshot.State.SSHTargets) != 1 || len(store.snapshot.State.Bundles[0].Definition.Environments) != 1 {
		t.Fatalf("reviewed bundle/SSH settings did not save through CAS: result=%#v snapshot=%#v err=%v", approved, store.snapshot.State, err)
	}
}

func TestSetupDraftSerializesConcurrentApprovalSoOnlyOneSaveWins(t *testing.T) {
	store := newTestSetupDraftStore()
	controller := newTestSetupDraftController(store)
	review, err := controller.SubmitSetupDraft(context.Background(), validSetupDraftSubmission(store.snapshot.Revision))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := controller.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	successes, failures := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 || store.writes != 1 {
		t.Fatalf("concurrent approval was not one-shot: successes=%d failures=%d writes=%d", successes, failures, store.writes)
	}
}

func containsSetupDraftItem(values []string, sought string) bool {
	for _, value := range values {
		if value == sought {
			return true
		}
	}
	return false
}
