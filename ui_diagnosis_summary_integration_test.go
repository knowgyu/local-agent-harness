package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestDashboardDiagnosisSummaryUIHookLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping dashboard summary UI integration fixture")
	}
	html, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	html = []byte(strings.ReplaceAll(strings.ReplaceAll(string(html), "\r\n", "\n"), "\r", "\n"))
	const marker = `<script nonce="{{.CSPNonce}}">`
	start := strings.Index(string(html), marker)
	if start < 0 {
		t.Fatal("inline UI script not found")
	}
	start += len(marker)
	const blockMarker = "const dashboardDiagnosisForm = document.getElementById('dashboard-diagnosis-form');"
	blockStart := strings.Index(string(html[start:]), blockMarker)
	if blockStart < 0 {
		t.Fatal("dashboard diagnosis UI block not found")
	}
	blockStart += start
	const blockEndMarker = "\n      }\n    </script>"
	blockEnd := strings.Index(string(html[blockStart:]), blockEndMarker)
	if blockEnd < 0 {
		t.Fatal("dashboard diagnosis UI block end not found")
	}
	script := string(html[blockStart : blockStart+blockEnd+len("\n      }")])

	fixture := `
const calls = [];
const listeners = {};
const diagnosisButton = {setAttribute() {}, removeAttribute() {}};
const diagnosisForm = {
  action: '/dashboard-diagnosis', attributes: new Set(),
  addEventListener(type, listener) { listeners.form = listener; },
  querySelector(selector) { return selector === 'button[type="submit"]' ? diagnosisButton : {value: 'fixture-csrf'}; },
  setAttribute(name) { this.attributes.add(name); },
  removeAttribute(name) { this.attributes.delete(name); }
};
const diagnosisSelection = {
  addEventListener(type, listener) { listeners.selection = listener; },
  selectedOptions: [{dataset: {serviceBundle: 'Environments', environment: 'qa-blue'}}]
};
const diagnosisStatus = {
  hidden: true, classList: {add() {}, remove() {}},
};
const diagnosisResult = {childNodes: []};
global.document = {getElementById(id) {
  return id === 'dashboard-diagnosis-form' ? diagnosisForm :
    id === 'dashboard-diagnosis-selection' ? diagnosisSelection :
    id === 'dashboard-diagnosis-status' ? diagnosisStatus :
    id === 'dashboard-diagnosis-result' ? diagnosisResult : null;
}};
global.LAHDashboardDiagnosisSummary = {
  clear() { calls.push('summary.clear'); },
  render(summary, container) {
    if (container !== diagnosisResult || summary.marker !== 'safe') throw new Error('safe summary received the wrong value or container');
    calls.push('summary.render');
  }
};
global.clearDynamicChildren = container => { calls.push('result.clear'); container.childNodes = []; };
global.setDynamic = (_element, key) => calls.push('status.' + key);
global.renderDashboardDiagnosis = (_result, container) => { calls.push('diagnosis.render'); container.childNodes = [{}]; };
global.fetch = async () => ({ok: true, json: async () => ({result: {}, summary: {marker: 'safe'}})});
eval(__SOURCE__);
(async () => {
  await listeners.form({preventDefault() {}});
  const success = calls.slice();
  const summaryIndex = success.lastIndexOf('summary.render');
  if (summaryIndex < 0 || success.indexOf('diagnosis.render') > summaryIndex) throw new Error('summary was not rendered after the diagnosis result');
  if (success.indexOf('summary.clear') < 0) throw new Error('new diagnosis did not clear the previous summary');

  calls.length = 0;
  listeners.selection();
  if (calls[0] !== 'summary.clear' || calls[1] !== 'result.clear') throw new Error('scope change did not clear the summary before removing result content');

  calls.length = 0;
  global.fetch = async () => ({ok: false, json: async () => ({})});
  await listeners.form({preventDefault() {}});
  if (calls.filter(value => value === 'summary.clear').length < 2) throw new Error('failed diagnosis did not clear both before submission and on rejection');
  if (calls.includes('summary.render')) throw new Error('failed diagnosis rendered a summary');
})().catch(error => { console.error(error.stack || error); process.exitCode = 1; });
`
	fixture = strings.Replace(fixture, "__SOURCE__", strconv.Quote(script), 1)
	command := exec.Command(node, "-e", fixture)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard summary UI lifecycle fixture failed: %v\n%s", err, output)
	}
}
