//go:build windows && integration && parallelmanual

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const readinessBrowserQAOptIn = "LAH_ENABLE_MANUAL_WINDOWS_READINESS_BROWSER_QA"

type readinessBrowserQAState struct {
	statusGets        atomic.Int32
	runtimeStatusGets atomic.Int32
	passiveGets       atomic.Int32
	inspectionGets    atomic.Int32
	failedPassive     atomic.Int32
	failedResponses   atomic.Int32
	mutatingRequests  atomic.Int32
	observerCalls     atomic.Int32
	externalCalls     atomic.Int32
	registrationOps   atomic.Int32
	registration      *readinessBrowserQARegistration
}

type readinessBrowserQAObserver struct{ state *readinessBrowserQAState }

func (o readinessBrowserQAObserver) ObserveResidentEndpoint(context.Context) ResidentEndpointObservation {
	o.state.observerCalls.Add(1)
	return ResidentEndpointObservation{State: ResidentEndpointUnknown, ObservedAt: time.Now().UTC()}
}

type readinessBrowserQARegistration struct {
	state       *readinessBrowserQAState
	mu          sync.Mutex
	calls       []string
	blockNext   atomic.Bool
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	statusByID  map[string]MCPClientStatus
	errorClient map[string]error
}

func (r *readinessBrowserQARegistration) InspectClient(ctx context.Context, clientID string) (MCPClientStatus, error) {
	r.mu.Lock()
	r.calls = append(r.calls, clientID)
	r.mu.Unlock()
	if r.blockNext.CompareAndSwap(true, false) {
		r.startOnce.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return MCPClientStatus{}, ctx.Err()
		}
	}
	if err := r.errorClient[clientID]; err != nil {
		return MCPClientStatus{}, err
	}
	status, ok := r.statusByID[clientID]
	if !ok {
		return MCPClientStatus{}, errors.New("synthetic client status unavailable")
	}
	return status, nil
}

func (r *readinessBrowserQARegistration) PlanClientRegistration(context.Context, MCPRegistrationSpec) (MCPClientRegistrationPlan, error) {
	r.state.registrationOps.Add(1)
	return MCPClientRegistrationPlan{}, errors.New("readiness browser fixture does not permit registration changes")
}

func (r *readinessBrowserQARegistration) BackupClientRegistration(context.Context, string) (MCPClientBackupReceipt, error) {
	r.state.registrationOps.Add(1)
	return MCPClientBackupReceipt{}, errors.New("readiness browser fixture does not permit registration changes")
}

func (r *readinessBrowserQARegistration) ApplyClientRegistration(context.Context, string, string) (MCPClientStatus, error) {
	r.state.registrationOps.Add(1)
	return MCPClientStatus{}, errors.New("readiness browser fixture does not permit registration changes")
}

func (r *readinessBrowserQARegistration) VerifyClientRegistration(context.Context, string) (MCPClientStatus, error) {
	r.state.registrationOps.Add(1)
	return MCPClientStatus{}, errors.New("readiness browser fixture does not permit registration changes")
}

func (r *readinessBrowserQARegistration) RestoreClientRegistration(context.Context, string, string) (MCPClientStatus, error) {
	r.state.registrationOps.Add(1)
	return MCPClientStatus{}, errors.New("readiness browser fixture does not permit registration changes")
}

func readinessBrowserQAHandler(app *app, state *readinessBrowserQAState) http.Handler {
	production := newLocalUIHandlerWithResidentObserver(app, readinessBrowserQAObserver{state: state})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/__readiness_nojs_probe" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html lang="ko"><body><main id="nojs-probe">before</main><noscript><p id="nojs-notice">JavaScript가 비활성화된 상태입니다.</p></noscript><script>document.querySelector('#nojs-probe').textContent='after';document.documentElement.dataset.scriptRan='true'</script></body></html>`))
			return
		}
		if r.Method != http.MethodGet {
			state.mutatingRequests.Add(1)
		}
		if r.Method == http.MethodGet && r.URL.Path == runtimeClientsUIStatusPath {
			state.runtimeStatusGets.Add(1)
		}
		if r.Method == http.MethodGet && r.URL.Path == readinessStatusPath {
			state.statusGets.Add(1)
			if readinessClientInspectionRequested(r) {
				state.inspectionGets.Add(1)
			} else {
				state.passiveGets.Add(1)
				if state.failedPassive.CompareAndSwap(1, 0) {
					state.failedResponses.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
		}
		production.ServeHTTP(w, r)
	})
}

func TestManualWindowsReadinessBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("headed readiness acceptance requires native Windows")
	}
	if os.Getenv(readinessBrowserQAOptIn) != "1" {
		t.Skip("set the explicit readiness browser QA opt-in to run the headed Windows check")
	}
	if os.Getenv(cleanupBrowserQARole) != "" {
		t.Skip("readiness browser acceptance runs only in the opted-in parent test process")
	}

	artifactDir := readinessBrowserQAArtifactDir(t)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal("could not create the private readiness screenshot directory")
	}
	baseURL, configPath, fixture := readinessBrowserQAStartFixture(t)

	suffix, err := cleanupBrowserQARandomHex(6)
	if err != nil {
		t.Fatal("could not create a unique readiness browser attempt")
	}
	session := "readiness-r3-" + suffix
	profile := filepath.Join(filepath.Dir(configPath), "private-playwright-profile")
	browserOpen := false
	t.Cleanup(func() {
		if browserOpen {
			cleanupBrowserQARunPlaywright(t, session, "close")
		}
	})
	cleanupBrowserQARunPlaywright(t, session, "open", baseURL+"/", "--headed", "--profile", profile)
	browserOpen = true

	linkBytes := readinessBrowserQAEval(t, session, `() => ({ready:document.readyState==="complete",readinessLink:!!document.querySelector('a[href="/readiness"]')})`)
	var home struct {
		Ready         bool `json:"ready"`
		ReadinessLink bool `json:"readinessLink"`
	}
	if err := json.Unmarshal(linkBytes, &home); err != nil {
		clearBytes(linkBytes)
		t.Fatal("home readiness link assertion returned an invalid result")
	}
	clearBytes(linkBytes)
	if !home.Ready || !home.ReadinessLink {
		t.Fatalf("home navigation contract: ready=%t readiness_link=%t", home.Ready, home.ReadinessLink)
	}
	homePageInspectionBaseline := fixture.registrationCallCount()
	readinessBrowserQAAction(t, session, "click", `a[href="/readiness"]`)
	readinessBrowserQAAwaitSummary(t, session, "needs_setup")

	firstBytes := readinessBrowserQAEval(t, session, `() => {const state=document.querySelector('#readiness-summary');const rows=document.querySelectorAll('#readiness-target-list > li.readiness-item');const targetNotice=rows.length===1&&rows[0].textContent.includes('저장된 서비스 대상이 없습니다');const links=Array.from(document.querySelectorAll('a[href]'));return {langKo:document.documentElement.lang==="ko",titleKo:document.querySelector('h1')?.textContent.trim()==="연결 준비 상태",stateNeedsSetup:state?.dataset.state==="needs_setup",zeroTargets:targetNotice,noTargetsText:(document.querySelector('#readiness-target-list')?.textContent||'').includes('저장된 서비스 대상이 없습니다'),clientInspectionNotRun:(document.querySelector('#readiness-client-inspection')?.textContent||'').includes('확인하지 않았습니다'),manualLocalOnly:(document.body.textContent||'').includes('이 브라우저의 현재 로컬 앱 주소에만 저장'),nav:{groups:links.some(a=>a.getAttribute('href')==='/#lah-pane-groups'),diagnosis:links.some(a=>a.getAttribute('href')==='/#lah-pane-diagnosis'),ssh:links.some(a=>a.getAttribute('href')==='/#lah-pane-ssh'),draft:links.some(a=>/draft|lah-pane-setup/i.test(a.getAttribute('href')||'')),clients:links.some(a=>a.getAttribute('href')==='/#lah-pane-clients'),advanced:links.some(a=>a.getAttribute('href')==='/#lah-pane-advanced')},noHorizontalOverflow:document.documentElement.scrollWidth<=document.documentElement.clientWidth}}`)
	var first struct {
		LangKo                 bool  `json:"langKo"`
		TitleKo                bool  `json:"titleKo"`
		StateNeedsSetup        bool  `json:"stateNeedsSetup"`
		ZeroTargets            bool  `json:"zeroTargets"`
		NoTargetsText          bool  `json:"noTargetsText"`
		ClientInspectionNotRun bool  `json:"clientInspectionNotRun"`
		ManualLocalOnly        bool  `json:"manualLocalOnly"`
		Features               []any `json:"features"`
		Nav                    struct {
			Groups    bool `json:"groups"`
			Diagnosis bool `json:"diagnosis"`
			SSH       bool `json:"ssh"`
			Draft     bool `json:"draft"`
			Clients   bool `json:"clients"`
			Advanced  bool `json:"advanced"`
		} `json:"nav"`
		NoHorizontalOverflow bool `json:"noHorizontalOverflow"`
	}
	firstErr := json.Unmarshal(firstBytes, &first)
	clearBytes(firstBytes)
	if firstErr != nil || !first.LangKo || !first.TitleKo || !first.StateNeedsSetup || !first.ZeroTargets || !first.NoTargetsText || !first.ClientInspectionNotRun || !first.ManualLocalOnly || !first.NoHorizontalOverflow || fixture.registrationCallCount() != homePageInspectionBaseline {
		t.Fatalf("fresh readiness view: ko=%t title_ko=%t needs_setup=%t zero_targets=%t no_targets_text=%t clients_not_inspected=%t manual_local_only=%t no_overflow=%t client_inspection_delta=%d", first.LangKo, first.TitleKo, first.StateNeedsSetup, first.ZeroTargets, first.NoTargetsText, first.ClientInspectionNotRun, first.ManualLocalOnly, first.NoHorizontalOverflow, fixture.registrationCallCount()-homePageInspectionBaseline)
	}

	viewportBytes := readinessBrowserQAEval(t, session, `() => {window.scrollTo(0,0);return window.scrollY===0}`)
	var atTop bool
	viewportErr := json.Unmarshal(viewportBytes, &atTop)
	clearBytes(viewportBytes)
	if viewportErr != nil || !atTop {
		t.Fatal("could not align the safe first-view screenshot to the top of the page")
	}
	for _, viewport := range []struct{ width, height, label string }{
		{"1440", "1000", "1440"},
		{"375", "812", "375"},
	} {
		readinessBrowserQAAction(t, session, "resize", viewport.width, viewport.height)
		path := filepath.Join(artifactDir, "readiness-r3-attempt-"+suffix+"-"+viewport.label+".png")
		readinessBrowserQAAction(t, session, "screenshot", "--filename", path)
		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("safe first-view screenshot missing for viewport %s", viewport.label)
		}
		bounds, decodeErr := png.DecodeConfig(file)
		_ = file.Close()
		if decodeErr != nil || bounds.Width != readinessBrowserQAInt(viewport.width) || bounds.Height != readinessBrowserQAInt(viewport.height) {
			t.Fatalf("safe screenshot dimensions mismatch for viewport %s", viewport.label)
		}
		t.Logf("screenshot=%s viewport=%sx%s", path, viewport.width, viewport.height)
	}

	readinessBrowserQAAction(t, session, "select", "#readiness-language", "en")
	englishBytes := readinessBrowserQAEval(t, session, `() => ({langEn:document.documentElement.lang==="en",selectorEn:document.querySelector('#readiness-language')?.value==="en",englishHeading:document.querySelector('h1')?.textContent.trim()==="Connection readiness"})`)
	var english struct{ LangEn, SelectorEn, EnglishHeading bool }
	englishErr := json.Unmarshal(englishBytes, &english)
	clearBytes(englishBytes)
	if englishErr != nil || !english.LangEn || !english.SelectorEn || !english.EnglishHeading {
		t.Fatalf("English selection: lang=%t selector=%t heading=%t", english.LangEn, english.SelectorEn, english.EnglishHeading)
	}
	readinessBrowserQAAction(t, session, "reload")
	readinessBrowserQAAwaitSummary(t, session, "needs_setup")
	englishBytes = readinessBrowserQAEval(t, session, `() => ({langEn:document.documentElement.lang==="en",selectorEn:document.querySelector('#readiness-language')?.value==="en",englishHeading:document.querySelector('h1')?.textContent.trim()==="Connection readiness"})`)
	englishErr = json.Unmarshal(englishBytes, &english)
	clearBytes(englishBytes)
	if englishErr != nil || !english.LangEn || !english.SelectorEn || !english.EnglishHeading {
		t.Fatalf("English choice did not persist across reload: lang=%t selector=%t heading=%t", english.LangEn, english.SelectorEn, english.EnglishHeading)
	}
	readinessBrowserQAAction(t, session, "select", "#readiness-language", "ko")

	readinessBrowserQAAction(t, session, "click", `input[data-manual="client-codex-listed"]`)
	markBytes := readinessBrowserQAEval(t, session, `() => {const key='lah-readiness-user-checks-v1';let shape=false,noValues=false;try{const data=JSON.parse(localStorage.getItem(key)||'null');shape=!!data&&typeof data.revision==='string'&&!!data.marks&&typeof data.marks['client-codex-listed']?.checkedAt==='string'&&Object.keys(data).sort().join(',')==='marks,revision';noValues=shape&&Object.values(data.marks).every(mark=>mark&&Object.keys(mark).every(field=>field==='checkedAt'));}catch{}return {checked:!!document.querySelector('[data-manual="client-codex-listed"]')?.checked,localMarkShape:shape,storesOnlyCheckTimes:noValues}}`)
	var mark struct {
		Checked          bool `json:"checked"`
		LocalMarkShape   bool `json:"localMarkShape"`
		StoresOnlyChecks bool `json:"storesOnlyCheckTimes"`
	}
	markErr := json.Unmarshal(markBytes, &mark)
	clearBytes(markBytes)
	if markErr != nil || !mark.Checked || !mark.LocalMarkShape || !mark.StoresOnlyChecks {
		t.Fatalf("manual check storage: checked=%t record_shape=%t times_only=%t", mark.Checked, mark.LocalMarkShape, mark.StoresOnlyChecks)
	}
	readinessBrowserQAAction(t, session, "reload")
	readinessBrowserQAAwaitSummary(t, session, "needs_setup")
	markBytes = readinessBrowserQAEval(t, session, `() => !!document.querySelector('[data-manual="client-codex-listed"]')?.checked`)
	var markPersists bool
	markErr = json.Unmarshal(markBytes, &markPersists)
	clearBytes(markBytes)
	if markErr != nil || !markPersists {
		t.Fatal("manual check did not persist in the isolated browser profile")
	}

	// Update only the protected temporary fixture. The browser should not observe
	// this new revision until its normal refresh succeeds.
	fixtureConfig := readinessBrowserQAConfiguredFixture()
	if err := writeConfig(configPath, fixtureConfig); err != nil {
		t.Fatal("could not update the isolated readiness settings fixture")
	}
	fixture.rememberFinalConfig(t, configPath)
	fixture.registration.armSingleDelay()
	fixture.state.failedPassive.Store(1)
	interleaving := readinessBrowserQAEval(t, session, `() => new Promise(resolve=>{const inspect=document.querySelector('#readiness-inspect-clients');const refresh=document.querySelector('#readiness-refresh');inspect.click();setTimeout(()=>{const strict=!!inspect.disabled&&!!refresh.disabled;if(strict){resolve({strict:true,refreshEnabled:false,errorObserved:false});return;}const refreshEnabled=!refresh.disabled;if(refreshEnabled)refresh.click();let turns=0;const timer=setInterval(()=>{turns++;const state=document.querySelector('#readiness-summary')?.dataset.state;const message=(document.querySelector('#readiness-message')?.textContent||'').trim();if((state==='unavailable'&&message.length>0)||turns>=60){clearInterval(timer);resolve({strict:false,refreshEnabled,errorObserved:state==='unavailable'&&message.length>0});}},10);},50);})`)
	var interleave struct {
		Strict         bool `json:"strict"`
		RefreshEnabled bool `json:"refreshEnabled"`
		ErrorObserved  bool `json:"errorObserved"`
	}
	interleaveErr := json.Unmarshal(interleaving, &interleave)
	clearBytes(interleaving)
	select {
	case <-fixture.registration.started:
	case <-time.After(2 * time.Second):
		t.Fatal("fake client inspection did not enter its bounded delay")
	}
	fixture.registration.releaseOnce()
	readinessBrowserQAAwaitInspectEnabled(t, session)
	if interleaveErr != nil {
		t.Fatal("the in-flight refresh policy result was invalid")
	}
	if interleave.Strict {
		strictBytes := readinessBrowserQAEval(t, session, `() => ({inspectDisabled:!!document.querySelector('#readiness-inspect-clients')?.disabled,refreshDisabled:!!document.querySelector('#readiness-refresh')?.disabled})`)
		var disabled struct{ InspectDisabled, RefreshDisabled bool }
		disabledErr := json.Unmarshal(strictBytes, &disabled)
		clearBytes(strictBytes)
		if disabledErr != nil || !disabled.InspectDisabled || !disabled.RefreshDisabled || interleave.RefreshEnabled {
			t.Fatalf("single-flight policy booleans: inspect_disabled=%t refresh_disabled=%t refresh_was_enabled=%t", disabled.InspectDisabled, disabled.RefreshDisabled, interleave.RefreshEnabled)
		}
		fixture.state.failedPassive.Store(0)
		t.Log("inflight_policy=strict_single_flight")
	} else {
		if !interleave.RefreshEnabled || !interleave.ErrorObserved {
			t.Fatalf("overlap setup: refresh_enabled=%t newer_error_observed=%t", interleave.RefreshEnabled, interleave.ErrorObserved)
		}
		staleBytes := readinessBrowserQAEval(t, session, `() => ({errorState:document.querySelector('#readiness-summary')?.dataset.state==='unavailable',errorVisible:(document.querySelector('#readiness-message')?.textContent||'').trim().length>0,oldCheckStillVisible:!!document.querySelector('[data-manual="client-codex-listed"]')?.checked,oldNoTargetSnapshot:(document.querySelector('#readiness-target-list')?.textContent||'').includes('저장된 서비스 대상이 없습니다')})`)
		var stale struct {
			ErrorState           bool `json:"errorState"`
			ErrorVisible         bool `json:"errorVisible"`
			OldCheckStillVisible bool `json:"oldCheckStillVisible"`
			OldNoTargetSnapshot  bool `json:"oldNoTargetSnapshot"`
		}
		staleErr := json.Unmarshal(staleBytes, &stale)
		clearBytes(staleBytes)
		if staleErr != nil || !stale.ErrorState || !stale.ErrorVisible || !stale.OldCheckStillVisible || !stale.OldNoTargetSnapshot {
			t.Fatalf("stale response overwrote newer error/state: error_state=%t error_visible=%t old_check=%t old_snapshot=%t", stale.ErrorState, stale.ErrorVisible, stale.OldCheckStillVisible, stale.OldNoTargetSnapshot)
		}
		t.Log("inflight_policy=latest_request_wins; stale_response_preserved_newer_error=true")
	}

	readinessBrowserQAAction(t, session, "click", "#readiness-refresh")
	readinessBrowserQAAwaitSummary(t, session, "configured")
	configuredBytes := readinessBrowserQAEval(t, session, `() => {const targets=Array.from(document.querySelectorAll('#readiness-target-list > li'));const allLinks=Array.from(document.querySelectorAll('a[href]')).map(a=>a.getAttribute('href')||'');const pageText=document.body.textContent||'';const manuals=Array.from(document.querySelectorAll('[data-manual]'));return {configured:document.querySelector('#readiness-summary')?.dataset.state==='configured',targetCount:targets.length,historySuccess:targets.some(row=>row.textContent.includes('성공(과거 이력)')),historyFailure:targets.some(row=>row.textContent.includes('실패(과거 이력)')),manualRevisionReset:!document.querySelector('[data-manual="client-codex-listed"]')?.checked&&pageText.includes('저장 설정이 바뀌어 이전 확인 표시를 지웠습니다'),allManualChecksFalse:manuals.every(input=>!input.checked),connectionNotClaimed:pageText.includes('활성 앱 연결: 앱에서 검증하지 않음')&&pageText.includes('실제 도구 호출: 앱에서 검증하지 않음'),groupsLink:allLinks.includes('/#lah-pane-groups'),diagnosisLink:allLinks.includes('/#lah-pane-diagnosis'),sshLink:allLinks.includes('/#lah-pane-ssh'),draftLink:allLinks.some(href=>/draft|lah-pane-setup/i.test(href)),clientsLink:allLinks.includes('/#lah-pane-clients'),advancedLink:allLinks.includes('/#lah-pane-advanced'),featureCards:document.querySelectorAll('#readiness-feature-list .feature-card').length>0,sampleInstructions:document.querySelectorAll('#readiness-feature-list .feature-card .example').length>0,copyExamples:document.querySelectorAll('#readiness-feature-list .tool-copy').length>0,unknownRuntimeVisible:Array.from(document.querySelectorAll('#readiness-optional-list .readiness-item')).some(row=>row.textContent.includes('상태를 알 수 없음')),noOverflow:document.documentElement.scrollWidth<=document.documentElement.clientWidth}}`)
	var configured struct {
		Configured            bool `json:"configured"`
		TargetCount           int  `json:"targetCount"`
		HistorySuccess        bool `json:"historySuccess"`
		HistoryFailure        bool `json:"historyFailure"`
		ManualRevisionReset   bool `json:"manualRevisionReset"`
		AllManualChecksFalse  bool `json:"allManualChecksFalse"`
		ConnectionNotClaimed  bool `json:"connectionNotClaimed"`
		GroupsLink            bool `json:"groupsLink"`
		DiagnosisLink         bool `json:"diagnosisLink"`
		SSHLink               bool `json:"sshLink"`
		DraftLink             bool `json:"draftLink"`
		ClientsLink           bool `json:"clientsLink"`
		AdvancedLink          bool `json:"advancedLink"`
		FeatureCards          bool `json:"featureCards"`
		SampleInstructions    bool `json:"sampleInstructions"`
		CopyExamples          bool `json:"copyExamples"`
		UnknownRuntimeVisible bool `json:"unknownRuntimeVisible"`
		NoOverflow            bool `json:"noOverflow"`
	}
	configuredErr := json.Unmarshal(configuredBytes, &configured)
	clearBytes(configuredBytes)
	if configuredErr != nil || !configured.Configured || configured.TargetCount == 0 || !configured.HistorySuccess || !configured.HistoryFailure || !configured.ManualRevisionReset || !configured.AllManualChecksFalse || !configured.ConnectionNotClaimed || !configured.GroupsLink || !configured.DiagnosisLink || !configured.SSHLink || !configured.DraftLink || !configured.ClientsLink || !configured.AdvancedLink || !configured.FeatureCards || !configured.SampleInstructions || !configured.CopyExamples || !configured.UnknownRuntimeVisible || !configured.NoOverflow {
		t.Fatalf("configured readiness booleans: configured=%t targets=%d success_history=%t failure_history=%t revision_reset=%t checks_false=%t connection_unverified=%t links_groups=%t diagnosis=%t ssh=%t draft=%t clients=%t advanced=%t feature_cards=%t examples=%t copy=%t runtime_unknown=%t no_overflow=%t", configured.Configured, configured.TargetCount, configured.HistorySuccess, configured.HistoryFailure, configured.ManualRevisionReset, configured.AllManualChecksFalse, configured.ConnectionNotClaimed, configured.GroupsLink, configured.DiagnosisLink, configured.SSHLink, configured.DraftLink, configured.ClientsLink, configured.AdvancedLink, configured.FeatureCards, configured.SampleInstructions, configured.CopyExamples, configured.UnknownRuntimeVisible, configured.NoOverflow)
	}

	// A blocked localStorage write may fall back to tab-scoped storage. The
	// fixture blocks only this QA key and restores the native method immediately.
	monkeyBytes := readinessBrowserQAEval(t, session, `() => {const original=Storage.prototype.setItem;document.getElementById('readiness-copy-status').textContent='';Object.defineProperty(window,'__lahReadinessOriginalSetItem',{value:original,writable:true,configurable:true});Object.defineProperty(Storage.prototype,'setItem',{configurable:true,value:function(key,value){if(this===window.localStorage&&key==='lah-readiness-user-checks-v1')throw new Error('synthetic local storage failure');return original.call(this,key,value)}});return true}`)
	clearBytes(monkeyBytes)
	readinessBrowserQAAction(t, session, "click", `input[data-manual="client-codex-listed"]`)
	fallbackBytes := readinessBrowserQAEval(t, session, `() => {const key='lah-readiness-user-checks-v1';let sessionRecord=false,localAbsent=false;try{sessionRecord=!!sessionStorage.getItem(key);localAbsent=localStorage.getItem(key)===null}catch{}return {checkboxChecked:!!document.querySelector('[data-manual="client-codex-listed"]')?.checked,sessionFallback:sessionRecord&&localAbsent,sessionNotice:(document.querySelector('#readiness-copy-status')?.textContent||'').includes('이 탭에서만')}}`)
	var fallback struct {
		CheckboxChecked bool `json:"checkboxChecked"`
		SessionFallback bool `json:"sessionFallback"`
		SessionNotice   bool `json:"sessionNotice"`
	}
	fallbackErr := json.Unmarshal(fallbackBytes, &fallback)
	clearBytes(fallbackBytes)
	restoreBytes := readinessBrowserQAEval(t, session, `() => {const original=window.__lahReadinessOriginalSetItem;if(original)Object.defineProperty(Storage.prototype,'setItem',{configurable:true,writable:true,value:original});return true}`)
	clearBytes(restoreBytes)
	if fallbackErr != nil || !fallback.CheckboxChecked || !fallback.SessionFallback || !fallback.SessionNotice {
		t.Fatalf("manual storage fallback: checked=%t session_record_only=%t notice=%t", fallback.CheckboxChecked, fallback.SessionFallback, fallback.SessionNotice)
	}
	readinessBrowserQAAction(t, session, "click", "#readiness-clear-checks")
	clearBytesUI := readinessBrowserQAEval(t, session, `() => ({allUnchecked:Array.from(document.querySelectorAll('[data-manual]')).every(input=>!input.checked),localClear:localStorage.getItem('lah-readiness-user-checks-v1')===null,sessionClear:sessionStorage.getItem('lah-readiness-user-checks-v1')===null})`)
	var cleared struct{ AllUnchecked, LocalClear, SessionClear bool }
	clearErr := json.Unmarshal(clearBytesUI, &cleared)
	clearBytes(clearBytesUI)
	if clearErr != nil || !cleared.AllUnchecked || !cleared.LocalClear || !cleared.SessionClear {
		t.Fatalf("manual reset booleans: unchecked=%t local_clear=%t session_clear=%t", cleared.AllUnchecked, cleared.LocalClear, cleared.SessionClear)
	}

	// A racing inspection may be superseded or hit its own per-client timeout;
	// record entry IDs before any fake delay so it remains distinguishable from
	// the separate explicit action below.
	raceCalls := fixture.registration.callsSince(homePageInspectionBaseline)
	t.Logf("race_inspection_entries=%d ids=%s", len(raceCalls), readinessBrowserQASafeClientIDs(raceCalls))

	// This explicit button is the only allowed client-inspection path for the
	// fixed three-client behavior assertion.
	inspectionBefore := fixture.registrationCallCount()
	readinessBrowserQAAction(t, session, "click", "#readiness-inspect-clients")
	readinessBrowserQAAwaitInspectEnabled(t, session)
	clientBytes := readinessBrowserQAEval(t, session, `() => {const rows=Array.from(document.querySelectorAll('#readiness-client-list > li'));const states=rows.map(row=>row.querySelector('.item-state')?.textContent||'');const copy=rows.map(row=>row.querySelector('.item-copy')?.textContent||'');return {rowCount:rows.length,installedRendered:states[0]?.includes('CLI 발견됨'),installedGapRendered:states[1]?.includes('설치된 CLI를 이 확인에서 확인하지 못했습니다'),gapNotFalseAbsent:!states[1]?.includes('CLI를 찾지 못함'),unavailableRendered:states[2]?.includes('클라이언트 상태를 안전하게 확인하지 못했습니다'),connectionAndToolUnverified:copy.every(text=>text.includes('활성 앱 연결: 앱에서 검증하지 않음')&&text.includes('실제 도구 호출: 앱에서 검증하지 않음')),inspectionTimestampRendered:(document.querySelector('#readiness-client-inspection')?.textContent||'').includes('명시적으로 요청한 CLI 확인 시각')}}`)
	var clients struct {
		RowCount                    int  `json:"rowCount"`
		InstalledRendered           bool `json:"installedRendered"`
		InstalledGapRendered        bool `json:"installedGapRendered"`
		GapNotFalseAbsent           bool `json:"gapNotFalseAbsent"`
		UnavailableRendered         bool `json:"unavailableRendered"`
		ConnectionAndToolUnverified bool `json:"connectionAndToolUnverified"`
		InspectionTimestampRendered bool `json:"inspectionTimestampRendered"`
	}
	clientsErr := json.Unmarshal(clientBytes, &clients)
	clearBytes(clientBytes)
	if clientsErr != nil || clients.RowCount != 3 || !clients.InstalledRendered || !clients.InstalledGapRendered || !clients.GapNotFalseAbsent || !clients.UnavailableRendered || !clients.ConnectionAndToolUnverified || !clients.InspectionTimestampRendered {
		t.Fatalf("rendered client inspection booleans: rows=%d installed=%t observation_gap=%t gap_not_false_absent=%t unavailable=%t connection_unverified=%t explicit_timestamp=%t", clients.RowCount, clients.InstalledRendered, clients.InstalledGapRendered, clients.GapNotFalseAbsent, clients.UnavailableRendered, clients.ConnectionAndToolUnverified, clients.InspectionTimestampRendered)
	}
	inspectedIDs := fixture.registration.callsSince(inspectionBefore)
	wantIDs := readinessClientIDs()
	if !readinessBrowserQAClientIDsEqual(inspectedIDs, wantIDs) {
		t.Fatalf("one explicit client check entries=%d ids=%s, want fixed IDs=%s", len(inspectedIDs), readinessBrowserQASafeClientIDs(inspectedIDs), readinessBrowserQASafeClientIDs(wantIDs))
	}
	inspectionSummaryBytes := readinessBrowserQAEval(t, session, `() => {window.__lahReadinessInspectionSummary=document.querySelector('#readiness-client-inspection')?.textContent||'';return true}`)
	clearBytes(inspectionSummaryBytes)
	readinessBrowserQAAction(t, session, "click", "#readiness-refresh")
	readinessBrowserQAAwaitSummary(t, session, "configured")
	inspectionFreshnessBytes := readinessBrowserQAEval(t, session, `() => ({sameExplicitInspectionSummary:window.__lahReadinessInspectionSummary===(document.querySelector('#readiness-client-inspection')?.textContent||''),connectionUnverified:(document.body.textContent||'').includes('활성 앱 연결: 앱에서 검증하지 않음'),toolUnverified:(document.body.textContent||'').includes('실제 도구 호출: 앱에서 검증하지 않음')})`)
	var freshness struct {
		SameSummary          bool `json:"sameExplicitInspectionSummary"`
		ConnectionUnverified bool `json:"connectionUnverified"`
		ToolUnverified       bool `json:"toolUnverified"`
	}
	freshnessErr := json.Unmarshal(inspectionFreshnessBytes, &freshness)
	clearBytes(inspectionFreshnessBytes)
	if freshnessErr != nil || !freshness.SameSummary || !freshness.ConnectionUnverified || !freshness.ToolUnverified {
		t.Fatalf("regular refresh changed inspection scope/evidence: summary_stable=%t connection_unverified=%t tool_unverified=%t", freshness.SameSummary, freshness.ConnectionUnverified, freshness.ToolUnverified)
	}
	if fixture.registrationCallCount() != homePageInspectionBaseline+6 {
		t.Fatalf("ordinary refresh performed an unexpected client inspection delta=%d", fixture.registrationCallCount()-homePageInspectionBaseline)
	}

	cleanupBrowserQARunPlaywright(t, session, "close")
	browserOpen = false
	fixture.assertReadOnly(t, configPath)
	t.Logf("readiness_status_gets=%d runtime_status_gets=%d passive_gets=%d explicit_inspection_gets=%d observer_calls=%d fake_client_inspections=%d", fixture.state.statusGets.Load(), fixture.state.runtimeStatusGets.Load(), fixture.state.passiveGets.Load(), fixture.state.inspectionGets.Load(), fixture.state.observerCalls.Load(), fixture.registrationCallCount())
}

func readinessBrowserQAStartFixture(t *testing.T) (string, string, *readinessBrowserQAFixture) {
	t.Helper()
	privateDir := filepath.Join(t.TempDir(), "private")
	marker := filepath.Join(privateDir, "acl-marker")
	if err := writeSetupDraftQueueFileAtomically(marker, []byte("1")); err != nil {
		t.Fatal("could not create the current-user-protected browser fixture directory")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal("could not remove the private fixture ACL marker")
	}
	configPath := filepath.Join(privateDir, "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not write the isolated empty settings")
	}
	if err := cleanupBrowserQASecureRewrite(configPath, maxConfigSize); err != nil {
		t.Fatal("could not protect the isolated settings file")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not start the loopback-only readiness fixture")
	}
	host := listener.Addr().String()
	csrf, err := cleanupBrowserQARandomHex(32)
	if err != nil {
		_ = listener.Close()
		t.Fatal("could not initialize the isolated readiness app")
	}
	state := &readinessBrowserQAState{}
	registration := &readinessBrowserQARegistration{
		state:       state,
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		statusByID:  make(map[string]MCPClientStatus),
		errorClient: map[string]error{mcpClientIDGemini: errors.New("synthetic inspection unavailable")},
	}
	registration.statusByID[mcpClientIDCodex] = MCPClientStatus{ClientID: mcpClientIDCodex, Installed: mcpClientInstalled, Registration: mcpClientRegistrationRegistered, EvidenceSource: mcpClientEvidenceConfigObserved, Backup: mcpClientBackupNotCreated, Connection: mcpClientConnectionUnverified, ToolCall: mcpClientConnectionUnverified}
	registration.statusByID[mcpClientIDClaude] = MCPClientStatus{ClientID: mcpClientIDClaude, Installed: mcpClientInstalledNotObserved, Registration: mcpClientRegistrationNotObserved, EvidenceSource: mcpClientEvidenceNotObserved, Backup: mcpClientBackupNotCreated, Connection: mcpClientConnectionUnverified, ToolCall: mcpClientConnectionUnverified}
	state.registration = registration
	legacyFixtureState := &uiRenewalFixtureState{}
	app := &app{
		configPath:         configPath,
		secrets:            uiRenewalFixtureSecretStore{state: legacyFixtureState},
		namedSecrets:       uiRenewalFixtureNamedSecrets{state: legacyFixtureState},
		userEnvironment:    uiRenewalFixtureEnvironment{state: legacyFixtureState},
		client:             &http.Client{Transport: uiRenewalFixtureTransport{state: legacyFixtureState}},
		clientRegistration: registration,
		host:               host,
		csrf:               csrf,
	}
	server := &http.Server{Handler: readinessBrowserQAHandler(app, state), ReadHeaderTimeout: 5 * time.Second}
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
	return "http://" + host, configPath, &readinessBrowserQAFixture{state: state, registration: registration, legacyState: legacyFixtureState, initialBytes: mustReadReadinessQAConfig(t, configPath)}
}

type readinessBrowserQAFixture struct {
	state        *readinessBrowserQAState
	registration *readinessBrowserQARegistration
	legacyState  *uiRenewalFixtureState
	initialBytes []byte
	finalHash    [32]byte
}

func (f *readinessBrowserQAFixture) rememberFinalConfig(t *testing.T, path string) {
	t.Helper()
	data := mustReadReadinessQAConfig(t, path)
	f.finalHash = sha256.Sum256(data)
	clearBytes(data)
}

func (f *readinessBrowserQAFixture) assertReadOnly(t *testing.T, path string) {
	t.Helper()
	defer clearBytes(f.initialBytes)
	if f.state.mutatingRequests.Load() != 0 || f.state.registrationOps.Load() != 0 || f.state.externalCalls.Load() != 0 || f.legacyState.namedSecretSaves.Load() != 0 || f.legacyState.namedSecretDeletes.Load() != 0 || f.legacyState.environmentWrites.Load() != 0 || f.legacyState.secretStoreCalls.Load() != 0 || f.legacyState.outboundRequests.Load() != 0 {
		t.Fatal("readiness browser acceptance attempted a UI write, credential operation, client registration mutation, or outbound request")
	}
	wantObservations := f.state.statusGets.Load() - f.state.failedResponses.Load() + f.state.runtimeStatusGets.Load()
	if f.state.observerCalls.Load() != wantObservations {
		t.Fatalf("local observer call count=%d, expected readiness/runtime status GETs=%d", f.state.observerCalls.Load(), wantObservations)
	}
	data := mustReadReadinessQAConfig(t, path)
	defer clearBytes(data)
	if sha256.Sum256(data) != f.finalHash || f.finalHash == ([32]byte{}) {
		t.Fatal("the isolated config changed after its single test-owned revision update")
	}
}

func (r *readinessBrowserQARegistration) armSingleDelay() {
	r.blockNext.Store(true)
}

func (r *readinessBrowserQARegistration) releaseOnce() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

func (r *readinessBrowserQARegistration) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *readinessBrowserQARegistration) callsSince(index int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index > len(r.calls) {
		return nil
	}
	return append([]string(nil), r.calls[index:]...)
}

func readinessBrowserQAClientIDsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func readinessBrowserQASafeClientIDs(ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		switch id {
		case mcpClientIDCodex:
			labels = append(labels, "codex")
		case mcpClientIDClaude:
			labels = append(labels, "claude")
		case mcpClientIDGemini:
			labels = append(labels, "gemini")
		default:
			labels = append(labels, "unknown")
		}
	}
	return strings.Join(labels, ",")
}

func (f *readinessBrowserQAFixture) registrationCallCount() int {
	return f.registration.callCount()
}

func readinessBrowserQAFixtureID(prefix, digit string) string {
	return prefix + ":" + strings.Repeat(digit, 32)
}

func readinessBrowserQAConfiguredFixture() config {
	githubID := readinessBrowserQAFixtureID("github", "a")
	secondGitHubID := readinessBrowserQAFixtureID("github", "e")
	dashboardID := readinessBrowserQAFixtureID("dashboard", "c")
	serviceID := readinessBrowserQAFixtureID("service", "d")
	githubRef := "cred:" + strings.Repeat("1", 32)
	secondGitHubRef := "cred:" + strings.Repeat("2", 32)
	dashboardRef := "cred:" + strings.Repeat("3", 32)
	return config{
		Version: configVersion,
		GitHubTargets: []target{
			{ID: githubID, Name: "Synthetic GitHub success", Origin: "https://fixture.invalid", Repository: "sample/repository", SecretRef: githubRef},
			{ID: secondGitHubID, Name: "Synthetic GitHub failure", Origin: "https://fixture.invalid", Repository: "sample/other", SecretRef: secondGitHubRef},
		},
		DashboardTargets: []dashboardTarget{{ID: dashboardID, Name: "Synthetic dashboard", BaseURL: "https://dashboard.fixture.invalid", SecretRef: dashboardRef}},
		ServiceBundles: []serviceBundle{{
			ID: serviceID, Name: "Synthetic group", GitHubTargetIDs: []string{githubID, secondGitHubID},
			Environments: []serviceEnvironment{{Name: "sandbox", DashboardTargetID: dashboardID, DashboardNamespace: "demo", DashboardDeployment: "demo"}},
		}},
		ConnectionTests: map[string]connectionTest{
			githubID:       {Result: "success", CompletedAt: "2026-10-06T00:00:00Z"},
			secondGitHubID: {Result: "failure", CompletedAt: "2026-10-06T00:01:00Z"},
		},
	}
}

func mustReadReadinessQAConfig(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read the isolated settings fixture")
	}
	return data
}

func readinessBrowserQAEval(t *testing.T, session, expression string) []byte {
	t.Helper()
	return cleanupBrowserQARunPlaywright(t, session, "eval", expression, "--raw")
}

func readinessBrowserQAAction(t *testing.T, session string, args ...string) {
	t.Helper()
	output := cleanupBrowserQARunPlaywright(t, session, args...)
	clearBytes(output)
}

func readinessBrowserQAAwaitSummary(t *testing.T, session, want string) {
	t.Helper()
	expression := `() => new Promise(resolve=>{const end=Date.now()+8000;const check=()=>{const state=document.querySelector('#readiness-summary')?.dataset.state;if(state===` + strconv.Quote(want) + `){resolve(true);return;}if(Date.now()>=end){resolve(false);return;}setTimeout(check,20)};check();})`
	data := readinessBrowserQAEval(t, session, expression)
	var ready bool
	err := json.Unmarshal(data, &ready)
	clearBytes(data)
	if err != nil || !ready {
		t.Fatalf("readiness summary did not reach expected state %s", want)
	}
}

func readinessBrowserQAAwaitInspectEnabled(t *testing.T, session string) {
	t.Helper()
	data := readinessBrowserQAEval(t, session, `() => new Promise(resolve=>{const end=Date.now()+8000;const check=()=>{const button=document.querySelector('#readiness-inspect-clients');if(button&&!button.disabled){resolve(true);return;}if(Date.now()>=end){resolve(false);return;}setTimeout(check,20)};check();})`)
	var ready bool
	err := json.Unmarshal(data, &ready)
	clearBytes(data)
	if err != nil || !ready {
		t.Fatal("the explicit client inspection did not finish within the bounded UI wait")
	}
}

func readinessBrowserQAArtifactDir(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate the isolated readiness acceptance workspace")
	}
	for current := filepath.Clean(workingDir); ; current = filepath.Dir(current) {
		if filepath.Base(current) == "readiness-browser-qa-20261006" {
			return filepath.Join(current, "screenshots")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	t.Fatal("readiness test is outside its dedicated worker directory")
	return ""
}

func readinessBrowserQAInt(value string) int {
	result, _ := strconv.Atoi(value)
	return result
}
