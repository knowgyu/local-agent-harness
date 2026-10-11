package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	onboardingAcceptanceCSRF       = "onboarding-acceptance-csrf-canary"
	onboardingAcceptanceCredential = "SYNTHETIC_QA_ONBOARDING_TOKEN_20261011"
)

type onboardingAcceptanceVault struct {
	mu      sync.Mutex
	values  map[string][]byte
	saves   int
	loads   int
	deletes int
}

func newOnboardingAcceptanceVault() *onboardingAcceptanceVault {
	return &onboardingAcceptanceVault{values: make(map[string][]byte)}
}

func (v *onboardingAcceptanceVault) Save(ref string, value []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[ref] = append([]byte(nil), value...)
	v.saves++
	return nil
}

func (v *onboardingAcceptanceVault) Load(ref string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loads++
	value, ok := v.values[ref]
	if !ok {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), value...), nil
}

func (v *onboardingAcceptanceVault) Delete(ref string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, ref)
	v.deletes++
	return nil
}

func (v *onboardingAcceptanceVault) snapshot() (map[string][]byte, int, int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	values := make(map[string][]byte, len(v.values))
	for ref, value := range v.values {
		values[ref] = append([]byte(nil), value...)
	}
	return values, v.saves, v.loads, v.deletes
}

type onboardingAcceptanceHTTPResult struct {
	status int
	body   []byte
}

func startOnboardingAcceptanceUI(t *testing.T, configPath string, vault secretStore, client *http.Client) (*app, *httptest.Server, *http.Client) {
	t.Helper()
	a := &app{configPath: configPath, secrets: vault, client: client, csrf: onboardingAcceptanceCSRF}
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)
	uiClient := server.Client()
	uiClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return a, server, uiClient
}

func doOnboardingAcceptanceUI(
	t *testing.T,
	client *http.Client,
	baseURL, method, path string,
	body any,
) onboardingAcceptanceHTTPResult {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequest(method, baseURL+path, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", baseURL)
		request.Header.Set("X-CSRF-Token", onboardingAcceptanceCSRF)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return onboardingAcceptanceHTTPResult{status: response.StatusCode, body: data}
}

func requireOnboardingAcceptanceStatus(t *testing.T, result onboardingAcceptanceHTTPResult, want int) {
	t.Helper()
	if result.status != want {
		t.Fatalf("onboarding UI status=%d, want %d; body=%s", result.status, want, result.body)
	}
}

func requireOnboardingAcceptanceError(t *testing.T, result onboardingAcceptanceHTTPResult, wantStatus int, wantCode string) {
	t.Helper()
	requireOnboardingAcceptanceStatus(t, result, wantStatus)
	var response onboardingUIErrorDTO
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode onboarding error response: %v; body=%s", err, result.body)
	}
	if response.ErrorCode != wantCode || response.Error != wantCode {
		t.Fatalf("onboarding error=%+v, want code %q", response, wantCode)
	}
}

func onboardingAcceptanceReadConfigBytes(t *testing.T, path string) ([]byte, config) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, cfg
}

func onboardingAcceptanceStorePath(t *testing.T, a *app) string {
	t.Helper()
	controller, ok := a.onboarding.(*onboardingAppController)
	if !ok || controller.store == nil {
		t.Fatal("local UI did not initialize its onboarding controller")
	}
	store, ok := controller.store.(*onboardingFileDraftStore)
	if !ok {
		t.Fatal("local UI did not use the config-scoped durable draft store")
	}
	return store.path
}

func decodeOnboardingAcceptanceDraft(t *testing.T, result onboardingAcceptanceHTTPResult) onboardingDraft {
	t.Helper()
	var response onboardingDraftResponseDTO
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode draft response: %v; body=%s", err, result.body)
	}
	return response.Draft
}

func decodeOnboardingAcceptanceCheck(t *testing.T, result onboardingAcceptanceHTTPResult) onboardingDraftCheckResponseDTO {
	t.Helper()
	var response onboardingDraftCheckResponseDTO
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode check response: %v; body=%s", err, result.body)
	}
	return response
}

func decodeOnboardingAcceptanceReview(t *testing.T, result onboardingAcceptanceHTTPResult) onboardingPolicyReview {
	t.Helper()
	var response onboardingPolicyReviewResponseDTO
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode policy review response: %v; body=%s", err, result.body)
	}
	return response.Review
}

func onboardingAcceptanceActivationRequest(
	revision uint64,
	checkGeneration uint64,
	review onboardingPolicyReview,
	permit, credential string,
	confirmed bool,
) onboardingDraftActivateRequestDTO {
	return onboardingDraftActivateRequestDTO{
		ExpectedRevision: revision,
		CheckGeneration:  checkGeneration,
		CandidateDigest:  review.CandidateDigest,
		PolicyDigest:     review.PolicyDigest,
		ConfigRevision:   review.ConfigRevision,
		PolicyConfirmed:  confirmed,
		CheckPermit:      permit,
		Credential:       credential,
	}
}

func assertOnboardingAcceptanceDraftStateUnchanged(
	t *testing.T,
	configPath string,
	wantConfig []byte,
	vault *onboardingAcceptanceVault,
	wantVault map[string][]byte,
	wantSaves, wantLoads, wantDeletes int,
) {
	t.Helper()
	gotConfig, _ := onboardingAcceptanceReadConfigBytes(t, configPath)
	if !reflect.DeepEqual(gotConfig, wantConfig) {
		t.Fatal("ordinary onboarding draft operations changed the active settings bytes")
	}
	gotVault, saves, loads, deletes := vault.snapshot()
	if !reflect.DeepEqual(gotVault, wantVault) || saves != wantSaves || loads != wantLoads || deletes != wantDeletes {
		t.Fatalf("ordinary onboarding draft operations changed the credential store: refs=%d/%d saves=%d/%d loads=%d/%d deletes=%d/%d",
			len(gotVault), len(wantVault), saves, wantSaves, loads, wantLoads, deletes, wantDeletes)
	}
}

func assertOnboardingActivationPreservesConfig(
	t *testing.T,
	before, after config,
	record onboardingActivationRecord,
	name, origin, repository string,
) target {
	t.Helper()
	if len(after.GitHubTargets) != len(before.GitHubTargets)+1 {
		t.Fatalf("GitHub target count=%d, want one addition to %d", len(after.GitHubTargets), len(before.GitHubTargets))
	}
	for _, prior := range before.GitHubTargets {
		index := findGitHubTargetIndex(after.GitHubTargets, prior.ID)
		if index < 0 || after.GitHubTargets[index] != prior {
			t.Fatalf("activation changed or removed existing GitHub target %q", prior.Name)
		}
	}
	index := findGitHubTargetIndex(after.GitHubTargets, record.TargetID)
	if index < 0 {
		t.Fatalf("activation target %q is missing from settings", record.TargetID)
	}
	created := after.GitHubTargets[index]
	if created.Name != name || created.Origin != origin || created.Repository != repository || created.Disabled || !secretRefPattern.MatchString(created.SecretRef) {
		t.Fatalf("activated target=%+v, candidate name/origin/repository or credential reference did not match", created)
	}
	if !reflect.DeepEqual(after.JenkinsTargets, before.JenkinsTargets) ||
		!reflect.DeepEqual(after.HarborTargets, before.HarborTargets) ||
		!reflect.DeepEqual(after.DashboardTargets, before.DashboardTargets) ||
		!reflect.DeepEqual(after.ServiceBundles, before.ServiceBundles) ||
		!reflect.DeepEqual(after.ConnectionTests, before.ConnectionTests) ||
		!reflect.DeepEqual(after.PythonTasks, before.PythonTasks) ||
		!reflect.DeepEqual(after.SSHTargets, before.SSHTargets) {
		t.Fatal("activation changed unrelated targets, service groups, history, tasks, or SSH definitions")
	}
	metadataByID := make(map[string]namedSecretMetadata, len(after.NamedSecrets))
	for _, metadata := range after.NamedSecrets {
		metadataByID[metadata.ID] = metadata
	}
	for _, metadata := range before.NamedSecrets {
		if got, exists := metadataByID[metadata.ID]; !exists || got != metadata {
			t.Fatalf("activation changed or removed unrelated named-secret metadata %q", metadata.ID)
		}
	}
	foundNewReference := false
	for _, metadata := range after.NamedSecrets {
		if metadata.CredentialRef == created.SecretRef {
			foundNewReference = true
		}
	}
	if !foundNewReference {
		t.Fatal("activated credential reference is missing its settings metadata")
	}
	return created
}

func TestOnboardingDraftActivationAndFirstReadAcceptance(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	initial := connectionHistoryConfig()
	if err := writeConfig(configPath, initial); err != nil {
		t.Fatal(err)
	}
	configBeforeDrafts, initialConfig := onboardingAcceptanceReadConfigBytes(t, configPath)
	vault := newOnboardingAcceptanceVault()
	wantVault, wantSaves, wantLoads, wantDeletes := vault.snapshot()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v3/repos/onboarding/project" || r.URL.RawQuery != "" ||
			r.URL.User != nil || r.URL.Fragment != "" || r.Header.Get("Authorization") != "Bearer "+onboardingAcceptanceCredential {
			t.Errorf("synthetic upstream received an unexpected request: %s %s", r.Method, r.URL.Redacted())
			http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"project","full_name":"onboarding/project","private":true,"default_branch":"main"}`)
	}))
	t.Cleanup(upstream.Close)
	upstreamClient := upstream.Client()
	upstreamClient.Timeout = 15 * time.Second

	app1, ui1, client1 := startOnboardingAcceptanceUI(t, configPath, vault, upstreamClient)
	mainCreate := doOnboardingAcceptanceUI(t, client1, ui1.URL, http.MethodPost, onboardingDraftsPath,
		onboardingDraftCreateRequestDTO{Service: onboardingServiceGitHub, Fields: onboardingDraftFields{Name: "Onboarding Candidate"}})
	requireOnboardingAcceptanceStatus(t, mainCreate, http.StatusCreated)
	mainDraft := decodeOnboardingAcceptanceDraft(t, mainCreate)
	if mainDraft.Revision != 1 || mainDraft.CheckGeneration != 0 || mainDraft.Fields.Name != "Onboarding Candidate" {
		t.Fatalf("new draft=%+v", mainDraft)
	}
	candidate := onboardingDraftFields{Name: mainDraft.Fields.Name, Origin: upstream.URL, Repository: "onboarding/project"}
	mainUpdate := doOnboardingAcceptanceUI(t, client1, ui1.URL, http.MethodPut, onboardingDraftItemPath+mainDraft.ID,
		onboardingDraftUpdateRequestDTO{ExpectedRevision: mainDraft.Revision, Fields: candidate})
	requireOnboardingAcceptanceStatus(t, mainUpdate, http.StatusOK)
	mainDraft = decodeOnboardingAcceptanceDraft(t, mainUpdate)
	if mainDraft.Revision != 2 || mainDraft.Fields != candidate {
		t.Fatalf("updated draft=%+v", mainDraft)
	}

	disposableCreate := doOnboardingAcceptanceUI(t, client1, ui1.URL, http.MethodPost, onboardingDraftsPath,
		onboardingDraftCreateRequestDTO{Service: onboardingServiceGitHub, Fields: onboardingDraftFields{Name: "Disposable Candidate"}})
	requireOnboardingAcceptanceStatus(t, disposableCreate, http.StatusCreated)
	disposable := decodeOnboardingAcceptanceDraft(t, disposableCreate)
	disposableDelete := doOnboardingAcceptanceUI(t, client1, ui1.URL, http.MethodDelete, onboardingDraftItemPath+disposable.ID,
		onboardingDraftDeleteRequestDTO{ExpectedRevision: disposable.Revision})
	requireOnboardingAcceptanceStatus(t, disposableDelete, http.StatusOK)
	assertOnboardingAcceptanceDraftStateUnchanged(t, configPath, configBeforeDrafts, vault, wantVault, wantSaves, wantLoads, wantDeletes)

	storePath := onboardingAcceptanceStorePath(t, app1)
	ui1.Close()
	app2, ui2, client2 := startOnboardingAcceptanceUI(t, configPath, vault, upstreamClient)
	resumed := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodGet, onboardingDraftItemPath+mainDraft.ID, nil)
	requireOnboardingAcceptanceStatus(t, resumed, http.StatusOK)
	if got := onboardingAcceptanceStorePath(t, app2); got != storePath {
		t.Fatalf("app recreation selected a different draft sidecar: got %q, want %q", got, storePath)
	}
	resumedDraft := decodeOnboardingAcceptanceDraft(t, resumed)
	if resumedDraft.ID != mainDraft.ID || resumedDraft.Revision != mainDraft.Revision || resumedDraft.CheckGeneration != 0 || resumedDraft.Fields != candidate {
		t.Fatalf("draft did not resume exactly after app recreation: %+v", resumedDraft)
	}
	assertOnboardingAcceptanceDraftStateUnchanged(t, configPath, configBeforeDrafts, vault, wantVault, wantSaves, wantLoads, wantDeletes)

	check1Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/check",
		onboardingDraftCheckRequestDTO{ExpectedRevision: resumedDraft.Revision, Credential: onboardingAcceptanceCredential})
	requireOnboardingAcceptanceStatus(t, check1Result, http.StatusOK)
	check1 := decodeOnboardingAcceptanceCheck(t, check1Result)
	if check1.Receipt.Outcome != onboardingCheckSucceeded || check1.Receipt.CheckGeneration != 1 || check1.Permit == "" || upstreamCalls.Load() != 1 {
		t.Fatalf("temporary connection check did not make exactly one successful synthetic read: receipt=%+v calls=%d", check1.Receipt, upstreamCalls.Load())
	}
	if strings.Contains(string(check1Result.body), onboardingAcceptanceCredential) {
		t.Fatal("temporary credential appeared in the check response")
	}
	review1Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/policy-review",
		onboardingPolicyReviewRequestDTO{ExpectedRevision: resumedDraft.Revision})
	requireOnboardingAcceptanceStatus(t, review1Result, http.StatusOK)
	review1 := decodeOnboardingAcceptanceReview(t, review1Result)
	if review1.CheckGeneration != check1.Receipt.CheckGeneration || review1.CandidateDigest != check1.Receipt.CandidateDigest || review1.PolicyDigest != check1.Receipt.PolicyDigest ||
		review1.ConfigRevision != check1.Receipt.ConfigRevision {
		t.Fatalf("check receipt and policy review do not bind the same snapshot: check=%+v review=%+v", check1.Receipt, review1)
	}

	policyNotConfirmed := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate",
		onboardingAcceptanceActivationRequest(resumedDraft.Revision, check1.Receipt.CheckGeneration, review1, check1.Permit, onboardingAcceptanceCredential, false))
	requireOnboardingAcceptanceError(t, policyNotConfirmed, http.StatusBadRequest, "invalid_request")
	wrongCredential := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate",
		onboardingAcceptanceActivationRequest(resumedDraft.Revision, check1.Receipt.CheckGeneration, review1, check1.Permit, "SYNTHETIC_WRONG_ONBOARDING_TOKEN", true))
	requireOnboardingAcceptanceError(t, wrongCredential, http.StatusConflict, "check_permit_required")

	// Editing even a label advances the CAS revision and clears the prior check.
	candidate.Name = "Onboarding Candidate Revised"
	edited := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPut, onboardingDraftItemPath+mainDraft.ID,
		onboardingDraftUpdateRequestDTO{ExpectedRevision: resumedDraft.Revision, Fields: candidate})
	requireOnboardingAcceptanceStatus(t, edited, http.StatusOK)
	editedDraft := decodeOnboardingAcceptanceDraft(t, edited)
	if editedDraft.Revision != resumedDraft.Revision+1 || editedDraft.CheckGeneration != check1.Receipt.CheckGeneration || editedDraft.Check != nil {
		t.Fatalf("draft edit did not advance revision and clear the old check: %+v", editedDraft)
	}
	staleDraftActivation := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate",
		onboardingAcceptanceActivationRequest(resumedDraft.Revision, check1.Receipt.CheckGeneration, review1, check1.Permit, onboardingAcceptanceCredential, true))
	requireOnboardingAcceptanceError(t, staleDraftActivation, http.StatusConflict, "stale_revision")

	check2Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/check",
		onboardingDraftCheckRequestDTO{ExpectedRevision: editedDraft.Revision, Credential: onboardingAcceptanceCredential})
	requireOnboardingAcceptanceStatus(t, check2Result, http.StatusOK)
	check2 := decodeOnboardingAcceptanceCheck(t, check2Result)
	if check2.Receipt.CheckGeneration != check1.Receipt.CheckGeneration+1 {
		t.Fatalf("second check generation=%d, want %d", check2.Receipt.CheckGeneration, check1.Receipt.CheckGeneration+1)
	}
	review2Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/policy-review",
		onboardingPolicyReviewRequestDTO{ExpectedRevision: editedDraft.Revision})
	requireOnboardingAcceptanceStatus(t, review2Result, http.StatusOK)
	review2 := decodeOnboardingAcceptanceReview(t, review2Result)
	if review2.CheckGeneration != check2.Receipt.CheckGeneration || upstreamCalls.Load() != 2 {
		t.Fatalf("unexpected synthetic check request count=%d, want two", upstreamCalls.Load())
	}

	// A history-only active-settings change makes the prior check revision stale.
	changedHistory := initialConfig
	changedHistory.ConnectionTests = make(map[string]connectionTest, len(initialConfig.ConnectionTests))
	for id, check := range initialConfig.ConnectionTests {
		changedHistory.ConnectionTests[id] = check
	}
	changedHistory.ConnectionTests[initialConfig.GitHubTargets[0].ID] = connectionTest{
		Result: "failure", CompletedAt: "2026-09-27T04:05:06Z",
	}
	if err := writeConfig(configPath, changedHistory); err != nil {
		t.Fatal(err)
	}
	configAtCurrentRevisionBytes, configAtCurrentRevision := onboardingAcceptanceReadConfigBytes(t, configPath)
	configChangedActivation := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate",
		onboardingAcceptanceActivationRequest(editedDraft.Revision, check2.Receipt.CheckGeneration, review2, check2.Permit, onboardingAcceptanceCredential, true))
	requireOnboardingAcceptanceError(t, configChangedActivation, http.StatusConflict, "check_required")
	if got, _, _, _ := vault.snapshot(); len(got) != 0 || upstreamCalls.Load() != 2 {
		t.Fatal("stale config activation changed credentials or performed an extra remote check")
	}
	gotConfigAfterRejectedActivation, _ := onboardingAcceptanceReadConfigBytes(t, configPath)
	if !reflect.DeepEqual(gotConfigAfterRejectedActivation, configAtCurrentRevisionBytes) {
		t.Fatal("stale config activation changed active settings")
	}

	check3Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/check",
		onboardingDraftCheckRequestDTO{ExpectedRevision: editedDraft.Revision, Credential: onboardingAcceptanceCredential})
	requireOnboardingAcceptanceStatus(t, check3Result, http.StatusOK)
	check3 := decodeOnboardingAcceptanceCheck(t, check3Result)
	if check3.Receipt.CheckGeneration != check2.Receipt.CheckGeneration+1 {
		t.Fatalf("third check generation=%d, want %d", check3.Receipt.CheckGeneration, check2.Receipt.CheckGeneration+1)
	}
	review3Result := doOnboardingAcceptanceUI(t, client2, ui2.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/policy-review",
		onboardingPolicyReviewRequestDTO{ExpectedRevision: editedDraft.Revision})
	requireOnboardingAcceptanceStatus(t, review3Result, http.StatusOK)
	review3 := decodeOnboardingAcceptanceReview(t, review3Result)
	if review3.CheckGeneration != check3.Receipt.CheckGeneration || review3.ConfigRevision == review2.ConfigRevision || review3.ConfigRevision != check3.Receipt.ConfigRevision {
		t.Fatalf("fresh check did not bind the changed settings revision: old=%q fresh=%q check=%q", review2.ConfigRevision, review3.ConfigRevision, check3.Receipt.ConfigRevision)
	}
	ui2.Close()

	app3, ui3, client3 := startOnboardingAcceptanceUI(t, configPath, vault, upstreamClient)
	resumedAfterSecondRestart := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingDraftItemPath+mainDraft.ID, nil)
	requireOnboardingAcceptanceStatus(t, resumedAfterSecondRestart, http.StatusOK)
	if resumedDraft := decodeOnboardingAcceptanceDraft(t, resumedAfterSecondRestart); resumedDraft.Revision != editedDraft.Revision || resumedDraft.CheckGeneration != check3.Receipt.CheckGeneration || resumedDraft.Check == nil || resumedDraft.Check.CheckGeneration != check3.Receipt.CheckGeneration {
		t.Fatalf("updated draft/check did not survive a second app recreation: %+v", resumedDraft)
	}
	restartPermitActivation := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate",
		onboardingAcceptanceActivationRequest(editedDraft.Revision, check3.Receipt.CheckGeneration, review3, check3.Permit, onboardingAcceptanceCredential, true))
	requireOnboardingAcceptanceError(t, restartPermitActivation, http.StatusConflict, "check_permit_required")

	check4Result := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/check",
		onboardingDraftCheckRequestDTO{ExpectedRevision: editedDraft.Revision, Credential: onboardingAcceptanceCredential})
	requireOnboardingAcceptanceStatus(t, check4Result, http.StatusOK)
	check4 := decodeOnboardingAcceptanceCheck(t, check4Result)
	if check4.Receipt.CheckGeneration != check3.Receipt.CheckGeneration+1 {
		t.Fatalf("fourth check generation=%d, want %d", check4.Receipt.CheckGeneration, check3.Receipt.CheckGeneration+1)
	}
	review4Result := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/policy-review",
		onboardingPolicyReviewRequestDTO{ExpectedRevision: editedDraft.Revision})
	requireOnboardingAcceptanceStatus(t, review4Result, http.StatusOK)
	review4 := decodeOnboardingAcceptanceReview(t, review4Result)
	if review4.CheckGeneration != check4.Receipt.CheckGeneration {
		t.Fatalf("final policy review generation=%d, want latest successful check generation %d", review4.CheckGeneration, check4.Receipt.CheckGeneration)
	}
	if upstreamCalls.Load() != 4 {
		t.Fatalf("synthetic check count=%d, want four checks across revision/config/restart gates", upstreamCalls.Load())
	}
	assertOnboardingAcceptanceDraftStateUnchanged(t, configPath, configAtCurrentRevisionBytes, vault, wantVault, wantSaves, wantLoads, wantDeletes)

	activationPayload := onboardingAcceptanceActivationRequest(editedDraft.Revision, check4.Receipt.CheckGeneration, review4, check4.Permit, onboardingAcceptanceCredential, true)
	activationJSON, err := json.Marshal(activationPayload)
	if err != nil {
		t.Fatal(err)
	}
	var activationWithoutGeneration map[string]json.RawMessage
	if err := json.Unmarshal(activationJSON, &activationWithoutGeneration); err != nil {
		t.Fatal(err)
	}
	delete(activationWithoutGeneration, "check_generation")
	missingGeneration := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate", activationWithoutGeneration)
	requireOnboardingAcceptanceError(t, missingGeneration, http.StatusBadRequest, "invalid_request")
	zeroGenerationPayload := activationPayload
	zeroGenerationPayload.CheckGeneration = 0
	zeroGeneration := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate", zeroGenerationPayload)
	requireOnboardingAcceptanceError(t, zeroGeneration, http.StatusBadRequest, "invalid_request")
	staleGenerationPayload := activationPayload
	staleGenerationPayload.CheckGeneration = check4.Receipt.CheckGeneration - 1
	staleGeneration := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate", staleGenerationPayload)
	requireOnboardingAcceptanceError(t, staleGeneration, http.StatusConflict, "check_required")
	assertOnboardingAcceptanceDraftStateUnchanged(t, configPath, configAtCurrentRevisionBytes, vault, wantVault, wantSaves, wantLoads, wantDeletes)
	activationResult := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate", activationPayload)
	requireOnboardingAcceptanceStatus(t, activationResult, http.StatusOK)
	if strings.Contains(string(activationResult.body), onboardingAcceptanceCredential) || strings.Contains(string(activationResult.body), check4.Permit) {
		t.Fatal("activation response exposed the temporary credential or transient permit")
	}
	var activationResponse onboardingDraftActivateResponseDTO
	if err := json.Unmarshal(activationResult.body, &activationResponse); err != nil {
		t.Fatalf("decode activation response: %v; body=%s", err, activationResult.body)
	}
	activation := activationResponse.Activation
	if activation.DraftID != mainDraft.ID || activation.Service != onboardingServiceGitHub ||
		activation.CheckGeneration != check4.Receipt.CheckGeneration || activation.DraftRevision != editedDraft.Revision ||
		activation.CandidateDigest != check4.Receipt.CandidateDigest || activation.PolicyDigest != check4.Receipt.PolicyDigest ||
		activation.ConfigRevision != check4.Receipt.ConfigRevision || !githubIDPattern.MatchString(activation.TargetID) {
		t.Fatalf("activation record did not match the checked and reviewed candidate: %+v", activation)
	}
	_, activeConfigAfterActivation := onboardingAcceptanceReadConfigBytes(t, configPath)
	createdTarget := assertOnboardingActivationPreservesConfig(t, configAtCurrentRevision, activeConfigAfterActivation,
		activation, candidate.Name, candidate.Origin, candidate.Repository)
	vaultAfterActivation, savesAfterActivation, _, deletesAfterActivation := vault.snapshot()
	if savesAfterActivation != 1 || deletesAfterActivation != 0 || len(vaultAfterActivation) != 1 ||
		string(vaultAfterActivation[createdTarget.SecretRef]) != onboardingAcceptanceCredential {
		t.Fatalf("activation did not store exactly the selected credential once: refs=%d saves=%d deletes=%d", len(vaultAfterActivation), savesAfterActivation, deletesAfterActivation)
	}
	if strings.Contains(string(configAtCurrentRevisionBytes), onboardingAcceptanceCredential) || strings.Contains(string(mustOnboardingAcceptanceJSON(t, activeConfigAfterActivation)), onboardingAcceptanceCredential) {
		t.Fatal("temporary credential was written to active settings")
	}

	// Replaying the exact accepted request returns the same record and has no second side effect.
	configBytesAfterActivation, _ := onboardingAcceptanceReadConfigBytes(t, configPath)
	replayResult := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodPost,
		onboardingDraftItemPath+mainDraft.ID+"/activate", activationPayload)
	requireOnboardingAcceptanceStatus(t, replayResult, http.StatusOK)
	var replayResponse onboardingDraftActivateResponseDTO
	if err := json.Unmarshal(replayResult.body, &replayResponse); err != nil || replayResponse.Activation != activation {
		t.Fatalf("activation replay=%+v err=%v, want original record %+v", replayResponse.Activation, err, activation)
	}
	configBytesAfterReplay, _ := onboardingAcceptanceReadConfigBytes(t, configPath)
	vaultAfterReplay, savesAfterReplay, _, deletesAfterReplay := vault.snapshot()
	if !reflect.DeepEqual(configBytesAfterReplay, configBytesAfterActivation) || savesAfterReplay != savesAfterActivation ||
		deletesAfterReplay != deletesAfterActivation || !reflect.DeepEqual(vaultAfterReplay, vaultAfterActivation) {
		t.Fatal("activation replay changed settings or performed a second credential-store write")
	}
	missingDraft := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingDraftItemPath+mainDraft.ID, nil)
	requireOnboardingAcceptanceError(t, missingDraft, http.StatusNotFound, "not_found")

	storePath = onboardingAcceptanceStorePath(t, app3)
	storedSidecar, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedSidecar), onboardingAcceptanceCredential) || strings.Contains(string(storedSidecar), check4.Permit) {
		t.Fatal("durable onboarding sidecar persisted a temporary credential or process-local check permit")
	}

	readReceiptsBefore := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingReadReceiptsPath, nil)
	requireOnboardingAcceptanceStatus(t, readReceiptsBefore, http.StatusOK)
	var emptyReceipts onboardingFirstReadListResponse
	if err := json.Unmarshal(readReceiptsBefore.body, &emptyReceipts); err != nil || len(emptyReceipts.Receipts) != 0 {
		t.Fatalf("no first-read receipt should exist before an MCP service read: receipts=%+v err=%v", emptyReceipts.Receipts, err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverSession, err := app3.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "onboarding-acceptance", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	catalog, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "registered_targets"})
	if err != nil || catalog == nil || catalog.IsError {
		t.Fatalf("passive target catalog call failed: result=%#v err=%v", catalog, err)
	}
	if upstreamCalls.Load() != 4 {
		t.Fatalf("passive registered_targets contacted the service; upstream calls=%d", upstreamCalls.Load())
	}
	readReceiptsAfterCatalog := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingReadReceiptsPath, nil)
	requireOnboardingAcceptanceStatus(t, readReceiptsAfterCatalog, http.StatusOK)
	if err := json.Unmarshal(readReceiptsAfterCatalog.body, &emptyReceipts); err != nil || len(emptyReceipts.Receipts) != 0 {
		t.Fatalf("passive MCP catalog call created a first-read receipt: receipts=%+v err=%v", emptyReceipts.Receipts, err)
	}

	unknownRead, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "github_repository", Arguments: map[string]any{"target": "Unknown acceptance fixture"},
	})
	if err != nil || unknownRead == nil || !unknownRead.IsError {
		t.Fatalf("unknown target read unexpectedly succeeded: result=%#v err=%v", unknownRead, err)
	}
	readReceiptsAfterFailure := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingReadReceiptsPath, nil)
	requireOnboardingAcceptanceStatus(t, readReceiptsAfterFailure, http.StatusOK)
	if err := json.Unmarshal(readReceiptsAfterFailure.body, &emptyReceipts); err != nil || len(emptyReceipts.Receipts) != 0 {
		t.Fatalf("failed/unbound MCP read created a first-read receipt: receipts=%+v err=%v", emptyReceipts.Receipts, err)
	}

	actualRead, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "github_repository", Arguments: map[string]any{"target": candidate.Name},
	})
	if err != nil || actualRead == nil || actualRead.IsError {
		t.Fatalf("registered GitHub service read failed: result=%#v err=%v", actualRead, err)
	}
	projected, err := json.Marshal(actualRead.StructuredContent)
	if err != nil || !strings.Contains(string(projected), "onboarding/project") {
		t.Fatalf("successful MCP read did not return the synthetic registered repository: %s err=%v", projected, err)
	}
	if upstreamCalls.Load() != 5 {
		t.Fatalf("successful MCP read did not make exactly one additional upstream request: calls=%d", upstreamCalls.Load())
	}
	readReceiptsAfterRead := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingReadReceiptsPath, nil)
	requireOnboardingAcceptanceStatus(t, readReceiptsAfterRead, http.StatusOK)
	var readReceipts onboardingFirstReadListResponse
	if err := json.Unmarshal(readReceiptsAfterRead.body, &readReceipts); err != nil || len(readReceipts.Receipts) != 1 {
		t.Fatalf("successful actual MCP service read did not create one receipt: receipts=%+v err=%v", readReceipts.Receipts, err)
	}
	firstRead := readReceipts.Receipts[0]
	if firstRead.Source != onboardingFirstReadSource || firstRead.Service != onboardingServiceGitHub || firstRead.Tool != "github_repository" ||
		!validFirstReadDefinitionDigest(firstRead.DefinitionDigest) || !validFirstReadReceiptTime(firstRead.ObservedAt) {
		t.Fatalf("first-read receipt lacks bounded app-observed evidence: %+v", firstRead)
	}
	for _, forbidden := range []string{onboardingAcceptanceCredential, createdTarget.SecretRef, createdTarget.Name, createdTarget.Repository} {
		if strings.Contains(string(readReceiptsAfterRead.body), forbidden) {
			t.Fatalf("first-read response disclosed target data or credentials: found %q", forbidden)
		}
	}
	readinessBeforeEdit := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, readinessStatusPath, nil)
	requireOnboardingAcceptanceStatus(t, readinessBeforeEdit, http.StatusOK)
	var readiness readinessStatusDTO
	if err := json.Unmarshal(readinessBeforeEdit.body, &readiness); err != nil || readiness.FirstRead.State != firstReadStateObserved ||
		readiness.FirstRead.Source != onboardingFirstReadSource || readiness.FirstRead.Tool != "github_repository" {
		t.Fatalf("readiness did not report the successful current MCP read: first_read=%+v err=%v", readiness.FirstRead, err)
	}

	// A saved target edit makes the receipt stale; UI/readiness expose only current evidence.
	configWithEditedTarget := activeConfigAfterActivation
	configWithEditedTarget.GitHubTargets = append([]target(nil), activeConfigAfterActivation.GitHubTargets...)
	createdIndex := findGitHubTargetIndex(configWithEditedTarget.GitHubTargets, createdTarget.ID)
	if createdIndex < 0 {
		t.Fatalf("activated target %q disappeared before edit", createdTarget.ID)
	}
	configWithEditedTarget.GitHubTargets[createdIndex].Repository = "onboarding/edited"
	if err := writeConfig(configPath, configWithEditedTarget); err != nil {
		t.Fatal(err)
	}
	readinessAfterEdit := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, readinessStatusPath, nil)
	requireOnboardingAcceptanceStatus(t, readinessAfterEdit, http.StatusOK)
	var staleReadiness readinessStatusDTO
	if err := json.Unmarshal(readinessAfterEdit.body, &staleReadiness); err != nil || staleReadiness.FirstRead.State != firstReadStateNotObserved {
		t.Fatalf("old receipt incorrectly verified the edited active target: first_read=%+v err=%v", staleReadiness.FirstRead, err)
	}
	readReceiptsRemain := doOnboardingAcceptanceUI(t, client3, ui3.URL, http.MethodGet, onboardingReadReceiptsPath, nil)
	requireOnboardingAcceptanceStatus(t, readReceiptsRemain, http.StatusOK)
	if err := json.Unmarshal(readReceiptsRemain.body, &readReceipts); err != nil || len(readReceipts.Receipts) != 0 {
		t.Fatalf("edited target should have no current UI read receipt: receipts=%+v err=%v", readReceipts.Receipts, err)
	}
	controller, ok := app3.onboarding.(*onboardingAppController)
	if !ok {
		t.Fatal("local app lost its onboarding controller")
	}
	historicalReceipts, err := controller.store.ListFirstReads(context.Background())
	if err != nil || len(historicalReceipts) != 1 || historicalReceipts[0].DefinitionDigest != firstRead.DefinitionDigest {
		t.Fatalf("stale opaque historical receipt was not retained in the sidecar: receipts=%+v err=%v", historicalReceipts, err)
	}
	if strings.Contains(string(readinessAfterEdit.body), onboardingAcceptanceCredential) {
		t.Fatal("readiness response disclosed the temporary credential")
	}
}

func mustOnboardingAcceptanceJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
