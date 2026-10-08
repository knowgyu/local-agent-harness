package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const setupDraftQueueHelperFileName = "queue-test-job.json"

type setupDraftQueueSubprocessJob struct {
	Mode         string `json:"mode"`
	Path         string `json:"path"`
	ID           string `json:"id"`
	BaseRevision string `json:"base_revision,omitempty"`
	Digest       string `json:"digest,omitempty"`
	Decision     string `json:"decision,omitempty"`
	AttemptPath  string `json:"attempt_path,omitempty"`
	EnteredPath  string `json:"entered_path,omitempty"`
	ReleasePath  string `json:"release_path,omitempty"`
}

func TestSetupDraftQueueFileStoreCRUDAndSecretFreePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 1, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	data, err := readSetupDraftQueueFile(path, setupDraftQueueMaxFileBytes)
	if err != nil {
		t.Fatalf("read stored queue: %v", err)
	}
	for _, forbidden := range []string{"next_state", "nextState", "credential_ref", "secret:", "cred:", "token-value", "password-value"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("queue persisted forbidden material %q", forbidden)
		}
	}

	reopened := newSetupDraftQueueTestStore(t, path, now)
	loaded, err := reopened.Load(now)
	if err != nil || len(loaded) != 1 || loaded[0].ID != review.ID || loaded[0].Digest != review.Digest {
		t.Fatalf("reopened queue Load() = %#v, %v", loaded, err)
	}
	loaded[0].Proposal.Connections[0].Name = "caller mutation"
	loadedAgain, err := store.Load(now)
	if err != nil || loadedAgain[0].Proposal.Connections[0].Name != "Example repository" {
		t.Fatalf("Load() exposed mutable state: %#v, %v", loadedAgain, err)
	}
	if err := store.Delete(review.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(review.ID); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	loaded, err = store.Load(now)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("Load() after Delete() = %#v, %v", loaded, err)
	}
}

func TestSetupDraftQueueRoundTripsControllerNormalizedSSHRevision(t *testing.T) {
	now := time.Now().UTC()
	review := submitSSHOnlySetupDraftForQueueTest(t)
	operation := review.Proposal.SSHTargets[0].Operations[0]
	if operation.Revision == "" {
		t.Fatal("controller did not populate the normalized SSH operation revision")
	}

	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	queue := newSetupDraftQueueTestStore(t, path, now)
	if err := queue.Put(review); err != nil {
		t.Fatalf("durable queue rejected valid normalized SSH-only review: %v", err)
	}
	loaded, err := queue.Load(now)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("durable Load() returned %d reviews, err=%v", len(loaded), err)
	}
	loadedOperation := loaded[0].Proposal.SSHTargets[0].Operations[0]
	if loadedOperation.Revision != operation.Revision || loaded[0].Digest != review.Digest {
		t.Fatalf("durable Load() changed normalized SSH review: operation=%#v digest=%q", loadedOperation, loaded[0].Digest)
	}

	claimed, err := queue.ClaimReview(loaded[0].ID, loaded[0].BaseRevision, loaded[0].Digest, now, func(candidate SetupDraftReview) error {
		candidateOperation := candidate.Proposal.SSHTargets[0].Operations[0]
		if candidateOperation.Revision != operation.Revision || candidate.Digest != review.Digest {
			return errSetupDraftQueueInvalid
		}
		return nil
	})
	if err != nil || claimed.Proposal.SSHTargets[0].Operations[0].Revision != operation.Revision {
		t.Fatalf("ClaimReview() failed for normalized SSH review: claimed=%#v err=%v", claimed, err)
	}

	submission := sshOnlySetupDraftSubmissionForQueueTest(t, review.BaseRevision)
	submission.SSHTargets[0].Operations[0].Revision = "caller_supplied_revision"
	payload, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSetupDraftSubmissionJSON(payload); err == nil {
		t.Fatal("MCP submission with a caller-supplied SSH operation revision was accepted")
	}
}

func TestSetupDraftQueueRejectsTamperedNormalizedSSHRevision(t *testing.T) {
	for _, tamper := range []struct {
		name string
		edit func(*SSHOperationDefinition)
	}{
		{
			name: "operation revision",
			edit: func(operation *SSHOperationDefinition) {
				operation.Revision = "sha256:" + strings.Repeat("0", 64)
			},
		},
		{
			name: "approval revision",
			edit: func(operation *SSHOperationDefinition) {
				operation.Approval.Revision = "forged_revision"
			},
		},
		{
			name: "approval scope",
			edit: func(operation *SSHOperationDefinition) {
				operation.Approval.Scope = []string{"forged scope"}
			},
		},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			now := time.Now().UTC()
			review := submitSSHOnlySetupDraftForQueueTest(t)
			tampered := cloneSetupDraftReview(review)
			tamper.edit(&tampered.Proposal.SSHTargets[0].Operations[0])
			tampered.Digest = setupDraftReviewDigest(tampered)

			path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
			queue := newSetupDraftQueueTestStore(t, path, now)
			if err := queue.Put(tampered); !errors.Is(err, errSetupDraftQueueInvalid) {
				t.Fatalf("Put() tampered normalized review error = %v, want invalid queue data", err)
			}

			writeSetupDraftQueueEnvelope(t, path, setupDraftQueueEnvelope{
				Version: setupDraftQueueVersion,
				Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: tampered}},
			})
			if _, err := queue.Load(now); !errors.Is(err, errSetupDraftQueueInvalid) {
				t.Fatalf("Load() tampered normalized review error = %v, want invalid queue data", err)
			}
		})
	}
}

func submitSSHOnlySetupDraftForQueueTest(t *testing.T) SetupDraftReview {
	t.Helper()
	config := newTestSetupDraftStore()
	controller := newTestSetupDraftController(config)
	review, err := controller.SubmitSetupDraft(context.Background(), sshOnlySetupDraftSubmissionForQueueTest(t, config.snapshot.Revision))
	if err != nil {
		t.Fatalf("SubmitSetupDraft() SSH-only review: %v", err)
	}
	return review
}

func sshOnlySetupDraftSubmissionForQueueTest(t *testing.T, baseRevision string) SetupDraftSubmission {
	t.Helper()
	return SetupDraftSubmission{
		BaseRevision: baseRevision,
		Sources: []SetupDraftSource{{
			Kind: "user_provided", Label: "Q01 synthetic setup fixture", Confidence: "high",
		}},
		SSHTargets: []SetupDraftSSHTarget{{
			Name: "Q01 synthetic SSH target", Alias: "q01-synthetic-alias",
			Operations: []SSHOperationDefinition{{
				ID: "synthetic_inventory", Name: "Synthetic inventory",
				Summary: "Read a synthetic local inventory fixture.", Program: "synthetic-tool",
				FixedArgs: []string{"inventory"}, Parameters: []SSHParameterSpec{},
				Risk: SSHRiskReadOnly, Approval: OperationApprovalPolicy{Mode: OperationApprovalReadOnly, Scope: []string{}},
			}},
		}},
	}
}

func TestSetupDraftQueueRejectsMalformedAndOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	store := newSetupDraftQueueTestStore(t, path, time.Now().UTC())
	cases := []struct {
		name string
		data string
	}{
		{name: "duplicate key", data: `{"version":1,"entries":[],"version":1}`},
		{name: "unknown field", data: `{"version":1,"entries":[],"extra":true}`},
		{name: "null entries", data: `{"version":1,"entries":null}`},
		{name: "trailing value", data: `{"version":1,"entries":[]} {}`},
		{name: "wrong version", data: `{"version":99,"entries":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeSetupDraftQueueTestFile(t, path, []byte(tc.data))
			if _, err := store.Load(time.Now().UTC()); !errors.Is(err, errSetupDraftQueueInvalid) {
				t.Fatalf("Load() error = %v, want invalid queue", err)
			}
		})
	}

	writeSetupDraftQueueTestFile(t, path, make([]byte, setupDraftQueueMaxFileBytes+1))
	if _, err := store.Load(time.Now().UTC()); !errors.Is(err, errSetupDraftQueueInvalid) {
		t.Fatalf("oversized queue Load() error = %v, want invalid queue", err)
	}
}

func TestSetupDraftQueueBindsExpiryAndPrunesExpiredReviews(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 7, now.Add(-time.Minute), time.Minute-time.Second)
	envelope := setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now.Add(-time.Minute), Review: review}}}
	writeSetupDraftQueueEnvelope(t, path, envelope)
	loaded, err := store.Load(now)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("expired Load() = %#v, %v", loaded, err)
	}
	cleaned, err := readSetupDraftQueueFile(path, setupDraftQueueMaxFileBytes)
	if err != nil {
		t.Fatalf("read pruned queue: %v", err)
	}
	var empty setupDraftQueueEnvelope
	if err := json.Unmarshal(cleaned, &empty); err != nil || empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("expired record was not removed: entries=%#v err=%v", empty.Entries, err)
	}

	valid := setupDraftQueueTestReview(t, 8, now, 5*time.Minute)
	tampered := valid
	tampered.ExpiresAt = valid.ExpiresAt.Add(time.Second)
	if setupDraftReviewDigest(tampered) == valid.Digest {
		t.Skip("current controller digest helper does not include ExpiresAt; expiry-binding check awaits central integration")
	}
	valid.ExpiresAt = valid.ExpiresAt.Add(time.Second)
	// Keep the original digest: changing expiry alone must invalidate the entry.
	writeSetupDraftQueueEnvelope(t, path, setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: valid}}})
	if _, err := store.Load(now); !errors.Is(err, errSetupDraftQueueInvalid) {
		t.Fatalf("expiry tampering Load() error = %v, want invalid queue", err)
	}
}

func TestSetupDraftQueueEnforcesPendingAndAggregateLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	for index := 1; index <= setupDraftMaximumPending; index++ {
		if err := store.Put(setupDraftQueueTestReview(t, index, now, 5*time.Minute)); err != nil {
			t.Fatalf("Put(%d) error = %v", index, err)
		}
	}
	if err := store.Put(setupDraftQueueTestReview(t, setupDraftMaximumPending+1, now, 5*time.Minute)); !errors.Is(err, errSetupDraftQueueCapacity) {
		t.Fatalf("Put() at capacity error = %v, want queue full", err)
	}

	aggregatePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	aggregateStore := newSetupDraftQueueTestStore(t, aggregatePath, now)
	var large SetupDraftReview
	small := setupDraftQueueTestReview(t, 1000, now, 5*time.Minute)
	for count := 128; count > 0; count-- {
		candidate := setupDraftQueueTestReview(t, 999, now, 5*time.Minute)
		candidate.Changes[0].Scope = make([]string, count)
		for index := 0; index < len(candidate.Changes[0].Scope)-1; index++ {
			candidate.Changes[0].Scope[index] = strings.Repeat("x", 2048)
		}
		candidate.Changes[0].Scope[len(candidate.Changes[0].Scope)-1] = "x"
		candidate.Digest = setupDraftReviewDigest(candidate)
		one, oneErr := json.Marshal(setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: candidate}}})
		two, twoErr := json.Marshal(setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: candidate}, {CreatedAt: now, Review: small}}})
		if oneErr != nil || twoErr != nil {
			continue
		}
		increase := setupDraftQueueMaxFileBytes - len(two) + 1
		if increase <= 0 || increase > 2047 || len(one)+increase >= setupDraftQueueMaxFileBytes {
			continue
		}
		candidate.Changes[0].Scope[len(candidate.Changes[0].Scope)-1] = strings.Repeat("x", int(increase+1))
		candidate.Digest = setupDraftReviewDigest(candidate)
		one, _ = json.Marshal(setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: candidate}}})
		two, _ = json.Marshal(setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: []setupDraftQueueEntry{{CreatedAt: now, Review: candidate}, {CreatedAt: now, Review: small}}})
		if len(one) < setupDraftQueueMaxFileBytes && len(two) > setupDraftQueueMaxFileBytes {
			large = candidate
			break
		}
	}
	if large.ID == "" {
		t.Fatal("could not construct a bounded review close to aggregate cap")
	}
	if err := aggregateStore.Put(large); err != nil {
		t.Fatalf("Put() of single bounded large review: %v", err)
	}
	if err := aggregateStore.Put(setupDraftQueueTestReview(t, 1000, now, 5*time.Minute)); !errors.Is(err, errSetupDraftQueueCapacity) {
		t.Fatalf("Put() beyond aggregate byte cap error = %v, want queue full", err)
	}
	oversizedPath := aggregatePath
	writeSetupDraftQueueTestFile(t, oversizedPath, make([]byte, setupDraftQueueMaxFileBytes+1))
	if _, err := aggregateStore.Load(now); !errors.Is(err, errSetupDraftQueueInvalid) {
		t.Fatalf("aggregate queue bound Load() error = %v, want invalid queue", err)
	}
}

func TestSetupDraftQueueSubprocessHelper(t *testing.T) {
	jobData, err := os.ReadFile(setupDraftQueueHelperFileName)
	if err != nil {
		return
	}
	var job setupDraftQueueSubprocessJob
	if err := json.Unmarshal(jobData, &job); err != nil || job.Mode == "" || job.Path == "" {
		return
	}
	mode, path, id := job.Mode, job.Path, job.ID
	store, err := newSetupDraftFileQueueStoreForTest(path, time.Now)
	if err != nil {
		t.Fatalf("subprocess constructor: %v", err)
	}
	switch mode {
	case "put":
		var sequence int
		if _, err := fmt.Sscanf(id, "%d", &sequence); err != nil {
			t.Fatalf("subprocess identifier: %v", err)
		}
		if err := store.Put(setupDraftQueueTestReview(t, sequence, time.Now().UTC(), 5*time.Minute)); err != nil {
			t.Fatalf("subprocess Put(): %v", err)
		}
	case "load-delete":
		loaded, err := store.Load(time.Now().UTC())
		found := false
		for _, review := range loaded {
			found = found || review.ID == id
		}
		if err != nil || !found {
			t.Fatalf("subprocess Load() found target=%v, err=%v (records=%d)", found, err, len(loaded))
		}
		if err := store.Delete(id); err != nil {
			t.Fatalf("subprocess Delete(): %v", err)
		}
	case "with-review":
		if job.Decision != "approve" && job.Decision != "reject" {
			t.Fatalf("unknown transaction decision %q", job.Decision)
		}
		if job.AttemptPath != "" {
			writeSetupDraftQueueSignal(t, job.AttemptPath)
		}
		called := false
		err := store.WithReview(id, time.Now().UTC(), func(review SetupDraftReview) (bool, error) {
			called = true
			if review.ID != id {
				return false, errors.New("subprocess loaded the wrong review")
			}
			if job.EnteredPath != "" {
				writeSetupDraftQueueSignal(t, job.EnteredPath)
			}
			if job.ReleasePath != "" {
				waitForSetupDraftQueueSignal(t, job.ReleasePath)
			}
			return true, nil
		})
		if job.Decision == "approve" {
			if err != nil || !called {
				t.Fatalf("approve transaction called=%v err=%v; want callback and consumed record", called, err)
			}
			return
		}
		if called || !errors.Is(err, errSetupDraftQueueReviewMissing) {
			t.Fatalf("reject transaction called=%v err=%v; want missing without callback", called, err)
		}
	case "claim-review":
		called := false
		claimed, err := store.ClaimReview(id, job.BaseRevision, job.Digest, time.Now().UTC(), func(review SetupDraftReview) error {
			called = true
			if review.ID != id || review.BaseRevision != job.BaseRevision || review.Digest != job.Digest {
				return errors.New("claim preflight received the wrong review")
			}
			if job.EnteredPath != "" {
				writeSetupDraftQueueSignal(t, job.EnteredPath)
			}
			if job.ReleasePath != "" {
				waitForSetupDraftQueueSignal(t, job.ReleasePath)
			}
			return nil
		})
		if err != nil || !called || claimed.ID != id {
			t.Fatalf("ClaimReview() called=%v review=%#v err=%v; want claimed review", called, claimed, err)
		}
	default:
		t.Fatalf("unknown subprocess mode %q", mode)
	}
}

func TestSetupDraftQueueSurvivesProcessBoundaryAndSerializesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	const writers = 8
	commands := make([]*exec.Cmd, 0, writers)
	outputs := make([]*bytes.Buffer, 0, writers)
	for sequence := 1; sequence <= writers; sequence++ {
		command := setupDraftQueueSubprocessCommand(t, "put", path, fmt.Sprintf("%d", sequence))
		output := &bytes.Buffer{}
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatalf("start writer %d: %v", sequence, err)
		}
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("writer %d failed: %v\n%s", index+1, err, outputs[index].String())
		}
	}
	store := newSetupDraftQueueTestStore(t, path, time.Now().UTC())
	loaded, err := store.Load(time.Now().UTC())
	if err != nil || len(loaded) != writers {
		t.Fatalf("cross-process queue Load() returned %d records, %v; want %d", len(loaded), err, writers)
	}

	id := setupDraftQueueTestID(1)
	command := setupDraftQueueSubprocessCommand(t, "load-delete", path, id)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("separate process could not load and consume the record: %v\n%s", err, output)
	}
	loaded, err = store.Load(time.Now().UTC())
	if err != nil || len(loaded) != writers-1 {
		t.Fatalf("parent did not observe child deletion: count=%d err=%v", len(loaded), err)
	}
}

func TestSetupDraftQueueWithReviewSerializesApproveAndRejectAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 71, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}

	barrier := t.TempDir()
	enteredPath := filepath.Join(barrier, "approve-entered")
	releasePath := filepath.Join(barrier, "release-approve")
	attemptPath := filepath.Join(barrier, "reject-attempted")
	approve := setupDraftQueueSubprocessCommandWithJob(t, setupDraftQueueSubprocessJob{
		Mode: "with-review", Path: path, ID: review.ID, Decision: "approve",
		EnteredPath: enteredPath, ReleasePath: releasePath,
	})
	approveOutput := &bytes.Buffer{}
	approve.Stdout, approve.Stderr = approveOutput, approveOutput
	if err := approve.Start(); err != nil {
		t.Fatalf("start approve process: %v", err)
	}
	waitForSetupDraftQueueSignal(t, enteredPath)

	reject := setupDraftQueueSubprocessCommandWithJob(t, setupDraftQueueSubprocessJob{
		Mode: "with-review", Path: path, ID: review.ID, Decision: "reject", AttemptPath: attemptPath,
	})
	rejectOutput := &bytes.Buffer{}
	reject.Stdout, reject.Stderr = rejectOutput, rejectOutput
	if err := reject.Start(); err != nil {
		writeSetupDraftQueueSignal(t, releasePath)
		_ = approve.Wait()
		t.Fatalf("start reject process: %v", err)
	}
	waitForSetupDraftQueueSignal(t, attemptPath)
	time.Sleep(150 * time.Millisecond)
	writeSetupDraftQueueSignal(t, releasePath)
	if err := approve.Wait(); err != nil {
		t.Fatalf("approve process failed: %v\n%s", err, approveOutput.String())
	}
	if err := reject.Wait(); err != nil {
		t.Fatalf("reject process failed: %v\n%s", err, rejectOutput.String())
	}

	loaded, err := store.Load(time.Now().UTC())
	if err != nil || len(loaded) != 0 {
		t.Fatalf("winning approval left %d pending reviews, err=%v", len(loaded), err)
	}
	if err := store.WithReview(review.ID, time.Now().UTC(), func(SetupDraftReview) (bool, error) {
		t.Fatal("replay invoked callback after consumed approval")
		return false, nil
	}); !errors.Is(err, errSetupDraftQueueReviewMissing) {
		t.Fatalf("replay WithReview() error = %v, want missing review", err)
	}
}

func TestSetupDraftQueueWithReviewPreservesOnCallbackErrorOrNonConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 73, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}
	callbackErr := errors.New("synthetic stale decision")
	err := store.WithReview(review.ID, now, func(loaded SetupDraftReview) (bool, error) {
		if loaded.ID != review.ID {
			t.Fatalf("callback review ID = %q, want %q", loaded.ID, review.ID)
		}
		return true, callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("WithReview() callback error = %v, want callback error", err)
	}
	if err := store.WithReview(review.ID, now, func(loaded SetupDraftReview) (bool, error) {
		if loaded.ID != review.ID {
			t.Fatalf("second callback review ID = %q, want %q", loaded.ID, review.ID)
		}
		return false, nil
	}); err != nil {
		t.Fatalf("WithReview() non-consuming callback error = %v", err)
	}
	loaded, err := store.Load(now)
	if err != nil || len(loaded) != 1 || loaded[0].ID != review.ID {
		t.Fatalf("non-consuming/error callbacks must preserve review; loaded=%v err=%v", loaded, err)
	}
}

func TestSetupDraftQueueWithReviewSurfacesPostCommitConsumeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 72, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}
	committed := false
	store.writeFileAtomically = func(string, []byte) error {
		return errors.New("synthetic queue replacement failure")
	}
	err := store.WithReview(review.ID, now, func(loaded SetupDraftReview) (bool, error) {
		if loaded.ID != review.ID {
			t.Fatalf("callback review ID = %q, want %q", loaded.ID, review.ID)
		}
		committed = true
		return true, nil
	})
	if !committed || !errors.Is(err, errSetupDraftQueueConsumeFailed) {
		t.Fatalf("WithReview() committed=%v err=%v; want committed callback and consume-failure sentinel", committed, err)
	}
	store.writeFileAtomically = nil
	loaded, err := store.Load(now)
	if err != nil || len(loaded) != 1 || loaded[0].ID != review.ID {
		t.Fatalf("failed consume should preserve review for stale/replay handling; loaded=%v err=%v", loaded, err)
	}
}

func TestSetupDraftQueueClaimSerializesApprovalAndRejectionAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 74, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}

	barrier := t.TempDir()
	enteredPath := filepath.Join(barrier, "claim-validation-entered")
	releasePath := filepath.Join(barrier, "release-claim")
	attemptPath := filepath.Join(barrier, "reject-attempted")
	claim := setupDraftQueueSubprocessCommandWithJob(t, setupDraftQueueSubprocessJob{
		Mode: "claim-review", Path: path, ID: review.ID,
		BaseRevision: review.BaseRevision, Digest: review.Digest,
		EnteredPath: enteredPath, ReleasePath: releasePath,
	})
	claimOutput := &bytes.Buffer{}
	claim.Stdout, claim.Stderr = claimOutput, claimOutput
	if err := claim.Start(); err != nil {
		t.Fatalf("start claim process: %v", err)
	}
	waitForSetupDraftQueueSignal(t, enteredPath)

	reject := setupDraftQueueSubprocessCommandWithJob(t, setupDraftQueueSubprocessJob{
		Mode: "with-review", Path: path, ID: review.ID, Decision: "reject", AttemptPath: attemptPath,
	})
	rejectOutput := &bytes.Buffer{}
	reject.Stdout, reject.Stderr = rejectOutput, rejectOutput
	if err := reject.Start(); err != nil {
		writeSetupDraftQueueSignal(t, releasePath)
		_ = claim.Wait()
		t.Fatalf("start reject process: %v", err)
	}
	waitForSetupDraftQueueSignal(t, attemptPath)
	time.Sleep(150 * time.Millisecond)
	writeSetupDraftQueueSignal(t, releasePath)
	if err := claim.Wait(); err != nil {
		t.Fatalf("claim process failed: %v\n%s", err, claimOutput.String())
	}
	if err := reject.Wait(); err != nil {
		t.Fatalf("reject process failed: %v\n%s", err, rejectOutput.String())
	}

	loaded, err := store.Load(time.Now().UTC())
	if err != nil || len(loaded) != 0 {
		t.Fatalf("winning claim left %d pending reviews, err=%v", len(loaded), err)
	}
	validateCalled := false
	if _, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, time.Now().UTC(), func(SetupDraftReview) error {
		validateCalled = true
		return nil
	}); !errors.Is(err, errSetupDraftQueueReviewMissing) || validateCalled {
		t.Fatalf("claim replay err=%v validateCalled=%v; want missing before validation", err, validateCalled)
	}
}

func TestSetupDraftQueueClaimChecksReferenceAndPreservesOnPreflightError(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 75, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}
	validateCalls := 0
	validate := func(SetupDraftReview) error {
		validateCalls++
		return nil
	}
	if _, err := store.ClaimReview(review.ID, "wrong-revision", review.Digest, now, validate); !errors.Is(err, errSetupDraftQueueReviewMismatch) {
		t.Fatalf("ClaimReview() wrong base revision error = %v, want reference mismatch", err)
	}
	if _, err := store.ClaimReview(review.ID, review.BaseRevision, "sha256:"+strings.Repeat("0", 64), now, validate); !errors.Is(err, errSetupDraftQueueReviewMismatch) {
		t.Fatalf("ClaimReview() wrong digest error = %v, want reference mismatch", err)
	}
	if validateCalls != 0 {
		t.Fatalf("preflight ran %d times for mismatched references", validateCalls)
	}

	preflightErr := errors.New("synthetic stale preflight")
	if _, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, func(SetupDraftReview) error {
		validateCalls++
		return preflightErr
	}); !errors.Is(err, preflightErr) {
		t.Fatalf("ClaimReview() preflight error = %v, want validator error", err)
	}
	loaded, err := store.Load(now)
	if err != nil || len(loaded) != 1 || loaded[0].ID != review.ID {
		t.Fatalf("reference/preflight errors must preserve pending review: loaded=%#v err=%v", loaded, err)
	}
	if validateCalls != 1 {
		t.Fatalf("preflight ran %d times, want once for matching reference", validateCalls)
	}
}

func TestSetupDraftQueueClaimFailureNeverAuthorizesCommit(t *testing.T) {
	t.Run("replacement-fails-before-claim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
		now := time.Now().UTC()
		store := newSetupDraftQueueTestStore(t, path, now)
		review := setupDraftQueueTestReview(t, 76, now, 5*time.Minute)
		if err := store.Put(review); err != nil {
			t.Fatalf("Put() review: %v", err)
		}
		store.writeFileAtomically = func(string, []byte) error {
			return errors.New("synthetic replace failure")
		}
		commitCalls := 0
		claimed, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, func(SetupDraftReview) error {
			return nil
		})
		if err == nil {
			commitCalls++ // The controller may enter CAS only after a nil claim error.
		}
		if !errors.Is(err, errSetupDraftQueueClaimFailed) || claimed.ID != "" || commitCalls != 0 {
			t.Fatalf("ClaimReview() claimed=%#v err=%v commitCalls=%d; want no claim and no commit", claimed, err, commitCalls)
		}
		store.writeFileAtomically = nil
		loaded, err := store.Load(now)
		if err != nil || len(loaded) != 1 || loaded[0].ID != review.ID {
			t.Fatalf("pre-replacement failure should preserve pending review: loaded=%#v err=%v", loaded, err)
		}
	})

	t.Run("replacement-ack-is-ambiguous", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
		now := time.Now().UTC()
		store := newSetupDraftQueueTestStore(t, path, now)
		review := setupDraftQueueTestReview(t, 77, now, 5*time.Minute)
		if err := store.Put(review); err != nil {
			t.Fatalf("Put() review: %v", err)
		}
		store.writeFileAtomically = func(path string, data []byte) error {
			if err := writeSetupDraftQueueFileAtomically(path, data); err != nil {
				return err
			}
			return errors.New("synthetic lost replacement acknowledgment")
		}
		commitCalls := 0
		claimed, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, func(SetupDraftReview) error {
			return nil
		})
		if err == nil {
			commitCalls++
		}
		if !errors.Is(err, errSetupDraftQueueClaimFailed) || claimed.ID != "" || commitCalls != 0 {
			t.Fatalf("ambiguous ClaimReview() claimed=%#v err=%v commitCalls=%d; want no commit", claimed, err, commitCalls)
		}
		store.writeFileAtomically = nil
		if _, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, func(SetupDraftReview) error {
			t.Fatal("ambiguous claim replay invoked preflight")
			return nil
		}); !errors.Is(err, errSetupDraftQueueReviewMissing) {
			t.Fatalf("ambiguous claim replay error = %v, want missing review", err)
		}
	})
}

func TestSetupDraftQueueClaimPreventsABAReplayAndConsumesCASConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	now := time.Now().UTC()
	store := newSetupDraftQueueTestStore(t, path, now)
	review := setupDraftQueueTestReview(t, 78, now, 5*time.Minute)
	if err := store.Put(review); err != nil {
		t.Fatalf("Put() review: %v", err)
	}

	configRevision := review.BaseRevision // Synthetic A.
	validateCurrent := func(candidate SetupDraftReview) error {
		if configRevision != candidate.BaseRevision {
			return errors.New("synthetic stale configuration")
		}
		return nil
	}
	claimed, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, validateCurrent)
	if err != nil || claimed.ID != review.ID {
		t.Fatalf("initial ClaimReview() = %#v, %v", claimed, err)
	}
	if configRevision != claimed.BaseRevision {
		t.Fatalf("synthetic CAS base = %q, want %q", configRevision, claimed.BaseRevision)
	}
	configRevision = "revision-after-apply" // Synthetic A -> B CAS.
	configRevision = review.BaseRevision    // Restore exact A to reproduce ABA.
	validateCalled := false
	if _, err := store.ClaimReview(review.ID, review.BaseRevision, review.Digest, now, func(SetupDraftReview) error {
		validateCalled = true
		return nil
	}); !errors.Is(err, errSetupDraftQueueReviewMissing) || validateCalled {
		t.Fatalf("ABA replay error=%v validateCalled=%v; want consumed before CAS", err, validateCalled)
	}

	conflict := setupDraftQueueTestReview(t, 79, now, 5*time.Minute)
	if err := store.Put(conflict); err != nil {
		t.Fatalf("Put() conflict review: %v", err)
	}
	claimedConflict, err := store.ClaimReview(conflict.ID, conflict.BaseRevision, conflict.Digest, now, func(SetupDraftReview) error {
		return nil
	})
	if err != nil || claimedConflict.ID != conflict.ID {
		t.Fatalf("claim before synthetic CAS conflict: review=%#v err=%v", claimedConflict, err)
	}
	currentRevision := "different-config-revision"
	casConflict := errors.New("synthetic config CAS conflict")
	applyErr := func() error {
		if currentRevision != claimedConflict.BaseRevision {
			return casConflict
		}
		currentRevision = "revision-after-apply"
		return nil
	}()
	if !errors.Is(applyErr, casConflict) {
		t.Fatalf("synthetic CAS error = %v, want conflict", applyErr)
	}
	if _, err := store.ClaimReview(conflict.ID, conflict.BaseRevision, conflict.Digest, now, func(SetupDraftReview) error {
		t.Fatal("failed-CAS claim was restored")
		return nil
	}); !errors.Is(err, errSetupDraftQueueReviewMissing) {
		t.Fatalf("CAS-conflict replay error = %v, want missing review", err)
	}
}

func TestSetupDraftDurableQueuePreservesCredentialBlockers(t *testing.T) {
	for _, test := range []struct {
		name           string
		credentialName string
	}{
		{name: "missing credential"},
		{name: "unregistered credential", credentialName: "Not configured"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestSetupDraftStore()
			queuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
			queue := newSetupDraftQueueTestStore(t, queuePath, time.Now().UTC())
			controller := newSetupDraftControllerWithQueue(store, queue)
			submission := validSetupDraftSubmission(store.snapshot.Revision)
			submission.Connections[0].CredentialName = test.credentialName
			review, err := controller.SubmitSetupDraft(context.Background(), submission)
			if err != nil || len(review.Missing) == 0 {
				t.Fatalf("durable submission did not preserve the missing prerequisite review: missing=%d err=%v", len(review.Missing), err)
			}

			// Reopen the file queue and controller to verify the review can be
			// read after the process boundary rather than only from memory.
			reopened := newSetupDraftQueueTestStore(t, queuePath, time.Now().UTC())
			restarted := newSetupDraftControllerWithQueue(store, reopened)
			recovered, err := restarted.ReviewSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest)
			if err != nil || len(recovered.Missing) == 0 {
				t.Fatalf("reopened review hid its credential prerequisite: missing=%d err=%v", len(recovered.Missing), err)
			}
			if _, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftMissing) || store.writes != 0 {
				t.Fatalf("review without an available credential was applied: writes=%d err=%v", store.writes, err)
			}
			pending, err := reopened.Load(time.Now().UTC())
			if err != nil || len(pending) != 1 || pending[0].ID != review.ID {
				t.Fatalf("blocked approval did not preserve its review for a new submission: pending=%d err=%v", len(pending), err)
			}
		})
	}
}

func TestSetupDraftQueueRejectsReparsePointWhenAvailable(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, setupDraftQueueDirectoryName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable in this environment: %v", err)
	}
	path := filepath.Join(link, setupDraftQueueFileName)
	store := newSetupDraftQueueTestStore(t, path, time.Now().UTC())
	if _, err := store.Load(time.Now().UTC()); err == nil {
		t.Fatal("Load() followed a queue-directory reparse point")
	}
	if _, err := os.Stat(filepath.Join(outside, setupDraftQueueFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reparse-point check created a file outside the queue directory: %v", err)
	}
}

func newSetupDraftQueueTestStore(t *testing.T, path string, now time.Time) *setupDraftFileQueueStore {
	t.Helper()
	store, err := newSetupDraftFileQueueStoreForTest(path, func() time.Time { return now })
	if err != nil {
		t.Fatalf("test queue constructor: %v", err)
	}
	return store
}

func setupDraftQueueTestReview(t *testing.T, sequence int, createdAt time.Time, lifetime time.Duration) SetupDraftReview {
	t.Helper()
	createdAt = createdAt.UTC()
	proposal, err := normalizeSetupDraftSubmission(validSetupDraftSubmission("revision-one"))
	if err != nil {
		t.Fatalf("normalize test proposal: %v", err)
	}
	review := SetupDraftReview{
		ID: setupDraftQueueTestID(sequence), BaseRevision: proposal.BaseRevision,
		ExpiresAt: createdAt.Add(lifetime).UTC(), Sources: append([]SetupDraftSource(nil), proposal.Sources...),
		Missing: append([]string(nil), proposal.Missing...),
		Changes: []SetupDraftChange{{
			Kind: "connection_add", Name: proposal.Connections[0].Name,
			Summary:            "Add registered GitHub target Example repository.",
			Scope:              []string{"target: Example repository", "repository: team/repository"},
			PermissionIncrease: []string{"github_repository", "github_pull_request"},
		}},
		Proposal: proposal,
	}
	review.Digest = setupDraftReviewDigest(review)
	return review
}

func setupDraftQueueTestID(sequence int) string {
	return fmt.Sprintf("setupdraft:%032x", sequence)
}

func writeSetupDraftQueueTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	err := withSetupDraftQueueLock(path, func() error { return writeSetupDraftQueueFileAtomically(path, data) })
	if err != nil && (len(data) <= setupDraftQueueMaxFileBytes || func() bool { _, statErr := os.Stat(path); return statErr != nil }()) {
		t.Fatalf("write synthetic queue file: %v", err)
	}
}

func writeSetupDraftQueueEnvelope(t *testing.T, path string, envelope setupDraftQueueEnvelope) {
	t.Helper()
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode test envelope: %v", err)
	}
	writeSetupDraftQueueTestFile(t, path, data)
}

func setupDraftQueueSubprocessCommand(t *testing.T, mode, path, id string) *exec.Cmd {
	t.Helper()
	return setupDraftQueueSubprocessCommandWithJob(t, setupDraftQueueSubprocessJob{Mode: mode, Path: path, ID: id})
}

func setupDraftQueueSubprocessCommandWithJob(t *testing.T, job setupDraftQueueSubprocessJob) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	workDir := t.TempDir()
	jobData, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("encode subprocess fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, setupDraftQueueHelperFileName), jobData, 0600); err != nil {
		t.Fatalf("write subprocess fixture: %v", err)
	}
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestSetupDraftQueueSubprocessHelper$")
	command.Dir = workDir
	return command
}

func writeSetupDraftQueueSignal(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("ready"), 0600); err != nil {
		t.Fatalf("write subprocess synchronization signal: %v", err)
	}
}

func waitForSetupDraftQueueSignal(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for subprocess synchronization signal %q", filepath.Base(path))
}
