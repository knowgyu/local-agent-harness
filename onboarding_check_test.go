package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const onboardingCheckTestCredential = "temporary-check-credential-canary"

func TestOnboardingCheckReadsServiceDraftsWithoutChangingConfigOrHistory(t *testing.T) {
	tests := []struct {
		name     string
		service  onboardingServiceKind
		fields   func(string) onboardingDraftFields
		check    func(*testing.T, *http.Request)
		response string
	}{
		{
			name:    "GitHub",
			service: onboardingServiceGitHub,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "github-check", Origin: origin, Repository: "org/repo"}
			},
			check: func(t *testing.T, req *http.Request) {
				t.Helper()
				if req.Method != http.MethodGet || req.URL.Path != "/api/v3/repos/org/repo" || req.Header.Get("Authorization") != "Bearer "+onboardingCheckTestCredential {
					t.Fatalf("unexpected GitHub request: method=%q path=%q authorization=%q", req.Method, req.URL.Path, req.Header.Get("Authorization"))
				}
			},
			response: `{"name":"repo","full_name":"org/repo","token":"github-response-canary"}`,
		},
		{
			name:    "Jenkins",
			service: onboardingServiceJenkins,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "jenkins-check", BaseURL: origin + "/jenkins", Username: "buildbot", JobPath: "team/service", Environment: "staging"}
			},
			check: func(t *testing.T, req *http.Request) {
				t.Helper()
				expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("buildbot:"+onboardingCheckTestCredential))
				if req.Method != http.MethodGet || req.URL.Path != "/jenkins/job/team/job/service/api/json" ||
					req.URL.Query().Get("tree") != "name,fullName,buildable,lastBuild[number,result,building]" || req.Header.Get("Authorization") != expectedAuth {
					t.Fatalf("unexpected Jenkins request: method=%q path=%q query=%q authorization=%q", req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Authorization"))
				}
			},
			response: `{"name":"service","fullName":"team/service","buildable":true,"token":"jenkins-response-canary"}`,
		},
		{
			name:    "Dashboard saved Bearer contract",
			service: onboardingServiceDashboard,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "dashboard-check", BaseURL: origin + "/proxy"}
			},
			check: func(t *testing.T, req *http.Request) {
				t.Helper()
				if req.Method != http.MethodGet || req.URL.Path != "/proxy/api/v1/namespace" || req.URL.Query().Get("itemsPerPage") != "100" ||
					req.Header.Get("Authorization") != "Bearer "+onboardingCheckTestCredential {
					t.Fatalf("unexpected Dashboard request: method=%q path=%q query=%q authorization=%q", req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Authorization"))
				}
			},
			response: `{"listMeta":{"totalItems":0},"namespaces":[],"token":"dashboard-response-canary"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				test.check(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()

			fixture := newOnboardingCheckFixture(t, server.Client(), test.service, test.fields(server.URL), nil, nil)
			beforeConfig, err := os.ReadFile(fixture.configPath)
			if err != nil {
				t.Fatal(err)
			}
			credential := []byte(onboardingCheckTestCredential)
			result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
				DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: credential,
			})
			if err != nil {
				t.Fatalf("CheckDraft() error = %v", err)
			}
			if result.Receipt.Outcome != onboardingCheckSucceeded || result.Receipt.FailureKind != "" || result.Permit == "" {
				t.Fatalf("unexpected check result: outcome=%q failure=%q permit_present=%t", result.Receipt.Outcome, result.Receipt.FailureKind, result.Permit != "")
			}
			if requests.Load() != 1 {
				t.Fatalf("request count = %d, want exactly one", requests.Load())
			}
			if !allZeroBytes(credential) {
				t.Fatal("temporary credential input was not cleared")
			}
			if !validOnboardingDigest(result.Receipt.CandidateDigest, "sha256:") ||
				!validOnboardingDigest(result.Receipt.PolicyDigest, "policy:sha256:") ||
				!validOnboardingDigest(result.Receipt.ConfigRevision, "config:sha256:") {
				t.Fatalf("receipt digests have invalid formats: %#v", result.Receipt)
			}

			afterConfig, err := os.ReadFile(fixture.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(beforeConfig) != string(afterConfig) {
				t.Fatal("checking a draft changed config.json")
			}
			cfg, err := readConfig(fixture.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.GitHubTargets)+len(cfg.JenkinsTargets)+len(cfg.DashboardTargets) != 0 || len(cfg.ConnectionTests) != 0 {
				t.Fatalf("draft check inserted an active target or connection history: %#v", cfg)
			}

			durable, err := os.ReadFile(fixture.store.path)
			if err != nil {
				t.Fatal(err)
			}
			serializedResult, err := json.Marshal(result.Receipt)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{onboardingCheckTestCredential, "github-response-canary", "jenkins-response-canary", "dashboard-response-canary", result.Permit} {
				if strings.Contains(string(durable), forbidden) || strings.Contains(string(serializedResult), forbidden) {
					t.Fatalf("durable check output contains forbidden value %q", forbidden)
				}
			}
		})
	}
}

func TestOnboardingCheckClassifiesFailuresWithoutLeakingResponseOrTransportErrors(t *testing.T) {
	tests := []struct {
		name      string
		service   onboardingServiceKind
		fields    func(string) onboardingDraftFields
		status    int
		want      connectionFailureKind
		transport bool
	}{
		{
			name: "GitHub authentication rejection", service: onboardingServiceGitHub,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "github-check", Origin: origin, Repository: "org/repo"}
			},
			status: http.StatusUnauthorized, want: connectionFailureAuthentication,
		},
		{
			name: "Jenkins rejects non-read success status", service: onboardingServiceJenkins,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "jenkins-check", BaseURL: origin, Username: "buildbot", JobPath: "team/service", Environment: "staging"}
			},
			status: http.StatusCreated, want: connectionFailureOther,
		},
		{
			name: "Dashboard redirect", service: onboardingServiceDashboard,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "dashboard-check", BaseURL: origin}
			},
			status: http.StatusFound, want: connectionFailureEndpoint,
		},
		{
			name: "transport error containing credential", service: onboardingServiceGitHub,
			fields: func(origin string) onboardingDraftFields {
				return onboardingDraftFields{Name: "github-check", Origin: origin, Repository: "org/repo"}
			},
			transport: true, want: connectionFailureAddress,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Diagnostic-Canary", "response-header-canary")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"message":"response-body-canary temporary-check-credential-canary"}`)
			}))
			defer server.Close()

			client := server.Client()
			if test.transport {
				client = &http.Client{Transport: onboardingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("transport exploded with temporary-check-credential-canary response-body-canary")
				})}
			}
			fixture := newOnboardingCheckFixture(t, client, test.service, test.fields(server.URL), nil, nil)
			result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
				DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
			})
			if err != nil {
				t.Fatalf("CheckDraft() returned raw error instead of a classified receipt: %v", err)
			}
			if result.Receipt.Outcome != onboardingCheckFailed || result.Receipt.FailureKind != test.want || result.Permit != "" {
				t.Fatalf("unexpected failure receipt: outcome=%q kind=%q permit_present=%t", result.Receipt.Outcome, result.Receipt.FailureKind, result.Permit != "")
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"temporary-check-credential-canary", "response-body-canary", "response-header-canary", "transport exploded"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("classified output leaked %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestOnboardingCheckRedirectDoesNotContactSecondHost(t *testing.T) {
	var secondHostRequests atomic.Int32
	second := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondHostRequests.Add(1)
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/redirected", http.StatusFound)
	}))
	defer first.Close()

	pool := x509.NewCertPool()
	pool.AddCert(first.Certificate())
	pool.AddCert(second.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}

	fixture := newOnboardingCheckFixture(t, client, onboardingServiceGitHub,
		onboardingDraftFields{Name: "redirect-check", Origin: first.URL, Repository: "org/repo"}, nil, nil)
	result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
	})
	if err != nil {
		t.Fatalf("CheckDraft() error = %v", err)
	}
	if result.Receipt.Outcome != onboardingCheckFailed || result.Receipt.FailureKind != connectionFailureEndpoint || result.Permit != "" {
		t.Fatalf("redirect did not fail closed: %#v", result)
	}
	if secondHostRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", secondHostRequests.Load())
	}
}

func TestOnboardingCheckClassifiesClientTimeout(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 30 * time.Millisecond
	fixture := newOnboardingCheckFixture(t, client, onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-timeout-check", Origin: server.URL, Repository: "org/repo"}, nil, nil)
	result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
	})
	if err != nil {
		t.Fatalf("CheckDraft() error = %v", err)
	}
	if result.Receipt.Outcome != onboardingCheckFailed || result.Receipt.FailureKind != connectionFailureTimeout || result.Permit != "" {
		t.Fatalf("timeout was not safely classified: %#v", result)
	}
	if requests.Load() != 1 {
		t.Fatalf("timeout triggered %d requests, want one", requests.Load())
	}
}

func TestOnboardingCheckRejectsStaleDraftAndCancelledRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	fields := onboardingDraftFields{Name: "github-check", Origin: server.URL, Repository: "org/repo"}
	fixture := newOnboardingCheckFixture(t, server.Client(), onboardingServiceGitHub, fields, nil, nil)
	updatedFields := fields
	updatedFields.Name = "github-check-updated"
	if _, err := fixture.store.UpdateDraft(context.Background(), fixture.draft.ID, fixture.draft.Revision, updatedFields); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
	})
	if !errors.Is(err, errOnboardingDraftStale) {
		t.Fatalf("stale revision error = %v, want %v", err, errOnboardingDraftStale)
	}
	if requests.Load() != 0 {
		t.Fatalf("stale draft triggered %d requests", requests.Load())
	}

	started := make(chan struct{})
	blockedServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer blockedServer.Close()
	cancelFixture := newOnboardingCheckFixture(t, blockedServer.Client(), onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-cancel-check", Origin: blockedServer.URL, Repository: "org/repo"}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, checkErr := cancelFixture.controller.CheckDraft(ctx, onboardingCheckRequest{
			DraftID: cancelFixture.draft.ID, ExpectedRevision: cancelFixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
		})
		done <- checkErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("cancel test server did not receive the explicit request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errOnboardingUnavailable) {
			t.Fatalf("cancelled check error = %v, want safe unavailable error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled check did not return promptly")
	}
	current, err := cancelFixture.store.GetDraft(context.Background(), cancelFixture.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Check != nil {
		t.Fatalf("cancelled read persisted a check receipt: %#v", current.Check)
	}
}

func TestOnboardingCheckDiscardsReceiptWhenConfigChangesDuringRead(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	secrets := memorySecrets{}
	fixture := newOnboardingCheckFixtureAt(t, configPath, &http.Client{}, secrets,
		onboardingServiceGitHub, onboardingDraftFields{Name: "github-check", Origin: "https://github.example", Repository: "org/repo"})
	first, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		changed := config{
			Version: configVersion,
			NamedSecrets: []namedSecretMetadata{{
				ID: "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "Existing", Purpose: "test metadata",
				CredentialRef: "cred:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			}},
		}
		if lockErr := withConfigLock(func() error { return writeConfig(configPath, changed) }); lockErr != nil {
			t.Errorf("change config while read is in flight: %v", lockErr)
		}
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	fixture.controller.client = server.Client()
	fixture.draft, err = fixture.store.UpdateDraft(context.Background(), fixture.draft.ID, fixture.draft.Revision,
		onboardingDraftFields{Name: "github-check", Origin: server.URL, Repository: "org/repo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
	})
	if !errors.Is(err, errOnboardingReviewChanged) {
		t.Fatalf("changed config error = %v, want review changed", err)
	}
	current, err := fixture.store.GetDraft(context.Background(), fixture.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Check != nil {
		t.Fatalf("config-stale read persisted a receipt: %#v", current.Check)
	}
	second, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("test did not actually change the config snapshot")
	}
}

func TestOnboardingCheckNamedSecretReceiptAndPermitDoNotPersistSecretMetadata(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+onboardingCheckTestCredential {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	metadata := namedSecretMetadata{
		ID: "secret:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "onboarding-test", Purpose: "temporary fixture",
		CredentialRef: "cred:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	secrets := memorySecrets{metadata.CredentialRef: []byte(onboardingCheckTestCredential)}
	fixture := newOnboardingCheckFixture(t, server.Client(), onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-check", Origin: server.URL, Repository: "org/repo"},
		secrets, &metadata)
	result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, NamedSecretID: metadata.ID,
	})
	if err != nil {
		t.Fatalf("CheckDraft() error = %v", err)
	}
	if result.Receipt.Outcome != onboardingCheckSucceeded || result.Permit == "" {
		t.Fatalf("unexpected named-secret result: outcome=%q permit_present=%t", result.Receipt.Outcome, result.Permit != "")
	}
	metadataDigest, err := onboardingSecretMetadataDigest(metadata)
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest, err := onboardingCandidateDigest(onboardingServiceGitHub, fixture.draft.Fields)
	if err != nil {
		t.Fatal(err)
	}
	proof := onboardingCheckProofInput{
		DraftID: fixture.draft.ID, DraftRevision: fixture.draft.Revision,
		CheckGeneration: result.Receipt.CheckGeneration,
		CandidateDigest: candidateDigest, PolicyDigest: onboardingPolicyDigest(onboardingServiceGitHub, fixture.draft.Fields),
		ConfigRevision: result.Receipt.ConfigRevision, Credential: []byte(onboardingCheckTestCredential),
		CredentialSource: onboardingCredentialNamed, NamedSecretID: metadata.ID,
		SecretMetadataDigest: metadataDigest,
	}
	if err := fixture.proofs.Validate(result.Permit, proof, time.Now()); err != nil {
		t.Fatalf("exact named-secret proof did not validate: %v", err)
	}
	encodedReceipt, err := json.Marshal(result.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := os.ReadFile(fixture.store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{metadata.ID, metadata.CredentialRef, onboardingCheckTestCredential, result.Permit} {
		if strings.Contains(string(encodedReceipt), forbidden) || strings.Contains(string(durable), forbidden) {
			t.Fatalf("receipt/store persisted transient value %q", forbidden)
		}
	}
}

func TestOnboardingCheckNewFailureRevokesPreviousSuccessfulPermit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer rejected-on-second-check" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"failure response must not escape"}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	fixture := newOnboardingCheckFixture(t, server.Client(), onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-latest-check", Origin: server.URL, Repository: "org/repo"}, nil, nil)

	first, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte("first-successful-credential"),
	})
	if err != nil || first.Receipt.Outcome != onboardingCheckSucceeded || first.Permit == "" {
		t.Fatalf("first check = outcome %q, permit=%t, err=%v", first.Receipt.Outcome, first.Permit != "", err)
	}
	firstProof := onboardingTemporaryCheckProof(first.Receipt, []byte("first-successful-credential"))
	second, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte("rejected-on-second-check"),
	})
	if err != nil {
		t.Fatalf("second failed check error = %v", err)
	}
	if second.Receipt.Outcome != onboardingCheckFailed || second.Receipt.FailureKind != connectionFailureAuthentication || second.Permit != "" {
		t.Fatalf("second check = outcome %q failure %q permit=%t", second.Receipt.Outcome, second.Receipt.FailureKind, second.Permit != "")
	}
	if second.Receipt.CheckGeneration <= first.Receipt.CheckGeneration {
		t.Fatalf("check generation did not advance: first=%d second=%d", first.Receipt.CheckGeneration, second.Receipt.CheckGeneration)
	}
	if err := fixture.proofs.Validate(first.Permit, firstProof, time.Now()); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("old successful permit remained usable after a failed new check: %v", err)
	}
	current, err := fixture.store.GetDraft(context.Background(), fixture.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Check == nil || current.Check.CheckGeneration != second.Receipt.CheckGeneration || current.Check.Outcome != onboardingCheckFailed {
		t.Fatalf("latest failed receipt was not durable: %#v", current.Check)
	}
}

func TestOnboardingCheckNewSuccessRevokesPreviousSuccessfulPermit(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	fixture := newOnboardingCheckFixture(t, server.Client(), onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-latest-check", Origin: server.URL, Repository: "org/repo"}, nil, nil)
	firstCredential := []byte("first-successful-credential")
	first, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: firstCredential,
	})
	if err != nil || first.Receipt.Outcome != onboardingCheckSucceeded || first.Permit == "" {
		t.Fatalf("first check = outcome %q permit=%t err=%v", first.Receipt.Outcome, first.Permit != "", err)
	}
	firstProof := onboardingTemporaryCheckProof(first.Receipt, []byte("first-successful-credential"))
	secondCredential := []byte("second-successful-credential")
	second, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: secondCredential,
	})
	if err != nil || second.Receipt.Outcome != onboardingCheckSucceeded || second.Permit == "" {
		t.Fatalf("second check = outcome %q permit=%t err=%v", second.Receipt.Outcome, second.Permit != "", err)
	}
	if second.Receipt.CheckGeneration != first.Receipt.CheckGeneration+1 {
		t.Fatalf("second check generation = %d, first = %d", second.Receipt.CheckGeneration, first.Receipt.CheckGeneration)
	}
	if err := fixture.proofs.Validate(first.Permit, firstProof, time.Now()); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("old permit remained usable after newer success: %v", err)
	}
	secondProof := onboardingTemporaryCheckProof(second.Receipt, []byte("second-successful-credential"))
	if err := fixture.proofs.Validate(second.Permit, secondProof, time.Now()); err != nil {
		t.Fatalf("new successful permit did not validate: %v", err)
	}
}

func TestOnboardingConcurrentChecksOnlyNewestGenerationCanPersistOrIssue(t *testing.T) {
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseFirstOnce, releaseSecondOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releaseFirst) })
	defer releaseSecondOnce.Do(func() { close(releaseSecond) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer before-concurrent-checks":
			_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
			return
		case "Bearer concurrent-first":
			started <- "first"
			<-releaseFirst
		case "Bearer concurrent-second":
			started <- "second"
			<-releaseSecond
		default:
			http.Error(w, "unexpected authorization", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"org/repo"}`)
	}))
	defer server.Close()
	fixture := newOnboardingCheckFixture(t, server.Client(), onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-concurrent-check", Origin: server.URL, Repository: "org/repo"}, nil, nil)
	previous, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte("before-concurrent-checks"),
	})
	if err != nil || previous.Receipt.Outcome != onboardingCheckSucceeded || previous.Permit == "" {
		t.Fatalf("initial check = outcome %q permit=%t err=%v", previous.Receipt.Outcome, previous.Permit != "", err)
	}
	previousProof := onboardingTemporaryCheckProof(previous.Receipt, []byte("before-concurrent-checks"))
	firstDone := make(chan onboardingCheckCompletion, 1)
	secondDone := make(chan onboardingCheckCompletion, 1)
	go func() {
		result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
			DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte("concurrent-first"),
		})
		firstDone <- onboardingCheckCompletion{result: result, err: err}
	}()
	if got := waitForCheckStart(t, started); got != "first" {
		t.Fatalf("first in-flight request identity = %q", got)
	}
	if err := fixture.proofs.Validate(previous.Permit, previousProof, time.Now()); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("prior permit remained usable after the next check started: %v", err)
	}
	go func() {
		result, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
			DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte("concurrent-second"),
		})
		secondDone <- onboardingCheckCompletion{result: result, err: err}
	}()
	if got := waitForCheckStart(t, started); got != "second" {
		t.Fatalf("second in-flight request identity = %q", got)
	}
	current, err := fixture.store.GetDraft(context.Background(), fixture.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Check != nil || current.CheckGeneration != 3 {
		t.Fatalf("new check did not clear receipt and advance generation before network: check=%#v generation=%d", current.Check, current.CheckGeneration)
	}
	releaseSecondOnce.Do(func() { close(releaseSecond) })
	second := waitForCheckResult(t, secondDone)
	if second.err != nil || second.result.Receipt.Outcome != onboardingCheckSucceeded || second.result.Receipt.CheckGeneration != 3 || second.result.Permit == "" {
		t.Fatalf("newer check result = generation %d outcome %q permit=%t err=%v", second.result.Receipt.CheckGeneration, second.result.Receipt.Outcome, second.result.Permit != "", second.err)
	}
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	first := waitForCheckResult(t, firstDone)
	if first.err == nil || first.result.Permit != "" {
		t.Fatalf("older in-flight check restored a receipt/permit: generation=%d outcome=%q permit=%t err=%v", first.result.Receipt.CheckGeneration, first.result.Receipt.Outcome, first.result.Permit != "", first.err)
	}
	if !errors.Is(first.err, errOnboardingCheckRequired) && !errors.Is(first.err, errOnboardingDraftStale) {
		t.Fatalf("older in-flight check returned an unexpected error: %v", first.err)
	}
	latest, err := fixture.store.GetDraft(context.Background(), fixture.draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Check == nil || latest.Check.CheckGeneration != second.result.Receipt.CheckGeneration || latest.Check.Outcome != onboardingCheckSucceeded {
		t.Fatalf("older completion replaced the latest durable receipt: %#v", latest.Check)
	}
	secondProof := onboardingTemporaryCheckProof(second.result.Receipt, []byte("concurrent-second"))
	if err := fixture.proofs.Validate(second.result.Permit, secondProof, time.Now()); err != nil {
		t.Fatalf("latest completion permit did not validate: %v", err)
	}
}

func onboardingTemporaryCheckProof(receipt onboardingCheckReceipt, credential []byte) onboardingCheckProofInput {
	return onboardingCheckProofInput{
		DraftID: receipt.DraftID, DraftRevision: receipt.DraftRevision, CheckGeneration: receipt.CheckGeneration,
		CandidateDigest: receipt.CandidateDigest, PolicyDigest: receipt.PolicyDigest,
		ConfigRevision: receipt.ConfigRevision, Credential: credential,
		CredentialSource: onboardingCredentialTemporary,
	}
}

func waitForCheckStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case value := <-started:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("explicit check did not reach its local HTTP fixture")
		return ""
	}
}

type onboardingCheckCompletion struct {
	result onboardingCheckResult
	err    error
}

func waitForCheckResult(t *testing.T, result <-chan onboardingCheckCompletion) onboardingCheckCompletion {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("explicit check did not finish")
		return onboardingCheckCompletion{}
	}
}

func TestOnboardingCheckProofRegistryBindsAndConsumesPermit(t *testing.T) {
	registry := newOnboardingCheckProofRegistryMemory()
	if registry == nil {
		t.Fatal("proof registry constructor failed")
	}
	now := time.Now().UTC()
	input := onboardingCheckProofInput{
		DraftID: "onboarding:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DraftRevision: 7,
		CheckGeneration: 7,
		CandidateDigest: "sha256:" + strings.Repeat("a", 64),
		PolicyDigest:    "policy:sha256:" + strings.Repeat("b", 64),
		ConfigRevision:  "config:sha256:" + strings.Repeat("c", 64),
		Credential:      []byte(onboardingCheckTestCredential), CredentialSource: onboardingCredentialTemporary,
	}
	if err := registry.BeginDraftCheck(input.DraftID, input.CheckGeneration); err != nil {
		t.Fatalf("BeginDraftCheck() error = %v", err)
	}
	if err := registry.BeginDraftCheck(input.DraftID, input.CheckGeneration-1); !errors.Is(err, errOnboardingDraftStale) {
		t.Fatalf("older concurrent generation moved proof registry backward: %v", err)
	}
	permit, err := registry.Issue(input, now)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	restartedRegistry := newOnboardingCheckProofRegistryMemory()
	if restartedRegistry == nil {
		t.Fatal("fresh proof registry constructor failed")
	}
	if err := restartedRegistry.Validate(permit, input, now.Add(time.Second)); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("permit survived controller restart: validation error = %v", err)
	}
	wrongCredential := input
	wrongCredential.Credential = []byte("different-credential")
	if err := registry.Validate(permit, wrongCredential, now); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("wrong credential validation error = %v, want check required", err)
	}
	if err := registry.Validate(permit, input, now.Add(onboardingCheckPermitTTL)); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("permit at exact expiry boundary error = %v, want check required", err)
	}
	permit, err = registry.Issue(input, now)
	if err != nil {
		t.Fatalf("Issue() second permit error = %v", err)
	}
	changedConfig := input
	changedConfig.ConfigRevision = "config:sha256:" + strings.Repeat("d", 64)
	if err := registry.Validate(permit, changedConfig, now.Add(time.Second)); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("changed config validation error = %v, want check required", err)
	}
	if err := registry.Validate(permit, input, now.Add(time.Second)); err != nil {
		t.Fatalf("exact proof validation error = %v", err)
	}
	if err := registry.Validate(permit, input, now.Add(2*time.Second)); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("replay validation error = %v, want check required", err)
	}
	permit, err = registry.Issue(input, now)
	if err != nil {
		t.Fatalf("Issue() third permit error = %v", err)
	}
	registry.InvalidateDraft(input.DraftID)
	if err := registry.Validate(permit, input, now.Add(time.Second)); !errors.Is(err, errOnboardingCheckRequired) {
		t.Fatalf("invalidated draft proof error = %v, want check required", err)
	}
}

func TestOnboardingCheckWithNilClientFailsClosed(t *testing.T) {
	fixture := newOnboardingCheckFixture(t, nil, onboardingServiceGitHub,
		onboardingDraftFields{Name: "github-check", Origin: "https://github.example", Repository: "org/repo"}, nil, nil)
	_, err := fixture.controller.CheckDraft(context.Background(), onboardingCheckRequest{
		DraftID: fixture.draft.ID, ExpectedRevision: fixture.draft.Revision, Credential: []byte(onboardingCheckTestCredential),
	})
	if !errors.Is(err, errOnboardingInvalid) && !errors.Is(err, errOnboardingUnavailable) {
		t.Fatalf("nil client error = %v, want a safe failure", err)
	}
}

type onboardingCheckFixture struct {
	controller *onboardingCheckController
	store      *onboardingFileDraftStore
	proofs     *onboardingCheckProofRegistryMemory
	draft      onboardingDraft
	configPath string
}

func newOnboardingCheckFixture(
	t *testing.T,
	client *http.Client,
	service onboardingServiceKind,
	fields onboardingDraftFields,
	secrets memorySecrets,
	metadata *namedSecretMetadata,
) onboardingCheckFixture {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	var named []namedSecretMetadata
	if metadata != nil {
		named = append(named, *metadata)
	}
	if err := writeConfig(configPath, config{Version: configVersion, NamedSecrets: named}); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return newOnboardingCheckFixtureAt(t, configPath, client, secrets, service, fields)
}

func newOnboardingCheckFixtureAt(
	t *testing.T,
	configPath string,
	client *http.Client,
	secrets memorySecrets,
	service onboardingServiceKind,
	fields onboardingDraftFields,
) onboardingCheckFixture {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), onboardingStoreDirectoryName, strings.Repeat("a", 64), onboardingStoreFileName)
	store, err := newOnboardingFileDraftStoreForTest(storePath, func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatalf("create test draft store: %v", err)
	}
	draft, err := store.CreateDraft(context.Background(), service, fields)
	if err != nil {
		t.Fatalf("create test draft: %v", err)
	}
	proofs := newOnboardingCheckProofRegistryMemory()
	if proofs == nil {
		t.Fatal("create test proof registry")
	}
	controller := newOnboardingCheckController(store, configPath, secrets, client, proofs)
	return onboardingCheckFixture{controller: controller, store: store, proofs: proofs, draft: draft, configPath: configPath}
}

type onboardingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f onboardingRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func allZeroBytes(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
