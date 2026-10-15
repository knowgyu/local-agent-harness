package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFreshSetupFlowApprovesAndObservesRegisteredGitHubRead(t *testing.T) {
	const (
		csrfToken        = "setup-flow-csrf-token"
		secretName       = "Setup GitHub credential"
		secretValue      = "setup-flow-synthetic-pat-0123456789"
		targetName       = "Setup flow repository"
		targetOrigin     = "https://github.example.invalid"
		targetRepository = "team/setup-flow"
		host             = sshUITestHost
	)

	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}

	secrets := &namedSecretStoreFake{}
	secretController := &namedSecretController{
		configPath: configPath,
		store:      secrets,
		cleanup:    newNamedSecretCleanupTestQueue(t),
		persist:    writeConfig,
		newID:      func() (string, error) { return newTargetID("secret") },
		newRef:     newCredentialReference,
	}
	namedSecretHandler := newNamedSecretUIHandler(host, csrfToken, secretController)
	secretRequest := namedSecretUIRequest(t, host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {csrfToken},
		"name":    {secretName},
		"purpose": {"read registered GitHub repository"},
		"value":   {secretValue},
	})
	secretResponse := httptest.NewRecorder()
	namedSecretHandler.ServeHTTP(secretResponse, secretRequest)
	var savedSecret namedSecretUISuccess
	if err := json.Unmarshal(secretResponse.Body.Bytes(), &savedSecret); err != nil {
		t.Fatalf("decode named-secret UI response: %v; body=%s", err, secretResponse.Body.String())
	}
	if secretResponse.Code != http.StatusOK || !savedSecret.OK || savedSecret.Secret.Name != secretName || !savedSecret.Secret.Configured {
		t.Fatalf("named-secret UI save status=%d result=%+v body=%s", secretResponse.Code, savedSecret, secretResponse.Body.String())
	}
	if len(secrets.saves) != 1 || len(secrets.loads) != 0 {
		t.Fatalf("named-secret preparation saves=%d loads=%d; want one save and no credential load", len(secrets.saves), len(secrets.loads))
	}

	draftQueuePath := filepath.Join(t.TempDir(), setupDraftQueueDirectoryName, setupDraftQueueFileName)
	draftQueue, err := newSetupDraftFileQueueStoreForTest(draftQueuePath, time.Now)
	if err != nil {
		t.Fatalf("create task-owned setup-draft queue: %v", err)
	}
	drafts := newSetupDraftControllerWithQueue(newSetupDraftConfigStore(configPath), draftQueue)

	var requestCount int
	client := &http.Client{Transport: githubFirstUseAcceptanceRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "github.example.invalid" || request.URL.EscapedPath() != "/api/v3/repos/"+targetRepository || request.URL.RawQuery != "" || request.URL.Fragment != "" {
			return nil, fmt.Errorf("unexpected setup-flow GitHub request %s %s", request.Method, request.URL.Redacted())
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+secretValue {
			return nil, errors.New("registered GitHub read did not use the named credential")
		}
		body := `{"name":"setup-flow","full_name":"team/setup-flow","private":true,"default_branch":"main","html_url":"https://github.example.invalid/team/setup-flow"}`
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       request,
		}, nil
	})}
	a := &app{
		configPath:   configPath,
		secrets:      secrets,
		namedSecrets: secretController,
		setupDrafts:  drafts,
		client:       client,
		host:         host,
		csrf:         csrfToken,
	}

	onboardingStore, err := newOnboardingDraftStoreForConfigPath(configPath)
	if err != nil {
		t.Fatalf("locate task-owned onboarding store: %v", err)
	}
	receiptPath := onboardingStore.(*onboardingFileDraftStore).path
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("onboarding receipt data existed before setup_status: stat err=%v", err)
	}
	settingsBeforeStatus, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	savedBeforeStatus, err := readConfig(configPath)
	if err != nil || len(savedBeforeStatus.GitHubTargets) != 0 || len(savedBeforeStatus.NamedSecrets) != 1 {
		t.Fatalf("prepared settings before setup_status = targets %d named secrets %d, err=%v", len(savedBeforeStatus.GitHubTargets), len(savedBeforeStatus.NamedSecrets), err)
	}
	credentialRef := savedBeforeStatus.NamedSecrets[0].CredentialRef

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect MCP server session: %v", err)
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "setup-flow-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect MCP client session: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
		_ = serverSession.Wait()
	})
	callTool := func(name string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("MCP %s call failed: result=%+v err=%v", name, result, err)
		}
		return result
	}
	setupStatus := func() setupStatusDTO {
		t.Helper()
		status, _ := decodeSetupStatusResult(t, callTool("setup_status", map[string]any{}))
		return status
	}
	decodeDraftReview := func(result *mcp.CallToolResult) SetupDraftReview {
		t.Helper()
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				var review SetupDraftReview
				if err := json.Unmarshal([]byte(text.Text), &review); err != nil {
					t.Fatalf("decode submit_setup_draft review: %v; body=%s", err, text.Text)
				}
				return review
			}
		}
		t.Fatal("submit_setup_draft omitted review JSON")
		return SetupDraftReview{}
	}
	approveThroughUI := func(review SetupDraftReview, expectedReviewText string) {
		t.Helper()
		ui := newSSHUITestHandler(nil, a.setupDrafts)
		fields := sshUIBaseForm()
		fields.Set("draft_id", review.ID)
		fields.Set("base_revision", review.BaseRevision)
		fields.Set("digest", review.Digest)
		reviewResponse := sshUIPost(t, ui, "/review-setup-draft", fields)
		if reviewResponse.Code != http.StatusOK || !strings.Contains(reviewResponse.Body.String(), expectedReviewText) {
			t.Fatalf("actual setup-draft review route status=%d body=%s", reviewResponse.Code, reviewResponse.Body.String())
		}
		fields.Set("confirm_approve", "yes")
		approvalResponse := sshUIPost(t, ui, "/approve-setup-draft", fields)
		if approvalResponse.Code != http.StatusSeeOther || approvalResponse.Header().Get("Location") != "/ssh-settings?state=draft-approved" {
			t.Fatalf("actual setup-draft approval route status=%d location=%q body=%s", approvalResponse.Code, approvalResponse.Header().Get("Location"), approvalResponse.Body.String())
		}
	}

	initialStatus := setupStatus()
	if len(initialStatus.Targets) != 0 || initialStatus.ConfigurationRev == "" {
		t.Fatalf("fresh setup_status targets=%+v revision=%q; want no targets and a revision", initialStatus.Targets, initialStatus.ConfigurationRev)
	}
	if requestCount != 0 || len(secrets.loads) != 0 {
		t.Fatalf("setup_status performed service or credential I/O: requests=%d credential loads=%d", requestCount, len(secrets.loads))
	}
	settingsAfterStatus, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(settingsBeforeStatus, settingsAfterStatus) {
		t.Fatal("setup_status changed settings bytes")
	}
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup_status wrote onboarding receipt data: stat err=%v", err)
	}

	firstSubmission := SetupDraftSubmission{
		BaseRevision: initialStatus.ConfigurationRev,
		Sources:      []SetupDraftSource{{Kind: "user_provided", Label: "Setup flow test fixture", Confidence: "high"}},
		Missing:      []string{},
		Connections: []SetupDraftConnection{{
			Kind:           SetupServiceGitHub,
			Action:         SetupDraftAdd,
			Name:           targetName,
			Origin:         targetOrigin,
			Repository:     targetRepository,
			CredentialName: secretName,
		}},
		Bundles:    []SetupDraftBundle{},
		SSHTargets: []SetupDraftSSHTarget{},
	}
	firstReview := decodeDraftReview(callTool("submit_setup_draft", mustJSONMap(t, firstSubmission)))
	if firstReview.BaseRevision != initialStatus.ConfigurationRev {
		t.Fatalf("first draft used revision %q, exact setup_status revision was %q", firstReview.BaseRevision, initialStatus.ConfigurationRev)
	}
	approveThroughUI(firstReview, targetName)

	savedWithTarget, err := readConfig(configPath)
	if err != nil || len(savedWithTarget.GitHubTargets) != 1 || savedWithTarget.GitHubTargets[0].Name != targetName || savedWithTarget.GitHubTargets[0].Repository != targetRepository {
		t.Fatalf("approved target was not saved: targets=%+v err=%v", savedWithTarget.GitHubTargets, err)
	}
	targetID := savedWithTarget.GitHubTargets[0].ID

	statusWithTarget := setupStatus()
	if len(statusWithTarget.Targets) != 1 || statusWithTarget.Targets[0].ID != targetID || statusWithTarget.ConfigurationRev == initialStatus.ConfigurationRev {
		t.Fatalf("setup_status after target approval = targets %+v revision %q", statusWithTarget.Targets, statusWithTarget.ConfigurationRev)
	}
	secondSubmission := SetupDraftSubmission{
		BaseRevision: statusWithTarget.ConfigurationRev,
		Sources:      []SetupDraftSource{{Kind: "user_provided", Label: "Setup flow service-bundle fixture", Confidence: "high"}},
		Missing:      []string{},
		Connections:  []SetupDraftConnection{},
		Bundles: []SetupDraftBundle{{
			Name:          "Setup flow",
			RepositoryIDs: []string{targetID},
			Environments:  []SetupDraftEnvironment{},
		}},
		SSHTargets: []SetupDraftSSHTarget{},
	}
	secondReview := decodeDraftReview(callTool("submit_setup_draft", mustJSONMap(t, secondSubmission)))
	if secondReview.BaseRevision != statusWithTarget.ConfigurationRev {
		t.Fatalf("bundle draft used revision %q, exact setup_status revision was %q", secondReview.BaseRevision, statusWithTarget.ConfigurationRev)
	}
	approveThroughUI(secondReview, "Setup flow")
	savedWithBundle, err := readConfig(configPath)
	if err != nil || len(savedWithBundle.ServiceBundles) != 1 || savedWithBundle.ServiceBundles[0].Name != "Setup flow" || len(savedWithBundle.ServiceBundles[0].GitHubTargetIDs) != 1 || savedWithBundle.ServiceBundles[0].GitHubTargetIDs[0] != targetID {
		t.Fatalf("approved service bundle was not saved with target ID %q: bundles=%+v err=%v", targetID, savedWithBundle.ServiceBundles, err)
	}

	loadsBeforeRead := len(secrets.loads)
	if loadsBeforeRead != 0 {
		t.Fatalf("setup and approval loaded credentials before the service read: %v", secrets.loads)
	}
	repositoryResponse := callTool("github_repository", map[string]any{"target": targetName})
	var repository repository
	encodedRepository, err := json.Marshal(repositoryResponse.StructuredContent)
	if err != nil {
		t.Fatalf("marshal github_repository result: %v", err)
	}
	if err := json.Unmarshal(encodedRepository, &repository); err != nil {
		t.Fatalf("decode github_repository result: %v", err)
	}
	if repository.FullName != targetRepository || requestCount != 1 || len(secrets.loads) != loadsBeforeRead+1 || secrets.loads[0] != credentialRef {
		t.Fatalf("registered repository read result=%+v requests=%d credential loads=%v", repository, requestCount, secrets.loads)
	}

	finalStatus := setupStatus()
	var observedTarget *readinessTargetDTO
	for index := range finalStatus.Targets {
		if finalStatus.Targets[index].ID == targetID {
			observedTarget = &finalStatus.Targets[index]
			break
		}
	}
	if observedTarget == nil || observedTarget.FirstRead.State != firstReadStateObserved || observedTarget.FirstRead.Source != onboardingFirstReadSource {
		t.Fatalf("setup_status did not report the successful MCP first read: target=%+v status=%+v", observedTarget, finalStatus)
	}
	if observedTarget.ConnectionTest.State != "not_tested" {
		t.Fatalf("MCP first read changed connection-test state: %+v", observedTarget.ConnectionTest)
	}
	if requestCount != 1 || len(secrets.loads) != loadsBeforeRead+1 {
		t.Fatalf("post-read setup_status caused extra service or credential I/O: requests=%d credential loads=%v", requestCount, secrets.loads)
	}
}

func mustJSONMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode MCP tool arguments: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("decode MCP tool arguments: %v", err)
	}
	return result
}
