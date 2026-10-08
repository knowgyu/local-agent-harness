//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const manualNamedSecretNativeGate = "LAH_ENABLE_WINDOWS_NAMED_SECRET_LIFECYCLE_QA"

type manualNamedSecretNativeEnvironment struct{}

func (manualNamedSecretNativeEnvironment) ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error) {
	return []UserEnvironmentEntry{}, nil
}

func (manualNamedSecretNativeEnvironment) SetUserEnvironment(context.Context, UserEnvironmentWrite) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errUserEnvironmentUnavailable
}

func (manualNamedSecretNativeEnvironment) DeleteUserEnvironment(context.Context, string) (UserEnvironmentResult, error) {
	return UserEnvironmentResult{}, errUserEnvironmentUnavailable
}

func TestManualWindowsNamedSecretCredentialManagerLifecycle(t *testing.T) {
	if os.Getenv(manualNamedSecretNativeGate) != "1" {
		t.Skipf("set %s=1 to run the native Windows Credential Manager named-secret lifecycle with synthetic data", manualNamedSecretNativeGate)
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

	store := &manualNamedSecretNativeTrackingStore{base: baseStore, owned: make(map[string]struct{})}
	t.Cleanup(func() { manualNamedSecretNativeCleanup(t, store) })

	dir := t.TempDir()
	if err := manualNamedSecretNativeProtectDirectory(dir); err != nil {
		t.Fatalf("could not protect isolated temporary settings directory: %s", err.Error())
	}
	configPath := filepath.Join(dir, "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not initialize isolated temporary settings")
	}
	if err := manualNamedSecretNativeVerifyPrivateFile(configPath); err != nil {
		t.Fatal("isolated temporary settings are not protected for the current user")
	}
	initialConfigBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("could not read isolated temporary settings")
	}

	controller := newNamedSecretController(configPath, store)
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

	cancelValue := manualNamedSecretNativeValue(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := controller.SaveNamedSecret(cancelled, NamedSecretWrite{
		Name: "LAH-QA-20261005-Cancelled", Purpose: "synthetic native lifecycle", Value: cancelValue,
	}); !errors.Is(err, errNamedSecretInvalid) {
		clearBytes(cancelValue)
		t.Fatal("cancelled write was not rejected before credential storage")
	}
	clearBytes(cancelValue)
	afterCancelledConfigBytes, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(initialConfigBytes, afterCancelledConfigBytes) || refIndex != 0 {
		t.Fatal("cancelled write changed temporary settings or consumed a credential reference")
	}
	clearBytes(initialConfigBytes)
	clearBytes(afterCancelledConfigBytes)

	app := &app{
		configPath:      configPath,
		secrets:         store,
		namedSecrets:    controller,
		userEnvironment: manualNamedSecretNativeEnvironment{},
		csrf:            "synthetic-named-secret-native-csrf",
	}
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	app.host = server.Listener.Addr().String()
	server.Config.Handler = newLocalUIHandler(app)
	server.Start()
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if !strings.HasPrefix(server.URL, "http://127.0.0.1:") {
		t.Fatal("test UI did not bind to IPv4 loopback")
	}

	logicalName := "LAH-QA-20261005-" + manualNamedSecretNativeSuffix(t)
	createResponse, createBody := manualNamedSecretNativePost(t, client, server.URL, app.host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {app.csrf},
		"name":    {logicalName},
		"purpose": {"synthetic native lifecycle"},
		"value":   {string(valueOne)},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(createBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if createResponse.StatusCode != http.StatusOK {
		t.Fatal("production named-secret create handler returned an unexpected status")
	}
	var createResult namedSecretUISuccess
	if json.Unmarshal(createBody, &createResult) != nil || !createResult.OK || !createResult.Secret.Configured ||
		createResult.Secret.Name != logicalName || createResult.Secret.InUse || !namedSecretIDPattern.MatchString(createResult.Secret.ID) {
		t.Fatal("production named-secret create handler did not return safe metadata")
	}
	secretID := createResult.Secret.ID
	clearBytes(createBody)
	if refIndex != 1 {
		t.Fatal("create did not use exactly one preflighted synthetic reference")
	}

	createdConfig := manualNamedSecretNativeReadConfig(t, configPath)
	if len(createdConfig.NamedSecrets) != 1 || createdConfig.NamedSecrets[0].ID != secretID ||
		createdConfig.NamedSecrets[0].CredentialRef != refOne || len(createdConfig.GitHubTargets) != 0 {
		t.Fatal("create did not persist only the expected synthetic named-secret metadata")
	}
	manualNamedSecretNativeAssertNoConfigValue(t, configPath, valueOne, valueTwo)
	manualNamedSecretNativeAssertCredential(t, store, refOne, valueOne)
	listed, err := controller.ListNamedSecrets(context.Background())
	if err != nil || len(listed) != 1 || listed[0].ID != secretID || !listed[0].Configured || listed[0].InUse {
		t.Fatal("native controller list did not return the safe synthetic metadata")
	}

	targetName := "LAH-QA synthetic GitHub target"
	targetSaveResponse, targetSaveBody := manualNamedSecretNativePost(t, client, server.URL, app.host, "/save", url.Values{
		"csrf":            {app.csrf},
		"target_id":       {"new"},
		"name":            {targetName},
		"origin":          {"https://github.example.invalid"},
		"repository":      {"synthetic/repository"},
		"token":           {""},
		"named_secret_id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(targetSaveBody), targetSaveResponse.Header.Get("Location")}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if targetSaveResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target save handler did not accept the selected named secret")
	}
	clearBytes(targetSaveBody)

	linkedConfig := manualNamedSecretNativeReadConfig(t, configPath)
	if len(linkedConfig.GitHubTargets) != 1 || len(linkedConfig.NamedSecrets) != 1 ||
		linkedConfig.GitHubTargets[0].SecretRef != refOne || linkedConfig.NamedSecrets[0].CredentialRef != refOne {
		t.Fatal("selected named secret was not linked to the synthetic service target")
	}
	targetID := linkedConfig.GitHubTargets[0].ID
	if !namedSecretViews(linkedConfig)[0].InUse {
		t.Fatal("selected named secret was not marked in use")
	}
	manualNamedSecretNativeAssertNoConfigValue(t, configPath, valueOne, valueTwo)
	manualNamedSecretNativeAssertTargetLoads(t, app, targetName, valueOne)

	pageResponse, pageBody := manualNamedSecretNativeGet(t, client, server.URL+"/?github_id="+url.QueryEscape(targetID))
	pageText := string(pageBody)
	manualNamedSecretNativeAssertSafe(t, []string{pageText}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if pageResponse.StatusCode != http.StatusOK || !strings.Contains(pageText, `data-selected-secret-id="`+secretID+`"`) ||
		!strings.Contains(pageText, logicalName) || !strings.Contains(pageText, `data-available="true"`) {
		t.Fatal("production settings page did not expose the safe selected-secret metadata for the picker")
	}
	clearBytes(pageBody)

	inUseResponse, inUseBody := manualNamedSecretNativePost(t, client, server.URL, app.host, uiRouteDeleteNamedSecret, url.Values{
		"csrf": {app.csrf}, "id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(inUseBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if inUseResponse.StatusCode != http.StatusConflict || !strings.Contains(string(inUseBody), `"error":"in_use"`) {
		t.Fatal("production delete handler did not refuse deletion while the target used the named secret")
	}
	clearBytes(inUseBody)
	manualNamedSecretNativeAssertCredential(t, store, refOne, valueOne)

	rotateResponse, rotateBody := manualNamedSecretNativePost(t, client, server.URL, app.host, uiRouteSaveNamedSecret, url.Values{
		"csrf":    {app.csrf},
		"id":      {secretID},
		"name":    {logicalName},
		"purpose": {"synthetic native lifecycle rotated"},
		"value":   {string(valueTwo)},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(rotateBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if rotateResponse.StatusCode != http.StatusOK {
		t.Fatal("production named-secret rotation handler returned an unexpected status")
	}
	var rotateResult namedSecretUISuccess
	if json.Unmarshal(rotateBody, &rotateResult) != nil || !rotateResult.OK || rotateResult.Secret.ID != secretID ||
		!rotateResult.Secret.Configured || !rotateResult.Secret.InUse || rotateResult.Warning != "" {
		t.Fatal("production rotation handler did not return safe active metadata")
	}
	clearBytes(rotateBody)
	if refIndex != 2 {
		t.Fatal("rotation did not use exactly the second preflighted synthetic reference")
	}

	rotatedConfig := manualNamedSecretNativeReadConfig(t, configPath)
	if len(rotatedConfig.GitHubTargets) != 1 || len(rotatedConfig.NamedSecrets) != 1 ||
		rotatedConfig.GitHubTargets[0].SecretRef != refTwo || rotatedConfig.NamedSecrets[0].CredentialRef != refTwo ||
		rotatedConfig.GitHubTargets[0].ID != targetID {
		t.Fatal("rotation did not move the existing selected target to the replacement reference")
	}
	manualNamedSecretNativeAssertCredential(t, store, refTwo, valueTwo)
	manualNamedSecretNativeAssertMissing(t, store, refOne)
	manualNamedSecretNativeAssertTargetLoads(t, app, targetName, valueTwo)
	manualNamedSecretNativeAssertNoConfigValue(t, configPath, valueOne, valueTwo)

	deleteTargetResponse, deleteTargetBody := manualNamedSecretNativePost(t, client, server.URL, app.host, "/delete-target", url.Values{
		"csrf":           {app.csrf},
		"kind":           {"github"},
		"target_id":      {targetID},
		"confirm_delete": {"yes"},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(deleteTargetBody), deleteTargetResponse.Header.Get("Location")}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if deleteTargetResponse.StatusCode != http.StatusSeeOther {
		t.Fatal("production target delete handler returned an unexpected status")
	}
	clearBytes(deleteTargetBody)
	unusedConfig := manualNamedSecretNativeReadConfig(t, configPath)
	if len(unusedConfig.GitHubTargets) != 0 || len(unusedConfig.NamedSecrets) != 1 || unusedConfig.NamedSecrets[0].CredentialRef != refTwo {
		t.Fatal("target deletion removed the still-saved named secret")
	}
	manualNamedSecretNativeAssertCredential(t, store, refTwo, valueTwo)
	listed, err = controller.ListNamedSecrets(context.Background())
	if err != nil || len(listed) != 1 || listed[0].InUse {
		t.Fatal("target deletion did not release the named-secret in-use state")
	}

	deleteSecretResponse, deleteSecretBody := manualNamedSecretNativePost(t, client, server.URL, app.host, uiRouteDeleteNamedSecret, url.Values{
		"csrf": {app.csrf}, "id": {secretID},
	})
	manualNamedSecretNativeAssertSafe(t, []string{string(deleteSecretBody)}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	if deleteSecretResponse.StatusCode != http.StatusOK || !strings.Contains(string(deleteSecretBody), `"ok":true`) {
		t.Fatal("production named-secret delete handler did not complete")
	}
	clearBytes(deleteSecretBody)
	remaining, err := controller.ListNamedSecrets(context.Background())
	if err != nil || len(remaining) != 0 {
		t.Fatal("deleted named-secret metadata remained in temporary settings")
	}
	manualNamedSecretNativeAssertMissing(t, store, refTwo)
	if err := manualNamedSecretNativeVerifyPrivateFile(configPath); err != nil {
		t.Fatal("temporary settings lost their current-user-only access control")
	}
	if err := manualNamedSecretNativeVerifyPrivateDirectory(dir); err != nil {
		t.Fatal("temporary settings directory lost its protected current-user-only access control")
	}
	if len(store.owned) != 0 {
		t.Fatal("native lifecycle left a run-owned Credential Manager entry for cleanup")
	}
	app.statusMu.RLock()
	status := app.status
	app.statusMu.RUnlock()
	manualNamedSecretNativeAssertSafe(t, []string{status}, byteSet{valueOne, valueTwo}, []string{refOne, refTwo})
	manualNamedSecretNativeAssertNoConfigValue(t, configPath, valueOne, valueTwo)
}

type byteSet [][]byte

func manualNamedSecretNativeAssertSafe(t *testing.T, outputs []string, values byteSet, refs []string) {
	t.Helper()
	for _, output := range outputs {
		for _, value := range values {
			if len(value) > 0 && strings.Contains(output, string(value)) {
				t.Fatal("synthetic credential value appeared in a UI response")
			}
		}
		for _, ref := range refs {
			if ref != "" && strings.Contains(output, ref) {
				t.Fatal("opaque credential reference appeared in a UI response")
			}
		}
	}
}

func manualNamedSecretNativeFreshReference(t *testing.T, store secretStore) string {
	t.Helper()
	for attempt := 0; attempt < 8; attempt++ {
		ref, err := newCredentialReference()
		if err != nil || !secretRefPattern.MatchString(ref) {
			t.Fatal("could not generate a synthetic credential reference")
		}
		value, loadErr := store.Load(ref)
		if errors.Is(loadErr, windows.ERROR_NOT_FOUND) {
			clearBytes(value)
			return ref
		}
		if loadErr == nil {
			clearBytes(value)
			continue
		}
		clearBytes(value)
		t.Fatal("could not safely confirm a fresh synthetic Credential Manager reference")
	}
	t.Fatal("could not generate an unused synthetic Credential Manager reference")
	return ""
}

func manualNamedSecretNativeValue(t *testing.T) []byte {
	t.Helper()
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		clearBytes(random)
		t.Fatal("could not generate synthetic credential data")
	}
	value := []byte("LAH native QA synthetic credential " + hex.EncodeToString(random))
	clearBytes(random)
	return value
}

func manualNamedSecretNativeSuffix(t *testing.T) string {
	t.Helper()
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		clearBytes(value)
		t.Fatal("could not generate a synthetic logical name")
	}
	suffix := ""
	for _, part := range value {
		suffix += string("0123456789abcdef"[part&15])
	}
	clearBytes(value)
	return suffix
}

func manualNamedSecretNativeAssertCredential(t *testing.T, store secretStore, ref string, expected []byte) {
	t.Helper()
	loaded, err := store.Load(ref)
	if err != nil {
		clearBytes(loaded)
		t.Fatal("native Credential Manager read failed for the synthetic entry")
	}
	matched := bytes.Equal(loaded, expected)
	clearBytes(loaded)
	if !matched {
		t.Fatal("native Credential Manager returned a different synthetic value")
	}
}

func manualNamedSecretNativeAssertMissing(t *testing.T, store secretStore, ref string) {
	t.Helper()
	loaded, err := store.Load(ref)
	clearBytes(loaded)
	if !errors.Is(err, windows.ERROR_NOT_FOUND) {
		t.Fatal("synthetic Credential Manager entry was not confirmed absent")
	}
}

type manualNamedSecretNativeTrackingStore struct {
	base  systemSecretStore
	owned map[string]struct{}
}

func (s *manualNamedSecretNativeTrackingStore) Save(ref string, value []byte) error {
	if err := s.base.Save(ref, value); err != nil {
		return err
	}
	s.owned[ref] = struct{}{}
	return nil
}

func (s *manualNamedSecretNativeTrackingStore) Load(ref string) ([]byte, error) {
	return s.base.Load(ref)
}

func (s *manualNamedSecretNativeTrackingStore) Delete(ref string) error {
	if err := s.base.Delete(ref); err != nil {
		return err
	}
	delete(s.owned, ref)
	return nil
}

var _ secretStore = (*manualNamedSecretNativeTrackingStore)(nil)

func manualNamedSecretNativeCleanup(t *testing.T, store *manualNamedSecretNativeTrackingStore) {
	t.Helper()
	for ref := range store.owned {
		cleared := false
		for attempt := 0; attempt < 4; attempt++ {
			loaded, err := store.base.Load(ref)
			if errors.Is(err, windows.ERROR_NOT_FOUND) {
				clearBytes(loaded)
				cleared = true
				break
			}
			if err == nil {
				clearBytes(loaded)
				err = store.Delete(ref)
			}
			clearBytes(loaded)
			if err == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			time.Sleep(150 * time.Millisecond)
		}
		if !cleared {
			loaded, err := store.base.Load(ref)
			clearBytes(loaded)
			cleared = errors.Is(err, windows.ERROR_NOT_FOUND)
		}
		if !cleared {
			t.Error("bounded cleanup could not confirm removal of a synthetic Credential Manager entry")
		}
	}
}

func manualNamedSecretNativeReadConfig(t *testing.T, path string) config {
	t.Helper()
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal("could not read isolated temporary settings")
	}
	return cfg
}

func manualNamedSecretNativeAssertNoConfigValue(t *testing.T, path string, values ...[]byte) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not inspect isolated temporary settings")
	}
	defer clearBytes(encoded)
	for _, value := range values {
		if len(value) > 0 && bytes.Contains(encoded, value) {
			t.Fatal("synthetic credential value appeared in temporary settings")
		}
	}
}

func manualNamedSecretNativeGet(t *testing.T, client *http.Client, address string) (*http.Response, []byte) {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatal("local synthetic UI request failed")
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		clearBytes(body)
		t.Fatal("local synthetic UI response could not be read")
	}
	return response, body
}

func manualNamedSecretNativePost(t *testing.T, client *http.Client, baseURL, host, path string, values url.Values) (*http.Response, []byte) {
	t.Helper()
	body := []byte(values.Encode())
	for key := range values {
		values.Set(key, "")
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		clearBytes(body)
		t.Fatal("could not create local synthetic UI request")
	}
	request.Host = host
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://"+host)
	response, err := client.Do(request)
	clearBytes(body)
	if err != nil {
		t.Fatal("local synthetic UI request failed")
	}
	responseBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		clearBytes(responseBody)
		t.Fatal("local synthetic UI response could not be read")
	}
	return response, responseBody
}

func manualNamedSecretNativeAssertTargetLoads(t *testing.T, app *app, targetName string, expected []byte) {
	t.Helper()
	_, loaded, err := app.loadGitHubTarget(targetName)
	if err != nil {
		clearBytes(loaded)
		t.Fatal("production GitHub target loader could not read the selected synthetic secret")
	}
	matched := bytes.Equal(loaded, expected)
	clearBytes(loaded)
	if !matched {
		t.Fatal("production GitHub target loader returned a different synthetic value")
	}
}

func manualNamedSecretNativeProtectDirectory(path string) error {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return errors.New("could not identify the current user")
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(tokenUser.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return errors.New("could not create a private temporary-directory ACL")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return errors.New("could not apply a private temporary-directory ACL")
	}
	if err := manualNamedSecretNativeVerifyPrivateDirectory(path); err != nil {
		return errors.New("private directory verification failed: " + err.Error())
	}
	return nil
}

func manualNamedSecretNativeVerifyPrivateDirectory(path string) error {
	return manualNamedSecretNativeVerifyPrivateACL(path, true)
}

func manualNamedSecretNativeVerifyPrivateFile(path string) error {
	return manualNamedSecretNativeVerifyPrivateACL(path, false)
}

func manualNamedSecretNativeVerifyPrivateACL(path string, requireProtected bool) error {
	var descriptor *windows.SECURITY_DESCRIPTOR
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return errors.New("security descriptor")
	}
	control, _, err := descriptor.Control()
	if err != nil || (requireProtected && control&windows.SE_DACL_PROTECTED == 0) {
		return errors.New("DACL protection flag")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return errors.New("DACL ACE count")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return errors.New("current-user SID")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("current-user owner")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("DACL includes a non-allow ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) {
			return errors.New("DACL includes another trustee")
		}
	}
	return nil
}
