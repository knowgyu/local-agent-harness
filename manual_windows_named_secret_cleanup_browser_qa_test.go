//go:build windows && integration && parallelmanual

package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	cleanupBrowserQAOptIn    = "LAH_ENABLE_MANUAL_WINDOWS_NAMED_SECRET_CLEANUP_BROWSER_QA"
	cleanupBrowserQARole     = "LAH_NAMED_SECRET_CLEANUP_QA_ROLE"
	cleanupBrowserQAConfig   = "LAH_NAMED_SECRET_CLEANUP_QA_CONFIG"
	cleanupBrowserQAQueue    = "LAH_NAMED_SECRET_CLEANUP_QA_QUEUE"
	cleanupBrowserQAStoreEnv = "LAH_NAMED_SECRET_CLEANUP_QA_STORE"
	cleanupBrowserQAProbeEnv = "LAH_NAMED_SECRET_CLEANUP_QA_PROBE"
	cleanupBrowserQAReadyEnv = "LAH_NAMED_SECRET_CLEANUP_QA_READY"
	cleanupBrowserQAAuditEnv = "LAH_NAMED_SECRET_CLEANUP_QA_AUDIT"
	cleanupBrowserQADelete   = "LAH_NAMED_SECRET_CLEANUP_QA_ALLOW_DELETE"
	cleanupBrowserQAResume   = "LAH_NAMED_SECRET_CLEANUP_QA_RESUME_PENDING"

	cleanupBrowserQAStoreMaxBytes   = 16 << 10
	cleanupBrowserQAProbeMaxBytes   = 1 << 10
	cleanupBrowserQAAuditMaxBytes   = 1 << 10
	cleanupBrowserQAResponseMaxByte = 2 << 20
)

var cleanupBrowserCredentialRefPattern = regexp.MustCompile(`cred:[0-9a-fA-F]{32}`)
var cleanupBrowserQACallCounter uint32

type cleanupBrowserQAStoreEnvelope struct {
	Version int                        `json:"version"`
	Entries map[string][]byte          `json:"entries"`
	Stats   cleanupBrowserQAStoreStats `json:"stats"`
}

type cleanupBrowserQAStoreStats struct {
	SaveCalls       int `json:"save_calls"`
	LoadCalls       int `json:"load_calls"`
	DeleteCalls     int `json:"delete_calls"`
	DeleteFailures  int `json:"delete_failures"`
	DeleteSuccesses int `json:"delete_successes"`
}

type cleanupBrowserQAStore struct {
	path        string
	allowDelete bool
	mu          sync.Mutex
}

type cleanupBrowserQAProbe struct {
	Values [][]byte `json:"values"`
}

type cleanupBrowserQAReady struct {
	URL  string `json:"url"`
	Host string `json:"host"`
	CSRF string `json:"csrf"`
}

type cleanupBrowserQAAudit struct {
	Responses            int  `json:"responses"`
	CanaryExposed        bool `json:"canary_exposed"`
	CredentialRefExposed bool `json:"credential_ref_exposed"`
	ScanOverflow         bool `json:"scan_overflow"`
}

type cleanupBrowserQAScanHandler struct {
	next     http.Handler
	path     string
	canaries [][]byte
	mu       sync.Mutex
	audit    cleanupBrowserQAAudit
}

type cleanupBrowserQACaptureWriter struct {
	http.ResponseWriter
	body     []byte
	overflow bool
}

type cleanupBrowserQAEmptyEnvironment struct{}

func (cleanupBrowserQAEmptyEnvironment) ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error) {
	return []UserEnvironmentEntry{}, nil
}

func (cleanupBrowserQAEmptyEnvironment) SetUserEnvironment(context.Context, UserEnvironmentWrite) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errors.New("synthetic environment backend is disabled")
}

func (cleanupBrowserQAEmptyEnvironment) DeleteUserEnvironment(context.Context, string) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errors.New("synthetic environment backend is disabled")
}

func TestManualWindowsNamedSecretCleanupBrowserQA(t *testing.T) {
	if os.Getenv(cleanupBrowserQAOptIn) != "1" {
		t.Skip("set the explicit opt-in environment variable to run headed Windows UI acceptance")
	}
	if os.Getenv(cleanupBrowserQARole) != "" {
		t.Skip("the child process runs the production local UI handler")
	}
	atomic.StoreUint32(&cleanupBrowserQACallCounter, 0)
	resumePending := os.Getenv(cleanupBrowserQAResume) == "1"

	workDir := filepath.Join(t.TempDir(), "private")
	queuePath := filepath.Join(workDir, namedSecretCleanupQueueFileName)
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("could not configure the isolated durable cleanup queue")
	}
	if _, err := queue.Load(); err != nil {
		t.Fatal("the isolated durable cleanup queue was not available")
	}
	if err := queue.writeLocked([]namedSecretCleanupIntent{}); err != nil {
		t.Fatal("could not initialize the isolated durable cleanup queue")
	}

	configPath := filepath.Join(workDir, "settings.json")
	storePath := filepath.Join(workDir, "synthetic-store.json")
	probePath := filepath.Join(workDir, "response-probe.json")
	readyOnePath := filepath.Join(workDir, "ui-one.json")
	readyTwoPath := filepath.Join(workDir, "ui-two.json")
	auditOnePath := filepath.Join(workDir, "audit-one.json")
	auditTwoPath := filepath.Join(workDir, "audit-two.json")

	oldValue, err := cleanupBrowserQARandomValue()
	if err != nil {
		t.Fatal("could not create the synthetic starting value")
	}
	newValue, err := cleanupBrowserQARandomValue()
	if err != nil {
		clearBytes(oldValue)
		t.Fatal("could not create the synthetic replacement value")
	}
	defer clearBytes(oldValue)
	defer clearBytes(newValue)

	oldRef, err := newCredentialReference()
	if err != nil {
		t.Fatal("could not create the synthetic starting reference")
	}
	newRef := ""
	if resumePending {
		newRef, err = newCredentialReference()
		if err != nil {
			t.Fatal("could not create the synthetic replacement reference")
		}
	}
	secretID, err := newTargetID("secret")
	if err != nil {
		t.Fatal("could not create the synthetic secret ID")
	}
	targetID, err := newTargetID("github")
	if err != nil {
		t.Fatal("could not create the synthetic consumer ID")
	}
	store := &cleanupBrowserQAStore{path: storePath, allowDelete: true}
	if err := store.writeLocked(cleanupBrowserQAStoreEnvelope{Entries: map[string][]byte{}}); err != nil {
		t.Fatal("could not initialize the isolated synthetic credential store")
	}
	if err := store.Save(oldRef, oldValue); err != nil {
		t.Fatal("could not seed the isolated synthetic credential store")
	}
	if resumePending {
		if err := store.Save(newRef, newValue); err != nil {
			t.Fatal("could not seed the isolated replacement credential")
		}
	}
	activeRef := oldRef
	if resumePending {
		activeRef = newRef
	}
	cfg := config{
		Version: configVersion,
		NamedSecrets: []namedSecretMetadata{{
			ID: secretID, Name: "QA Cleanup Secret", Purpose: "synthetic cleanup acceptance", CredentialRef: activeRef,
		}},
		GitHubTargets: []target{{
			ID: targetID, Name: "QA Cleanup Consumer", Origin: "https://github.example.invalid",
			Repository: "qa/cleanup", SecretRef: activeRef,
		}},
	}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal("could not write isolated synthetic settings")
	}
	if err := cleanupBrowserQASecureRewrite(configPath, maxConfigSize); err != nil {
		t.Fatal("could not protect the isolated synthetic settings file")
	}
	if resumePending {
		if err := queue.Put(namedSecretCleanupIntent{ID: secretID, OldRef: oldRef, NewRef: newRef, DeleteOld: true}); err != nil {
			t.Fatal("could not seed the isolated pending cleanup intent")
		}
		if err := queue.MarkCommitted(secretID, oldRef, newRef); err != nil {
			t.Fatal("could not commit the isolated cleanup intent")
		}
	}
	probeBytes, err := json.Marshal(cleanupBrowserQAProbe{Values: [][]byte{append([]byte(nil), oldValue...), append([]byte(nil), newValue...)}})
	if err != nil || len(probeBytes) > cleanupBrowserQAProbeMaxBytes {
		clearBytes(probeBytes)
		t.Fatal("could not prepare the bounded private response probe")
	}
	if err := writeSetupDraftQueueFileAtomically(probePath, probeBytes); err != nil {
		clearBytes(probeBytes)
		t.Fatal("could not write the private response probe")
	}
	clearBytes(probeBytes)

	queueBytes, err := os.ReadFile(queuePath)
	if err != nil || int64(len(queueBytes)) > namedSecretCleanupQueueMaxBytes {
		clearBytes(queueBytes)
		t.Fatal("the durable cleanup queue exceeded its private size bound")
	}
	clearBytes(queueBytes)
	if err := cleanupBrowserQAAssertPrivateQueue(queuePath); err != nil {
		t.Fatal("the durable cleanup queue is not protected by the current-user ACL")
	}

	sessionSuffix, err := cleanupBrowserQARandomHex(6)
	if err != nil {
		t.Fatal("could not create a browser session name")
	}
	session := "cleanup-" + sessionSuffix
	profilePath := filepath.Join(t.TempDir(), "browser-profile")
	var browserOpen bool
	t.Cleanup(func() {
		if browserOpen {
			cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "close")
		}
	})

	childOne := cleanupBrowserQAStartChild(t, "deny", configPath, queuePath, storePath, probePath, readyOnePath, auditOnePath)
	readyOne := cleanupBrowserQAWaitReady(t, childOne, readyOnePath)
	browserOpen = true
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "open", readyOne.URL+"/", "--headed", "--profile", profilePath)
	page := cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "goto", readyOne.URL+"/")
	clearBytes(page)
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
	if !bytes.Contains(page, []byte("QA Cleanup Secret")) {
		clearBytes(page)
		t.Fatal("headed UI did not render the synthetic saved secret")
	}
	clearBytes(page)
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "select", "#language-select", "en")
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "a[data-pane-link='secrets']")

	if !resumePending {
		cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "button[data-focus-key='secret-edit:"+secretID+"']")
		cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "fill", "#lah-named-secret-value", string(newValue))
		cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "#lah-named-secret-save")
		cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "dialog-accept")
		page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
		if !cleanupBrowserQAPendingStatusVisible(page) || !cleanupBrowserQARetryActionVisible(page) {
			clearBytes(page)
			t.Fatal("rotation did not show pending cleanup and a retry action in the UI")
		}
		clearBytes(page)
	} else {
		page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
		if !cleanupBrowserQAPendingStatusVisible(page) || !cleanupBrowserQARetryActionVisible(page) {
			clearBytes(page)
			t.Fatal("seeded pending cleanup was not visible in the isolated UI")
		}
		clearBytes(page)
	}
	if err := cleanupBrowserQAAssertPending(queue, secretID); err != nil {
		t.Fatal("rotation did not persist the committed cleanup intent")
	}
	rotated, err := readConfig(configPath)
	if err != nil || len(rotated.NamedSecrets) != 1 || len(rotated.GitHubTargets) != 1 || rotated.NamedSecrets[0].ID != secretID || rotated.NamedSecrets[0].CredentialRef == oldRef || rotated.GitHubTargets[0].SecretRef != rotated.NamedSecrets[0].CredentialRef || (resumePending && rotated.NamedSecrets[0].CredentialRef != newRef) {
		t.Fatal("rotation did not persist the replacement reference and consumer mapping")
	}
	newRef = rotated.NamedSecrets[0].CredentialRef
	if err := cleanupBrowserQAAssertRefState(store, oldRef, newRef, true, true); err != nil {
		t.Fatal("fake backend did not retain both credentials after injected deletion failure")
	}

	cleanupBrowserQACheckRouteRejections(t, readyOne, secretID, store, oldValue, newValue)
	if _, code := cleanupBrowserQAPost(t, readyOne, uiRouteDeleteNamedSecret, url.Values{"csrf": {readyOne.CSRF}, "id": {secretID}}, oldValue, newValue, ""); code != "cleanup_failed" {
		t.Fatal("delete was not blocked separately while cleanup remained pending")
	}
	if err := cleanupBrowserQAAssertPending(queue, secretID); err != nil {
		t.Fatal("pending delete attempt removed the durable cleanup intent")
	}
	cleanupBrowserQAStopChild(t, childOne)
	if err := cleanupBrowserQAAssertAudit(auditOnePath); err != nil {
		t.Fatal("first UI process observed a secret or reference in an HTTP response")
	}

	childTwo := cleanupBrowserQAStartChild(t, "allow", configPath, queuePath, storePath, probePath, readyTwoPath, auditTwoPath)
	readyTwo := cleanupBrowserQAWaitReady(t, childTwo, readyTwoPath)
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "goto", readyTwo.URL+"/")
	clearBytes(page)
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "a[data-pane-link='secrets']")
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
	if !cleanupBrowserQAPendingStatusVisible(page) || !cleanupBrowserQARetryActionVisible(page) {
		clearBytes(page)
		t.Fatal("pending cleanup did not survive a fresh UI process and page load")
	}
	clearBytes(page)
	if err := cleanupBrowserQAAssertPending(queue, secretID); err != nil {
		t.Fatal("cleanup queue did not survive the UI process restart")
	}
	cleanupBrowserQACheckRouteRejections(t, readyTwo, secretID, store, oldValue, newValue)

	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "button[data-focus-key^='secret-cleanup:']")
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
	if cleanupBrowserQAPendingStatusVisible(page) || cleanupBrowserQARetryActionVisible(page) {
		clearBytes(page)
		t.Fatal("successful retry left pending cleanup visible in the UI")
	}
	clearBytes(page)
	if err := cleanupBrowserQAAssertQueueEmpty(queue); err != nil {
		t.Fatal("successful retry did not remove the durable cleanup intent")
	}
	if err := cleanupBrowserQAAssertRefState(store, oldRef, newRef, false, true); err != nil {
		t.Fatal("successful retry did not remove only the obsolete credential")
	}

	if status, code := cleanupBrowserQAPost(t, readyTwo, uiRouteDeleteNamedSecret, url.Values{"csrf": {readyTwo.CSRF}, "id": {secretID}}, oldValue, newValue, ""); status != http.StatusConflict || code != "in_use" {
		t.Fatal("ordinary in-use deletion was not blocked distinctly after pending cleanup cleared")
	}
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "goto", readyTwo.URL+"/?github_id="+url.QueryEscape(targetID))
	clearBytes(page)
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "a[data-pane-link='connections']")
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "check", "form[action='/delete-target'] input[name='confirm_delete']")
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "form[action='/delete-target'] button[type='submit']")
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
	if !bytes.Contains(page, []byte("Target deleted from local settings")) {
		clearBytes(page)
		t.Fatal("production UI did not remove the synthetic credential consumer")
	}
	clearBytes(page)
	settingsAfterTargetDelete, err := readConfig(configPath)
	if err != nil || len(settingsAfterTargetDelete.GitHubTargets) != 0 || len(settingsAfterTargetDelete.NamedSecrets) != 1 || settingsAfterTargetDelete.NamedSecrets[0].CredentialRef != newRef {
		t.Fatal("UI consumer removal changed the named secret unexpectedly")
	}
	if err := cleanupBrowserQAAssertRefState(store, oldRef, newRef, false, true); err != nil {
		t.Fatal("consumer removal deleted the reusable named credential before its UI deletion")
	}

	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "goto", readyTwo.URL+"/")
	clearBytes(page)
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "a[data-pane-link='secrets']")
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "click", "button[data-focus-key='secret-delete:"+secretID+"']")
	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "dialog-accept")
	page = cleanupBrowserQAPlaywrightSnapshot(t, session, oldValue, newValue, "snapshot")
	if bytes.Contains(page, []byte("QA Cleanup Secret")) {
		clearBytes(page)
		t.Fatal("UI still rendered the named secret after its unused deletion")
	}
	clearBytes(page)
	settingsAfterSecretDelete, err := readConfig(configPath)
	if err != nil || len(settingsAfterSecretDelete.NamedSecrets) != 0 || len(settingsAfterSecretDelete.GitHubTargets) != 0 {
		t.Fatal("UI deletion did not remove the unused named secret metadata")
	}
	if err := cleanupBrowserQAAssertQueueEmpty(queue); err != nil {
		t.Fatal("cleanup queue was not empty after named-secret deletion")
	}
	if err := cleanupBrowserQAAssertRefState(store, oldRef, newRef, false, false); err != nil {
		t.Fatal("unused named-secret deletion did not remove its synthetic credential")
	}

	cleanupBrowserQAPlaywright(t, session, oldValue, newValue, "close")
	browserOpen = false
	cleanupBrowserQAStopChild(t, childTwo)
	if err := cleanupBrowserQAAssertAudit(auditTwoPath); err != nil {
		t.Fatal("restarted UI process observed a secret or reference in an HTTP response")
	}
	if err := cleanupBrowserQAAssertPrivateQueue(queuePath); err != nil {
		t.Fatal("durable cleanup queue lost its protected current-user ACL")
	}
}

func cleanupBrowserQAPendingStatusVisible(page []byte) bool {
	lower := bytes.ToLower(page)
	return bytes.Contains(lower, []byte("pending")) || bytes.Contains(page, []byte("남아 있습니다"))
}

func cleanupBrowserQARetryActionVisible(page []byte) bool {
	lower := bytes.ToLower(page)
	return bytes.Contains(lower, []byte("retry cleanup")) || bytes.Contains(page, []byte("정리 재시도"))
}

func TestManualWindowsNamedSecretCleanupBrowserQAChild(t *testing.T) {
	if os.Getenv(cleanupBrowserQARole) == "" {
		t.Skip("child process entry point for the headed browser acceptance")
	}
	configPath := os.Getenv(cleanupBrowserQAConfig)
	queuePath := os.Getenv(cleanupBrowserQAQueue)
	storePath := os.Getenv(cleanupBrowserQAStoreEnv)
	probePath := os.Getenv(cleanupBrowserQAProbeEnv)
	readyPath := os.Getenv(cleanupBrowserQAReadyEnv)
	auditPath := os.Getenv(cleanupBrowserQAAuditEnv)
	if configPath == "" || queuePath == "" || storePath == "" || probePath == "" || readyPath == "" || auditPath == "" {
		t.Fatal("isolated child process inputs are incomplete")
	}
	probeBytes, err := readSetupDraftQueueFile(probePath, cleanupBrowserQAProbeMaxBytes)
	if err != nil {
		t.Fatal("private response probe is unavailable")
	}
	var probe cleanupBrowserQAProbe
	decodeErr := json.Unmarshal(probeBytes, &probe)
	clearBytes(probeBytes)
	if decodeErr != nil || len(probe.Values) != 2 || len(probe.Values[0]) == 0 || len(probe.Values[1]) == 0 {
		for _, value := range probe.Values {
			clearBytes(value)
		}
		t.Fatal("private response probe is invalid")
	}
	defer func() {
		for _, value := range probe.Values {
			clearBytes(value)
		}
	}()

	store := &cleanupBrowserQAStore{path: storePath, allowDelete: os.Getenv(cleanupBrowserQADelete) == "1"}
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("isolated durable cleanup queue is invalid")
	}
	controller := &namedSecretController{
		configPath: configPath,
		store:      store,
		cleanup:    queue,
		newID:      func() (string, error) { return newTargetID("secret") },
		newRef:     newCredentialReference,
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("isolated loopback listener is unavailable")
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	host := net.JoinHostPort("127.0.0.1", fmt.Sprint(address.Port))
	csrf, err := cleanupBrowserQARandomHex(32)
	if err != nil {
		t.Fatal("could not initialize the isolated local UI")
	}
	a := &app{
		configPath:      configPath,
		secrets:         store,
		namedSecrets:    controller,
		userEnvironment: cleanupBrowserQAEmptyEnvironment{},
		client:          newGitHubClient(),
		host:            host,
		csrf:            csrf,
	}
	scanner := &cleanupBrowserQAScanHandler{next: newLocalUIHandler(a), path: auditPath, canaries: probe.Values}
	server := &http.Server{
		Handler:           scanner,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ready, err := json.Marshal(cleanupBrowserQAReady{URL: "http://" + host, Host: host, CSRF: csrf})
	if err != nil || len(ready) > cleanupBrowserQAAuditMaxBytes || writeSetupDraftQueueFileAtomically(readyPath, ready) != nil {
		clearBytes(ready)
		t.Fatal("could not publish the isolated local UI endpoint")
	}
	clearBytes(ready)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("isolated local UI server stopped unexpectedly")
	}
}

func (s *cleanupBrowserQAStore) Save(ref string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !secretRefPattern.MatchString(ref) || len(value) == 0 || len(value) > maxSecretSize {
		return errors.New("synthetic credential store rejected the write")
	}
	state, err := s.readLocked()
	if err != nil {
		return errors.New("synthetic credential store is unavailable")
	}
	defer cleanupBrowserQAClearEntries(state.Entries)
	if state.Entries == nil {
		state.Entries = make(map[string][]byte)
	}
	if prior, ok := state.Entries[ref]; ok {
		clearBytes(prior)
	}
	state.Entries[ref] = append([]byte(nil), value...)
	state.Stats.SaveCalls++
	if err := s.writeLocked(state); err != nil {
		return errors.New("synthetic credential store is unavailable")
	}
	return nil
}

func (s *cleanupBrowserQAStore) Load(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !secretRefPattern.MatchString(ref) {
		return nil, errors.New("synthetic credential store rejected the read")
	}
	state, err := s.readLocked()
	if err != nil {
		return nil, errors.New("synthetic credential store is unavailable")
	}
	defer cleanupBrowserQAClearEntries(state.Entries)
	value, exists := state.Entries[ref]
	state.Stats.LoadCalls++
	if err := s.writeLocked(state); err != nil {
		return nil, errors.New("synthetic credential store is unavailable")
	}
	if !exists {
		return nil, errors.New("synthetic credential is unavailable")
	}
	return append([]byte(nil), value...), nil
}

func (s *cleanupBrowserQAStore) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !secretRefPattern.MatchString(ref) {
		return errors.New("synthetic credential store rejected the delete")
	}
	state, err := s.readLocked()
	if err != nil {
		return errors.New("synthetic credential store is unavailable")
	}
	defer cleanupBrowserQAClearEntries(state.Entries)
	state.Stats.DeleteCalls++
	if !s.allowDelete {
		state.Stats.DeleteFailures++
		if s.writeLocked(state) != nil {
			return errors.New("synthetic credential store is unavailable")
		}
		return errors.New("synthetic delete failure")
	}
	value, exists := state.Entries[ref]
	if !exists {
		state.Stats.DeleteFailures++
		if s.writeLocked(state) != nil {
			return errors.New("synthetic credential store is unavailable")
		}
		return errors.New("synthetic credential is unavailable")
	}
	clearBytes(value)
	delete(state.Entries, ref)
	state.Stats.DeleteSuccesses++
	if err := s.writeLocked(state); err != nil {
		return errors.New("synthetic credential store is unavailable")
	}
	return nil
}

func (s *cleanupBrowserQAStore) readLocked() (cleanupBrowserQAStoreEnvelope, error) {
	if s == nil || s.path == "" {
		return cleanupBrowserQAStoreEnvelope{}, errors.New("synthetic credential store is unavailable")
	}
	data, err := readSetupDraftQueueFile(s.path, cleanupBrowserQAStoreMaxBytes)
	if err != nil || len(data) == 0 {
		return cleanupBrowserQAStoreEnvelope{}, errors.New("synthetic credential store is unavailable")
	}
	defer clearBytes(data)
	var state cleanupBrowserQAStoreEnvelope
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Entries == nil || len(state.Entries) > 8 {
		cleanupBrowserQAClearEntries(state.Entries)
		return cleanupBrowserQAStoreEnvelope{}, errors.New("synthetic credential store is unavailable")
	}
	for ref, value := range state.Entries {
		if !secretRefPattern.MatchString(ref) || len(value) == 0 || len(value) > maxSecretSize {
			cleanupBrowserQAClearEntries(state.Entries)
			return cleanupBrowserQAStoreEnvelope{}, errors.New("synthetic credential store is unavailable")
		}
	}
	return state, nil
}

func (s *cleanupBrowserQAStore) writeLocked(state cleanupBrowserQAStoreEnvelope) error {
	if len(state.Entries) > 8 {
		return errors.New("synthetic credential store is full")
	}
	state.Version = 1
	data, err := json.Marshal(state)
	if err != nil || len(data) > cleanupBrowserQAStoreMaxBytes {
		clearBytes(data)
		return errors.New("synthetic credential store exceeded its bound")
	}
	defer clearBytes(data)
	return writeSetupDraftQueueFileAtomically(s.path, data)
}

func (s *cleanupBrowserQAStore) state() (cleanupBrowserQAStoreEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readLocked()
	if err != nil {
		return cleanupBrowserQAStoreEnvelope{}, err
	}
	copyState := cleanupBrowserQAStoreEnvelope{Version: state.Version, Stats: state.Stats, Entries: make(map[string][]byte, len(state.Entries))}
	for ref, value := range state.Entries {
		copyState.Entries[ref] = append([]byte(nil), value...)
	}
	cleanupBrowserQAClearEntries(state.Entries)
	return copyState, nil
}

func (s *cleanupBrowserQAStore) stats() (cleanupBrowserQAStoreStats, error) {
	state, err := s.state()
	if err != nil {
		return cleanupBrowserQAStoreStats{}, err
	}
	defer cleanupBrowserQAClearEntries(state.Entries)
	return state.Stats, nil
}

func cleanupBrowserQAClearEntries(entries map[string][]byte) {
	for _, value := range entries {
		clearBytes(value)
	}
}

func (h *cleanupBrowserQAScanHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	capture := &cleanupBrowserQACaptureWriter{ResponseWriter: w}
	h.next.ServeHTTP(capture, r)
	canaryExposed := false
	for _, value := range h.canaries {
		if len(value) > 0 && bytes.Contains(capture.body, value) {
			canaryExposed = true
		}
	}
	credentialRefExposed := cleanupBrowserCredentialRefPattern.Match(capture.body)
	for key, values := range w.Header() {
		for _, value := range values {
			encoded := []byte(key + ":" + value)
			for _, canary := range h.canaries {
				if len(canary) > 0 && bytes.Contains(encoded, canary) {
					canaryExposed = true
				}
			}
			credentialRefExposed = credentialRefExposed || cleanupBrowserCredentialRefPattern.Match(encoded)
			clearBytes(encoded)
		}
	}
	clearBytes(capture.body)
	h.mu.Lock()
	h.audit.Responses++
	h.audit.CanaryExposed = h.audit.CanaryExposed || canaryExposed
	h.audit.CredentialRefExposed = h.audit.CredentialRefExposed || credentialRefExposed
	h.audit.ScanOverflow = h.audit.ScanOverflow || capture.overflow
	data, err := json.Marshal(h.audit)
	if err == nil && len(data) <= cleanupBrowserQAAuditMaxBytes {
		_ = writeSetupDraftQueueFileAtomically(h.path, data)
	}
	h.mu.Unlock()
	clearBytes(data)
}

func (w *cleanupBrowserQACaptureWriter) Write(data []byte) (int, error) {
	if len(w.body)+len(data) <= cleanupBrowserQAResponseMaxByte {
		w.body = append(w.body, data...)
	} else {
		w.overflow = true
		remaining := cleanupBrowserQAResponseMaxByte - len(w.body)
		if remaining > 0 {
			w.body = append(w.body, data[:remaining]...)
		}
	}
	return w.ResponseWriter.Write(data)
}

func cleanupBrowserQARandomValue() ([]byte, error) {
	value, err := cleanupBrowserQARandomHex(24)
	if err != nil {
		return nil, err
	}
	return []byte("qa-" + value), nil
}

func cleanupBrowserQARandomHex(count int) (string, error) {
	if count < 1 || count > 64 {
		return "", errors.New("invalid random length")
	}
	data := make([]byte, count)
	if _, err := cryptorand.Read(data); err != nil {
		clearBytes(data)
		return "", err
	}
	encoded := hex.EncodeToString(data)
	clearBytes(data)
	return encoded, nil
}

func cleanupBrowserQAPlaywright(t *testing.T, session string, oldValue, newValue []byte, args ...string) {
	t.Helper()
	output := cleanupBrowserQARunPlaywright(t, session, args...)
	containsSyntheticValue := bytes.Contains(output, oldValue) || bytes.Contains(output, newValue)
	containsCredentialRef := cleanupBrowserCredentialRefPattern.Match(output)
	step := "other"
	if len(args) > 0 {
		step = cleanupBrowserQASafeStage(args[0])
	}
	if step == "fill" && containsSyntheticValue && !containsCredentialRef {
		// The CLI may acknowledge a fill by echoing its supplied test input. The
		// acknowledgement stays in memory and is discarded; page snapshots and
		// the local server response audit still reject either synthetic value.
		clearBytes(output)
		return
	}
	if containsSyntheticValue || containsCredentialRef {
		clearBytes(output)
		t.Fatal("headed browser output contained a synthetic value or credential reference")
	}
	clearBytes(output)
}

func cleanupBrowserQAPlaywrightSnapshot(t *testing.T, session string, oldValue, newValue []byte, args ...string) []byte {
	t.Helper()
	output := cleanupBrowserQARunPlaywright(t, session, args...)
	if bytes.Contains(output, oldValue) || bytes.Contains(output, newValue) || cleanupBrowserCredentialRefPattern.Match(output) {
		clearBytes(output)
		t.Fatal("headed UI output contained a synthetic value or credential reference")
	}
	return output
}

func cleanupBrowserQARunPlaywright(t *testing.T, session string, args ...string) []byte {
	t.Helper()
	step := "unknown"
	if len(args) > 0 {
		step = cleanupBrowserQASafeStage(args[0])
	}
	call := atomic.AddUint32(&cleanupBrowserQACallCounter, 1)
	t.Logf("browser_stage_start=%02d:%s", call, step)
	commandTimeout := 60 * time.Second
	if step == "close" {
		commandTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	nodePath, cliPath, err := cleanupBrowserQAResolvePlaywrightCLI()
	if err != nil {
		t.Fatal("cached Playwright CLI is unavailable for the headed UI harness")
	}
	cliArgs := []string{cliPath, "--session", session}
	cliArgs = append(cliArgs, args...)
	command := exec.CommandContext(ctx, nodePath, cliArgs...)
	command.Dir = filepath.Dir(os.Args[0])
	command.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if command.Process == nil || command.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
		killer.Stdout = io.Discard
		killer.Stderr = io.Discard
		if err := killer.Run(); err != nil {
			return errors.New("could not stop the timed-out Playwright process tree")
		}
		return nil
	}
	capture := &cleanupBrowserQACapture{limit: 2 << 20}
	command.Stdout, command.Stderr = capture, capture
	err = command.Run()
	output := capture.bytes()
	overflow := capture.isOverflowed()
	capture.clear()
	if err != nil || overflow {
		clearBytes(output)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && step != "close" {
			cleanupBrowserQACloseSessionAfterTimeout(nodePath, cliPath, session)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("headed browser command timed out at safe stage %q", step)
		}
		t.Fatalf("headed browser command failed at safe stage %q", step)
	}
	t.Logf("browser_stage_done=%02d:%s", call, step)
	return output
}

func cleanupBrowserQASafeStage(value string) string {
	switch value {
	case "open", "goto", "snapshot", "click", "fill", "select", "dialog-accept", "check", "close":
		return value
	default:
		return "other"
	}
}

func cleanupBrowserQAResolvePlaywrightCLI() (string, string, error) {
	nodePath, err := exec.LookPath("node.exe")
	if err != nil {
		return "", "", errors.New("Node.js runtime is unavailable")
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return "", "", errors.New("local npm cache is unavailable")
	}
	pattern := filepath.Join(localAppData, "npm-cache", "_npx", "*", "node_modules", "@playwright", "cli", "playwright-cli.js")
	candidates, err := filepath.Glob(pattern)
	if err != nil {
		return "", "", errors.New("local Playwright CLI cache could not be inspected")
	}
	for _, candidate := range candidates {
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() {
			return nodePath, candidate, nil
		}
	}
	return "", "", errors.New("Playwright CLI is not present in the local npx cache")
}

func cleanupBrowserQACloseSessionAfterTimeout(nodePath, cliPath, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, cliPath, "--session", session, "close")
	command.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
	command.WaitDelay = time.Second
	command.Cancel = func() error {
		if command.Process == nil || command.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
		killer.Stdout = io.Discard
		killer.Stderr = io.Discard
		if err := killer.Run(); err != nil {
			return errors.New("could not stop the timed-out browser cleanup process tree")
		}
		return nil
	}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	_ = command.Run()
}

type cleanupBrowserQACapture struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	overflow bool
}

func (c *cleanupBrowserQACapture) Write(value []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.limit - len(c.data)
	if remaining <= 0 {
		c.overflow = true
		return len(value), nil
	}
	if len(value) > remaining {
		c.data = append(c.data, value[:remaining]...)
		c.overflow = true
		return len(value), nil
	}
	c.data = append(c.data, value...)
	return len(value), nil
}

func (c *cleanupBrowserQACapture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}

func (c *cleanupBrowserQACapture) isOverflowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.overflow
}

func (c *cleanupBrowserQACapture) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clearBytes(c.data)
	c.data = nil
}

func cleanupBrowserQACheckRouteRejections(t *testing.T, ready cleanupBrowserQAReady, id string, store *cleanupBrowserQAStore, oldValue, newValue []byte) {
	t.Helper()
	before, err := store.stats()
	if err != nil {
		t.Fatal("could not read safe fake-store operation counters")
	}
	base := url.Values{"csrf": {ready.CSRF}, "id": {id}}
	wrongCSRF := url.Values{"csrf": {"wrong"}, "id": {id}}
	if status, code := cleanupBrowserQAPost(t, ready, uiRouteRetryNamedSecretCleanup, wrongCSRF, oldValue, newValue, ""); status != http.StatusForbidden || code != "request_rejected" {
		t.Fatal("retry route did not reject an invalid CSRF token")
	}
	if status, code := cleanupBrowserQAPost(t, ready, uiRouteRetryNamedSecretCleanup, base, oldValue, newValue, "http://attacker.invalid"); status != http.StatusForbidden || code != "" {
		t.Fatal("retry route did not reject a foreign Origin")
	}
	unknownID := "secret:" + strings.Repeat("f", 32)
	if unknownID == id {
		unknownID = "secret:" + strings.Repeat("e", 32)
	}
	unknown := url.Values{"csrf": {ready.CSRF}, "id": {unknownID}}
	if status, code := cleanupBrowserQAPost(t, ready, uiRouteRetryNamedSecretCleanup, unknown, oldValue, newValue, ""); status != http.StatusConflict || code != "not_found" {
		t.Fatal("retry route did not reject an unknown logical secret ID")
	}
	hostStatus, _ := cleanupBrowserQAPostWithHost(t, ready, uiRouteRetryNamedSecretCleanup, base, "attacker.invalid", oldValue, newValue, "")
	if hostStatus < http.StatusBadRequest || hostStatus >= http.StatusInternalServerError {
		t.Fatalf("mismatched Host request was not rejected with a client error status=%d", hostStatus)
	}
	t.Logf("host_probe_rejected=true status=%d", hostStatus)
	after, err := store.stats()
	if err != nil || before != after {
		t.Fatal("protected-route rejection reached the synthetic credential store")
	}
}

func cleanupBrowserQAPost(t *testing.T, ready cleanupBrowserQAReady, path string, values url.Values, oldValue, newValue []byte, origin string) (int, string) {
	return cleanupBrowserQAPostWithHost(t, ready, path, values, ready.Host, oldValue, newValue, origin)
}

func cleanupBrowserQAPostWithHost(t *testing.T, ready cleanupBrowserQAReady, path string, values url.Values, host string, oldValue, newValue []byte, origin string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, ready.URL+path, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal("could not prepare a local protected-route request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("protected-route request did not reach the local UI")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		clearBytes(body)
		t.Fatal("protected-route response exceeded its small evidence bound")
	}
	defer clearBytes(body)
	if bytes.Contains(body, oldValue) || bytes.Contains(body, newValue) || cleanupBrowserCredentialRefPattern.Match(body) {
		t.Fatal("protected-route response exposed a synthetic value or credential reference")
	}
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	return response.StatusCode, payload.Error
}

func cleanupBrowserQAStartChild(t *testing.T, mode, configPath, queuePath, storePath, probePath, readyPath, auditPath string) *cleanupBrowserQAChild {
	t.Helper()
	_ = os.Remove(readyPath)
	command := exec.Command(os.Args[0], "-test.run=^TestManualWindowsNamedSecretCleanupBrowserQAChild$")
	command.Env = cleanupBrowserQAChildEnvironment(mode, configPath, queuePath, storePath, probePath, readyPath, auditPath)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	child := &cleanupBrowserQAChild{command: command, done: make(chan error, 1)}
	if err := command.Start(); err != nil {
		t.Fatal("could not start the isolated production UI child")
	}
	go func() { child.done <- command.Wait() }()
	t.Cleanup(func() { cleanupBrowserQAStopChild(t, child) })
	return child
}

type cleanupBrowserQAChild struct {
	command *exec.Cmd
	done    chan error
	stopped bool
}

func cleanupBrowserQAChildEnvironment(mode, configPath, queuePath, storePath, probePath, readyPath, auditPath string) []string {
	environment := make([]string, 0, 13)
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	allowDelete := "0"
	if mode == "allow" {
		allowDelete = "1"
	}
	for _, pair := range [][2]string{
		{cleanupBrowserQARole, "server"}, {cleanupBrowserQAConfig, configPath},
		{cleanupBrowserQAQueue, queuePath}, {cleanupBrowserQAStoreEnv, storePath},
		{cleanupBrowserQAProbeEnv, probePath}, {cleanupBrowserQAReadyEnv, readyPath},
		{cleanupBrowserQAAuditEnv, auditPath}, {cleanupBrowserQADelete, allowDelete},
	} {
		environment = append(environment, pair[0]+"="+pair[1])
	}
	return environment
}

func cleanupBrowserQAWaitReady(t *testing.T, child *cleanupBrowserQAChild, path string) cleanupBrowserQAReady {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-child.done:
			t.Fatal("isolated UI child exited before publishing its loopback endpoint")
		default:
		}
		data, err := readSetupDraftQueueFile(path, cleanupBrowserQAAuditMaxBytes)
		if err == nil {
			var ready cleanupBrowserQAReady
			decodeErr := json.Unmarshal(data, &ready)
			clearBytes(data)
			if decodeErr == nil && strings.HasPrefix(ready.URL, "http://127.0.0.1:") && ready.Host != "" && ready.CSRF != "" {
				return ready
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the isolated UI child endpoint")
	return cleanupBrowserQAReady{}
}

func cleanupBrowserQAStopChild(t *testing.T, child *cleanupBrowserQAChild) {
	t.Helper()
	if child == nil || child.stopped || child.command == nil || child.command.Process == nil {
		return
	}
	child.stopped = true
	_ = child.command.Process.Kill()
	select {
	case <-child.done:
	case <-time.After(5 * time.Second):
		t.Fatal("isolated UI child did not stop")
	}
}

func cleanupBrowserQAAssertPending(queue namedSecretCleanupQueue, id string) error {
	intents, err := queue.Load()
	if err != nil || len(intents) != 1 || intents[0].ID != id || !intents[0].Committed || !intents[0].DeleteOld {
		return errors.New("pending cleanup intent is not present")
	}
	return nil
}

func cleanupBrowserQAAssertQueueEmpty(queue namedSecretCleanupQueue) error {
	intents, err := queue.Load()
	if err != nil || len(intents) != 0 {
		return errors.New("cleanup queue is not empty")
	}
	return nil
}

func cleanupBrowserQAAssertRefState(store *cleanupBrowserQAStore, oldRef, newRef string, wantOld, wantNew bool) error {
	state, err := store.state()
	if err != nil {
		return errors.New("synthetic credential store state is unavailable")
	}
	defer cleanupBrowserQAClearEntries(state.Entries)
	_, oldExists := state.Entries[oldRef]
	_, newExists := state.Entries[newRef]
	if oldExists != wantOld || newExists != wantNew || state.Stats.DeleteCalls < state.Stats.DeleteFailures+state.Stats.DeleteSuccesses {
		return errors.New("synthetic credential store state did not match the expected lifecycle")
	}
	info, err := os.Stat(store.path)
	if err != nil || info.Size() > cleanupBrowserQAStoreMaxBytes {
		return errors.New("synthetic credential store exceeded its private size bound")
	}
	return nil
}

func cleanupBrowserQAAssertAudit(path string) error {
	data, err := readSetupDraftQueueFile(path, cleanupBrowserQAAuditMaxBytes)
	if err != nil {
		return errors.New("safe HTTP response audit is unavailable")
	}
	defer clearBytes(data)
	var audit cleanupBrowserQAAudit
	if json.Unmarshal(data, &audit) != nil || audit.Responses == 0 || audit.CanaryExposed || audit.CredentialRefExposed || audit.ScanOverflow {
		return errors.New("HTTP response audit detected unsafe or incomplete evidence")
	}
	return nil
}

func cleanupBrowserQAAssertPrivateQueue(path string) error {
	queue, err := newNamedSecretCleanupFileQueueAt(path)
	if err != nil {
		return errors.New("durable queue path is invalid")
	}
	if _, err := queue.Load(); err != nil {
		return errors.New("durable queue ACL validation failed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > namedSecretCleanupQueueMaxBytes {
		return errors.New("durable queue is unavailable or exceeded its size bound")
	}
	return nil
}

func cleanupBrowserQASecureRewrite(path string, maxBytes int64) error {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) > maxBytes {
		clearBytes(data)
		return errors.New("isolated settings are unavailable")
	}
	defer clearBytes(data)
	if err := writeSetupDraftQueueFileAtomically(path, data); err != nil {
		return errors.New("isolated settings ACL could not be applied")
	}
	verified, err := readSetupDraftQueueFile(path, maxBytes)
	if err != nil {
		return errors.New("isolated settings ACL validation failed")
	}
	clearBytes(verified)
	return nil
}
