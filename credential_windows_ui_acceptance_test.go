//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const windowsCredentialManagerUISaveGate = "LAH_ENABLE_WINDOWS_CREDENTIAL_MANAGER_UI_SAVE_TEST"

func TestWindowsCredentialManagerProductionUISaveRotation(t *testing.T) {
	if os.Getenv(windowsCredentialManagerUISaveGate) != "1" {
		t.Skipf("set %s=1 to exercise production UI credential rotation with synthetic values", windowsCredentialManagerUISaveGate)
	}

	store := &windowsCredentialManagerTrackingStore{
		base: systemSecretStore{},
		refs: make(map[string]struct{}),
	}
	t.Cleanup(func() {
		for ref := range store.refs {
			if err := store.base.Delete(ref); err != nil {
				t.Error("cleanup synthetic Credential Manager entry")
			}
		}
	})

	oldRef, oldToken := windowsCredentialManagerSyntheticPair(t, "old")
	newToken := windowsCredentialManagerSyntheticValue(t, "replacement")
	defer clear(oldToken)
	defer clear(newToken)

	if existing, err := store.Load(oldRef); err == nil {
		clear(existing)
		t.Fatal("fresh synthetic credential reference was already present")
	}
	if err := store.Save(oldRef, oldToken); err != nil {
		t.Fatal("store the synthetic prior value in Windows Credential Manager")
	}

	oldTarget, siblingTarget, _, _, _ := serviceBundleTargets()
	oldTarget.SecretRef = oldRef
	cfg := serviceBundleConfig()
	cfg.GitHubTargets[0] = oldTarget
	cfg.ConnectionTests = map[string]connectionTest{
		oldTarget.ID:     {Result: "failure", CompletedAt: "2026-09-26T01:02:03Z"},
		siblingTarget.ID: {Result: "success", CompletedAt: "2026-09-26T01:03:04Z"},
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	a := &app{configPath: configPath, secrets: store, csrf: "synthetic-windows-ui-save-csrf"}
	uiServer := httptest.NewUnstartedServer(http.NotFoundHandler())
	a.host = uiServer.Listener.Addr().String()
	uiServer.Config.Handler = newLocalUIHandler(a)
	uiServer.Start()
	t.Cleanup(uiServer.Close)
	if !strings.HasPrefix(uiServer.URL, "http://127.0.0.1:") {
		t.Fatalf("synthetic UI server did not bind loopback HTTP: %q", uiServer.URL)
	}
	uiClient := uiServer.Client()
	uiClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	initialResponse, err := uiClient.Get(uiServer.URL + "/?github_id=" + url.QueryEscape(oldTarget.ID))
	if err != nil {
		t.Fatal(err)
	}
	initialBody, err := io.ReadAll(initialResponse.Body)
	initialResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	initialHTML := string(initialBody)
	csrfMatch := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(initialBody)
	if initialResponse.StatusCode != http.StatusOK || len(csrfMatch) != 2 ||
		!strings.Contains(initialHTML, `value="`+oldTarget.ID+`" selected`) ||
		!strings.Contains(initialHTML, `value="`+oldTarget.Origin+`"`) ||
		!strings.Contains(initialHTML, `value="`+oldTarget.Repository+`"`) {
		t.Fatalf("initial UI did not render the registered synthetic target: status=%d", initialResponse.StatusCode)
	}
	initialTokenField := regexp.MustCompile(`<input\b[^>]*\bname="token"[^>]*>`).FindString(initialHTML)
	if initialTokenField == "" || regexp.MustCompile(`\svalue=`).MatchString(initialTokenField) {
		t.Fatalf("initial UI must keep its saved token field blank: %q", initialTokenField)
	}
	if strings.Contains(initialHTML, string(oldToken)) || strings.Contains(initialHTML, oldRef) || strings.Contains(initialHTML, string(newToken)) {
		t.Fatal("initial UI exposed synthetic credential material")
	}

	form := url.Values{
		"csrf":       {string(csrfMatch[1])},
		"target_id":  {oldTarget.ID},
		"name":       {oldTarget.Name},
		"origin":     {oldTarget.Origin},
		"repository": {oldTarget.Repository},
		"token":      {string(newToken)},
	}
	saveRequest, err := http.NewRequest(http.MethodPost, uiServer.URL+"/save", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	saveRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveRequest.Header.Set("Origin", uiServer.URL)
	saveResponse, err := uiClient.Do(saveRequest)
	if err != nil {
		t.Fatal(err)
	}
	saveBody, err := io.ReadAll(saveResponse.Body)
	saveResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if saveResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("production /save rejected the synthetic same-origin rotation: status=%d", saveResponse.StatusCode)
	}
	redirect, err := url.Parse(saveResponse.Header.Get("Location"))
	if err != nil || redirect.Path != "/" || redirect.Query().Get("github_id") != oldTarget.ID {
		t.Fatalf("credential rotation did not redirect to the same target: location=%q err=%v", saveResponse.Header.Get("Location"), err)
	}
	if strings.Contains(string(saveBody), string(oldToken)) || strings.Contains(string(saveBody), string(newToken)) || strings.Contains(saveResponse.Header.Get("Location"), oldRef) {
		t.Fatal("save response exposed synthetic credential material")
	}

	settingsBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	savedConfig, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	index := findGitHubTargetIndex(savedConfig.GitHubTargets, oldTarget.ID)
	if index < 0 {
		t.Fatal("rotation replaced or removed the registered target")
	}
	savedTarget := savedConfig.GitHubTargets[index]
	newRef := savedTarget.SecretRef
	wantTarget := oldTarget
	wantTarget.SecretRef = newRef
	if savedTarget != wantTarget || !secretRefPattern.MatchString(newRef) || newRef == oldRef {
		t.Fatalf("saved target=%+v, want existing metadata with a fresh opaque reference", savedTarget)
	}
	if !reflect.DeepEqual(savedConfig.ServiceBundles, cfg.ServiceBundles) {
		t.Fatal("credential rotation changed the service-bundle mapping")
	}
	if strings.Contains(string(settingsBytes), string(oldToken)) || strings.Contains(string(settingsBytes), string(newToken)) ||
		strings.Contains(string(settingsBytes), oldRef) || !strings.Contains(string(settingsBytes), newRef) {
		t.Fatal("settings must contain only the replacement reference, never either credential value or the old reference")
	}
	if _, exists := savedConfig.ConnectionTests[oldTarget.ID]; exists ||
		savedConfig.ConnectionTests[siblingTarget.ID] != cfg.ConnectionTests[siblingTarget.ID] || len(savedConfig.ConnectionTests) != 1 {
		t.Fatal("rotation must clear only the selected target's historical connection result")
	}

	loaded, err := store.Load(newRef)
	if err != nil {
		t.Fatal("production /save replacement was not readable from Windows Credential Manager")
	}
	matched := bytes.Equal(loaded, newToken)
	clear(loaded)
	if !matched {
		t.Fatal("production /save stored a different replacement value")
	}
	deletedOldValue, err := store.Load(oldRef)
	clear(deletedOldValue)
	if err == nil {
		t.Fatal("production /save did not delete the replaced Credential Manager entry")
	}

	registeredResponse, err := uiClient.Get(uiServer.URL + redirect.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	registeredBody, err := io.ReadAll(registeredResponse.Body)
	registeredResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	registeredHTML := string(registeredBody)
	registeredTokenField := regexp.MustCompile(`<input\b[^>]*\bname="token"[^>]*>`).FindString(registeredHTML)
	if registeredResponse.StatusCode != http.StatusOK ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.ID+`" selected`) ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.Origin+`"`) ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.Repository+`"`) ||
		!strings.Contains(registeredHTML, "GitHub target saved. Test the connection before using MCP.") ||
		registeredTokenField == "" || regexp.MustCompile(`\svalue=`).MatchString(registeredTokenField) {
		t.Fatalf("saved UI did not retain the selected target with a blank token field: status=%d", registeredResponse.StatusCode)
	}
	for _, output := range []string{string(initialBody), string(saveBody), saveResponse.Header.Get("Location"), registeredHTML} {
		for _, forbidden := range []string{string(oldToken), string(newToken), oldRef, newRef} {
			if strings.Contains(output, forbidden) {
				t.Fatal("UI response exposed synthetic credential material")
			}
		}
	}
}

func windowsCredentialManagerSyntheticPair(t *testing.T, label string) (string, []byte) {
	t.Helper()
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal("generate a synthetic credential reference")
	}
	ref := "cred:" + hex.EncodeToString(bytes)
	clear(bytes)
	return ref, windowsCredentialManagerSyntheticValue(t, label)
}

func windowsCredentialManagerSyntheticValue(t *testing.T, label string) []byte {
	t.Helper()
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal("generate a synthetic credential value")
	}
	value := []byte("synthetic-canary-" + label + "-" + hex.EncodeToString(bytes))
	clear(bytes)
	return value
}

type windowsCredentialManagerTrackingStore struct {
	base systemSecretStore
	refs map[string]struct{}
}

func (s *windowsCredentialManagerTrackingStore) Save(ref string, value []byte) error {
	if err := s.base.Save(ref, value); err != nil {
		return err
	}
	s.refs[ref] = struct{}{}
	return nil
}

func (s *windowsCredentialManagerTrackingStore) Load(ref string) ([]byte, error) {
	return s.base.Load(ref)
}

func (s *windowsCredentialManagerTrackingStore) Delete(ref string) error {
	if err := s.base.Delete(ref); err != nil {
		return err
	}
	delete(s.refs, ref)
	return nil
}

var _ secretStore = (*windowsCredentialManagerTrackingStore)(nil)
