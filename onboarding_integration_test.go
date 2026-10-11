package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

func TestFirstReadReceiptUsesDefinitionLoadedBeforeInFlightRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	oldConfig := config{
		Version: configVersion,
		GitHubTargets: []target{{
			ID:         "github:0123456789abcdef0123456789abcdef",
			Name:       "Engineering",
			Origin:     "https://github.example.invalid",
			Repository: "ops/agent",
			SecretRef:  "cred:0123456789abcdef0123456789abcdef",
		}},
	}
	if err := writeConfig(path, oldConfig); err != nil {
		t.Fatal(err)
	}
	const token = "first-read-integration-secret-canary"
	secrets := memorySecrets{oldConfig.GitHubTargets[0].SecretRef: []byte(token)}
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	client := &http.Client{Transport: harborTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(requestStarted)
		<-releaseResponse
		return harborTestResponse(r, http.StatusOK,
			`{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main","html_url":"https://github.example.invalid/ops/agent"}`), nil
	})}
	a := &app{configPath: path, secrets: secrets, client: client}
	readContext, observer := withFirstReadCallObserver(context.Background(), "github", "github_repository")
	readDone := make(chan error, 1)
	go func() {
		_, err := a.registeredRepository(readContext, "Engineering")
		readDone <- err
	}()
	<-requestStarted

	newConfig := oldConfig
	newConfig.GitHubTargets = append([]target(nil), oldConfig.GitHubTargets...)
	newConfig.GitHubTargets[0].Repository = "ops/changed-during-read"
	if err := writeConfig(path, newConfig); err != nil {
		close(releaseResponse)
		t.Fatal(err)
	}
	close(releaseResponse)
	if err := <-readDone; err != nil {
		t.Fatalf("registered read failed: %v", err)
	}
	if observer.Digest() == "" {
		t.Fatal("registered read did not capture its loaded definition")
	}
	// This is the same post-success callback used by the MCP read wrapper.
	a.recordCapturedMCPRead(context.Background(), observer)

	controller, err := a.ensureOnboardingController()
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := controller.(*onboardingAppController).store.ListFirstReads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := firstReadReadinessFromConfig(oldConfig, receipts); got.State != firstReadStateObserved {
		t.Fatalf("receipt did not match the exact definition used by the read: %+v", got)
	}
	if got := firstReadReadinessFromConfig(newConfig, receipts); got.State != firstReadStateNotObserved {
		t.Fatalf("receipt was incorrectly attributed to the changed target: %+v", got)
	}
	currentReceipts, err := controller.ListFirstReads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(currentReceipts) != 0 {
		t.Fatalf("current receipt API exposed a stale target observation: %+v", currentReceipts)
	}
}
