//go:build windows && integration && parallelmanual

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const uiRenewalBrowserQAOptIn = "LAH_ENABLE_MANUAL_WINDOWS_UI_RENEWAL_BROWSER_QA"

type uiRenewalFixtureState struct {
	configHash          [32]byte
	dashboardSaveStatus atomic.Int32
	dashboardSavePosts  atomic.Int32
	namedSecretSaves    atomic.Int32
	namedSecretDeletes  atomic.Int32
	environmentWrites   atomic.Int32
	secretStoreCalls    atomic.Int32
	outboundRequests    atomic.Int32
}

type uiRenewalFixtureNamedSecrets struct{ state *uiRenewalFixtureState }

func (c uiRenewalFixtureNamedSecrets) ListNamedSecrets(context.Context) ([]NamedSecretView, error) {
	return []NamedSecretView{}, nil
}

func (c uiRenewalFixtureNamedSecrets) SaveNamedSecret(_ context.Context, write NamedSecretWrite) (NamedSecretView, error) {
	c.state.namedSecretSaves.Add(1)
	clearBytes(write.Value)
	return NamedSecretView{}, errors.New("isolated UI fixture does not permit secret writes")
}

func (c uiRenewalFixtureNamedSecrets) DeleteNamedSecret(context.Context, string) error {
	c.state.namedSecretDeletes.Add(1)
	return errors.New("isolated UI fixture does not permit secret deletion")
}

type uiRenewalFixtureEnvironment struct{ state *uiRenewalFixtureState }

func (c uiRenewalFixtureEnvironment) ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error) {
	return []UserEnvironmentEntry{}, nil
}

func (c uiRenewalFixtureEnvironment) SetUserEnvironment(_ context.Context, write UserEnvironmentWrite) (UserEnvironmentResult, error) {
	c.state.environmentWrites.Add(1)
	clearBytes(write.Value)
	return UserEnvironmentResult{}, errors.New("isolated UI fixture does not permit environment writes")
}

func (c uiRenewalFixtureEnvironment) DeleteUserEnvironment(context.Context, string) (UserEnvironmentResult, error) {
	c.state.environmentWrites.Add(1)
	return UserEnvironmentResult{}, errors.New("isolated UI fixture does not permit environment deletion")
}

type uiRenewalFixtureSecretStore struct{ state *uiRenewalFixtureState }

func (s uiRenewalFixtureSecretStore) Save(_ string, value []byte) error {
	s.state.secretStoreCalls.Add(1)
	clearBytes(value)
	return errors.New("isolated UI fixture does not permit credential writes")
}

func (s uiRenewalFixtureSecretStore) Load(string) ([]byte, error) {
	s.state.secretStoreCalls.Add(1)
	return nil, errors.New("isolated UI fixture does not permit credential reads")
}

func (s uiRenewalFixtureSecretStore) Delete(string) error {
	s.state.secretStoreCalls.Add(1)
	return errors.New("isolated UI fixture does not permit credential deletion")
}

type uiRenewalFixtureTransport struct{ state *uiRenewalFixtureState }

type uiRenewalCaptureStatusWriter struct {
	http.ResponseWriter
	state *uiRenewalFixtureState
	wrote bool
}

func (w *uiRenewalCaptureStatusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		w.state.dashboardSaveStatus.Store(int32(status))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *uiRenewalCaptureStatusWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func uiRenewalFixtureHandler(app *app, state *uiRenewalFixtureState) http.Handler {
	handler := newLocalUIHandler(app)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/save-dashboard" {
			state.dashboardSavePosts.Add(1)
			handler.ServeHTTP(&uiRenewalCaptureStatusWriter{ResponseWriter: w, state: state}, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func (t uiRenewalFixtureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.state.outboundRequests.Add(1)
	return nil, errors.New("external requests are disabled in the isolated UI fixture")
}

func uiRenewalStartFixture(t *testing.T) (baseURL, configPath string, state *uiRenewalFixtureState) {
	t.Helper()
	privateDir := filepath.Join(t.TempDir(), "private")
	aclMarker := filepath.Join(privateDir, "fixture-acl-marker")
	if err := writeSetupDraftQueueFileAtomically(aclMarker, []byte("1")); err != nil {
		t.Fatal("could not create the current-user-protected visual fixture directory")
	}
	if err := os.Remove(aclMarker); err != nil {
		t.Fatal("could not remove the temporary visual fixture marker")
	}
	configPath = filepath.Join(privateDir, "empty-settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not write the isolated empty settings file")
	}
	if err := cleanupBrowserQASecureRewrite(configPath, maxConfigSize); err != nil {
		t.Fatalf("could not protect the isolated settings fixture (%v)", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not start the loopback-only UI fixture")
	}
	host := listener.Addr().String()
	csrf, err := cleanupBrowserQARandomHex(32)
	if err != nil {
		_ = listener.Close()
		t.Fatal("could not initialize the isolated local UI")
	}
	state = &uiRenewalFixtureState{}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		_ = listener.Close()
		t.Fatal("could not fingerprint the isolated settings fixture")
	}
	state.configHash = sha256.Sum256(configBytes)
	clearBytes(configBytes)
	fixtureApp := &app{
		configPath:      configPath,
		secrets:         uiRenewalFixtureSecretStore{state: state},
		namedSecrets:    uiRenewalFixtureNamedSecrets{state: state},
		userEnvironment: uiRenewalFixtureEnvironment{state: state},
		client:          &http.Client{Transport: uiRenewalFixtureTransport{state: state}},
		host:            host,
		csrf:            csrf,
	}
	server := &http.Server{Handler: uiRenewalFixtureHandler(fixtureApp, state), ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		select {
		case <-serveDone:
		case <-ctx.Done():
			_ = server.Close()
		}
	})
	return "http://" + host, configPath, state
}

func TestManualWindowsUIRenewalBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("headed UI renewal acceptance requires native Windows")
	}
	if os.Getenv(uiRenewalBrowserQAOptIn) != "1" || os.Getenv(cleanupBrowserQAOptIn) != "1" {
		t.Skip("set both explicit UI renewal and named-secret cleanup browser QA opt-ins")
	}
	if os.Getenv(cleanupBrowserQARole) != "" {
		t.Skip("the child process runs only the production local UI handler")
	}

	if !t.Run("fresh-shell-responsive-visuals", testUIRenewalFreshShellScreenshots) {
		return
	}
	if !t.Run("javascript-disabled-native-reachability", testUIRenewalNoJavaScriptReachability) {
		return
	}
	t.Run("uninterrupted-fake-backed-cleanup-lifecycle", func(t *testing.T) {
		TestManualWindowsNamedSecretCleanupBrowserQA(t)
	})
}

func testUIRenewalFreshShellScreenshots(t *testing.T) {
	artifactDir := uiRenewalBrowserArtifactDir(t)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal("could not create the private UI screenshot directory")
	}
	baseURL, configPath, fixtureState := uiRenewalStartFixture(t)

	sessionSuffix, err := cleanupBrowserQARandomHex(6)
	if err != nil {
		t.Fatal("could not create an isolated headed browser session")
	}
	session := "ui-renewal-" + sessionSuffix
	profile := filepath.Join(filepath.Dir(configPath), "playwright-profile")
	browserOpen := false
	t.Cleanup(func() {
		if browserOpen {
			cleanupBrowserQARunPlaywright(t, session, "close")
		}
	})
	cleanupBrowserQARunPlaywright(t, session, "open", baseURL+"/", "--headed", "--profile", profile)
	browserOpen = true
	page := uiRenewalBrowserEval(t, session, `() => {const nav=document.querySelector('#lah-shell-nav');const links=Array.from(nav?.querySelectorAll('[data-pane-link]')||[]);const actions=Array.from(document.querySelectorAll('#lah-pane-connections .lah-quick-actions > .lah-quick-action'));const secretsPane=document.querySelector('#lah-pane-environment');return {ready:document.readyState === "complete",langKo:document.documentElement.lang === "ko",navVisible:!!nav&&!nav.hidden,navLinkCount:links.length,currentConnections:nav?.querySelector('[data-pane-link="connections"]')?.getAttribute('aria-current') === "page",connectionsVisible:!!document.querySelector('#lah-pane-connections')&&!document.querySelector('#lah-pane-connections').hidden&&!document.querySelector('#lah-pane-connections').inert,quickActionCount:actions.length,quickActionsVisible:actions.length===3&&actions.every(action=>action.getClientRects().length>0),secretsParked:!!secretsPane?.querySelector('[data-pane-content="environment"] > #lah-secrets-environment')&&!!secretsPane.hidden&&!!secretsPane.inert}}`)
	var firstPaint struct {
		Ready               bool `json:"ready"`
		LangKo              bool `json:"langKo"`
		NavVisible          bool `json:"navVisible"`
		NavLinkCount        int  `json:"navLinkCount"`
		CurrentConnections  bool `json:"currentConnections"`
		ConnectionsVisible  bool `json:"connectionsVisible"`
		QuickActionCount    int  `json:"quickActionCount"`
		QuickActionsVisible bool `json:"quickActionsVisible"`
		SecretsParked       bool `json:"secretsParked"`
	}
	pageErr := json.Unmarshal(page, &firstPaint)
	clearBytes(page)
	if pageErr != nil || !firstPaint.Ready || !firstPaint.LangKo || !firstPaint.NavVisible || firstPaint.NavLinkCount != 10 || !firstPaint.CurrentConnections || !firstPaint.ConnectionsVisible || firstPaint.QuickActionCount != 3 || !firstPaint.QuickActionsVisible || !firstPaint.SecretsParked {
		t.Fatal("the fresh Korean page did not retain its initial navigation, shortcuts, and parked secret panel")
	}

	viewportBytes := uiRenewalBrowserEval(t, session, `() => {window.scrollTo(0,0);return window.scrollY === 0}`)
	var viewportAtTop bool
	viewportErr := json.Unmarshal(viewportBytes, &viewportAtTop)
	clearBytes(viewportBytes)
	if viewportErr != nil || !viewportAtTop {
		t.Fatal("the headed screenshot viewport could not be reset to the top of the page")
	}

	for _, viewport := range []struct {
		width  string
		height string
		name   string
	}{{"1440", "1000", "ui-renewal-r5-attempt-" + sessionSuffix + "-1440.png"}, {"1024", "900", "ui-renewal-r5-attempt-" + sessionSuffix + "-1024.png"}, {"375", "812", "ui-renewal-r5-attempt-" + sessionSuffix + "-375.png"}} {
		cleanupBrowserQARunPlaywright(t, session, "resize", viewport.width, viewport.height)
		path := filepath.Join(artifactDir, viewport.name)
		cleanupBrowserQARunPlaywright(t, session, "screenshot", "--filename", path)
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("headed screenshot was not saved at viewport %s", viewport.width)
		}
		bounds, decodeErr := png.DecodeConfig(file)
		_ = file.Close()
		if decodeErr != nil || bounds.Width != atoiUIRenewal(viewport.width) || bounds.Height != atoiUIRenewal(viewport.height) {
			t.Fatalf("headed screenshot dimensions did not match viewport %s", viewport.width)
		}
	}
	testUIRenewalHydratedShellFlow(t, session, baseURL, fixtureState)
	cleanupBrowserQARunPlaywright(t, session, "close")
	browserOpen = false
	uiRenewalAssertFixtureWasReadOnly(t, fixtureState, configPath)
}

func testUIRenewalHydratedShellFlow(t *testing.T, session, baseURL string, fixtureState *uiRenewalFixtureState) {
	t.Helper()
	stateBytes := uiRenewalBrowserEval(t, session, `() => ({langKo:document.documentElement.lang === "ko", selectorKo:document.querySelector('#language-select')?.value === "ko", introKo:document.querySelector('.intro')?.textContent.trim() === document.querySelector('.intro')?.dataset.messageKo, localeGuard:!!document.querySelector('script[data-locale-guard]'), navReady:!!document.querySelector('#lah-shell-nav') && !document.querySelector('#lah-shell-nav').hidden})`)
	var localeState struct {
		LangKo      bool `json:"langKo"`
		SelectorKo  bool `json:"selectorKo"`
		IntroKo     bool `json:"introKo"`
		LocaleGuard bool `json:"localeGuard"`
		NavReady    bool `json:"navReady"`
	}
	localeErr := json.Unmarshal(stateBytes, &localeState)
	clearBytes(stateBytes)
	if localeErr != nil || !localeState.LangKo || !localeState.SelectorKo || !localeState.IntroKo || !localeState.LocaleGuard || !localeState.NavReady {
		t.Fatal("fresh Korean rendering did not finish before the page became visible")
	}

	cleanupBrowserQARunPlaywright(t, session, "select", "#language-select", "en")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({langEn:document.documentElement.lang === "en", selectorEn:document.querySelector('#language-select')?.value === "en", introEn:document.querySelector('.intro')?.textContent.trim() === document.querySelector('.intro')?.dataset.messageEn})`)
	var englishState struct {
		LangEn     bool `json:"langEn"`
		SelectorEn bool `json:"selectorEn"`
		IntroEn    bool `json:"introEn"`
	}
	localeErr = json.Unmarshal(stateBytes, &englishState)
	clearBytes(stateBytes)
	if localeErr != nil || !englishState.LangEn || !englishState.SelectorEn || !englishState.IntroEn {
		t.Fatal("the explicit English locale choice did not update the visible page")
	}
	cleanupBrowserQARunPlaywright(t, session, "reload")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({langEn:document.documentElement.lang === "en", selectorEn:document.querySelector('#language-select')?.value === "en", introEn:document.querySelector('.intro')?.textContent.trim() === document.querySelector('.intro')?.dataset.messageEn})`)
	localeErr = json.Unmarshal(stateBytes, &englishState)
	clearBytes(stateBytes)
	if localeErr != nil || !englishState.LangEn || !englishState.SelectorEn || !englishState.IntroEn {
		t.Fatal("the explicit English locale choice did not persist across reload")
	}

	uiRenewalBrowserAction(t, session, "fill", "#github-name", "UI navigation draft")
	uiRenewalBrowserAction(t, session, "click", `a[data-pane-link="groups"]`)
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({hashGroups:location.hash === "#lah-pane-groups", currentGroups:document.querySelector('a[data-pane-link="groups"]')?.getAttribute('aria-current') === "page", groupsVisible:!document.querySelector('#lah-pane-groups')?.hidden && !document.querySelector('#lah-pane-groups')?.inert, connectionsHidden:!!document.querySelector('#lah-pane-connections')?.hidden && !!document.querySelector('#lah-pane-connections')?.inert, draftPreserved:document.querySelector('#github-name')?.value === "UI navigation draft"})`)
	var navState struct {
		HashGroups        bool `json:"hashGroups"`
		CurrentGroups     bool `json:"currentGroups"`
		GroupsVisible     bool `json:"groupsVisible"`
		ConnectionsHidden bool `json:"connectionsHidden"`
		DraftPreserved    bool `json:"draftPreserved"`
	}
	navErr := json.Unmarshal(stateBytes, &navState)
	clearBytes(stateBytes)
	if navErr != nil || !navState.HashGroups || !navState.CurrentGroups || !navState.GroupsVisible || !navState.ConnectionsHidden || !navState.DraftPreserved {
		t.Fatalf("hydrated navigation state failed: hash_groups=%t aria_current=%t groups_visible=%t connections_hidden_inert=%t draft_preserved=%t", navState.HashGroups, navState.CurrentGroups, navState.GroupsVisible, navState.ConnectionsHidden, navState.DraftPreserved)
	}
	stateBytes = uiRenewalBrowserEval(t, session, `() => {const link=document.querySelector('a[data-pane-link="groups"]');link?.focus();return !!link&&document.activeElement===link}`)
	var groupLinkFocused bool
	navErr = json.Unmarshal(stateBytes, &groupLinkFocused)
	clearBytes(stateBytes)
	if navErr != nil || !groupLinkFocused {
		t.Fatal("the navigation link could not receive keyboard focus")
	}
	uiRenewalBrowserAction(t, session, "press", "Tab")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({tabAdvancedToSecrets:document.activeElement===document.querySelector('a[data-pane-link="secrets"]'), tabRemainsInNav:document.querySelector('#lah-shell-nav')?.contains(document.activeElement), inactivePanelInert:!!document.querySelector('#lah-pane-connections')?.inert})`)
	var keyboardState struct {
		TabAdvancedToSecrets bool `json:"tabAdvancedToSecrets"`
		TabRemainsInNav      bool `json:"tabRemainsInNav"`
		InactiveInert        bool `json:"inactivePanelInert"`
	}
	navErr = json.Unmarshal(stateBytes, &keyboardState)
	clearBytes(stateBytes)
	if navErr != nil || !keyboardState.TabAdvancedToSecrets || !keyboardState.TabRemainsInNav || !keyboardState.InactiveInert {
		t.Fatal("keyboard navigation entered an inactive content pane")
	}

	uiRenewalBrowserAction(t, session, "press", "Enter")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({secretsActive:location.hash === "#lah-pane-secrets" && document.querySelector('a[data-pane-link="secrets"]')?.getAttribute('aria-current') === "page", metadataAvailable:document.querySelector('#lah-secrets-environment')?.dataset.available === "true" && !document.querySelector('#lah-secrets-environment')?.hidden, unavailableNoticeHidden:!!document.querySelector('#lah-secrets-unavailable')?.hidden, namedSecretListVisible:!document.querySelector('#lah-named-secrets-heading')?.closest('section')?.hidden && !document.querySelector('#lah-named-secrets-heading')?.closest('section')?.inert, environmentListInactive:!!document.querySelector('#lah-user-environment-heading')?.closest('section')?.hidden && !!document.querySelector('#lah-user-environment-heading')?.closest('section')?.inert, noCredentialReference:!document.documentElement.innerHTML.includes("cred:")})`)
	var secretPaneState struct {
		SecretsActive           bool `json:"secretsActive"`
		MetadataAvailable       bool `json:"metadataAvailable"`
		UnavailableNoticeHidden bool `json:"unavailableNoticeHidden"`
		NamedSecretListVisible  bool `json:"namedSecretListVisible"`
		EnvironmentListInactive bool `json:"environmentListInactive"`
		NoCredentialReference   bool `json:"noCredentialReference"`
	}
	navErr = json.Unmarshal(stateBytes, &secretPaneState)
	clearBytes(stateBytes)
	if navErr != nil || !secretPaneState.SecretsActive || !secretPaneState.MetadataAvailable || !secretPaneState.UnavailableNoticeHidden || !secretPaneState.NamedSecretListVisible || !secretPaneState.EnvironmentListInactive || !secretPaneState.NoCredentialReference {
		t.Fatal("the available secret metadata panel did not render safely when selected")
	}
	uiRenewalBrowserAction(t, session, "go-back")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({backToGroups:location.hash === "#lah-pane-groups", currentGroups:document.querySelector('a[data-pane-link="groups"]')?.getAttribute('aria-current') === "page", inactiveSecrets:!!document.querySelector('#lah-pane-secrets')?.hidden && !!document.querySelector('#lah-pane-secrets')?.inert})`)
	var backState struct {
		BackToGroups    bool `json:"backToGroups"`
		CurrentGroups   bool `json:"currentGroups"`
		InactiveSecrets bool `json:"inactiveSecrets"`
	}
	navErr = json.Unmarshal(stateBytes, &backState)
	clearBytes(stateBytes)
	if navErr != nil || !backState.BackToGroups || !backState.CurrentGroups || !backState.InactiveSecrets {
		t.Fatal("browser back did not restore the previously active pane")
	}

	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/#ui-renewal-unknown-pane")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({fallbackConnections:location.hash === "#lah-pane-connections", currentConnections:document.querySelector('a[data-pane-link="connections"]')?.getAttribute('aria-current') === "page", skipHref:document.querySelector('a.skip')?.getAttribute('href') || ""})`)
	var fallbackState struct {
		FallbackConnections bool   `json:"fallbackConnections"`
		CurrentConnections  bool   `json:"currentConnections"`
		SkipHref            string `json:"skipHref"`
	}
	navErr = json.Unmarshal(stateBytes, &fallbackState)
	clearBytes(stateBytes)
	if navErr != nil || !fallbackState.FallbackConnections || !fallbackState.CurrentConnections || fallbackState.SkipHref != "#lah-pane-connections-heading" {
		t.Fatal("an unknown fragment did not fall back to the connections pane")
	}
	stateBytes = uiRenewalBrowserEval(t, session, `() => {document.body.tabIndex=-1;document.body.focus();return document.activeElement===document.body}`)
	var keyboardStartAtBody bool
	navErr = json.Unmarshal(stateBytes, &keyboardStartAtBody)
	clearBytes(stateBytes)
	if navErr != nil || !keyboardStartAtBody {
		t.Fatal("the browser keyboard baseline could not be set to the document body")
	}
	uiRenewalBrowserAction(t, session, "press", "Tab")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({skipFocused:document.activeElement===document.querySelector('a.skip'),skipTarget:document.querySelector('a.skip')?.getAttribute('href')==="#lah-pane-connections-heading"})`)
	var skipKeyboardState struct {
		SkipFocused bool `json:"skipFocused"`
		SkipTarget  bool `json:"skipTarget"`
	}
	navErr = json.Unmarshal(stateBytes, &skipKeyboardState)
	clearBytes(stateBytes)
	if navErr != nil || !skipKeyboardState.SkipFocused || !skipKeyboardState.SkipTarget {
		t.Fatalf("keyboard skip start failed: skip_focused=%t skip_target=%t", skipKeyboardState.SkipFocused, skipKeyboardState.SkipTarget)
	}
	uiRenewalBrowserAction(t, session, "press", "Enter")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({skipToContent:document.querySelector('a.skip')?.getAttribute('href') === "#lah-pane-connections-heading", focusedContent:document.activeElement === document.querySelector('#lah-pane-connections-heading'), contentVisible:!!document.querySelector('#lah-pane-connections-heading')?.getClientRects().length})`)
	var skipState struct {
		SkipToContent  bool `json:"skipToContent"`
		FocusedContent bool `json:"focusedContent"`
		ContentVisible bool `json:"contentVisible"`
	}
	navErr = json.Unmarshal(stateBytes, &skipState)
	clearBytes(stateBytes)
	if navErr != nil || !skipState.SkipToContent || !skipState.FocusedContent || !skipState.ContentVisible {
		t.Fatalf("keyboard skip activation failed: href_correct=%t content_focused=%t content_visible=%t", skipState.SkipToContent, skipState.FocusedContent, skipState.ContentVisible)
	}
	testUIRenewalSecretChildFragments(t, session, baseURL)
	if fixtureState.dashboardSavePosts.Load() != 0 {
		t.Fatal("the invalid-form focus check unexpectedly ran before the dedicated submission")
	}
	testUIRenewalInvalidDashboardFocus(t, session, fixtureState)
}

func testUIRenewalSecretChildFragments(t *testing.T, session, baseURL string) {
	t.Helper()
	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/#lah-named-secrets-heading")
	stateBytes := uiRenewalBrowserEval(t, session, `() => ({directHash:location.hash==="#lah-named-secrets-heading",secretsActive:!document.querySelector('#lah-pane-secrets')?.hidden&&!document.querySelector('#lah-pane-secrets')?.inert,currentSecrets:document.querySelector('a[data-pane-link="secrets"]')?.getAttribute('aria-current')==="page",namedSectionVisible:!document.querySelector('#lah-named-secrets-heading')?.closest('section')?.hidden&&!document.querySelector('#lah-named-secrets-heading')?.closest('section')?.inert,namedFormVisible:!!document.querySelector('#lah-named-secret-form')?.getClientRects().length,environmentSectionInactive:!!document.querySelector('#lah-user-environment-heading')?.closest('section')?.hidden&&!!document.querySelector('#lah-user-environment-heading')?.closest('section')?.inert})`)
	var namedSecretRoute struct {
		DirectHash                 bool `json:"directHash"`
		SecretsActive              bool `json:"secretsActive"`
		CurrentSecrets             bool `json:"currentSecrets"`
		NamedSectionVisible        bool `json:"namedSectionVisible"`
		NamedFormVisible           bool `json:"namedFormVisible"`
		EnvironmentSectionInactive bool `json:"environmentSectionInactive"`
	}
	navErr := json.Unmarshal(stateBytes, &namedSecretRoute)
	clearBytes(stateBytes)
	if navErr != nil || !namedSecretRoute.DirectHash || !namedSecretRoute.SecretsActive || !namedSecretRoute.CurrentSecrets || !namedSecretRoute.NamedSectionVisible || !namedSecretRoute.NamedFormVisible || !namedSecretRoute.EnvironmentSectionInactive {
		t.Fatalf("named-secret child route failed: direct_hash=%t secrets_active=%t aria_current=%t named_section_visible=%t named_form_visible=%t environment_inactive=%t", namedSecretRoute.DirectHash, namedSecretRoute.SecretsActive, namedSecretRoute.CurrentSecrets, namedSecretRoute.NamedSectionVisible, namedSecretRoute.NamedFormVisible, namedSecretRoute.EnvironmentSectionInactive)
	}

	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/#lah-user-environment-heading")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({directHash:location.hash==="#lah-user-environment-heading",environmentActive:!document.querySelector('#lah-pane-environment')?.hidden&&!document.querySelector('#lah-pane-environment')?.inert,currentEnvironment:document.querySelector('a[data-pane-link="environment"]')?.getAttribute('aria-current')==="page",environmentSectionVisible:!document.querySelector('#lah-user-environment-heading')?.closest('section')?.hidden&&!document.querySelector('#lah-user-environment-heading')?.closest('section')?.inert,environmentFormVisible:!!document.querySelector('#lah-user-environment-form')?.getClientRects().length,namedSectionInactive:!!document.querySelector('#lah-named-secrets-heading')?.closest('section')?.hidden&&!!document.querySelector('#lah-named-secrets-heading')?.closest('section')?.inert})`)
	var environmentRoute struct {
		DirectHash           bool `json:"directHash"`
		EnvironmentActive    bool `json:"environmentActive"`
		CurrentEnvironment   bool `json:"currentEnvironment"`
		EnvironmentVisible   bool `json:"environmentSectionVisible"`
		EnvironmentForm      bool `json:"environmentFormVisible"`
		NamedSectionInactive bool `json:"namedSectionInactive"`
	}
	navErr = json.Unmarshal(stateBytes, &environmentRoute)
	clearBytes(stateBytes)
	if navErr != nil || !environmentRoute.DirectHash || !environmentRoute.EnvironmentActive || !environmentRoute.CurrentEnvironment || !environmentRoute.EnvironmentVisible || !environmentRoute.EnvironmentForm || !environmentRoute.NamedSectionInactive {
		t.Fatalf("user-environment child route failed: direct_hash=%t environment_active=%t aria_current=%t environment_visible=%t environment_form_visible=%t named_section_inactive=%t", environmentRoute.DirectHash, environmentRoute.EnvironmentActive, environmentRoute.CurrentEnvironment, environmentRoute.EnvironmentVisible, environmentRoute.EnvironmentForm, environmentRoute.NamedSectionInactive)
	}

	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/#lah-pane-connections")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({connectionsActive:location.hash==="#lah-pane-connections"&&!document.querySelector('#lah-pane-connections')?.hidden&&!document.querySelector('#lah-pane-connections')?.inert,currentConnections:document.querySelector('a[data-pane-link="connections"]')?.getAttribute('aria-current')==="page"})`)
	var connectionsRoute struct {
		ConnectionsActive  bool `json:"connectionsActive"`
		CurrentConnections bool `json:"currentConnections"`
	}
	navErr = json.Unmarshal(stateBytes, &connectionsRoute)
	clearBytes(stateBytes)
	if navErr != nil || !connectionsRoute.ConnectionsActive || !connectionsRoute.CurrentConnections {
		t.Fatalf("connection pane restore failed: connections_active=%t aria_current=%t", connectionsRoute.ConnectionsActive, connectionsRoute.CurrentConnections)
	}
}

func testUIRenewalInvalidDashboardFocus(t *testing.T, session string, fixtureState *uiRenewalFixtureState) {
	t.Helper()
	uiRenewalBrowserAction(t, session, "click", `#lah-pane-connections details[data-connection-section="dashboard-heading"] > summary`)
	uiRenewalBrowserAction(t, session, "fill", "#dashboard-name", "UI invalid-field focus")
	uiRenewalBrowserAction(t, session, "fill", "#dashboard-url", "http://invalid.invalid")
	inputValue, err := cleanupBrowserQARandomValue()
	if err != nil {
		t.Fatal("could not create a private form-submission input")
	}
	privacySentinel, err := cleanupBrowserQARandomValue()
	if err != nil {
		clearBytes(inputValue)
		t.Fatal("could not create a private response sentinel")
	}
	defer clearBytes(inputValue)
	defer clearBytes(privacySentinel)
	cleanupBrowserQAPlaywright(t, session, inputValue, privacySentinel, "fill", "#dashboard-token", string(inputValue))
	uiRenewalBrowserAction(t, session, "click", `#dashboard-save-form button[type="submit"]`)
	if fixtureState.dashboardSavePosts.Load() != 1 || fixtureState.dashboardSaveStatus.Load() != int32(http.StatusUnprocessableEntity) {
		t.Fatal("the browser form did not receive the expected validation response")
	}
	page := cleanupBrowserQAPlaywrightSnapshot(t, session, inputValue, privacySentinel, "snapshot")
	if len(page) == 0 {
		t.Fatal("the invalid-form response page was empty")
	}
	clearBytes(page)
	stateBytes := uiRenewalBrowserEval(t, session, `() => ({invalidURLFocused:document.activeElement?.id === "dashboard-url", invalidURLMarked:document.querySelector('#dashboard-url')?.getAttribute('aria-invalid') === "true", connectionsActive:!document.querySelector('#lah-pane-connections')?.hidden && !document.querySelector('#lah-pane-connections')?.inert, connectionCurrent:document.querySelector('a[data-pane-link="connections"]')?.getAttribute('aria-current') === "page", dashboardDetailsOpen:!!document.querySelector('details[data-connection-section="dashboard-heading"]')?.open, tokenFieldCleared:document.querySelector('#dashboard-token')?.value === "", noCredentialReference:!document.documentElement.innerHTML.includes("cred:")})`)
	var state struct {
		InvalidURLFocused     bool `json:"invalidURLFocused"`
		InvalidURLMarked      bool `json:"invalidURLMarked"`
		ConnectionsActive     bool `json:"connectionsActive"`
		ConnectionCurrent     bool `json:"connectionCurrent"`
		DashboardDetailsOpen  bool `json:"dashboardDetailsOpen"`
		TokenFieldCleared     bool `json:"tokenFieldCleared"`
		NoCredentialReference bool `json:"noCredentialReference"`
	}
	stateErr := json.Unmarshal(stateBytes, &state)
	clearBytes(stateBytes)
	if stateErr != nil || !state.InvalidURLFocused || !state.InvalidURLMarked || !state.ConnectionsActive || !state.ConnectionCurrent || !state.DashboardDetailsOpen || !state.TokenFieldCleared || !state.NoCredentialReference {
		t.Fatal("the returned validation page did not reveal and focus the invalid field safely")
	}
}

func uiRenewalBrowserAction(t *testing.T, session string, args ...string) {
	t.Helper()
	output := cleanupBrowserQARunPlaywright(t, session, args...)
	clearBytes(output)
}

func testUIRenewalNoJavaScriptReachability(t *testing.T) {
	baseURL, configPath, fixtureState := uiRenewalStartFixture(t)

	sessionSuffix, err := cleanupBrowserQARandomHex(6)
	if err != nil {
		t.Fatal("could not create an isolated no-JavaScript browser session")
	}
	session := "ui-renewal-nojs-" + sessionSuffix
	profile := filepath.Join(filepath.Dir(configPath), "playwright-nojs-profile")
	cliConfigPath := filepath.Join(filepath.Dir(configPath), "playwright-nojs.json")
	cliConfig, err := json.Marshal(map[string]any{
		"browser": map[string]any{
			"browserName": "chromium", "userDataDir": profile,
			"contextOptions": map[string]any{"javaScriptEnabled": false},
		},
	})
	if err != nil || os.WriteFile(cliConfigPath, cliConfig, 0o600) != nil {
		t.Fatal("could not prepare the isolated JavaScript-disabled browser context")
	}

	browserOpen := false
	t.Cleanup(func() {
		if browserOpen {
			cleanupBrowserQARunPlaywright(t, session, "close")
		}
	})
	url := baseURL + "/"
	uiRenewalOpenNoJSBrowser(t, session, cliConfigPath, url)
	browserOpen = true
	stateBytes := uiRenewalBrowserEval(t, session, `() => ({mainVisible:!!document.querySelector('main')?.getClientRects().length,formVisible:[...document.querySelectorAll('form')].some(form=>form.getClientRects().length>0),skipLink:document.querySelector('a.skip')?.getAttribute('href')||'',shellHidden:!!document.querySelector('#lah-shell')?.hidden,navHidden:!!document.querySelector('#lah-shell-nav')?.hidden,englishFallback:document.documentElement.lang==='en'})`)
	var state struct {
		MainVisible     bool   `json:"mainVisible"`
		FormVisible     bool   `json:"formVisible"`
		SkipLink        string `json:"skipLink"`
		ShellHidden     bool   `json:"shellHidden"`
		NavHidden       bool   `json:"navHidden"`
		EnglishFallback bool   `json:"englishFallback"`
		Hash            string `json:"hash"`
		FocusedMain     bool   `json:"focusedMain"`
		ScrollY         int    `json:"scrollY"`
	}
	stateErr := json.Unmarshal(stateBytes, &state)
	clearBytes(stateBytes)
	if stateErr != nil || !state.MainVisible || !state.FormVisible || state.SkipLink != "#connections" || !state.ShellHidden || !state.NavHidden || !state.EnglishFallback {
		t.Fatal("JavaScript-disabled UI did not expose its main form and native skip link")
	}
	cleanupBrowserQARunPlaywright(t, session, "press", "Tab")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({skipFocused:document.activeElement===document.querySelector('a.skip')})`)
	var skipFocused bool
	stateErr = json.Unmarshal(stateBytes, &skipFocused)
	clearBytes(stateBytes)
	if stateErr != nil || !skipFocused {
		t.Fatal("JavaScript-disabled keyboard navigation did not focus the native skip link")
	}
	cleanupBrowserQARunPlaywright(t, session, "press", "Enter")
	stateBytes = uiRenewalBrowserEval(t, session, `() => ({hash:location.hash,mainVisible:!!document.querySelector('main')?.getClientRects().length,focusedMain:document.activeElement===document.querySelector('#connections'),scrollY:Math.round(window.scrollY)})`)
	stateErr = json.Unmarshal(stateBytes, &state)
	clearBytes(stateBytes)
	if stateErr != nil || !state.MainVisible || state.Hash != "#connections" {
		t.Fatal("native skip link did not reach the main connections section without JavaScript")
	}
	cleanupBrowserQARunPlaywright(t, session, "close")
	browserOpen = false
	uiRenewalAssertFixtureWasReadOnly(t, fixtureState, configPath)
}

func uiRenewalOpenNoJSBrowser(t *testing.T, session, configPath, url string) {
	t.Helper()
	nodePath, cliPath, err := cleanupBrowserQAResolvePlaywrightCLI()
	if err != nil {
		t.Fatal("cached Playwright CLI is unavailable for the no-JavaScript browser fixture")
	}
	logFile, err := os.CreateTemp(filepath.Dir(configPath), "nojs-cli-open-*.log")
	if err != nil {
		t.Fatal("could not create the private no-JavaScript CLI diagnostic file")
	}
	logPath := logFile.Name()
	defer os.Remove(logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, cliPath, "--session", session, "open", url, "--config", configPath, "--headed")
	command.Dir = filepath.Dir(os.Args[0])
	command.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
	command.WaitDelay = 2 * time.Second
	command.Stdout, command.Stderr = logFile, logFile
	command.Cancel = func() error {
		if command.Process == nil || command.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
		killer.Stdout, killer.Stderr = io.Discard, io.Discard
		if killErr := killer.Run(); killErr != nil {
			return errors.New("could not stop the timed-out no-JavaScript Playwright process tree")
		}
		return nil
	}
	runErr := command.Run()
	closeErr := logFile.Close()
	if runErr == nil && closeErr == nil {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		uiRenewalQuietCloseBrowserSession(nodePath, cliPath, session)
		t.Fatal("no-JavaScript Playwright open exceeded the 60-second command limit")
	}

	var exitErr *exec.ExitError
	exitCode := -1
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	var safeMessage string
	if info, statErr := os.Stat(logPath); statErr != nil {
		safeMessage = "diagnostic_unavailable"
	} else if info.Size() > 16<<10 {
		safeMessage = "diagnostic_exceeded_capture_bound"
	} else if output, readErr := os.ReadFile(logPath); readErr != nil {
		safeMessage = "diagnostic_unavailable"
	} else {
		safeMessage = uiRenewalSafeCLIMessage(output, configPath, filepath.Dir(configPath), url)
		clearBytes(output)
	}
	uiRenewalQuietCloseBrowserSession(nodePath, cliPath, session)
	t.Fatalf("no-JavaScript Playwright open failed: exit_code=%d cli_message=%s", exitCode, safeMessage)
}

func uiRenewalQuietCloseBrowserSession(nodePath, cliPath, session string) {
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
		killer.Stdout, killer.Stderr = io.Discard, io.Discard
		return killer.Run()
	}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	_ = command.Run()
}

func uiRenewalSafeCLIMessage(output []byte, configDir, parentDir, url string) string {
	message := strings.TrimSpace(string(output))
	message = strings.ReplaceAll(message, configDir, "<private-config>")
	message = strings.ReplaceAll(message, parentDir, "<private-temp>")
	message = strings.ReplaceAll(message, url, "<loopback-url>")
	message = strings.ReplaceAll(message, "127.0.0.1", "<loopback>")
	lower := strings.ToLower(message)
	if strings.Contains(lower, "cred:") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") {
		return "diagnostic_redacted_for_privacy"
	}
	if message == "" {
		return "empty_cli_diagnostic"
	}
	line := strings.SplitN(message, "\n", 2)[0]
	line = strings.Map(func(value rune) rune {
		if value >= '0' && value <= '9' {
			return '#'
		}
		return value
	}, line)
	if len(line) > 180 {
		line = line[:180]
	}
	return line
}

func uiRenewalAssertFixtureWasReadOnly(t *testing.T, state *uiRenewalFixtureState, configPath string) {
	t.Helper()
	if posts := state.dashboardSavePosts.Load(); posts != 0 && (posts != 1 || state.dashboardSaveStatus.Load() != int32(http.StatusUnprocessableEntity)) {
		t.Fatal("the isolated invalid-form browser request did not remain a single validation-only response")
	}
	if state.namedSecretSaves.Load() != 0 || state.namedSecretDeletes.Load() != 0 ||
		state.environmentWrites.Load() != 0 || state.secretStoreCalls.Load() != 0 || state.outboundRequests.Load() != 0 {
		t.Fatal("the isolated visual UI fixture attempted a backend mutation or outbound request")
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("the isolated visual UI settings file became unreadable")
	}
	defer clearBytes(configBytes)
	if sha256.Sum256(configBytes) != state.configHash {
		t.Fatal("the isolated visual UI settings file changed during read-only browser checks")
	}
}

func uiRenewalBrowserEval(t *testing.T, session, expression string) []byte {
	t.Helper()
	return cleanupBrowserQARunPlaywright(t, session, "eval", expression, "--raw")
}

func uiRenewalBrowserArtifactDir(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate the isolated UI acceptance workspace")
	}
	for current := filepath.Clean(workingDir); ; current = filepath.Dir(current) {
		if filepath.Base(current) == "cleanup-ui-renewal-20261005" {
			return filepath.Join(current, "screenshots")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	t.Fatal("the test is outside its dedicated UI renewal artifact directory")
	return ""
}

func atoiUIRenewal(value string) int {
	result := 0
	for _, digit := range value {
		result = result*10 + int(digit-'0')
	}
	return result
}
