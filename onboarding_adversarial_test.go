package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// onboardingAmbiguousCleanupSecrets makes the first config replacement fail
// after saving a credential, then simulates a vault deletion failure. Restoring
// the config file lets a restarted controller exercise the durable recovery gate.
type onboardingAmbiguousCleanupSecrets struct {
	memorySecrets
	configPath string
	backupPath string
	saves      int
	deletes    int
}

func (s *onboardingAmbiguousCleanupSecrets) Save(ref string, value []byte) error {
	s.saves++
	if err := s.memorySecrets.Save(ref, value); err != nil {
		return err
	}
	if s.saves != 1 {
		return nil
	}
	if err := os.Rename(s.configPath, s.backupPath); err != nil {
		return err
	}
	return os.Mkdir(s.configPath, 0o700)
}

func (s *onboardingAmbiguousCleanupSecrets) Load(ref string) ([]byte, error) {
	return s.memorySecrets.Load(ref)
}

func (s *onboardingAmbiguousCleanupSecrets) Delete(string) error {
	s.deletes++
	return errors.New("injected vault cleanup failure")
}

type onboardingCountingRoundTripper struct {
	delegate http.RoundTripper
	calls    int
}

func (r *onboardingCountingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	r.calls++
	return r.delegate.RoundTrip(request)
}

func TestOnboardingAmbiguousSecretCleanupDoesNotCreateAnotherCredentialOnRecovery(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	storePath := onboardingTestStorePath(t)
	store, err := newOnboardingFileDraftStoreForTest(storePath, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.CreateDraft(context.Background(), onboardingServiceGitHub, onboardingDraftFields{
		Name: "recover cleanup failure", Origin: "https://github.example.invalid", Repository: "org/recover",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := &onboardingAmbiguousCleanupSecrets{
		memorySecrets: memorySecrets{}, configPath: configPath,
		backupPath: filepath.Join(filepath.Dir(configPath), "config-before-fault.json"),
	}
	transport := &onboardingCountingRoundTripper{delegate: onboardingActivationTestClient().Transport}
	client := &http.Client{Transport: transport}
	proofs := newOnboardingCheckProofRegistryMemory()
	controller := newOnboardingCheckController(store, configPath, secrets, client, proofs)
	firstCheck, err := controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: draft.ID, ExpectedRevision: draft.Revision, Credential: []byte(onboardingActivationTestCredential),
	})
	if err != nil || firstCheck.Permit == "" {
		t.Fatalf("initial CheckDraft() = %#v, %v", firstCheck, err)
	}
	writes := 0
	store.writeFileAtomically = func(path string, data []byte) error {
		writes++
		if writes == 2 {
			// Reservation write #1 must survive. Fail the subsequent durable
			// RecoveryRequired marker write after the helper reports ambiguity.
			return errors.New("injected recovery marker write failure")
		}
		return writeOnboardingStoreFileAtomically(path, data)
	}
	firstRequest := onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: draft.ID, ExpectedRevision: draft.Revision,
			CheckGeneration: firstCheck.Receipt.CheckGeneration,
			CandidateDigest: firstCheck.Receipt.CandidateDigest, PolicyDigest: firstCheck.Receipt.PolicyDigest,
			ConfigRevision: firstCheck.Receipt.ConfigRevision,
		},
		PolicyConfirmed: true, CheckPermit: firstCheck.Permit,
		Credential: []byte(onboardingActivationTestCredential),
	}
	if _, err := controller.ActivateDraft(context.Background(), firstRequest); err == nil || writes != 2 {
		t.Fatalf("activation error = %v, sidecar writes=%d; want marker persistence failure", err, writes)
	}
	if secrets.saves != 1 || secrets.deletes != 1 || len(secrets.memorySecrets) != 1 {
		t.Fatalf("first failed activation: saves=%d deletes=%d stored credentials=%d", secrets.saves, secrets.deletes, len(secrets.memorySecrets))
	}
	envelope, err := store.readForTest(context.Background())
	if err != nil || len(envelope.PendingActivations) != 1 || envelope.PendingActivations[0].RecoveryRequired {
		t.Fatalf("failed marker write unexpectedly changed durable gate = %#v, %v", envelope.PendingActivations, err)
	}

	// The fault leaves a directory at configPath so writeConfig cannot replace
	// it. Restore the original readable config before simulating process restart.
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(secrets.backupPath, configPath); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := newOnboardingFileDraftStoreForTest(storePath, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	restartedController := newOnboardingCheckController(
		restartedStore, configPath, secrets, client, newOnboardingCheckProofRegistryMemory(),
	)
	beforeBlockedCheck, err := restartedStore.GetDraft(context.Background(), draft.ID)
	if err != nil || beforeBlockedCheck.ActivationRecoveryRequired || beforeBlockedCheck.CheckGeneration != firstCheck.Receipt.CheckGeneration {
		t.Fatalf("restarted draft recovery state = %#v, %v", beforeBlockedCheck, err)
	}
	check, err := restartedController.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: draft.ID, ExpectedRevision: draft.Revision, Credential: []byte(onboardingActivationTestCredential),
	})
	if err != nil || check.Permit == "" || check.Receipt.CheckGeneration != firstCheck.Receipt.CheckGeneration+1 {
		t.Fatalf("fresh recovery CheckDraft() = %#v, %v", check, err)
	}
	request := onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: draft.ID, ExpectedRevision: draft.Revision,
			CheckGeneration: check.Receipt.CheckGeneration,
			CandidateDigest: check.Receipt.CandidateDigest, PolicyDigest: check.Receipt.PolicyDigest,
			ConfigRevision: check.Receipt.ConfigRevision,
		},
		PolicyConfirmed: true, CheckPermit: check.Permit,
		Credential: []byte(onboardingActivationTestCredential),
	}
	if _, err := restartedController.ActivateDraft(context.Background(), request); !errors.Is(err, errOnboardingActivationRecoveryRequired) {
		t.Fatalf("activation retry with absent reserved target = %v, want manual recovery required", err)
	}
	if transport.calls != 2 {
		t.Fatalf("initial and fresh-check recovery made %d HTTP requests, want 2", transport.calls)
	}
	afterBlockedCheck, err := restartedStore.GetDraft(context.Background(), draft.ID)
	if err != nil || !afterBlockedCheck.ActivationRecoveryRequired ||
		afterBlockedCheck.CheckGeneration != check.Receipt.CheckGeneration || afterBlockedCheck.Check == nil ||
		afterBlockedCheck.Check.CheckGeneration != check.Receipt.CheckGeneration {
		t.Fatalf("manual-recovery retry state = %#v, %v", afterBlockedCheck, err)
	}
	callbackCalls := 0
	if _, err := restartedStore.WithActivation(context.Background(), request.CAS, func(onboardingDraft, string) error {
		callbackCalls++
		return nil
	}); !errors.Is(err, errOnboardingActivationRecoveryRequired) || callbackCalls != 0 {
		t.Fatalf("gated WithActivation() error=%v callback calls=%d", err, callbackCalls)
	}

	if secrets.saves != 1 || len(secrets.memorySecrets) != 1 {
		t.Fatalf("recovery created another credential: save calls=%d stored credentials=%d", secrets.saves, len(secrets.memorySecrets))
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GitHubTargets) > 1 {
		t.Fatalf("recovery created duplicate targets: %#v", cfg.GitHubTargets)
	}
	if len(cfg.GitHubTargets) == 1 {
		if _, err := secrets.Load(cfg.GitHubTargets[0].SecretRef); err != nil {
			t.Fatalf("recovered target refers to an unavailable credential: %v", err)
		}
	}
}
