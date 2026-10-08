package main

import (
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
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGitHubFirstUseAcceptanceSavesTestsAndReadsRegisteredRepository(t *testing.T) {
	const (
		csrfCanary          = "github-first-use-csrf-canary"
		patCanary           = "github_pat_synthetic_first_use_canary_0123456789ABCDEF"
		targetName          = "Synthetic Enterprise"
		targetOrigin        = "https://enterprise.example.invalid"
		targetRepository    = "ops/agent"
		upstreamURLCanary   = "https://fixture-upstream-url-canary.invalid/private/repository"
		upstreamHeaderValue = "fixture-upstream-header-canary"
		rawBodyCanary       = "fixture-raw-body-canary"
	)

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	secrets := &githubFirstUseAcceptanceSecrets{values: make(map[string][]byte)}
	var requestCount int
	client := &http.Client{Transport: githubFirstUseAcceptanceRoundTripper(func(r *http.Request) (*http.Response, error) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "enterprise.example.invalid" ||
			r.URL.EscapedPath() != "/api/v3/repos/ops/agent" || r.URL.RawQuery != "" || r.URL.Fragment != "" || r.URL.User != nil {
			return nil, fmt.Errorf("unexpected synthetic GitHub request %s %s", r.Method, r.URL.Redacted())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+patCanary {
			return nil, errors.New("synthetic GitHub request did not use the registered PAT")
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "local-agent-harness" {
			return nil, errors.New("synthetic GitHub request headers did not match the adapter contract")
		}
		if r.Body != nil {
			return nil, errors.New("synthetic GitHub GET unexpectedly had a request body")
		}

		body := `{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main","html_url":"` + upstreamURLCanary + `","description":"Synthetic description ` + patCanary + ` safely retained.","unprojected":"` + rawBodyCanary + ` Authorization: Bearer ` + patCanary + `"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Upstream-Canary": []string{upstreamHeaderValue}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	}), Timeout: 3 * time.Second}
	a := &app{configPath: configPath, secrets: secrets, client: client, csrf: csrfCanary}

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
	initialPage, err := uiClient.Get(uiServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	initialBody, err := io.ReadAll(initialPage.Body)
	initialPage.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if initialPage.StatusCode != http.StatusOK {
		t.Fatalf("initial synthetic UI page status=%d", initialPage.StatusCode)
	}
	csrfMatch := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindSubmatch(initialBody)
	if len(csrfMatch) != 2 || string(csrfMatch[1]) != csrfCanary {
		t.Fatal("initial UI page did not render the configured CSRF token")
	}

	form := url.Values{
		"csrf":       {string(csrfMatch[1])},
		"target_id":  {"new"},
		"name":       {targetName},
		"origin":     {targetOrigin},
		"repository": {targetRepository},
		"token":      {patCanary},
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
		t.Fatalf("production /save handler rejected the rendered same-origin CSRF form: status=%d body=%s", saveResponse.StatusCode, saveBody)
	}
	location := saveResponse.Header.Get("Location")
	redirectTargetURL, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	redirectedTargetID := redirectTargetURL.Query().Get("github_id")
	if redirectTargetURL.Path != "/" || !githubIDPattern.MatchString(redirectedTargetID) {
		t.Fatalf("save redirect = %q, want the newly registered GitHub target selection", location)
	}

	configBeforeMCP, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var savedConfig config
	if err := json.Unmarshal(configBeforeMCP, &savedConfig); err != nil {
		t.Fatal(err)
	}
	if len(savedConfig.GitHubTargets) != 1 {
		t.Fatalf("saved GitHub target count=%d, want one", len(savedConfig.GitHubTargets))
	}
	savedTarget := savedConfig.GitHubTargets[0]
	if savedTarget.ID == "" || savedTarget.Name != targetName || savedTarget.Origin != targetOrigin || savedTarget.Repository != targetRepository || !secretRefPattern.MatchString(savedTarget.SecretRef) {
		t.Fatalf("saved GitHub target does not match the submitted registration: %+v", savedTarget)
	}
	if redirectedTargetID != savedTarget.ID {
		t.Fatalf("save redirect selected target %q, want saved target %q", redirectedTargetID, savedTarget.ID)
	}
	if strings.Contains(string(configBeforeMCP), patCanary) || !strings.Contains(string(configBeforeMCP), savedTarget.SecretRef) {
		t.Fatal("saved settings must contain the generated credential reference and must not contain the PAT")
	}
	secrets.mu.Lock()
	savedSecret, secretExists := secrets.values[savedTarget.SecretRef]
	saveCount := len(secrets.saves)
	secrets.mu.Unlock()
	if !secretExists || string(savedSecret) != patCanary || saveCount != 1 {
		t.Fatal("production save handler did not write the PAT once under the generated credential reference")
	}

	registeredPage, err := uiClient.Get(uiServer.URL + redirectTargetURL.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	registeredBody, err := io.ReadAll(registeredPage.Body)
	registeredPage.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	registeredHTML := string(registeredBody)
	selectedOption := `value="` + savedTarget.ID + `" selected`
	if registeredPage.StatusCode != http.StatusOK ||
		!strings.Contains(registeredHTML, "GitHub target saved. Test the connection before using MCP.") ||
		!strings.Contains(registeredHTML, selectedOption) ||
		!strings.Contains(registeredHTML, `value="`+targetOrigin+`"`) ||
		!strings.Contains(registeredHTML, `value="`+targetRepository+`"`) {
		t.Fatalf("saved target did not render the normal success state: status=%d", registeredPage.StatusCode)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "github-first-use-acceptance", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	connectionResponse, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "registered_target_connection_test",
		Arguments: map[string]any{"adapter": "github", "target": targetName},
	})
	if err != nil || connectionResponse == nil || connectionResponse.IsError {
		t.Fatalf("registered GitHub connection test failed: result=%#v err=%v", connectionResponse, err)
	}
	connectionJSON, err := json.Marshal(connectionResponse.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var connectionResult registeredTargetConnectionTestResult
	if err := json.Unmarshal(connectionJSON, &connectionResult); err != nil {
		t.Fatal(err)
	}
	if connectionResult.Adapter != "github" || connectionResult.Target != targetName || connectionResult.Result != "success" || connectionResult.CompletedAt == "" || connectionResult.Message != "Connection test succeeded. Historical status saved." {
		t.Fatalf("registered GitHub connection result=%+v", connectionResult)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, connectionResult.CompletedAt)
	if err != nil || completedAt.Location() != time.UTC {
		t.Fatalf("connection result completion time %q is not valid UTC: %v", connectionResult.CompletedAt, err)
	}

	repositoryResponse, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "github_repository",
		Arguments: map[string]any{"target": targetName},
	})
	if err != nil || repositoryResponse == nil || repositoryResponse.IsError {
		t.Fatalf("registered GitHub repository read failed: result=%#v err=%v", repositoryResponse, err)
	}
	projectedJSON, err := json.Marshal(repositoryResponse.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var projected repository
	if err := json.Unmarshal(projectedJSON, &projected); err != nil {
		t.Fatal(err)
	}
	wantRepository := repository{
		Name:          "agent",
		FullName:      "ops/agent",
		Private:       true,
		DefaultBranch: "main",
		HTMLURL:       targetOrigin + "/" + targetRepository,
		Description:   "Synthetic description [REDACTED] safely retained.",
	}
	if projected != wantRepository {
		t.Fatalf("projected GitHub repository=%+v, want %+v", projected, wantRepository)
	}

	fullMCPResponse, err := json.Marshal(repositoryResponse)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []struct {
		name string
		body string
	}{
		{name: "initial UI page", body: string(initialBody)},
		{name: "save response", body: string(saveResponse.Header.Get("Location")) + string(saveBody)},
		{name: "registered UI page", body: string(registeredBody)},
		{name: "connection MCP result", body: mustMarshalGitHubFirstUse(t, connectionResponse)},
		{name: "repository MCP result", body: string(fullMCPResponse)},
	}
	for _, output := range outputs {
		for _, forbidden := range []string{
			patCanary,
			savedTarget.SecretRef,
			"cred:",
			upstreamURLCanary,
			upstreamHeaderValue,
			rawBodyCanary,
			"Authorization: Bearer",
		} {
			if strings.Contains(output.body, forbidden) {
				t.Errorf("%s exposed private fixture value %q", output.name, forbidden)
			}
		}
	}

	storedConfig, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	history, exists := storedConfig.ConnectionTests[savedTarget.ID]
	if !exists || history.Result != "success" || history.CompletedAt != connectionResult.CompletedAt {
		t.Fatalf("saved connection history=%+v, exists=%v; want the successful MCP connection test", history, exists)
	}
	if _, err := time.Parse(time.RFC3339Nano, history.CompletedAt); err != nil {
		t.Fatalf("saved connection history time %q is invalid: %v", history.CompletedAt, err)
	}
	secrets.mu.Lock()
	loads, writes, deletes := len(secrets.loads), len(secrets.saves), len(secrets.deletes)
	secrets.mu.Unlock()
	if loads != 2 || writes != 1 || deletes != 0 {
		t.Fatalf("fake credential store operations: loads=%d saves=%d deletes=%d, want 2/1/0", loads, writes, deletes)
	}
	if requestCount != 2 {
		t.Fatalf("fixture transport received %d requests, want exactly two registered GETs and no other outbound calls", requestCount)
	}
}

func TestGitHubCredentialRotationAcceptanceKeepsTargetAndUsesReplacement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleteErr error
	}{
		{name: "old credential removed"},
		{name: "old credential deletion failure is reported", deleteErr: errors.New("synthetic credential deletion failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runGitHubCredentialRotationAcceptance(t, tc.deleteErr)
		})
	}
}

func runGitHubCredentialRotationAcceptance(t *testing.T, deleteErr error) {
	t.Helper()
	const (
		csrfCanary    = "github-rotation-csrf-canary"
		oldToken      = "github_pat_synthetic_old_canary_0123456789ABCDEF"
		newToken      = "github_pat_synthetic_replacement_canary_0123456789ABCDEF"
		fixtureBody   = `{"name":"agent","full_name":"ops/agent","private":true,"default_branch":"main"}`
		connectionUTC = "2026-09-27T00:00:00Z"
		siblingUTC    = "2026-09-27T00:01:00Z"
	)

	oldTarget, siblingTarget, _, _, _ := serviceBundleTargets()
	cfg := serviceBundleConfig()
	cfg.ConnectionTests = map[string]connectionTest{
		oldTarget.ID:     {Result: "failure", CompletedAt: connectionUTC},
		siblingTarget.ID: {Result: "success", CompletedAt: siblingUTC},
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	secrets := &githubFirstUseAcceptanceSecrets{
		values:    map[string][]byte{oldTarget.SecretRef: []byte(oldToken)},
		deleteErr: deleteErr,
	}
	secrets.onDelete = func(ref string) error {
		if ref != oldTarget.SecretRef {
			return fmt.Errorf("save handler attempted to delete unexpected credential reference %q", ref)
		}
		persisted, err := readConfig(configPath)
		if err != nil {
			return err
		}
		index := findGitHubTargetIndex(persisted.GitHubTargets, oldTarget.ID)
		if index < 0 || persisted.GitHubTargets[index].SecretRef == oldTarget.SecretRef || !secretRefPattern.MatchString(persisted.GitHubTargets[index].SecretRef) {
			return errors.New("settings did not commit the replacement reference before deleting the old credential")
		}
		return nil
	}

	var requestCount int
	client := &http.Client{Transport: githubFirstUseAcceptanceRoundTripper(func(r *http.Request) (*http.Response, error) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "github.example.invalid" ||
			r.URL.EscapedPath() != "/api/v3/repos/ops/agent" || r.URL.RawQuery != "" || r.URL.Fragment != "" || r.URL.User != nil {
			return nil, fmt.Errorf("unexpected synthetic GitHub request %s %s", r.Method, r.URL.Redacted())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+newToken {
			return nil, errors.New("synthetic GitHub request did not use the replacement PAT")
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "local-agent-harness" {
			return nil, errors.New("synthetic GitHub request headers did not match the adapter contract")
		}
		if r.Body != nil {
			return nil, errors.New("synthetic GitHub GET unexpectedly had a request body")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(fixtureBody)),
			Request:    r,
		}, nil
	}), Timeout: 3 * time.Second}
	a := &app{configPath: configPath, secrets: secrets, client: client, csrf: csrfCanary}

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
	if initialResponse.StatusCode != http.StatusOK || len(csrfMatch) != 2 || string(csrfMatch[1]) != csrfCanary ||
		!strings.Contains(initialHTML, `value="`+oldTarget.ID+`" selected`) ||
		!strings.Contains(initialHTML, `value="`+oldTarget.Origin+`"`) ||
		!strings.Contains(initialHTML, `value="`+oldTarget.Repository+`"`) {
		t.Fatalf("initial UI did not render the selected registered target: status=%d", initialResponse.StatusCode)
	}
	initialTokenField := regexp.MustCompile(`<input\b[^>]*\bname="token"[^>]*>`).FindString(initialHTML)
	if initialTokenField == "" || regexp.MustCompile(`\svalue=`).MatchString(initialTokenField) {
		t.Fatalf("initial UI must keep the saved token field blank: %q", initialTokenField)
	}
	secrets.mu.Lock()
	initialLoadCount := len(secrets.loads)
	secrets.mu.Unlock()
	if strings.Contains(initialHTML, oldToken) || strings.Contains(initialHTML, oldTarget.SecretRef) || strings.Contains(initialHTML, newToken) ||
		requestCount != 0 || initialLoadCount != 0 {
		t.Fatal("initial UI exposed a credential or performed an outbound/secret-store read")
	}

	form := url.Values{
		"csrf":       {string(csrfMatch[1])},
		"target_id":  {oldTarget.ID},
		"name":       {oldTarget.Name},
		"origin":     {oldTarget.Origin},
		"repository": {oldTarget.Repository},
		"token":      {newToken},
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
		t.Fatalf("production /save handler rejected the rendered same-origin rotation form: status=%d body=%s", saveResponse.StatusCode, saveBody)
	}
	redirectURL, err := url.Parse(saveResponse.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if redirectURL.Path != "/" || redirectURL.Query().Get("github_id") != oldTarget.ID {
		t.Fatalf("rotation redirect=%q, want the same registered target ID", saveResponse.Header.Get("Location"))
	}

	settingsBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var savedConfig config
	if err := json.Unmarshal(settingsBytes, &savedConfig); err != nil {
		t.Fatal(err)
	}
	index := findGitHubTargetIndex(savedConfig.GitHubTargets, oldTarget.ID)
	if index < 0 {
		t.Fatal("credential rotation replaced or removed the registered target ID")
	}
	savedTarget := savedConfig.GitHubTargets[index]
	wantTarget := oldTarget
	wantTarget.SecretRef = savedTarget.SecretRef
	if savedTarget != wantTarget || !secretRefPattern.MatchString(savedTarget.SecretRef) || savedTarget.SecretRef == oldTarget.SecretRef {
		t.Fatalf("rotated target=%+v, want existing metadata and a fresh opaque reference", savedTarget)
	}
	if !reflect.DeepEqual(savedConfig.ServiceBundles, cfg.ServiceBundles) {
		t.Fatalf("credential rotation changed service-bundle mappings: got=%+v want=%+v", savedConfig.ServiceBundles, cfg.ServiceBundles)
	}
	if strings.Contains(string(settingsBytes), oldToken) || strings.Contains(string(settingsBytes), newToken) || !strings.Contains(string(settingsBytes), savedTarget.SecretRef) {
		t.Fatal("settings must keep the new credential reference without either credential value")
	}
	if _, exists := savedConfig.ConnectionTests[oldTarget.ID]; exists {
		t.Fatal("rotation must clear the selected target's previous connection history")
	}
	if savedConfig.ConnectionTests[siblingTarget.ID] != cfg.ConnectionTests[siblingTarget.ID] || len(savedConfig.ConnectionTests) != 1 {
		t.Fatalf("rotation should preserve only the unrelated target history: %+v", savedConfig.ConnectionTests)
	}
	secrets.mu.Lock()
	newSecret, newSecretExists := secrets.values[savedTarget.SecretRef]
	oldSecretExists := secrets.values[oldTarget.SecretRef] != nil
	saveCount, deleteCount, loadCount := len(secrets.saves), len(secrets.deletes), len(secrets.loads)
	deletedOldRef := len(secrets.deletes) == 1 && secrets.deletes[0] == oldTarget.SecretRef
	deleteOrderVerified := secrets.deleteSawCommittedReplacement
	remainingCredentials := len(secrets.values)
	secrets.mu.Unlock()
	wantOldSecretExists, wantRemainingCredentials := deleteErr != nil, 1
	if wantOldSecretExists {
		wantRemainingCredentials++
	}
	if !newSecretExists || string(newSecret) != newToken || oldSecretExists != wantOldSecretExists || remainingCredentials != wantRemainingCredentials ||
		saveCount != 1 || deleteCount != 1 || loadCount != 0 || !deletedOldRef || !deleteOrderVerified {
		t.Fatalf("fake secret store after rotation: saves=%d deletes=%d loads=%d remaining=%d replacement=%v old=%v deleteOrder=%v",
			saveCount, deleteCount, loadCount, remainingCredentials, newSecretExists, oldSecretExists, deleteOrderVerified)
	}
	if requestCount != 0 {
		t.Fatalf("credential rotation made %d outbound requests before an explicit connection test", requestCount)
	}

	registeredResponse, err := uiClient.Get(uiServer.URL + redirectURL.RequestURI())
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
	wantStatus := "GitHub target saved. Test the connection before using MCP."
	if deleteErr != nil {
		wantStatus = "GitHub target saved, but the previous unused credential could not be removed."
	}
	if registeredResponse.StatusCode != http.StatusOK ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.ID+`" selected`) ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.Origin+`"`) ||
		!strings.Contains(registeredHTML, `value="`+oldTarget.Repository+`"`) ||
		!strings.Contains(registeredHTML, wantStatus) ||
		registeredTokenField == "" || regexp.MustCompile(`\svalue=`).MatchString(registeredTokenField) {
		t.Fatalf("redirected UI did not preserve the selected target with a blank token field: status=%d tokenField=%q", registeredResponse.StatusCode, registeredTokenField)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "github-rotation-acceptance", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	connectionResponse, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "registered_target_connection_test",
		Arguments: map[string]any{"adapter": "github", "target": oldTarget.Name},
	})
	if err != nil || connectionResponse == nil || connectionResponse.IsError {
		t.Fatalf("rotated GitHub credential connection test failed: result=%#v err=%v", connectionResponse, err)
	}
	connectionJSON, err := json.Marshal(connectionResponse.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var connectionResult registeredTargetConnectionTestResult
	if err := json.Unmarshal(connectionJSON, &connectionResult); err != nil {
		t.Fatal(err)
	}
	if connectionResult.Adapter != "github" || connectionResult.Target != oldTarget.Name || connectionResult.Result != "success" ||
		connectionResult.CompletedAt == "" || connectionResult.Message != "Connection test succeeded. Historical status saved." {
		t.Fatalf("rotated GitHub connection result=%+v", connectionResult)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, connectionResult.CompletedAt)
	if err != nil || completedAt.Location() != time.UTC {
		t.Fatalf("rotated connection completion time %q is not valid UTC: %v", connectionResult.CompletedAt, err)
	}

	for _, output := range []struct {
		name string
		body string
	}{
		{name: "initial UI page", body: initialHTML},
		{name: "save response", body: string(saveResponse.Header.Get("Location")) + string(saveBody)},
		{name: "redirected UI page", body: registeredHTML},
		{name: "connection MCP result", body: mustMarshalGitHubFirstUse(t, connectionResponse)},
	} {
		for _, forbidden := range []string{oldToken, newToken, oldTarget.SecretRef, savedTarget.SecretRef} {
			if strings.Contains(output.body, forbidden) {
				t.Errorf("%s exposed credential material %q", output.name, forbidden)
			}
		}
	}

	storedConfig, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if history := storedConfig.ConnectionTests[oldTarget.ID]; history.Result != "success" || history.CompletedAt != connectionResult.CompletedAt {
		t.Fatalf("rotated target history=%+v, want the successful replacement-credential test", history)
	}
	if storedConfig.ConnectionTests[siblingTarget.ID] != cfg.ConnectionTests[siblingTarget.ID] {
		t.Fatalf("connection test changed unrelated target history: %+v", storedConfig.ConnectionTests[siblingTarget.ID])
	}
	secrets.mu.Lock()
	loads, writes, deletes := len(secrets.loads), len(secrets.saves), len(secrets.deletes)
	loadedReplacement := len(secrets.loads) == 1 && secrets.loads[0] == savedTarget.SecretRef
	finalReplacement, replacementExists := secrets.values[savedTarget.SecretRef]
	finalOldExists := secrets.values[oldTarget.SecretRef] != nil
	secrets.mu.Unlock()
	if loads != 1 || writes != 1 || deletes != 1 || !loadedReplacement || !replacementExists || string(finalReplacement) != newToken || finalOldExists != wantOldSecretExists {
		t.Fatalf("fake credential store after MCP test: loads=%d saves=%d deletes=%d loadedReplacement=%v replacement=%v old=%v",
			loads, writes, deletes, loadedReplacement, replacementExists, finalOldExists)
	}
	if requestCount != 1 {
		t.Fatalf("fixture transport received %d requests, want exactly one replacement-credential connection test", requestCount)
	}
}

type githubFirstUseAcceptanceSecrets struct {
	mu                            sync.Mutex
	values                        map[string][]byte
	saves                         []string
	loads                         []string
	deletes                       []string
	onDelete                      func(string) error
	deleteErr                     error
	deleteSawCommittedReplacement bool
}

func (s *githubFirstUseAcceptanceSecrets) Save(ref string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string][]byte)
	}
	s.values[ref] = append([]byte(nil), value...)
	s.saves = append(s.saves, ref)
	return nil
}

func (s *githubFirstUseAcceptanceSecrets) Load(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads = append(s.loads, ref)
	value, ok := s.values[ref]
	if !ok {
		return nil, http.ErrNoCookie
	}
	return append([]byte(nil), value...), nil
}

func (s *githubFirstUseAcceptanceSecrets) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, ref)
	if s.onDelete != nil {
		if err := s.onDelete(ref); err != nil {
			return err
		}
		s.deleteSawCommittedReplacement = true
	}
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, ref)
	return nil
}

type githubFirstUseAcceptanceRoundTripper func(*http.Request) (*http.Response, error)

func (f githubFirstUseAcceptanceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mustMarshalGitHubFirstUse(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
