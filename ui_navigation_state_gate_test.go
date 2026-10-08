package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestUIShellPreservesSecretsFragmentFailClosedGate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping the bounded rendered-fragment gate fixture")
	}

	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	page := uiNavigationRenderRootPage(t, &app{configPath: configPath, csrf: "state-gate-test-csrf"})
	fragmentScript := strings.ReplaceAll(uiNavigationScriptContaining(t, page, "window.LAHSecretsEnvironment = Object.freeze"), "\r\n", "\n")
	shellScript := strings.ReplaceAll(uiNavigationScriptWithID(t, page, "lah-shell-navigation-script"), "\r\n", "\n")

	validators := uiNavigationExtractSection(t, fragmentScript, "  const validID =", "  const language =", false)
	renderFunction := uiNavigationExtractSection(t, fragmentScript, "  function render(secrets,", "  function clear()", false)
	clearFunction := uiNavigationExtractSection(t, fragmentScript, "  function clear()", "  window.LAHSecretsEnvironment =", false)
	apiExport := uiNavigationExtractSection(t, fragmentScript, "  window.LAHSecretsEnvironment = Object.freeze", "\n  if (root.dataset.available === 'true')", false)
	fragmentInit := uiNavigationExtractSection(t, fragmentScript, "  if (root.dataset.available === 'true') {", "  if (typeof MutationObserver === 'function')", false)
	setSecretView := uiNavigationExtractSection(t, shellScript, "      function setSecretView(key) {", "\n      }\n", true)

	secretsNotice := uiNavigationElementByID(t, page, "lah-secrets-unavailable", "p")
	environmentNotice := uiNavigationElementByID(t, page, "lah-environment-unavailable", "p")
	secretsNoticeRole := uiNavigationAttributeValue(uiNavigationOpeningTagByID(t, page, "lah-secrets-unavailable"), "role")
	environmentNoticeRole := uiNavigationAttributeValue(uiNavigationOpeningTagByID(t, page, "lah-environment-unavailable"), "role")

	type fixtureCase struct {
		Name                  string `json:"name"`
		Available             string `json:"available"`
		Secrets               string `json:"secrets"`
		Environment           string `json:"environment"`
		Cleanup               string `json:"cleanup"`
		Route                 string `json:"route"`
		RunFragment           bool   `json:"run_fragment"`
		ValidEmpty            bool   `json:"valid_empty"`
		SecretNoticeText      string `json:"secret_notice_text"`
		EnvironmentNoticeText string `json:"environment_notice_text"`
		SecretNoticeRole      string `json:"secret_notice_role"`
		EnvironmentNoticeRole string `json:"environment_notice_role"`
	}
	var cases []fixtureCase
	for _, input := range []fixtureCase{
		{Name: "availability false", Available: "false", Secrets: "[]", Environment: "[]", Cleanup: `{"status":"unavailable","pending_ids":[]}`, RunFragment: true},
		{Name: "malformed secret JSON", Available: "true", Secrets: "{broken", Environment: "[]", Cleanup: `{"status":"unavailable","pending_ids":[]}`, RunFragment: true},
		{Name: "malformed environment list", Available: "true", Secrets: "[]", Environment: `{"name":"not-a-list"}`, Cleanup: `{"status":"unavailable","pending_ids":[]}`, RunFragment: true},
		{Name: "malformed cleanup state", Available: "true", Secrets: "[]", Environment: "[]", Cleanup: `{"status":"available","pending_ids":["secret:invalid"]}`, RunFragment: true},
		{Name: "fragment script unavailable", Available: "true", Secrets: "[]", Environment: "[]", Cleanup: `{"status":"unavailable","pending_ids":[]}`, RunFragment: false},
		{Name: "valid empty lists", Available: "true", Secrets: "[]", Environment: "[]", Cleanup: `{"status":"unavailable","pending_ids":[]}`, RunFragment: true, ValidEmpty: true},
	} {
		for _, route := range []string{"secrets", "environment"} {
			input.Route = route
			input.SecretNoticeText = uiNavigationTextContent(secretsNotice)
			input.EnvironmentNoticeText = uiNavigationTextContent(environmentNotice)
			input.SecretNoticeRole = secretsNoticeRole
			input.EnvironmentNoticeRole = environmentNoticeRole
			cases = append(cases, input)
		}
	}

	encodedCases, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	encodedValidators, _ := json.Marshal(validators)
	encodedRender, _ := json.Marshal(renderFunction)
	encodedClear, _ := json.Marshal(clearFunction)
	encodedExport, _ := json.Marshal(apiExport)
	encodedInit, _ := json.Marshal(fragmentInit)
	encodedSetSecretView, _ := json.Marshal(setSecretView)

	program := "const vm=require('node:vm');\n" +
		"const cases=" + string(encodedCases) + ";\n" +
		"const validators=" + string(encodedValidators) + ",renderFunction=" + string(encodedRender) + ",clearFunction=" + string(encodedClear) + ";\n" +
		"const apiExport=" + string(encodedExport) + ",fragmentInit=" + string(encodedInit) + ";\n" +
		"const setSecretViewSource=" + string(encodedSetSecretView) + ";\n" +
		uiNavigationStateGateNodeFixture

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-")
	command.Stdin = strings.NewReader(program)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("rendered production fragment/state-gate fixture failed: %v\n%s", err, output)
	}
}

func uiNavigationScriptContaining(t *testing.T, page, token string) string {
	t.Helper()
	for _, match := range regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(page, -1) {
		if len(match) == 3 && strings.Contains(match[2], token) {
			return match[2]
		}
	}
	t.Fatalf("rendered page has no script containing %q", token)
	return ""
}

func uiNavigationScriptWithID(t *testing.T, page, id string) string {
	t.Helper()
	for _, match := range regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(page, -1) {
		if len(match) == 3 && uiNavigationAttributeValue(match[1], "id") == id {
			return match[2]
		}
	}
	t.Fatalf("rendered page has no script with id %q", id)
	return ""
}

const uiNavigationStateGateNodeFixture = `
class Element {
  constructor() { this.hidden=false; this.inert=false; this.value=''; this.dataset={}; this.children=[]; this.parentElement=null; }
  replaceChildren(...children) { for (const child of this.children) child.parentElement=null; this.children=[]; this.append(...children); }
  append(...children) { for (const child of children) { if (child.parentElement) child.parentElement.children=child.parentElement.children.filter(value=>value!==child); child.parentElement=this; this.children.push(child); } }
  reset() {}
  contains(node) { return node===this || this.children.some(child=>child.contains(node)); }
}
function assert(value, message) { if (!value) throw new Error(message); }
function effectiveHidden(node) { for (let current=node; current; current=current.parentElement) if (current.hidden || current.inert) return true; return false; }
function makeGlobals(state) {
  const root=new Element(); root.dataset={available:state.available,namedSecrets:state.secrets,userEnvironment:state.environment,namedSecretCleanup:state.cleanup};
  const namedSecrets=new Element(), userEnvironment=new Element(), secretForm=new Element(), environmentForm=new Element();
  root.append(namedSecrets,userEnvironment); namedSecrets.append(secretForm); userEnvironment.append(environmentForm);
  const secretRows=new Element(), envRows=new Element(), secretsPaneContent=new Element(), environmentPaneContent=new Element();
  const secretsUnavailable=new Element(), environmentUnavailable=new Element();
  secretsUnavailable.hidden=true; environmentUnavailable.hidden=true;
  secretsUnavailable.textContent=state.secret_notice_text; environmentUnavailable.textContent=state.environment_notice_text;
  const secretsUnavailableNode={hidden:true,attrs:{role:state.secret_notice_role},textContent:state.secret_notice_text};
  const environmentUnavailableNode={hidden:true,attrs:{role:state.environment_notice_role},textContent:state.environment_notice_text};
	return {root,secretsRoot:root,namedSecrets,userEnvironment,secretForm,envForm:environmentForm,environmentForm,secretRows,envRows,secretID:new Element(),secretValue:new Element(),envValue:new Element(),
    secretsPaneContent,environmentPaneContent,secretsUnavailable:secretsUnavailableNode,environmentUnavailable:environmentUnavailableNode,window:{}};
}
for (const state of cases) {
  const globals=makeGlobals(state), context=vm.createContext(globals);
  if (state.run_fragment) {
    const fragmentProgram='let currentSecrets=[],currentEnvironment=[],currentCleanupState={status:"unavailable",pending_ids:[]},lastMessage="",lastMessageKind="";\n' +
      'function applyStrings(){} function renderPickers(){} function attachTargetSecretPicker(){}\n' +
      'function rerender(){secretRows.replaceChildren(...currentSecrets);envRows.replaceChildren(...currentEnvironment);}\n' +
      validators+'\n'+renderFunction+'\n'+clearFunction+'\n'+
      'const attachSecretPicker=()=>{};const createSecretPicker=()=>{};\n'+apiExport+'\n'+fragmentInit;
    vm.runInContext(fragmentProgram,context,{timeout:2000});
  }
  vm.runInContext(setSecretViewSource+'\nsetSecretView("environment");',context,{timeout:2000});
  const parkedInEnvironment=globals.root.parentElement===globals.environmentPaneContent;
  const fragmentHiddenBeforeRoute=globals.root.hidden;
  vm.runInContext('setSecretView('+JSON.stringify(state.route)+');',context,{timeout:2000});
  assert(parkedInEnvironment,state.name+': initial shell placement did not park the fragment in its environment destination');
  assert(globals.root.parentElement===(state.route==='secrets'?globals.secretsPaneContent:globals.environmentPaneContent),state.name+'/'+state.route+': fragment did not move to the selected destination');
  assert(globals.secretsUnavailable.attrs.role==='status'&&globals.environmentUnavailable.attrs.role==='status',state.name+': unavailable notices are not status messages');
  if (state.valid_empty) {
    assert(!fragmentHiddenBeforeRoute&&!globals.root.hidden,state.name+'/'+state.route+': valid empty metadata was hidden');
    assert(!effectiveHidden(state.route==='secrets'?globals.secretForm:globals.environmentForm),state.name+'/'+state.route+': selected valid form is inaccessible');
    assert(effectiveHidden(state.route==='secrets'?globals.environmentForm:globals.secretForm),state.name+'/'+state.route+': inactive valid form is still accessible');
    assert(globals.secretsUnavailable.hidden&&globals.environmentUnavailable.hidden,state.name+': valid data displayed an unavailable notice');
  } else {
    assert(globals.root.hidden,state.name+'/'+state.route+': invalid/unavailable data was reopened during shell routing');
    assert(effectiveHidden(globals.secretForm)&&effectiveHidden(globals.environmentForm),state.name+'/'+state.route+': invalid settings form remained accessible');
    assert(globals.secretRows.children.length===0&&globals.envRows.children.length===0,state.name+': invalid metadata looked like an empty saved list');
    assert(globals.secretsUnavailable.hidden===(state.route!=='secrets')&&globals.environmentUnavailable.hidden===(state.route!=='environment'),state.name+'/'+state.route+': unavailable notice did not follow selected pane');
    assert(globals.secretsUnavailable.textContent===state.secret_notice_text&&globals.environmentUnavailable.textContent===state.environment_notice_text,state.name+': fixed unavailable text changed');
  }
}
process.stdout.write('rendered fragment fail-closed cases passed');
`
