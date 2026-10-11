package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const onboardingActivationTestCredential = "activation-test-credential"

func TestOnboardingPolicyReviewUsesFixedServiceScopesAndSeparateDigests(t *testing.T) {
	github := onboardingDraftFields{Name: "first", Origin: "https://github.example.invalid", Repository: "org/one"}
	githubReview, err := onboardingPolicyReviewForDraft(onboardingDraft{
		ID: "onboarding:" + strings.Repeat("a", 32), Revision: 1, Service: onboardingServiceGitHub, Fields: github,
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := github
	changed.Name = "second"
	changed.Repository = "org/two"
	changedCandidate, err := onboardingCandidateDigest(onboardingServiceGitHub, changed)
	if err != nil {
		t.Fatal(err)
	}
	if githubReview.PolicyDigest != onboardingPolicyDigest(onboardingServiceGitHub, changed) || githubReview.CandidateDigest == changedCandidate {
		t.Fatalf("candidate edit did not change only the candidate digest: review=%#v changed=%q", githubReview, changedCandidate)
	}
	if githubReview.PolicyDigest[:len("policy:sha256:")] != "policy:sha256:" {
		t.Fatalf("policy digest has noncanonical prefix: %q", githubReview.PolicyDigest)
	}
	if !reflect.DeepEqual(githubReview.IncludedTools, []string{"github_repository", "github_pull_request"}) ||
		len(githubReview.MappingRequired) != 0 || len(githubReview.ExcludedTools) != 0 {
		t.Fatalf("GitHub fixed read scope = %#v", githubReview)
	}

	jenkinsFields := onboardingDraftFields{Name: "ci", BaseURL: "https://jenkins.example.invalid", Username: "builder", JobPath: "team/job", Environment: "staging"}
	jenkinsReview, err := onboardingPolicyReviewForDraft(onboardingDraft{
		ID: "onboarding:" + strings.Repeat("b", 32), Revision: 1, Service: onboardingServiceJenkins, Fields: jenkinsFields,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jenkinsReview.IncludedTools, []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log"}) ||
		!strings.Contains(strings.Join(jenkinsReview.PolicyNotes, " "), "not preapproved") || jenkinsReview.PolicyDigest == githubReview.PolicyDigest {
		t.Fatalf("Jenkins fixed read scope = %#v", jenkinsReview)
	}

	dashboardReview, err := onboardingPolicyReviewForDraft(onboardingDraft{
		ID: "onboarding:" + strings.Repeat("c", 32), Revision: 1, Service: onboardingServiceDashboard,
		Fields: onboardingDraftFields{Name: "metrics", BaseURL: "https://dashboard.example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dashboardReview.IncludedTools, []string{"dashboard_namespaces"}) ||
		!reflect.DeepEqual(dashboardReview.MappingRequired, []string{
			"dashboard_deployment_status", "dashboard_deployment_diagnosis", "dashboard_deployment_events",
			"dashboard_deployment_pods", "dashboard_deployment_pod_logs",
		}) || !strings.Contains(strings.Join(dashboardReview.PolicyNotes, " "), "mappings") {
		t.Fatalf("Dashboard fixed read scope = %#v", dashboardReview)
	}
}

func TestOnboardingActivationPreservesExistingConfigV9Settings(t *testing.T) {
	base := sshConfigStoreTestConfig(t)
	credentialRef := base.NamedSecrets[0].CredentialRef
	secrets := memorySecrets{credentialRef: []byte("existing-stored-token")}
	harness := newOnboardingActivationHarness(t, base, secrets, onboardingServiceGitHub,
		onboardingDraftFields{Name: "Onboarded repository", Origin: "https://new-github.example.invalid", Repository: "org/new"})
	before, err := readConfig(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	request := harness.checkedRequest(t, onboardingActivationTestCredential)
	record, err := harness.controller.ActivateDraft(context.Background(), request)
	if err != nil {
		t.Fatalf("ActivateDraft() error = %v", err)
	}
	after, err := readConfig(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != configVersion || len(after.GitHubTargets) != len(before.GitHubTargets)+1 ||
		after.GitHubTargets[0] != before.GitHubTargets[0] || after.GitHubTargets[1].ID != record.TargetID ||
		after.GitHubTargets[1].Disabled || after.JenkinsTargets[0] != before.JenkinsTargets[0] ||
		!reflect.DeepEqual(after.HarborTargets, before.HarborTargets) || !reflect.DeepEqual(after.DashboardTargets, before.DashboardTargets) ||
		!reflect.DeepEqual(after.ServiceBundles, before.ServiceBundles) || !reflect.DeepEqual(after.PythonTasks, before.PythonTasks) ||
		!reflect.DeepEqual(after.SSHTargets, before.SSHTargets) || len(after.NamedSecrets) < len(before.NamedSecrets) ||
		after.NamedSecrets[0] != before.NamedSecrets[0] || !reflect.DeepEqual(after.ConnectionTests, before.ConnectionTests) {
		t.Fatalf("onboarding did not preserve unrelated config v9 state:\nbefore=%#v\nafter=%#v", before, after)
	}
	stored, err := secrets.Load(after.GitHubTargets[1].SecretRef)
	if err != nil || string(stored) != onboardingActivationTestCredential {
		clear(stored)
		t.Fatalf("saved credential = %q, %v", stored, err)
	}
	clear(stored)
	configBytes, err := os.ReadFile(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	storeBytes, err := os.ReadFile(harness.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configBytes, []byte(onboardingActivationTestCredential)) ||
		bytes.Contains(storeBytes, []byte(onboardingActivationTestCredential)) || bytes.Contains(storeBytes, []byte(request.CheckPermit)) {
		t.Fatal("activation credential or transient check permit was persisted in config or sidecar storage")
	}
}

func TestOnboardingActivationCreatesFixedScopeTargetsForEveryService(t *testing.T) {
	tests := []struct {
		name    string
		service onboardingServiceKind
		fields  onboardingDraftFields
		verify  func(*testing.T, config, onboardingActivationRecord, memorySecrets)
	}{
		{
			name: "GitHub", service: onboardingServiceGitHub,
			fields: onboardingDraftFields{Name: "repo", Origin: "https://github.example.invalid", Repository: "org/repo"},
			verify: func(t *testing.T, cfg config, record onboardingActivationRecord, secrets memorySecrets) {
				t.Helper()
				if len(cfg.GitHubTargets) != 1 || cfg.GitHubTargets[0].ID != record.TargetID || cfg.GitHubTargets[0].Disabled {
					t.Fatalf("GitHub target not active under reserved ID: %#v", cfg.GitHubTargets)
				}
				assertActivationCredentialStored(t, secrets, cfg.GitHubTargets[0].SecretRef)
			},
		},
		{
			name: "Jenkins", service: onboardingServiceJenkins,
			fields: onboardingDraftFields{
				Name: "build", BaseURL: "https://jenkins.example.invalid", Username: "builder",
				JobPath: "team/job", Environment: "staging",
			},
			verify: func(t *testing.T, cfg config, record onboardingActivationRecord, secrets memorySecrets) {
				t.Helper()
				if len(cfg.JenkinsTargets) != 1 || cfg.JenkinsTargets[0].ID != record.TargetID || cfg.JenkinsTargets[0].Disabled ||
					cfg.JenkinsTargets[0].NonProductionPreapproved || cfg.JenkinsTargets[0].NonProductionApprovalScope != "" {
					t.Fatalf("Jenkins target not safely active under reserved ID: %#v", cfg.JenkinsTargets)
				}
				assertActivationCredentialStored(t, secrets, cfg.JenkinsTargets[0].SecretRef)
			},
		},
		{
			name: "Dashboard", service: onboardingServiceDashboard,
			fields: onboardingDraftFields{Name: "cluster", BaseURL: "https://dashboard.example.invalid"},
			verify: func(t *testing.T, cfg config, record onboardingActivationRecord, secrets memorySecrets) {
				t.Helper()
				if len(cfg.DashboardTargets) != 1 || cfg.DashboardTargets[0].ID != record.TargetID || cfg.DashboardTargets[0].Disabled {
					t.Fatalf("Dashboard target not active under reserved ID: %#v", cfg.DashboardTargets)
				}
				assertActivationCredentialStored(t, secrets, cfg.DashboardTargets[0].SecretRef)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, test.service, test.fields)
			request := harness.checkedRequest(t, onboardingActivationTestCredential)
			record, err := harness.controller.ActivateDraft(context.Background(), request)
			if err != nil {
				t.Fatalf("ActivateDraft() error = %v", err)
			}
			cfg, err := readConfig(harness.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Version != configVersion || record.Service != test.service || !validOnboardingTargetID(test.service, record.TargetID) {
				t.Fatalf("activation record or config version mismatch: record=%#v version=%d", record, cfg.Version)
			}
			test.verify(t, cfg, record, harness.secrets)
		})
	}
}

func assertActivationCredentialStored(t *testing.T, secrets memorySecrets, ref string) {
	t.Helper()
	stored, err := secrets.Load(ref)
	if err != nil || string(stored) != onboardingActivationTestCredential {
		clear(stored)
		t.Fatalf("saved credential = %q, %v", stored, err)
	}
	clear(stored)
}

func TestOnboardingActivationRejectsDifferentCredentialWithoutConsumingProof(t *testing.T) {
	harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, onboardingServiceGitHub,
		onboardingDraftFields{Name: "new target", Origin: "https://github.example.invalid", Repository: "org/repo"})
	request := harness.checkedRequest(t, onboardingActivationTestCredential)
	before, err := readConfig(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = harness.controller.ActivateDraft(context.Background(), onboardingActivationRequest{
		CAS: request.CAS, PolicyConfirmed: true, CheckPermit: request.CheckPermit,
		Credential: []byte("different-activation-credential"),
	})
	if !errors.Is(err, errOnboardingCheckPermit) {
		t.Fatalf("wrong credential activation error = %v, want fresh check", err)
	}
	if strings.Contains(err.Error(), "different-activation-credential") {
		t.Fatalf("activation error disclosed the supplied credential: %v", err)
	}
	after, err := readConfig(harness.configPath)
	if err != nil || !reflect.DeepEqual(after, before) || len(harness.secrets) != 0 {
		t.Fatalf("wrong credential mutated settings or vault: before=%#v after=%#v secrets=%#v err=%v", before, after, harness.secrets, err)
	}
	request.Credential = []byte(onboardingActivationTestCredential)
	if _, err := harness.controller.ActivateDraft(context.Background(), request); err != nil {
		t.Fatalf("correct checked credential could not use the unconsumed proof: %v", err)
	}
}

func TestOnboardingActivationRequiresLatestCheckGeneration(t *testing.T) {
	harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, onboardingServiceGitHub,
		onboardingDraftFields{Name: "latest check", Origin: "https://github.example.invalid", Repository: "org/latest"})
	staleRequest := harness.checkedRequest(t, onboardingActivationTestCredential)
	before, err := readConfig(harness.configPath)
	if err != nil {
		t.Fatal(err)
	}

	latest, err := harness.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: harness.draft.ID, ExpectedRevision: harness.draft.Revision,
		Credential: []byte(onboardingActivationTestCredential),
	})
	if err != nil || latest.Receipt.CheckGeneration != staleRequest.CAS.CheckGeneration+1 || latest.Permit == "" {
		t.Fatalf("newer check = %#v, %v", latest, err)
	}
	_, err = harness.controller.ActivateDraft(context.Background(), staleRequest)
	if !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("activation with superseded generation error = %v, want fresh successful check", err)
	}
	afterStaleAttempt, err := readConfig(harness.configPath)
	if err != nil || !reflect.DeepEqual(afterStaleAttempt, before) || len(harness.secrets) != 0 {
		t.Fatalf("stale generation changed config or vault: before=%#v after=%#v secrets=%#v err=%v", before, afterStaleAttempt, harness.secrets, err)
	}

	latestRequest := onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: harness.draft.ID, ExpectedRevision: harness.draft.Revision,
			CheckGeneration: latest.Receipt.CheckGeneration,
			CandidateDigest: latest.Receipt.CandidateDigest, PolicyDigest: latest.Receipt.PolicyDigest,
			ConfigRevision: latest.Receipt.ConfigRevision,
		},
		PolicyConfirmed: true, CheckPermit: latest.Permit,
		Credential: []byte(onboardingActivationTestCredential),
	}
	if _, err := harness.controller.ActivateDraft(context.Background(), latestRequest); err != nil {
		t.Fatalf("latest generation's proof was not usable after stale activation: %v", err)
	}
}

func TestOnboardingConcurrentActivationCreatesOneTargetAndReturnsOneRecord(t *testing.T) {
	harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, onboardingServiceGitHub,
		onboardingDraftFields{Name: "concurrent target", Origin: "https://github.example.invalid", Repository: "org/concurrent"})
	request := harness.checkedRequest(t, onboardingActivationTestCredential)
	start := make(chan struct{})
	type result struct {
		record onboardingActivationRecord
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			activation := request
			activation.Credential = []byte(onboardingActivationTestCredential)
			record, err := harness.controller.ActivateDraft(context.Background(), activation)
			results <- result{record: record, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var first onboardingActivationRecord
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("concurrent ActivateDraft() error = %v", outcome.err)
		}
		if first.DraftID == "" {
			first = outcome.record
		} else if outcome.record != first {
			t.Fatalf("concurrent activation records differ: first=%#v next=%#v", first, outcome.record)
		}
	}
	after, err := readConfig(harness.configPath)
	if err != nil || len(after.GitHubTargets) != 1 || after.GitHubTargets[0].ID != first.TargetID || len(harness.secrets) != 1 {
		t.Fatalf("concurrent activation produced duplicates: targets=%#v secrets=%d err=%v", after.GitHubTargets, len(harness.secrets), err)
	}
	envelope, err := harness.store.readForTest(context.Background())
	if err != nil || len(envelope.Activations) != 1 || len(envelope.PendingActivations) != 0 || len(envelope.Drafts) != 0 {
		t.Fatalf("concurrent activation sidecar = %#v, %v", envelope, err)
	}
}

func TestOnboardingActivationNamedSecretMetadataMustStillMatch(t *testing.T) {
	const (
		secretID = "secret:" + "11111111111111111111111111111111"
		oldRef   = "cred:" + "22222222222222222222222222222222"
		newRef   = "cred:" + "33333333333333333333333333333333"
	)
	controller := &onboardingCheckController{secrets: memorySecrets{oldRef: []byte("named-token"), newRef: []byte("rotated-token")}}
	checkedMetadata := namedSecretMetadata{ID: secretID, Name: "Build token", Purpose: "test", CredentialRef: oldRef}
	checkedDigest, err := onboardingSecretMetadataDigest(checkedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	request := onboardingActivationRequest{NamedSecretID: secretID}
	proof := onboardingCheckProofInput{
		CredentialSource: onboardingCredentialNamed, NamedSecretID: secretID,
		SecretMetadataDigest: checkedDigest,
	}
	if controller.activationNamedSecretStillMatches(config{}, secretID, checkedDigest) ||
		controller.activationCredentialMatches(config{}, oldRef, onboardingServiceGitHub, request, proof, []byte("named-token")) {
		t.Fatal("missing named-secret metadata was accepted")
	}
	rotatedMetadata := namedSecretMetadata{ID: secretID, Name: "Build token", Purpose: "test", CredentialRef: newRef}
	rotatedConfig := config{NamedSecrets: []namedSecretMetadata{rotatedMetadata}}
	if controller.activationNamedSecretStillMatches(rotatedConfig, secretID, checkedDigest) ||
		controller.activationCredentialMatches(rotatedConfig, oldRef, onboardingServiceGitHub, request, proof, []byte("named-token")) {
		t.Fatal("rotated named-secret metadata was accepted for the old proof")
	}
}

func TestOnboardingActivationReleasesReservationForDuplicateNameAndStaleConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *onboardingActivationHarness)
		fields onboardingDraftFields
	}{
		{
			name: "duplicate name",
			fields: onboardingDraftFields{
				Name: "GitHub fixture", Origin: "https://other-github.example.invalid", Repository: "org/new",
			},
		},
		{
			name: "config changed after check",
			mutate: func(t *testing.T, harness *onboardingActivationHarness) {
				t.Helper()
				cfg, err := readConfig(harness.configPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg.GitHubTargets[0].Name = "GitHub fixture updated"
				if err := writeConfig(harness.configPath, cfg); err != nil {
					t.Fatal(err)
				}
			},
			fields: onboardingDraftFields{
				Name: "Onboarding target", Origin: "https://other-github.example.invalid", Repository: "org/new",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := sshConfigStoreTestConfig(t)
			secrets := memorySecrets{base.NamedSecrets[0].CredentialRef: []byte("existing-stored-token")}
			harness := newOnboardingActivationHarness(t, base, secrets, onboardingServiceGitHub, test.fields)
			request := harness.checkedRequest(t, onboardingActivationTestCredential)
			before, err := readConfig(harness.configPath)
			if err != nil {
				t.Fatal(err)
			}
			secretCount := len(secrets)
			if test.mutate != nil {
				test.mutate(t, &harness)
				before, err = readConfig(harness.configPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, activationErr := harness.controller.ActivateDraft(context.Background(), request)
			if activationErr == nil {
				t.Fatal("ActivateDraft() unexpectedly succeeded")
			}
			after, err := readConfig(harness.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) || len(secrets) != secretCount {
				t.Fatalf("failed activation mutated config or vault: before=%#v after=%#v secrets=%#v error=%v", before, after, secrets, activationErr)
			}
			updatedFields := harness.draft.Fields
			updatedFields.Name += " revised"
			updated, err := harness.store.UpdateDraft(context.Background(), harness.draft.ID, harness.draft.Revision, updatedFields)
			if err != nil || updated.Revision != harness.draft.Revision+1 {
				t.Fatalf("UpdateDraft() after safe failure = %#v, %v; want editable draft", updated, err)
			}
			if err := harness.store.DeleteDraft(context.Background(), updated.ID, updated.Revision); err != nil {
				t.Fatalf("DeleteDraft() after safe failure = %v", err)
			}
		})
	}
}

func TestOnboardingActivationAmbiguousSaveRequiresManualRecoveryAfterRestart(t *testing.T) {
	harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, onboardingServiceGitHub,
		onboardingDraftFields{Name: "ambiguous save", Origin: "https://github.example.invalid", Repository: "org/ambiguous"})

	originalSave := saveOnboardingActivationTarget
	t.Cleanup(func() { saveOnboardingActivationTarget = originalSave })
	saveCalls := 0
	orphanRef := ""
	saveOnboardingActivationTarget = func(
		controller *onboardingCheckController,
		stableTargetID string,
		_ onboardingServiceKind,
		_ onboardingDraftFields,
		_ onboardingActivationRequest,
		credential []byte,
	) (string, error) {
		saveCalls++
		orphanRef = "cred:" + strings.Repeat(string(rune('a'+saveCalls)), 32)
		if err := controller.secrets.Save(orphanRef, credential); err != nil {
			return "", err
		}
		// Match the save helpers' ambiguous failure shape: cleanup failed, so
		// they return the reserved target ID even though settings stayed absent.
		return stableTargetID, errors.New("unused credential could not be removed")
	}

	firstRequest := harness.checkedRequest(t, onboardingActivationTestCredential)
	_, err := harness.controller.ActivateDraft(context.Background(), firstRequest)
	if !errors.Is(err, errOnboardingActivationRecoveryRequired) || saveCalls != 1 || len(harness.secrets) != 1 {
		t.Fatalf("ambiguous activation error=%v save calls=%d orphan secrets=%d", err, saveCalls, len(harness.secrets))
	}
	if strings.Contains(err.Error(), onboardingActivationTestCredential) {
		t.Fatalf("manual recovery error exposed credential material: %v", err)
	}
	configAfterFailure, err := readConfig(harness.configPath)
	if err != nil || len(configAfterFailure.GitHubTargets) != 0 {
		t.Fatalf("ambiguous helper unexpectedly committed a target: %#v, %v", configAfterFailure.GitHubTargets, err)
	}
	envelope, err := harness.store.readForTest(context.Background())
	if err != nil || len(envelope.PendingActivations) != 1 || !envelope.PendingActivations[0].RecoveryRequired ||
		len(envelope.Drafts) != 1 || len(envelope.Activations) != 0 {
		t.Fatalf("sidecar did not persist the manual-recovery gate: %#v, %v", envelope, err)
	}
	sidecarBytes, err := os.ReadFile(harness.store.path)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenValues := [][]byte{
		[]byte(onboardingActivationTestCredential), []byte(orphanRef), []byte(firstRequest.CheckPermit),
	}
	for _, forbidden := range forbiddenValues {
		if bytes.Contains(sidecarBytes, forbidden) {
			t.Fatal("sidecar persisted recovery-sensitive value")
		}
	}
	reservedID := envelope.PendingActivations[0].TargetID

	// Reopen the sidecar twice. The durable gate blocks new checks and activation
	// callbacks, so neither restart can create another orphan credential.
	for attempt := 1; attempt <= 2; attempt++ {
		reopenedStore, err := newOnboardingFileDraftStoreForTest(harness.store.path, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := reopenedStore.GetDraft(context.Background(), harness.draft.ID)
		if err != nil || !draft.ActivationRecoveryRequired {
			t.Fatalf("reopened draft recovery state %d = %#v, %v", attempt, draft, err)
		}
		checkRequests := 0
		checkClient := &http.Client{Transport: onboardingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			checkRequests++
			return nil, errors.New("unexpected request after ambiguous recovery")
		})}
		controller := newOnboardingCheckController(
			reopenedStore, harness.configPath, harness.secrets, checkClient, newOnboardingCheckProofRegistryMemory(),
		)
		_, err = controller.CheckDraft(context.Background(), onboardingCheckRequest{
			DraftID: draft.ID, ExpectedRevision: draft.Revision,
			Credential: []byte(onboardingActivationTestCredential),
		})
		if !errors.Is(err, errOnboardingActivationRecoveryRequired) || checkRequests != 0 {
			t.Fatalf("CheckDraft() after ambiguous recovery %d = %v, HTTP calls=%d", attempt, err, checkRequests)
		}
		callbackCalls := 0
		_, err = reopenedStore.WithActivation(context.Background(), firstRequest.CAS, func(onboardingDraft, string) error {
			callbackCalls++
			return nil
		})
		if !errors.Is(err, errOnboardingActivationRecoveryRequired) || callbackCalls != 0 || saveCalls != 1 || len(harness.secrets) != 1 {
			t.Fatalf("retry %d error=%v callback calls=%d save calls=%d orphan secrets=%d", attempt, err, callbackCalls, saveCalls, len(harness.secrets))
		}
		envelope, err = reopenedStore.readForTest(context.Background())
		if err != nil || len(envelope.PendingActivations) != 1 || !envelope.PendingActivations[0].RecoveryRequired ||
			envelope.PendingActivations[0].TargetID != reservedID {
			t.Fatalf("recovery gate/reservation changed on retry %d: %#v, %v", attempt, envelope.PendingActivations, err)
		}
	}
}

func TestOnboardingActivationRecoversAfterRestartWithFreshCheckAndStableTargetID(t *testing.T) {
	harness := newOnboardingActivationHarness(t, config{Version: configVersion}, memorySecrets{}, onboardingServiceGitHub,
		onboardingDraftFields{Name: "recover target", Origin: "https://github.example.invalid", Repository: "org/recover"})
	firstRequest := harness.checkedRequest(t, onboardingActivationTestCredential)
	writes := 0
	harness.store.writeFileAtomically = func(path string, data []byte) error {
		writes++
		if writes == 2 {
			return errors.New("sidecar finalize failure")
		}
		return writeOnboardingStoreFileAtomically(path, data)
	}
	_, err := harness.controller.ActivateDraft(context.Background(), firstRequest)
	if !errors.Is(err, errOnboardingActivationPending) || writes != 2 {
		t.Fatalf("first ActivateDraft() error=%v sidecar writes=%d, want pending finalize failure", err, writes)
	}
	envelope, err := harness.store.readForTest(context.Background())
	if err != nil || len(envelope.PendingActivations) != 1 || len(envelope.Drafts) != 1 || len(envelope.Activations) != 0 {
		t.Fatalf("sidecar after finalize failure = %#v, %v", envelope, err)
	}
	reservedID := envelope.PendingActivations[0].TargetID
	cfgAfterCommit, err := readConfig(harness.configPath)
	if err != nil || len(cfgAfterCommit.GitHubTargets) != 1 || cfgAfterCommit.GitHubTargets[0].ID != reservedID {
		t.Fatalf("reserved target after config commit = %#v, %v", cfgAfterCommit.GitHubTargets, err)
	}

	// Simulate a process restart: reopen the durable sidecar and create a new
	// proof registry. The old permit is gone, so recovery must check again.
	restartedStore, err := newOnboardingFileDraftStoreForTest(harness.store.path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	proofs := newOnboardingCheckProofRegistryMemory()
	if proofs == nil {
		t.Fatal("could not create fresh proof registry")
	}
	controller := newOnboardingCheckController(restartedStore, harness.configPath, harness.secrets, onboardingActivationTestClient(), proofs)
	check, err := controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: harness.draft.ID, ExpectedRevision: harness.draft.Revision,
		Credential: []byte(onboardingActivationTestCredential),
	})
	if err != nil || check.Permit == "" || check.Receipt.ConfigRevision == firstRequest.CAS.ConfigRevision ||
		check.Receipt.CheckGeneration <= firstRequest.CAS.CheckGeneration {
		t.Fatalf("fresh recovery check = %#v, %v", check, err)
	}
	recoveryRequest := onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: harness.draft.ID, ExpectedRevision: harness.draft.Revision,
			CheckGeneration: check.Receipt.CheckGeneration,
			CandidateDigest: check.Receipt.CandidateDigest, PolicyDigest: check.Receipt.PolicyDigest,
			ConfigRevision: check.Receipt.ConfigRevision,
		},
		PolicyConfirmed: true, CheckPermit: check.Permit,
		Credential: []byte(onboardingActivationTestCredential),
	}
	record, err := controller.ActivateDraft(context.Background(), recoveryRequest)
	if err != nil || record.TargetID != reservedID || record.ConfigRevision != check.Receipt.ConfigRevision {
		t.Fatalf("recovered ActivateDraft() = %#v, %v; reserved ID %q", record, err, reservedID)
	}
	after, err := readConfig(harness.configPath)
	if err != nil || len(after.GitHubTargets) != 1 || after.GitHubTargets[0].ID != reservedID || len(harness.secrets) != 1 {
		t.Fatalf("recovery duplicated target/credential: targets=%#v secrets=%#v err=%v", after.GitHubTargets, harness.secrets, err)
	}

	replayed, err := controller.ActivateDraft(context.Background(), onboardingActivationRequest{
		CAS: recoveryRequest.CAS, PolicyConfirmed: true, CheckPermit: recoveryRequest.CheckPermit,
		Credential: []byte(onboardingActivationTestCredential),
	})
	if err != nil || replayed != record {
		t.Fatalf("completed activation replay = %#v, %v; want %#v", replayed, err, record)
	}
	wrongRevision := recoveryRequest
	wrongRevision.Credential = []byte(onboardingActivationTestCredential)
	wrongRevision.CAS.ExpectedRevision++
	if _, err := controller.ActivateDraft(context.Background(), wrongRevision); !errors.Is(err, errOnboardingReviewChanged) {
		t.Fatalf("replay with a different draft revision error = %v, want review changed", err)
	}
	wrongGeneration := recoveryRequest
	wrongGeneration.Credential = []byte(onboardingActivationTestCredential)
	wrongGeneration.CAS.CheckGeneration++
	if _, err := controller.ActivateDraft(context.Background(), wrongGeneration); !errors.Is(err, errOnboardingReviewChanged) {
		t.Fatalf("replay with a different check generation error = %v, want review changed", err)
	}
	if err := withConfigLock(func() error {
		current, readErr := readConfig(harness.configPath)
		if readErr != nil {
			return readErr
		}
		current.GitHubTargets[0].Name = "manually changed target"
		return writeConfig(harness.configPath, current)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = controller.ActivateDraft(context.Background(), onboardingActivationRequest{
		CAS: recoveryRequest.CAS, PolicyConfirmed: true, CheckPermit: recoveryRequest.CheckPermit,
		Credential: []byte(onboardingActivationTestCredential),
	})
	if !errors.Is(err, errOnboardingReviewChanged) {
		t.Fatalf("replay after target edit error = %v, want review changed", err)
	}
}

type onboardingActivationHarness struct {
	controller *onboardingCheckController
	store      *onboardingFileDraftStore
	secrets    memorySecrets
	draft      onboardingDraft
	configPath string
}

func newOnboardingActivationHarness(
	t *testing.T,
	cfg config,
	secrets memorySecrets,
	service onboardingServiceKind,
	fields onboardingDraftFields,
) onboardingActivationHarness {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatalf("write activation test config: %v", err)
	}
	return newOnboardingActivationHarnessAt(t, configPath, onboardingTestStorePath(t), secrets, service, fields)
}

func newOnboardingActivationHarnessAt(
	t *testing.T,
	configPath string,
	storePath string,
	secrets memorySecrets,
	service onboardingServiceKind,
	fields onboardingDraftFields,
) onboardingActivationHarness {
	t.Helper()
	store, err := newOnboardingFileDraftStoreForTest(storePath, time.Now)
	if err != nil {
		t.Fatalf("create activation store: %v", err)
	}
	draft, err := store.CreateDraft(context.Background(), service, fields)
	if err != nil {
		t.Fatalf("create activation draft: %v", err)
	}
	proofs := newOnboardingCheckProofRegistryMemory()
	if proofs == nil {
		t.Fatal("create activation proof registry")
	}
	return onboardingActivationHarness{
		controller: newOnboardingCheckController(store, configPath, secrets, onboardingActivationTestClient(), proofs),
		store:      store, secrets: secrets, draft: draft, configPath: configPath,
	}
}

func (h onboardingActivationHarness) checkedRequest(t *testing.T, credential string) onboardingActivationRequest {
	t.Helper()
	check, err := h.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: h.draft.ID, ExpectedRevision: h.draft.Revision, Credential: []byte(credential),
	})
	if err != nil || check.Receipt.Outcome != onboardingCheckSucceeded || check.Permit == "" {
		t.Fatalf("CheckDraft() = %#v, %v", check, err)
	}
	review, err := h.controller.ReviewDraft(context.Background(), h.draft.ID, h.draft.Revision)
	if err != nil || review.CandidateDigest != check.Receipt.CandidateDigest || review.PolicyDigest != check.Receipt.PolicyDigest ||
		review.ConfigRevision != check.Receipt.ConfigRevision || review.CheckGeneration != check.Receipt.CheckGeneration {
		t.Fatalf("ReviewDraft() = %#v, %v; receipt=%#v", review, err, check.Receipt)
	}
	return onboardingActivationRequest{
		CAS: onboardingActivationCAS{
			DraftID: h.draft.ID, ExpectedRevision: h.draft.Revision,
			CheckGeneration: check.Receipt.CheckGeneration,
			CandidateDigest: review.CandidateDigest, PolicyDigest: review.PolicyDigest,
			ConfigRevision: review.ConfigRevision,
		},
		PolicyConfirmed: true, CheckPermit: check.Permit, Credential: []byte(credential),
	}
}

func onboardingActivationTestClient() *http.Client {
	return &http.Client{Transport: onboardingRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		var responseBody string
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/v3/repos/"):
			fullName := strings.TrimPrefix(request.URL.Path, "/api/v3/repos/")
			name := fullName
			if index := strings.LastIndexByte(name, '/'); index >= 0 {
				name = name[index+1:]
			}
			responseBody = `{"name":"` + name + `","full_name":"` + fullName + `"}`
		case strings.HasSuffix(request.URL.Path, "/api/json"):
			responseBody = `{"name":"job","fullName":"team/job","buildable":true}`
		case strings.HasSuffix(request.URL.Path, "/api/v1/namespace"):
			responseBody = `{"listMeta":{"totalItems":0},"namespaces":[]}`
		default:
			return nil, errors.New("unexpected onboarding check endpoint")
		}
		body := io.NopCloser(strings.NewReader(responseBody))
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: request,
		}, nil
	})}
}
