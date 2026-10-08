package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFileImporterHandlersUseGenerationGuard(t *testing.T) {
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	for _, importer := range []struct {
		name      string
		requestID string
		nextName  string
	}{
		{name: "gitImportForm", requestID: "gitImportRequest", nextName: "runbookImportForm"},
		{name: "runbookImportForm", requestID: "runbookImportRequest", nextName: "sshImportForm"},
		{name: "sshImportForm", requestID: "sshImportRequest", nextName: "settingsImportForm"},
		{name: "settingsImportForm", requestID: "settingsImportRequest", nextName: "jsonRemoteImportForm"},
	} {
		start := strings.Index(pageText, "const "+importer.name+" = document.getElementById(")
		if start < 0 {
			t.Fatalf("%s is missing", importer.name)
		}
		end := strings.Index(pageText[start:], "const "+importer.nextName+" = document.getElementById(")
		if end < 0 {
			t.Fatalf("%s handler boundary is missing", importer.name)
		}
		handler := pageText[start : start+end]
		if !strings.Contains(handler, "const "+importer.requestID+" = bindSelectedFilePreview(") ||
			!strings.Contains(handler, "if (!"+importer.requestID+".isCurrent(requestGeneration)) return;") ||
			!strings.Contains(handler, importer.requestID+".finish(requestGeneration)") {
			t.Errorf("%s does not invalidate stale preview responses", importer.name)
		}
	}
}

func TestSelectedFilePreviewInvalidatesPendingRequest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless selected-file preview fixture")
	}
	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	start := strings.Index(pageText, "      function bindSelectedFilePreview(")
	if start < 0 {
		t.Fatal("selected-file preview guard is missing from ui.html")
	}
	end := strings.Index(pageText[start:], "\n      function renderDashboardDiagnosis(")
	if end < 0 {
		t.Fatal("selected-file preview guard boundary is missing")
	}
	guard := pageText[start : start+end]
	fixture := `
const preview = {};
const status = {};
const submitButton = {disabled: false};
const listeners = new Map();
const input = {addEventListener(name, listener) { listeners.set(name, listener); }};
function clearDynamicChildren(node) { node.cleared = true; }
function setDynamic(node, key) { node.key = key; }
`
	checks := `
const state = bindSelectedFilePreview(input, status, preview, submitButton);
const previousRequest = state.begin();
submitButton.disabled = true;
listeners.get('change')();
if (state.isCurrent(previousRequest)) throw new Error('selection change left a request current');
if (submitButton.disabled) throw new Error('selection change left the submit button disabled');
if (!preview.cleared || status.key !== 'importSelectionChanged') throw new Error('selection change did not clear and explain the old preview');
submitButton.disabled = true;
state.finish(previousRequest);
if (!submitButton.disabled) throw new Error('stale request re-enabled the submit button');
const currentRequest = state.begin();
state.finish(currentRequest);
if (submitButton.disabled) throw new Error('current request did not re-enable the submit button');
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(fixture + guard + checks)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("selected-file preview fixture failed: %v\n%s", err, output)
	}
}
