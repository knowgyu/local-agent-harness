//go:build windows && integration

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	setupDraftCrashRoleEnv     = "LAH_QA_SETUP_DRAFT_CRASH_ROLE"
	setupDraftCrashConfigEnv   = "LAH_QA_SETUP_DRAFT_CRASH_CONFIG"
	setupDraftCrashQueueEnv    = "LAH_QA_SETUP_DRAFT_CRASH_QUEUE"
	setupDraftCrashBarrierEnv  = "LAH_QA_SETUP_DRAFT_CRASH_BARRIER"
	setupDraftCrashIDEnv       = "LAH_QA_SETUP_DRAFT_CRASH_ID"
	setupDraftCrashRevisionEnv = "LAH_QA_SETUP_DRAFT_CRASH_REVISION"
	setupDraftCrashDigestEnv   = "LAH_QA_SETUP_DRAFT_CRASH_DIGEST"
)

// TestSetupDraftApprovalCrashAcceptance kills only its own helper after the
// durable one-shot review claim and before the settings compare-and-swap.
// Everything is rooted in t.TempDir and the fixture starts at config v9, so
// migration behavior is outside this acceptance boundary.
func TestSetupDraftApprovalCrashAcceptance(t *testing.T) {
	if os.Getenv(setupDraftCrashRoleEnv) != "" {
		t.Skip("subprocess helper is exercised by the parent acceptance")
	}

	root := t.TempDir()
	configPath := filepath.Join(root, "settings.json")
	queuePath := filepath.Join(root, setupDraftQueueDirectoryName, setupDraftQueueFileName)
	barrierPath := filepath.Join(root, "claimed-before-cas")
	credentialRef := "cred:" + strings.Repeat("b", 32)
	cfg := config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID: "secret:" + strings.Repeat("a", 32), Name: "Synthetic GitHub key",
			Purpose: "crash acceptance fixture", CredentialRef: credentialRef,
		}},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal("could not initialize the isolated v9 settings fixture")
	}
	initialBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("could not capture the isolated settings fixture")
	}

	queue := setupDraftCrashQueue(t, queuePath)
	store := newSetupDraftConfigStore(configPath)
	controller := newSetupDraftControllerWithQueue(store, queue)
	initial, err := store.Snapshot()
	if err != nil {
		t.Fatal("could not inspect the isolated v9 settings fixture")
	}
	review, err := controller.SubmitSetupDraft(context.Background(), setupDraftCrashSubmission(initial.Revision))
	if err != nil {
		t.Fatalf("could not create the synthetic setup review: %v", err)
	}
	childTemp := filepath.Join(root, "child-temp")
	if err := os.Mkdir(childTemp, 0o700); err != nil {
		t.Fatal("could not create the helper's temporary directory")
	}
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("could not resolve the current test helper")
	}
	command := exec.Command(binary, "-test.run=^TestSetupDraftApprovalCrashHelper$", "-test.timeout=45s")
	command.Env = []string{
		setupDraftCrashRoleEnv + "=claim-before-cas",
		setupDraftCrashConfigEnv + "=" + configPath,
		setupDraftCrashQueueEnv + "=" + queuePath,
		setupDraftCrashBarrierEnv + "=" + barrierPath,
		setupDraftCrashIDEnv + "=" + review.ID,
		setupDraftCrashRevisionEnv + "=" + review.BaseRevision,
		setupDraftCrashDigestEnv + "=" + review.Digest,
		"TEMP=" + childTemp,
		"TMP=" + childTemp,
	}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("could not start the isolated approval helper")
	}
	childFinished := false
	defer func() {
		if !childFinished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(barrierPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal("could not observe the approval barrier")
		}
		if time.Now().After(deadline) {
			t.Fatal("approval helper did not reach the post-claim, pre-CAS barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}

	claimedQueue := setupDraftCrashQueue(t, queuePath)
	remaining, err := claimedQueue.Load(time.Now().UTC())
	if err != nil || len(remaining) != 0 {
		t.Fatal("the approval barrier was reached without a durable one-shot claim")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal("could not terminate the owned approval helper at the barrier")
	}
	waitErr := command.Wait()
	childFinished = true
	if waitErr == nil {
		t.Fatal("the approval helper exited normally instead of being terminated at the barrier")
	}

	settingsAfterCrash, err := os.ReadFile(configPath)
	if err != nil || string(settingsAfterCrash) != string(initialBytes) {
		t.Fatal("settings changed before the approval compare-and-swap")
	}

	// A newly constructed controller models a fresh process reopening the same
	// durable queue and settings file. The claimed review must remain consumed.
	restartedQueue := setupDraftCrashQueue(t, queuePath)
	restartedStore := newSetupDraftConfigStore(configPath)
	restarted := newSetupDraftControllerWithQueue(restartedStore, restartedQueue)
	unchanged, err := restartedStore.Snapshot()
	if err != nil || unchanged.Revision != initial.Revision {
		t.Fatal("settings did not retain their pre-approval revision after restart")
	}
	if _, err := restarted.ApproveSetupDraft(context.Background(), review.ID, review.BaseRevision, review.Digest); !errors.Is(err, errSetupDraftInvalid) {
		t.Fatal("the claimed review was replayable after process restart")
	}

	newReview, err := restarted.SubmitSetupDraft(context.Background(), setupDraftCrashSubmission(unchanged.Revision))
	if err != nil || newReview.ID == review.ID {
		t.Fatal("a fresh review could not replace the consumed approval")
	}
	approved, err := restarted.ApproveSetupDraft(context.Background(), newReview.ID, newReview.BaseRevision, newReview.Digest)
	if err != nil || !approved.Applied {
		t.Fatal("a freshly reviewed and approved setup draft did not apply")
	}
	final, err := restartedStore.Snapshot()
	if err != nil || len(final.State.Connections) != 1 {
		t.Fatal("the fresh approval did not persist exactly one connection")
	}
	saved := final.State.Connections[0]
	if saved.Name != "Example repository" || saved.CredentialName != "Synthetic GitHub key" {
		t.Fatal("the fresh approval did not preserve the reviewed target and logical secret metadata")
	}
	finalQueue := setupDraftCrashQueue(t, queuePath)
	remaining, err = finalQueue.Load(time.Now().UTC())
	if err != nil || len(remaining) != 0 {
		t.Fatal("the applied fresh review remained available for replay")
	}
}

// TestSetupDraftApprovalCrashHelper blocks at the exact post-claim/pre-CAS
// Snapshot. The parent test force-terminates this process after seeing the
// fixed marker; timeout failure is closed and cannot proceed to CompareAndSwap.
func TestSetupDraftApprovalCrashHelper(t *testing.T) {
	if os.Getenv(setupDraftCrashRoleEnv) != "claim-before-cas" {
		t.Skip("helper entry point for the parent acceptance only")
	}
	configPath := os.Getenv(setupDraftCrashConfigEnv)
	queuePath := os.Getenv(setupDraftCrashQueueEnv)
	barrierPath := os.Getenv(setupDraftCrashBarrierEnv)
	id := os.Getenv(setupDraftCrashIDEnv)
	baseRevision := os.Getenv(setupDraftCrashRevisionEnv)
	digest := os.Getenv(setupDraftCrashDigestEnv)
	if configPath == "" || queuePath == "" || barrierPath == "" || id == "" || baseRevision == "" || digest == "" {
		t.Fatal("approval helper fixture is incomplete")
	}
	queue, err := newSetupDraftFileQueueStoreForTest(queuePath, time.Now)
	if err != nil {
		t.Fatal("could not reopen the isolated durable review queue")
	}
	store := &setupDraftCrashBlockingStore{
		delegate: newSetupDraftConfigStore(configPath), barrierPath: barrierPath,
	}
	controller := newSetupDraftControllerWithQueue(store, queue)
	_, err = controller.ApproveSetupDraft(context.Background(), id, baseRevision, digest)
	if err == nil {
		t.Fatal("approval unexpectedly returned from the pre-CAS barrier")
	}
	t.Fatal("approval helper left the claim-before-CAS barrier unexpectedly")
}

type setupDraftCrashBlockingStore struct {
	delegate    setupDraftStore
	snapshotN   int
	barrierPath string
}

func (s *setupDraftCrashBlockingStore) Snapshot() (setupDraftConfigSnapshot, error) {
	s.snapshotN++
	if s.snapshotN == 2 {
		if err := os.WriteFile(s.barrierPath, []byte("claimed-before-cas"), 0o600); err != nil {
			return setupDraftConfigSnapshot{}, errSetupDraftStore
		}
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		return setupDraftConfigSnapshot{}, errSetupDraftStore
	}
	return s.delegate.Snapshot()
}

func (s *setupDraftCrashBlockingStore) CompareAndSwap(expectedRevision string, next setupDraftState) (string, error) {
	return s.delegate.CompareAndSwap(expectedRevision, next)
}

func setupDraftCrashQueue(t *testing.T, path string) *setupDraftFileQueueStore {
	t.Helper()
	queue, err := newSetupDraftFileQueueStoreForTest(path, time.Now)
	if err != nil {
		t.Fatal("could not open the isolated durable setup review queue")
	}
	return queue
}

func setupDraftCrashSubmission(revision string) SetupDraftSubmission {
	submission := validSetupDraftSubmission(revision)
	submission.Connections[0].CredentialName = "Synthetic GitHub key"
	return submission
}
