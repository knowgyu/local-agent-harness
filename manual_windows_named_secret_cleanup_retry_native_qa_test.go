//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"golang.org/x/sys/windows"
)

func TestManualWindowsNamedSecretCleanupRetrySurvivesControllerReopen(t *testing.T) {
	if os.Getenv(manualNamedSecretCleanupRetryHelper) == "1" {
		manualNamedSecretCleanupRetryServeChild(t)
		return
	}
	if os.Getenv(manualNamedSecretNativeGate) != "1" {
		t.Skipf("set %s=1 to run the native Windows Credential Manager cleanup-retry lifecycle with synthetic data", manualNamedSecretNativeGate)
	}

	baseStore := systemSecretStore{}
	refOne := manualNamedSecretNativeFreshReference(t, baseStore)
	refTwo := manualNamedSecretNativeFreshReference(t, baseStore)
	if refOne == refTwo {
		t.Fatal("could not reserve two distinct synthetic references")
	}
	valueOne := manualNamedSecretNativeValue(t)
	valueTwo := manualNamedSecretNativeValue(t)
	defer clearBytes(valueOne)
	defer clearBytes(valueTwo)

	trackingStore := &manualNamedSecretNativeTrackingStore{base: baseStore, owned: make(map[string]struct{})}
	store := &manualNamedSecretNativeFailOnceDeleteStore{base: trackingStore, deleteCalls: make(map[string]int)}
	t.Cleanup(func() { manualNamedSecretNativeCleanup(t, trackingStore) })

	dir := t.TempDir()
	if err := manualNamedSecretNativeProtectDirectory(dir); err != nil {
		t.Fatal("could not protect isolated temporary settings directory")
	}
	configPath := filepath.Join(dir, "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not initialize isolated temporary settings")
	}

	queueDir := filepath.Join(dir, "LocalAgentHarness")
	queuePath := filepath.Join(queueDir, namedSecretCleanupQueueFileName)
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("could not initialize isolated cleanup queue")
	}

	controller := newNamedSecretController(configPath, store)
	controller.cleanup = queue
	refs := []string{refOne, refTwo}
	refIndex := 0
	controller.newRef = func() (string, error) {
		if refIndex >= len(refs) {
			return "", errNamedSecretInvalid
		}
		ref := refs[refIndex]
		refIndex++
		return ref, nil
	}

	appOne := &app{
		configPath:      configPath,
		secrets:         store,
		namedSecrets:    controller,
		userEnvironment: manualNamedSecretNativeEnvironment{},
		csrf:            "synthetic-named-secret-cleanup-csrf",
	}
	serverOne, clientOne := manualNamedSecretNativeStartUI(t, appOne)

	logicalName := "LAH-QA-20261005-" + manualNamedSecretNativeSuffix(t)
	createResponse, createBody := manualNamedSecretNativePost(t, clientOne, serverOne.URL, appOne.host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {appOne.csrf},
		"name":    {logicalName},
		"purpose": {"synthetic cleanup retry lifecycle"},
		"value":   {string(valueOne)},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(createBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if createResponse.StatusCode != http.StatusOK {
		status := createResponse.StatusCode
		var safeError secretsEnvironmentUIError
		_ = json.Unmarshal(createBody, &safeError)
		clearBytes(createBody)
		t.Fatalf("production named-secret create handler returned an unexpected status: %d (%s)", status, safeError.Error)
	}
	var createResult namedSecretUISuccess
	if json.Unmarshal(createBody, &createResult) != nil || !createResult.OK || !createResult.Secret.Configured {
		t.Fatal("production named-secret create handler did not return safe metadata")
	}
	secretID := createResult.Secret.ID
	clearBytes(createBody)
	if refIndex != 1 {
		t.Fatal("create did not use exactly one preflighted synthetic reference")
	}

	targetName := "LAH-QA synthetic cleanup target"
	targetResponse, targetBody := manualNamedSecretNativePost(t, clientOne, serverOne.URL, appOne.host, "/save", url.Values{
		"csrf":            {appOne.csrf},
		"target_id":       {"new"},
		"name":            {targetName},
		"origin":          {"https://github.example.invalid"},
		"repository":      {"synthetic/cleanup"},
		"token":           {""},
		"named_secret_id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(targetBody), targetResponse.Header.Get("Location")}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if targetResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target handler did not accept the selected synthetic named secret")
	}
	clearBytes(targetBody)
	targetConfig := manualNamedSecretNativeReadConfig(t, configPath)
	if len(targetConfig.GitHubTargets) != 1 || len(targetConfig.NamedSecrets) != 1 ||
		targetConfig.GitHubTargets[0].SecretRef != refOne || targetConfig.NamedSecrets[0].CredentialRef != refOne {
		t.Fatal("synthetic target did not reference the selected named secret")
	}
	targetID := targetConfig.GitHubTargets[0].ID
	manualNamedSecretNativeAssertTargetLoads(t, appOne, targetName, valueOne)

	store.failNextDelete = true
	rotateResponse, rotateBody := manualNamedSecretNativePost(t, clientOne, serverOne.URL, appOne.host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {appOne.csrf},
		"id":      {secretID},
		"name":    {logicalName},
		"purpose": {"synthetic cleanup retry rotation"},
		"value":   {string(valueTwo)},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(rotateBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if rotateResponse.StatusCode != http.StatusOK {
		t.Fatal("production named-secret rotation did not complete safely after the injected first-delete failure")
	}
	var rotateResult namedSecretUISuccess
	if json.Unmarshal(rotateBody, &rotateResult) != nil || !rotateResult.OK || rotateResult.Secret.ID != secretID ||
		!rotateResult.Secret.InUse || rotateResult.Warning != "cleanup_pending" {
		t.Fatal("production rotation did not report pending cleanup without exposing credential data")
	}
	clearBytes(rotateBody)
	if refIndex != 2 || store.failNextDelete {
		t.Fatal("rotation did not consume the replacement reference and one-shot delete failure")
	}
	manualNamedSecretNativeAssertCredential(t, baseStore, refOne, valueOne)
	manualNamedSecretNativeAssertCredential(t, baseStore, refTwo, valueTwo)
	manualNamedSecretNativeAssertNoConfigValue(t, configPath, valueOne, valueTwo)

	pendingIDs, err := controller.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(pendingIDs) != 1 || pendingIDs[0] != secretID {
		t.Fatal("durable cleanup status did not list only the safe logical secret ID")
	}
	queueInfo, err := os.Stat(queuePath)
	if err != nil || queueInfo.Size() == 0 {
		t.Fatal("durable cleanup queue was not present after the failed native delete")
	}
	if err := manualNamedSecretNativeVerifyQueuePath(queueDir, true); err != nil {
		t.Fatal("cleanup queue directory lost its protected current-user and system access control")
	}
	if err := manualNamedSecretNativeVerifyQueuePath(queuePath, false); err != nil {
		t.Fatal("cleanup queue file lost its protected current-user and system access control")
	}

	serverOne.Close()
	reopenedQueue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("could not reopen isolated cleanup queue")
	}
	reopenedController := newNamedSecretController(configPath, store)
	reopenedController.cleanup = reopenedQueue
	reopenedPending, err := reopenedController.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(reopenedPending) != 1 || reopenedPending[0] != secretID {
		t.Fatal("new controller did not recover the durable pending cleanup ID")
	}

	const retryCSRF = "synthetic-named-secret-cleanup-retry-csrf"
	appTwo := &app{
		configPath:      configPath,
		secrets:         store,
		namedSecrets:    reopenedController,
		userEnvironment: manualNamedSecretNativeEnvironment{},
		csrf:            retryCSRF,
	}
	retryProcess := manualNamedSecretNativeStartCleanupRetryProcess(t, configPath, queuePath, retryCSRF)
	retryResponse, retryBody := manualNamedSecretNativePost(t, retryProcess.client, retryProcess.baseURL, retryProcess.host, uiRouteRetryNamedSecretCleanup, url.Values{
		"csrf": {retryCSRF}, "id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(retryBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if retryResponse.StatusCode != http.StatusOK {
		t.Fatal("production cleanup retry route failed to complete the pending operation")
	}
	var retryResult namedSecretCleanupUISuccess
	if json.Unmarshal(retryBody, &retryResult) != nil || !retryResult.OK || retryResult.CleanupPending {
		t.Fatal("production cleanup retry route did not return a safe completed status")
	}
	clearBytes(retryBody)
	manualNamedSecretNativeAssertMissing(t, baseStore, refOne)
	delete(trackingStore.owned, refOne)
	manualNamedSecretNativeAssertCredential(t, baseStore, refTwo, valueTwo)
	if store.deleteCalls[refOne] != 1 || store.deleteCalls[refTwo] != 0 {
		t.Fatal("retry unexpectedly called the first process delete wrapper again")
	}

	pendingIDs, err = reopenedController.PendingNamedSecretCleanupIDs(context.Background())
	if err != nil || len(pendingIDs) != 0 {
		t.Fatal("successful cleanup retry left a pending logical ID")
	}
	pending, err := reopenedController.RetryNamedSecretCleanup(context.Background(), secretID)
	if err != nil || pending {
		t.Fatal("repeating cleanup for a completed logical ID was not idempotent")
	}
	activeGuardResponse, activeGuardBody := manualNamedSecretNativePost(t, retryProcess.client, retryProcess.baseURL, retryProcess.host, uiRouteRetryNamedSecretCleanup, url.Values{
		"csrf": {retryCSRF}, "id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(activeGuardBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if activeGuardResponse.StatusCode != http.StatusConflict || !stringsContainsJSONError(activeGuardBody, "not_found") {
		t.Fatal("production retry route did not guard an active secret with no pending cleanup")
	}
	clearBytes(activeGuardBody)
	manualNamedSecretNativeAssertCredential(t, baseStore, refTwo, valueTwo)
	manualNamedSecretNativeAssertTargetLoads(t, appTwo, targetName, valueTwo)
	if store.deleteCalls[refTwo] != 0 {
		t.Fatal("idempotent retry attempted to delete the active replacement reference")
	}

	deleteTargetResponse, deleteTargetBody := manualNamedSecretNativePost(t, retryProcess.client, retryProcess.baseURL, retryProcess.host, "/delete-target", url.Values{
		"csrf": {appTwo.csrf}, "kind": {"github"}, "target_id": {targetID}, "confirm_delete": {"yes"},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(deleteTargetBody), deleteTargetResponse.Header.Get("Location")}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if deleteTargetResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target delete handler returned an unexpected status")
	}
	clearBytes(deleteTargetBody)
	deleteSecretResponse, deleteSecretBody := manualNamedSecretNativePost(t, retryProcess.client, retryProcess.baseURL, retryProcess.host, uiRouteDeleteNamedSecret, url.Values{
		"csrf": {appTwo.csrf}, "id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(deleteSecretBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if deleteSecretResponse.StatusCode != http.StatusOK {
		t.Fatal("production named-secret delete handler failed after cleanup")
	}
	clearBytes(deleteSecretBody)
	manualNamedSecretNativeAssertMissing(t, baseStore, refTwo)
	delete(trackingStore.owned, refTwo)
	if len(trackingStore.owned) != 0 {
		t.Fatal("native retry lifecycle left a run-owned Credential Manager entry for cleanup")
	}
	if err := manualNamedSecretNativeVerifyPrivateFile(configPath); err != nil {
		t.Fatal("temporary settings are no longer current-user-only")
	}
}

const (
	manualNamedSecretCleanupRetryHelper = "LAH_QA_NATIVE_CLEANUP_RETRY_HELPER"
	manualNamedSecretCleanupRetryConfig = "LAH_QA_NATIVE_CLEANUP_RETRY_CONFIG"
	manualNamedSecretCleanupRetryQueue  = "LAH_QA_NATIVE_CLEANUP_RETRY_QUEUE"
	manualNamedSecretCleanupRetryCSRF   = "LAH_QA_NATIVE_CLEANUP_RETRY_CSRF"
)

type manualNamedSecretCleanupRetryProcess struct {
	cmd     *exec.Cmd
	baseURL string
	host    string
	client  *http.Client
}

func manualNamedSecretNativeStartCleanupRetryProcess(t *testing.T, configPath, queuePath, csrf string) *manualNamedSecretCleanupRetryProcess {
	t.Helper()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("could not resolve the isolated test executable")
	}
	cmd := exec.Command(executable, "-test.run=^TestManualWindowsNamedSecretCleanupRetrySurvivesControllerReopen$")
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		manualNamedSecretNativeGate + "=1",
		manualNamedSecretCleanupRetryHelper + "=1",
		manualNamedSecretCleanupRetryConfig + "=" + configPath,
		manualNamedSecretCleanupRetryQueue + "=" + queuePath,
		manualNamedSecretCleanupRetryCSRF + "=" + csrf,
	}
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("could not capture isolated retry-helper readiness")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("could not start isolated retry-helper process")
	}
	process := &manualNamedSecretCleanupRetryProcess{cmd: cmd}
	t.Cleanup(func() { process.stop() })

	ready := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		ready <- struct {
			line string
			err  error
		}{line: line, err: readErr}
	}()
	select {
	case result := <-ready:
		if result.err != nil || !strings.HasPrefix(result.line, "READY 127.0.0.1:") {
			process.stop()
			t.Fatal("isolated retry-helper process did not report loopback readiness")
		}
		process.host = strings.TrimSpace(strings.TrimPrefix(result.line, "READY "))
		process.baseURL = "http://" + process.host
	case <-time.After(10 * time.Second):
		process.stop()
		t.Fatal("isolated retry-helper process did not become ready before timeout")
	}
	process.client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return process
}

func (p *manualNamedSecretCleanupRetryProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd = nil
}

func manualNamedSecretCleanupRetryServeChild(t *testing.T) {
	configPath := os.Getenv(manualNamedSecretCleanupRetryConfig)
	queuePath := os.Getenv(manualNamedSecretCleanupRetryQueue)
	csrf := os.Getenv(manualNamedSecretCleanupRetryCSRF)
	if configPath == "" || queuePath == "" || csrf == "" || os.Getenv("SystemRoot") == "" {
		t.Fatal("isolated retry-helper process received incomplete test-only setup")
	}
	queue, err := newNamedSecretCleanupFileQueueAt(queuePath)
	if err != nil {
		t.Fatal("isolated retry-helper process could not reopen the cleanup queue")
	}
	secrets := systemSecretStore{}
	controller := newNamedSecretController(configPath, secrets)
	controller.cleanup = queue
	a := &app{
		configPath:      configPath,
		secrets:         secrets,
		namedSecrets:    controller,
		userEnvironment: manualNamedSecretNativeEnvironment{},
		csrf:            csrf,
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("isolated retry-helper process could not bind loopback")
	}
	a.host = listener.Addr().String()
	server := &http.Server{Handler: newLocalUIHandler(a), ReadHeaderTimeout: 2 * time.Second}
	if _, err := fmt.Fprintf(os.Stdout, "READY %s\n", a.host); err != nil {
		t.Fatal("isolated retry-helper process could not report readiness")
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("isolated retry-helper server stopped unexpectedly")
	}
	t.Fatal("isolated retry-helper server exited unexpectedly")
}

func (s *manualNamedSecretNativeFailOnceDeleteStore) Save(ref string, value []byte) error {
	return s.base.Save(ref, value)
}

func (s *manualNamedSecretNativeFailOnceDeleteStore) Load(ref string) ([]byte, error) {
	return s.base.Load(ref)
}

func (s *manualNamedSecretNativeFailOnceDeleteStore) Delete(ref string) error {
	s.deleteCalls[ref]++
	if s.failNextDelete {
		s.failNextDelete = false
		return errors.New("synthetic one-shot delete failure")
	}
	return s.base.Delete(ref)
}

func manualNamedSecretNativeVerifyQueuePath(path string, directory bool) error {
	security, _, err := setupDraftWindowsSecurityAttributes()
	if err != nil || security == nil {
		return errors.New("queue security policy unavailable")
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := openSetupDraftWindowsObject(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, flags)
	if err != nil {
		return errors.New("queue path could not be opened for security verification")
	}
	defer windows.CloseHandle(handle)
	if !setupDraftWindowsSecurityMatches(handle, windows.SE_FILE_OBJECT, security.userSID, false) {
		return errors.New("queue path ACL does not match the protected policy")
	}
	info, err := setupDraftWindowsHandleInfo(handle)
	if err != nil || directory != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("queue path type or reparse check failed")
	}
	return nil
}

type manualNamedSecretNativeFailOnceDeleteStore struct {
	base           *manualNamedSecretNativeTrackingStore
	deleteCalls    map[string]int
	failNextDelete bool
}

var _ secretStore = (*manualNamedSecretNativeFailOnceDeleteStore)(nil)

func manualNamedSecretNativeStartUI(t *testing.T, a *app) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(a)
	server.Start()
	t.Cleanup(server.Close)
	if server.URL == "" {
		t.Fatal("synthetic settings server did not start")
	}
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return server, client
}

func stringsContainsJSONError(body []byte, code string) bool {
	var result map[string]string
	return json.Unmarshal(body, &result) == nil && result["error"] == code
}
