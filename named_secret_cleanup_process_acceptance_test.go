//go:build windows && integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	cleanupProcessRoleEnv   = "LAH_QA_CLEANUP_PROCESS_ROLE"
	cleanupProcessConfigEnv = "LAH_QA_CLEANUP_PROCESS_CONFIG"
	cleanupProcessQueueEnv  = "LAH_QA_CLEANUP_PROCESS_QUEUE"
	cleanupProcessStoreEnv  = "LAH_QA_CLEANUP_PROCESS_STORE"
	cleanupProcessReadyEnv  = "LAH_QA_CLEANUP_PROCESS_READY"
	cleanupProcessCSRFEnv   = "LAH_QA_CLEANUP_PROCESS_CSRF"
	cleanupProcessMaxBytes  = 64 << 10
)

type cleanupProcessStoreState struct {
	Version int                      `json:"version"`
	Entries map[string][]byte        `json:"entries"`
	Stats   cleanupProcessStoreStats `json:"stats"`
}

type cleanupProcessStoreStats struct {
	Saves          int `json:"saves"`
	Loads          int `json:"loads"`
	DeleteFailures int `json:"delete_failures"`
	DeleteSuccess  int `json:"delete_successes"`
}

type cleanupProcessSecretStore struct {
	path        string
	denyDeletes bool
}

type cleanupProcessNoEnvironment struct{}

func (cleanupProcessNoEnvironment) ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error) {
	return []UserEnvironmentEntry{}, nil
}

func (cleanupProcessNoEnvironment) SetUserEnvironment(context.Context, UserEnvironmentWrite) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errors.New("synthetic environment backend disabled")
}

func (cleanupProcessNoEnvironment) DeleteUserEnvironment(context.Context, string) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errors.New("synthetic environment backend disabled")
}

func TestNamedSecretCleanupHandlerProcessAcceptance(t *testing.T) {
	if os.Getenv(cleanupProcessRoleEnv) != "" {
		t.Skip("the helper subprocess has a separate test entry point")
	}

	root := t.TempDir()
	configPath := filepath.Join(root, "settings.json")
	queuePath := filepath.Join(root, setupDraftQueueDirectoryName, namedSecretCleanupQueueFileName)
	storePath := filepath.Join(root, setupDraftQueueDirectoryName, "qa-synthetic-secret-store.json")
	readyPath := filepath.Join(root, "retry-ready.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not initialize the isolated v9 settings fixture")
	}
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("could not construct the isolated durable cleanup queue")
	}
	store := &cleanupProcessSecretStore{path: storePath, denyDeletes: true}
	if err := store.initialize(); err != nil {
		t.Fatal("could not initialize the synthetic credential store")
	}

	csrf, err := cleanupProcessRandomHex(32)
	if err != nil {
		t.Fatal("could not initialize the local test handler")
	}
	controller := cleanupProcessController(configPath, store, queue)
	appOne := cleanupProcessApp(configPath, store, controller, csrf)
	serverOne, clientOne := cleanupProcessStartHTTP(t, appOne)
	oldValue := cleanupProcessValue(t)
	defer clearBytes(oldValue)
	newValue := cleanupProcessValue(t)
	defer clearBytes(newValue)

	createResponse, createBody := cleanupProcessPost(t, clientOne, serverOne.URL, appOne.host, uiRouteSaveNamedSecret, url.Values{
		"csrf": {csrf}, "name": {"Synthetic cleanup lifecycle"}, "purpose": {"isolated regression fixture"}, "value": {string(oldValue)},
	})
	cleanupProcessAssertSafe(t, createBody, createResponse.Header.Get("Location"), oldValue, newValue)
	var created namedSecretUISuccess
	if createResponse.StatusCode != http.StatusOK || json.Unmarshal(createBody, &created) != nil ||
		!created.OK || !created.Secret.Configured || created.Secret.InUse || !namedSecretIDPattern.MatchString(created.Secret.ID) {
		t.Fatal("production create handler did not return safe synthetic metadata")
	}
	secretID := created.Secret.ID
	clearBytes(createBody)

	targetResponse, targetBody := cleanupProcessPost(t, clientOne, serverOne.URL, appOne.host, "/save", url.Values{
		"csrf":            {csrf},
		"target_id":       {"new"},
		"name":            {"Synthetic cleanup consumer"},
		"origin":          {"https://github.example.invalid"},
		"repository":      {"synthetic/cleanup-process"},
		"token":           {""},
		"named_secret_id": {secretID},
	})
	cleanupProcessAssertSafe(t, targetBody, targetResponse.Header.Get("Location"), oldValue, newValue)
	if targetResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target handler did not accept the saved logical secret ID")
	}
	clearBytes(targetBody)
	configBeforeRotation := cleanupProcessReadConfig(t, configPath)
	if len(configBeforeRotation.NamedSecrets) != 1 || len(configBeforeRotation.GitHubTargets) != 1 {
		t.Fatal("synthetic target and named-secret metadata were not both persisted")
	}
	oldRef := configBeforeRotation.NamedSecrets[0].CredentialRef
	targetID := configBeforeRotation.GitHubTargets[0].ID
	if configBeforeRotation.GitHubTargets[0].SecretRef != oldRef {
		t.Fatal("saved target did not point to the selected named-secret reference")
	}
	cleanupProcessAssertStoreValue(t, store, oldRef, oldValue)

	rotateResponse, rotateBody := cleanupProcessPost(t, clientOne, serverOne.URL, appOne.host, uiRouteSaveNamedSecret, url.Values{
		"csrf": {csrf}, "id": {secretID}, "name": {"Synthetic cleanup lifecycle"},
		"purpose": {"isolated rotation with failed cleanup"}, "value": {string(newValue)},
	})
	cleanupProcessAssertSafe(t, rotateBody, rotateResponse.Header.Get("Location"), oldValue, newValue)
	var rotated namedSecretUISuccess
	if rotateResponse.StatusCode != http.StatusOK || json.Unmarshal(rotateBody, &rotated) != nil ||
		!rotated.OK || rotated.Secret.ID != secretID || !rotated.Secret.InUse || rotated.Warning != "cleanup_pending" {
		t.Fatal("production rotation did not report the committed value with pending cleanup")
	}
	clearBytes(rotateBody)

	configAfterRotation := cleanupProcessReadConfig(t, configPath)
	if len(configAfterRotation.NamedSecrets) != 1 || len(configAfterRotation.GitHubTargets) != 1 {
		t.Fatal("rotation unexpectedly changed synthetic target or secret counts")
	}
	newRef := configAfterRotation.NamedSecrets[0].CredentialRef
	if newRef == oldRef || configAfterRotation.GitHubTargets[0].SecretRef != newRef {
		t.Fatal("rotation did not update the named secret and its active consumer together")
	}
	cleanupProcessAssertStoreValue(t, store, oldRef, oldValue)
	cleanupProcessAssertStoreValue(t, store, newRef, newValue)
	configBytes, err := os.ReadFile(configPath)
	if err != nil || bytes.Contains(configBytes, oldValue) || bytes.Contains(configBytes, newValue) {
		clearBytes(configBytes)
		t.Fatal("settings persisted a synthetic credential value")
	}
	clearBytes(configBytes)
	queueBytes, err := os.ReadFile(queuePath)
	if err != nil || bytes.Contains(queueBytes, oldValue) || bytes.Contains(queueBytes, newValue) {
		clearBytes(queueBytes)
		t.Fatal("durable cleanup queue was missing or contained a synthetic value")
	}
	clearBytes(queueBytes)
	pendingIDs, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(pendingIDs) != 1 || pendingIDs[0] != secretID {
		t.Fatal("failed deletion did not leave one durable logical cleanup item")
	}
	storeState, err := store.snapshot()
	if err != nil || storeState.Stats.DeleteFailures != 1 || storeState.Stats.DeleteSuccess != 0 {
		cleanupProcessClearStoreState(storeState)
		t.Fatal("synthetic old-reference delete did not fail before process restart")
	}
	cleanupProcessClearStoreState(storeState)
	serverOne.Close()

	controllerAfterClose := cleanupProcessController(configPath, store, queue)
	pendingIDs, err = controllerAfterClose.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(pendingIDs) != 1 || pendingIDs[0] != secretID {
		t.Fatal("reopened controller did not recover the durable logical cleanup item")
	}

	child := cleanupProcessStartRetryChild(t, configPath, queuePath, storePath, readyPath)
	ready := child.ready
	retryResponse, retryBody := cleanupProcessPost(t, child.client, ready.URL, ready.Host, uiRouteRetryNamedSecretCleanup, url.Values{
		"csrf": {ready.CSRF}, "id": {secretID},
	})
	cleanupProcessAssertSafe(t, retryBody, retryResponse.Header.Get("Location"), oldValue, newValue)
	var retryResult namedSecretCleanupUISuccess
	if retryResponse.StatusCode != http.StatusOK || json.Unmarshal(retryBody, &retryResult) != nil ||
		!retryResult.OK || retryResult.CleanupPending {
		t.Fatal("fresh process production retry handler did not complete the durable cleanup")
	}
	clearBytes(retryBody)

	if pending, err := controllerAfterClose.RetryNamedSecretCleanup(context.Background(), secretID); err != nil || pending {
		t.Fatal("repeating cleanup after process restart was not idempotent")
	}
	if pendingIDs, err = controllerAfterClose.PendingNamedSecretCleanupIDs(context.Background()); err != nil || len(pendingIDs) != 0 {
		t.Fatal("successful fresh-process retry left a pending cleanup item")
	}
	retriedState, err := store.snapshot()
	if err != nil {
		t.Fatal("could not inspect synthetic store after process retry")
	}
	oldStored, oldExists := retriedState.Entries[oldRef]
	newStored, newExists := retriedState.Entries[newRef]
	if oldExists || !newExists || !bytes.Equal(newStored, newValue) || retriedState.Stats.DeleteSuccess != 1 || retriedState.Stats.DeleteFailures < 1 {
		cleanupProcessClearStoreState(retriedState)
		t.Fatal("process retry did not remove only the obsolete credential while retaining the active value")
	}
	clearBytes(oldStored)
	cleanupProcessClearStoreState(retriedState)
	cleanupProcessAssertStoreValue(t, store, newRef, newValue)
	statsBeforeGuard, err := store.stats()
	if err != nil {
		t.Fatal("could not read synthetic store counters")
	}
	guardResponse, guardBody := cleanupProcessPost(t, child.client, ready.URL, ready.Host, uiRouteRetryNamedSecretCleanup, url.Values{
		"csrf": {ready.CSRF}, "id": {secretID},
	})
	cleanupProcessAssertSafe(t, guardBody, guardResponse.Header.Get("Location"), oldValue, newValue)
	if guardResponse.StatusCode != http.StatusConflict || !strings.Contains(string(guardBody), `"error":"not_found"`) {
		t.Fatal("retry route did not reject the active-only logical ID after cleanup")
	}
	clearBytes(guardBody)
	statsAfterGuard, err := store.stats()
	if err != nil || statsAfterGuard != statsBeforeGuard {
		t.Fatal("rejected repeat retry changed synthetic store state")
	}

	deleteTargetResponse, deleteTargetBody := cleanupProcessPost(t, child.client, ready.URL, ready.Host, "/delete-target", url.Values{
		"csrf": {ready.CSRF}, "kind": {"github"}, "target_id": {targetID}, "confirm_delete": {"yes"},
	})
	cleanupProcessAssertSafe(t, deleteTargetBody, deleteTargetResponse.Header.Get("Location"), oldValue, newValue)
	if deleteTargetResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target delete handler did not detach the synthetic consumer")
	}
	clearBytes(deleteTargetBody)
	configAfterDetach := cleanupProcessReadConfig(t, configPath)
	if len(configAfterDetach.GitHubTargets) != 0 || len(configAfterDetach.NamedSecrets) != 1 ||
		configAfterDetach.NamedSecrets[0].CredentialRef != newRef {
		t.Fatal("detaching the target did not leave the configured named secret unused")
	}
	unusedResponse, unusedBody := cleanupProcessPost(t, child.client, ready.URL, ready.Host, uiRouteDeleteNamedSecret, url.Values{
		"csrf": {ready.CSRF}, "id": {secretID},
	})
	cleanupProcessAssertSafe(t, unusedBody, unusedResponse.Header.Get("Location"), oldValue, newValue)
	if unusedResponse.StatusCode != http.StatusOK || !strings.Contains(string(unusedBody), `"ok":true`) {
		t.Fatal("production named-secret delete handler did not remove the now-unused secret")
	}
	clearBytes(unusedBody)
	configAfterDelete := cleanupProcessReadConfig(t, configPath)
	if len(configAfterDelete.NamedSecrets) != 0 || len(configAfterDelete.GitHubTargets) != 0 {
		t.Fatal("unused named-secret deletion left configuration metadata")
	}
	storeState, err = store.snapshot()
	if err != nil {
		t.Fatal("could not verify final synthetic store state")
	}
	defer cleanupProcessClearStoreState(storeState)
	if len(storeState.Entries) != 0 || storeState.Stats.DeleteSuccess != 2 {
		t.Fatal("process retry plus unused-secret deletion left synthetic credentials behind")
	}
}

func TestNamedSecretCleanupHandlerProcessChild(t *testing.T) {
	if os.Getenv(cleanupProcessRoleEnv) != "server" {
		t.Skip("helper process entry point for cleanup HTTP acceptance")
	}
	configPath := os.Getenv(cleanupProcessConfigEnv)
	queuePath := os.Getenv(cleanupProcessQueueEnv)
	storePath := os.Getenv(cleanupProcessStoreEnv)
	readyPath := os.Getenv(cleanupProcessReadyEnv)
	csrf := os.Getenv(cleanupProcessCSRFEnv)
	if configPath == "" || queuePath == "" || storePath == "" || readyPath == "" || csrf == "" {
		t.Fatal("retry helper received incomplete isolated paths")
	}
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("retry helper could not reopen the durable queue")
	}
	store := &cleanupProcessSecretStore{path: storePath}
	controller := cleanupProcessController(configPath, store, queue)
	a := cleanupProcessApp(configPath, store, controller, csrf)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("retry helper could not bind IPv4 loopback")
	}
	defer listener.Close()
	a.host = listener.Addr().String()
	server := &http.Server{Handler: newLocalUIHandler(a), ReadHeaderTimeout: 2 * time.Second}
	ready, err := json.Marshal(cleanupProcessReady{URL: "http://" + a.host, Host: a.host, CSRF: csrf, PID: os.Getpid()})
	if err != nil || len(ready) > 2<<10 || os.WriteFile(readyPath, ready, 0o600) != nil {
		clearBytes(ready)
		t.Fatal("retry helper could not publish loopback readiness")
	}
	clearBytes(ready)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("retry helper HTTP server stopped unexpectedly")
	}
	t.Fatal("retry helper HTTP server exited unexpectedly")
}

type cleanupProcessReady struct {
	URL  string `json:"url"`
	Host string `json:"host"`
	CSRF string `json:"csrf"`
	PID  int    `json:"pid"`
}

type cleanupProcessChild struct {
	cmd    *exec.Cmd
	done   chan error
	ready  cleanupProcessReady
	client *http.Client
}

func cleanupProcessStartRetryChild(t *testing.T, configPath, queuePath, storePath, readyPath string) *cleanupProcessChild {
	t.Helper()
	csrf, err := cleanupProcessRandomHex(32)
	if err != nil {
		t.Fatal("could not initialize fresh-process request protection")
	}
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("could not resolve the test executable")
	}
	cmd := exec.Command(executable, "-test.run=^TestNamedSecretCleanupHandlerProcessChild$")
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"TEMP=" + filepath.Dir(configPath),
		"TMP=" + filepath.Dir(configPath),
		cleanupProcessRoleEnv + "=server",
		cleanupProcessConfigEnv + "=" + configPath,
		cleanupProcessQueueEnv + "=" + queuePath,
		cleanupProcessStoreEnv + "=" + storePath,
		cleanupProcessReadyEnv + "=" + readyPath,
		cleanupProcessCSRFEnv + "=" + csrf,
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	child := &cleanupProcessChild{cmd: cmd, done: make(chan error, 1)}
	if err := cmd.Start(); err != nil {
		t.Fatal("could not start the fresh retry process")
	}
	go func() {
		child.done <- cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(func() { cleanupProcessStopChild(child) })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-child.done:
			t.Fatal("fresh retry process exited before publishing readiness")
		default:
		}
		data, readErr := os.ReadFile(readyPath)
		if readErr == nil {
			if len(data) > 2<<10 || json.Unmarshal(data, &child.ready) != nil {
				clearBytes(data)
				t.Fatal("fresh retry process readiness was malformed")
			}
			clearBytes(data)
			if child.ready.PID == os.Getpid() || child.ready.PID <= 0 || child.ready.Host == "" ||
				child.ready.URL != "http://"+child.ready.Host || child.ready.CSRF != csrf {
				t.Fatal("fresh retry process did not provide a distinct loopback server")
			}
			child.client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			return child
		}
		time.Sleep(25 * time.Millisecond)
	}
	cleanupProcessStopChild(child)
	t.Fatal("fresh retry process did not become ready before timeout")
	return nil
}

func cleanupProcessStopChild(child *cleanupProcessChild) {
	if child == nil || child.cmd == nil || child.cmd.Process == nil {
		return
	}
	_ = child.cmd.Process.Kill()
	select {
	case <-child.done:
	case <-time.After(5 * time.Second):
	}
	child.cmd = nil
}

func cleanupProcessApp(configPath string, store secretStore, controller *namedSecretController, csrf string) *app {
	return &app{
		configPath:      configPath,
		secrets:         store,
		namedSecrets:    controller,
		userEnvironment: cleanupProcessNoEnvironment{},
		client:          newGitHubClient(),
		csrf:            csrf,
	}
}

func cleanupProcessController(configPath string, store secretStore, queue namedSecretCleanupQueue) *namedSecretController {
	return &namedSecretController{
		configPath: configPath,
		store:      store,
		cleanup:    queue,
		persist:    writeConfig,
		newID:      func() (string, error) { return newTargetID("secret") },
		newRef:     newCredentialReference,
	}
}

func cleanupProcessStartHTTP(t *testing.T, a *app) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return server, client
}

func cleanupProcessPost(t *testing.T, client *http.Client, baseURL, host, route string, values url.Values) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+route, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal("could not create the synthetic local POST")
	}
	request.Host = host
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://"+host)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("production local UI request did not complete")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cleanupProcessMaxBytes+1))
	if err != nil || len(body) > cleanupProcessMaxBytes {
		clearBytes(body)
		t.Fatal("production local UI response exceeded the bounded test capture")
	}
	return response, body
}

func cleanupProcessAssertSafe(t *testing.T, body []byte, location string, values ...[]byte) {
	t.Helper()
	for _, value := range values {
		if len(value) != 0 && (bytes.Contains(body, value) || strings.Contains(location, string(value))) {
			t.Fatal("production response exposed a synthetic secret value")
		}
	}
	if bytes.Contains(body, []byte("credential_ref")) || bytes.Contains(body, []byte("cred:")) ||
		strings.Contains(location, "credential_ref") || strings.Contains(location, "cred:") {
		t.Fatal("production response exposed a credential reference")
	}
}

func cleanupProcessReadConfig(t *testing.T, path string) config {
	t.Helper()
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal("could not read isolated settings snapshot")
	}
	return cfg
}

func cleanupProcessAssertStoreValue(t *testing.T, store secretStore, ref string, want []byte) {
	t.Helper()
	got, err := store.Load(ref)
	if err != nil {
		t.Fatal("synthetic store did not return an expected active value")
	}
	defer clearBytes(got)
	if !bytes.Equal(got, want) {
		t.Fatal("synthetic store value did not match the expected state")
	}
}

func cleanupProcessRandomHex(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	defer clearBytes(buffer)
	return hex.EncodeToString(buffer), nil
}

func cleanupProcessValue(t *testing.T) []byte {
	t.Helper()
	suffix, err := cleanupProcessRandomHex(32)
	if err != nil {
		t.Fatal("could not generate a synthetic value")
	}
	return []byte("synthetic-" + suffix)
}

func (s *cleanupProcessSecretStore) initialize() error {
	return s.write(cleanupProcessStoreState{Version: 1, Entries: map[string][]byte{}})
}

func (s *cleanupProcessSecretStore) Save(ref string, value []byte) error {
	if s == nil || s.path == "" || !secretRefPattern.MatchString(ref) || len(value) == 0 || len(value) > maxSecretSize {
		return errors.New("synthetic store rejected save")
	}
	state, err := s.read()
	if err != nil {
		return errors.New("synthetic store unavailable")
	}
	defer cleanupProcessClearStoreState(state)
	if prior, ok := state.Entries[ref]; ok {
		clearBytes(prior)
	}
	state.Entries[ref] = append([]byte(nil), value...)
	state.Stats.Saves++
	if err := s.write(state); err != nil {
		return errors.New("synthetic store unavailable")
	}
	return nil
}

func (s *cleanupProcessSecretStore) Load(ref string) ([]byte, error) {
	if s == nil || s.path == "" || !secretRefPattern.MatchString(ref) {
		return nil, errors.New("synthetic store rejected load")
	}
	state, err := s.read()
	if err != nil {
		return nil, errors.New("synthetic store unavailable")
	}
	defer cleanupProcessClearStoreState(state)
	value, ok := state.Entries[ref]
	state.Stats.Loads++
	if err := s.write(state); err != nil {
		return nil, errors.New("synthetic store unavailable")
	}
	if !ok {
		return nil, errors.New("synthetic value unavailable")
	}
	return append([]byte(nil), value...), nil
}

func (s *cleanupProcessSecretStore) Delete(ref string) error {
	if s == nil || s.path == "" || !secretRefPattern.MatchString(ref) {
		return errors.New("synthetic store rejected delete")
	}
	state, err := s.read()
	if err != nil {
		return errors.New("synthetic store unavailable")
	}
	defer cleanupProcessClearStoreState(state)
	if s.denyDeletes {
		state.Stats.DeleteFailures++
		if s.write(state) != nil {
			return errors.New("synthetic store unavailable")
		}
		return errors.New("synthetic delete failure")
	}
	value, ok := state.Entries[ref]
	if !ok {
		state.Stats.DeleteFailures++
		_ = s.write(state)
		return errors.New("synthetic value unavailable")
	}
	clearBytes(value)
	delete(state.Entries, ref)
	state.Stats.DeleteSuccess++
	if s.write(state) != nil {
		return errors.New("synthetic store unavailable")
	}
	return nil
}

func (s *cleanupProcessSecretStore) read() (cleanupProcessStoreState, error) {
	data, err := os.ReadFile(s.path)
	if err != nil || len(data) == 0 || len(data) > cleanupProcessMaxBytes {
		clearBytes(data)
		return cleanupProcessStoreState{}, errors.New("synthetic store unavailable")
	}
	defer clearBytes(data)
	var state cleanupProcessStoreState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Entries == nil || len(state.Entries) > 8 {
		cleanupProcessClearStoreState(state)
		return cleanupProcessStoreState{}, errors.New("synthetic store unavailable")
	}
	for ref, value := range state.Entries {
		if !secretRefPattern.MatchString(ref) || len(value) == 0 || len(value) > maxSecretSize {
			cleanupProcessClearStoreState(state)
			return cleanupProcessStoreState{}, errors.New("synthetic store unavailable")
		}
	}
	return state, nil
}

func (s *cleanupProcessSecretStore) write(state cleanupProcessStoreState) error {
	if len(state.Entries) > 8 {
		return errors.New("synthetic store full")
	}
	state.Version = 1
	data, err := json.Marshal(state)
	if err != nil || len(data) > cleanupProcessMaxBytes {
		clearBytes(data)
		return errors.New("synthetic store exceeded bound")
	}
	defer clearBytes(data)
	return writeSetupDraftQueueFileAtomically(s.path, data)
}

func (s *cleanupProcessSecretStore) snapshot() (cleanupProcessStoreState, error) {
	state, err := s.read()
	if err != nil {
		return cleanupProcessStoreState{}, err
	}
	copyState := cleanupProcessStoreState{Version: state.Version, Stats: state.Stats, Entries: make(map[string][]byte, len(state.Entries))}
	for ref, value := range state.Entries {
		copyState.Entries[ref] = append([]byte(nil), value...)
	}
	cleanupProcessClearStoreState(state)
	return copyState, nil
}

func (s *cleanupProcessSecretStore) stats() (cleanupProcessStoreStats, error) {
	state, err := s.read()
	if err != nil {
		return cleanupProcessStoreStats{}, err
	}
	defer cleanupProcessClearStoreState(state)
	return state.Stats, nil
}

func cleanupProcessClearStoreState(state cleanupProcessStoreState) {
	for _, value := range state.Entries {
		clearBytes(value)
	}
}

var _ secretStore = (*cleanupProcessSecretStore)(nil)
