package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

type fakeGatewayTokenCredentialProvider struct {
	value []byte
}

func (f *fakeGatewayTokenCredentialProvider) LoadGatewayToken() ([]byte, error) {
	return append([]byte(nil), f.value...), nil
}

func (f *fakeGatewayTokenCredentialProvider) SaveGatewayToken(value []byte) error {
	f.value = append([]byte(nil), value...)
	return nil
}

func (f *fakeGatewayTokenCredentialProvider) DeleteGatewayToken() error {
	f.value = nil
	return nil
}

var _ gatewayTokenCredentialProvider = (*fakeGatewayTokenCredentialProvider)(nil)

func TestParallelContractRevisionMatchesLockFile(t *testing.T) {
	contents, err := os.ReadFile("docs/parallel/contracts.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Revision string `json:"revision"`
		Owners   struct {
			Persistence string `json:"persistence_schema_and_migration_semantics"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(contents, &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Revision != parallelContractRevision || lock.Owners.Persistence != "B01" {
		t.Fatalf(
			"contract lock does not match code revision or B01 schema ownership: revision=%q owner=%q",
			lock.Revision,
			lock.Owners.Persistence,
		)
	}
}

func TestParallelContractJSONDoesNotExposeSecretInputs(t *testing.T) {
	const canary = "a00-secret-canary-value"

	secretRequest, err := json.Marshal(NamedSecretWrite{
		ID:    "secret-id",
		Name:  "build token",
		Value: []byte(canary),
	})
	if err != nil {
		t.Fatal(err)
	}
	environmentRequest, err := json.Marshal(UserEnvironmentWrite{
		Name:  "SERVICE_TOKEN",
		Value: []byte(canary),
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"named secret input":     secretRequest,
		"user environment input": environmentRequest,
	} {
		if bytes.Contains(encoded, []byte(canary)) || bytes.Contains(encoded, []byte(`"value"`)) {
			t.Errorf("%s serialized its raw value: %s", name, encoded)
		}
	}
}

func TestGatewayTokenCredentialTargetIsReservedAndStable(t *testing.T) {
	if gatewayCredentialTargetName != "LocalAgentHarness/MCPGateway/token-v1" {
		t.Fatalf("gateway lookup name changed: %q", gatewayCredentialTargetName)
	}
	if strings.HasPrefix(gatewayCredentialTargetName, "LocalAgentHarness/cred:") {
		t.Fatalf("gateway token shares the service-secret target namespace: %q", gatewayCredentialTargetName)
	}
}

func TestGatewayTokenProviderIsInjectable(t *testing.T) {
	provider := &fakeGatewayTokenCredentialProvider{}
	if err := provider.SaveGatewayToken([]byte("synthetic-token")); err != nil {
		t.Fatal(err)
	}
	loaded, err := provider.LoadGatewayToken()
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded) != "synthetic-token" {
		t.Fatal("fake provider did not return the value it stored")
	}
	if err := provider.DeleteGatewayToken(); err != nil {
		t.Fatal(err)
	}
	loaded, err = provider.LoadGatewayToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatal("fake provider did not clear its value")
	}
}

func TestReservedRouteAndToolNamesAreUnique(t *testing.T) {
	routes := []string{
		uiRouteSaveNamedSecret,
		uiRouteDeleteNamedSecret,
		uiRouteSaveUserEnvironment,
		uiRouteDeleteUserEnvironment,
		uiRouteSaveSSHTarget,
		uiRouteDeleteSSHTarget,
		uiRouteSaveSSHOperation,
		uiRouteDeleteSSHOperation,
		uiRouteReviewSetupDraft,
		uiRouteApproveSetupDraft,
		uiRouteRejectSetupDraft,
		uiRouteRuntimeStatus,
		uiRouteSetLoginStartup,
		uiRouteClientPlan,
		uiRouteClientBackup,
		uiRouteClientApply,
		uiRouteClientVerify,
		uiRouteClientRestore,
	}
	tools := []string{
		mcpToolRegisteredSSHTargets,
		mcpToolRunSSHOperation,
		mcpToolSubmitSetupDraft,
	}
	if hasDuplicateString(routes) || hasDuplicateString(tools) {
		t.Fatal("parallel route or MCP tool name is duplicated")
	}
	if routes[0] != "/save-named-secret" || tools[2] != "submit_setup_draft" {
		t.Fatal("reserved route or tool contract changed without a revision update")
	}
}

func TestSSHRunAndClientRegistrationInputsHaveNoCredentialFields(t *testing.T) {
	sshInput, err := json.Marshal(SSHOperationInput{
		TargetID:    "ssh-target-id",
		OperationID: "read-deployments",
		Parameters:  map[string]json.RawMessage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := json.Marshal(MCPRegistrationSpec{
		ClientID:    "codex",
		DisplayName: "Local Agent Harness",
		Transport:   MCPTransportStdio,
		Args:        []string{},
		Scope:       []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"SSH execution":       sshInput,
		"client registration": registration,
	} {
		for _, forbidden := range []string{`"host"`, `"endpoint"`, `"command"`, `"token"`, `"credential_ref"`, `"secret"`} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Errorf("%s unexpectedly contains %s: %s", name, forbidden, encoded)
			}
		}
	}
}

func TestRootTemplateRendersEmbeddedFeaturePartials(t *testing.T) {
	var output bytes.Buffer
	if err := page.ExecuteTemplate(&output, "ui.html", pageData{}); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	if !strings.Contains(html, `data-lah-ui-fragment-anchor="a00"`) {
		t.Fatalf(
			"root template did not render the embedded partial anchor; names=%v tail=%q",
			uiFragmentTemplateNames(page),
			html[max(0, len(html)-180):],
		)
	}
	for _, id := range []string{
		"runtime-clients",
		"lah-secrets-environment",
		"ssh-settings-entry",
		"dashboard-diagnosis-safe-summary",
	} {
		if count := strings.Count(html, `id="`+id+`"`); count != 1 {
			t.Errorf("root page rendered fragment region %q %d times, want exactly once", id, count)
		}
	}
}

func TestFeaturePartialEscapesDynamicData(t *testing.T) {
	files := fstest.MapFS{
		"ui.html": &fstest.MapFile{Data: []byte(`{{renderUIFragments .}}`)},
		"ui_fragments/test.html": &fstest.MapFile{
			Data: []byte(`{{define "lah-ui-fragment-test"}}<span>{{.}}</span>{{end}}`),
		},
	}
	parsed, err := parseUIPage(files)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := parsed.ExecuteTemplate(&output, "ui.html", `<script>alert(1)</script>`); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `<script>`) || !strings.Contains(output.String(), `&lt;script&gt;`) {
		t.Fatalf("feature partial did not escape dynamic input: %q", output.String())
	}
}

func hasDuplicateString(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
