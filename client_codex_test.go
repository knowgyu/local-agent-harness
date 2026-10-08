package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAdapterPlanPreservesUnmanagedSettings(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	original := readCodexFixture(t)
	current := clientRegistrationConfig{exists: true, bytes: original}

	mutation, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), current)
	if err != nil {
		t.Fatalf("plan returned error: %v", err)
	}
	if len(mutation.changes) != 1 || mutation.changes[0] != mcpRegistrationChangeAdded {
		t.Fatalf("unexpected changes: %#v", mutation.changes)
	}
	if !strings.HasPrefix(string(mutation.config.bytes), string(original)) {
		t.Fatal("plan changed or removed the existing config bytes")
	}
	if !strings.Contains(string(mutation.config.bytes), "[mcp_servers.local-agent-harness]") {
		t.Fatal("plan did not add the managed Codex entry")
	}
	if !strings.Contains(string(mutation.config.bytes), `args = ["--mcp"]`) {
		t.Fatal("plan did not add the fixed stdio argument")
	}
	if len(mutation.config.bytes) > codexMaxConfigBytes {
		t.Fatal("plan exceeded the config size limit")
	}
}

func TestCodexAdapterPlanIsIdempotentForExactEntry(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	original := clientRegistrationConfig{exists: true, bytes: readCodexFixture(t)}
	first, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), original)
	if err != nil {
		t.Fatalf("first plan returned error: %v", err)
	}
	second, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), first.config)
	if err != nil {
		t.Fatalf("second plan returned error: %v", err)
	}
	if len(second.changes) != 1 || second.changes[0] != mcpRegistrationChangeUnchanged {
		t.Fatalf("second plan was not unchanged: %#v", second.changes)
	}
	if !sameCodexConfig(first.config, second.config) {
		t.Fatal("idempotent plan changed the config")
	}
}

func TestCodexAdapterPlanRejectsOccupiedName(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	occupied := "[mcp_servers.local-agent-harness]\ncommand = \"C:\\\\other\\\\harness.exe\"\nargs = [\"--mcp\"]\n"
	_, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{
		exists: true,
		bytes:  []byte(occupied),
	})
	if !errors.Is(err, errClientRegistrationNameConflict) {
		t.Fatalf("got %v, want name conflict", err)
	}
}

func TestCodexAdapterPlanRejectsNestedOccupiedName(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	occupied := "[mcp_servers.local-agent-harness.env]\nTOKEN = \"synthetic\"\n"
	_, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{
		exists: true,
		bytes:  []byte(occupied),
	})
	if !errors.Is(err, errClientRegistrationNameConflict) {
		t.Fatalf("got %v, want name conflict", err)
	}
}

func TestCodexAdapterRejectsDuplicateAssignmentsOutsideManagedTable(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	command, err := quoteTOMLBasicString(adapter.appExecutable)
	if err != nil {
		t.Fatalf("quote command path: %v", err)
	}
	duplicateSettings := strings.Join([]string{
		"[unmanaged]",
		"setting = 1",
		"setting = 2",
		"",
		"[mcp_servers.local-agent-harness]",
		"command = " + command,
		"args = [\"--mcp\"]",
	}, "\n")
	_, err = adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{
		exists: true,
		bytes:  []byte(duplicateSettings),
	})
	if !errors.Is(err, errCodexSettingsUnreadable) {
		t.Fatalf("duplicate unmanaged key returned %v, want safe parse failure", err)
	}
}

func TestCodexAdapterRejectsInlineTableAssignmentCollisions(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	command, err := quoteTOMLBasicString(adapter.appExecutable)
	if err != nil {
		t.Fatalf("quote command path: %v", err)
	}
	invalidValues := []string{
		`{ key = 1, key = 2 }`,
		`{ "key" = 1, key = 2 }`,
		`{ parent.child = 1, parent = 2 }`,
		`{ parent = 2, parent.child = 1 }`,
		`{ nested = { key = 1, key = 2 } }`,
	}
	for _, value := range invalidValues {
		config := strings.Join([]string{
			"[unmanaged]",
			"settings = " + value,
			"",
			"[mcp_servers.local-agent-harness]",
			"command = " + command,
			"args = [\"--mcp\"]",
		}, "\n")
		_, err := adapter.statusForConfig(clientRegistrationConfig{
			exists: true,
			bytes:  []byte(config),
		})
		if !errors.Is(err, errCodexSettingsUnreadable) {
			t.Errorf("inline-table value %s returned %v, want safe parse failure", value, err)
		}
	}
}

func TestCodexAdapterAcceptsDistinctInlineTableScopes(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	command, err := quoteTOMLBasicString(adapter.appExecutable)
	if err != nil {
		t.Fatalf("quote command path: %v", err)
	}
	config := strings.Join([]string{
		"[unmanaged]",
		"settings = { first = { key = 1 }, second = { key = 2 }, dotted.child = 3, dotted.sibling = 4 }",
		"",
		"[mcp_servers.local-agent-harness]",
		"command = " + command,
		"args = [\"--mcp\"]",
	}, "\n")
	status, err := adapter.statusForConfig(clientRegistrationConfig{
		exists: true,
		bytes:  []byte(config),
	})
	if err != nil {
		t.Fatalf("valid separate inline-table scopes returned error: %v", err)
	}
	if status.Registration != mcpClientRegistrationRegistered {
		t.Fatalf("valid managed entry was not recognized: %#v", status)
	}
}

func TestCodexAdapterAcceptsRepeatedArrayTableEntries(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	arrayTables := strings.Join([]string{
		"[[plugins]]",
		"name = \"first\"",
		"[plugins.metadata]",
		"enabled = true",
		"[[plugins]]",
		"name = \"second\"",
		"[plugins.metadata]",
		"enabled = false",
	}, "\n")
	mutation, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{
		exists: true,
		bytes:  []byte(arrayTables),
	})
	if err != nil {
		t.Fatalf("valid repeated array tables returned error: %v", err)
	}
	if !bytes.HasPrefix(mutation.config.bytes, []byte(arrayTables)) {
		t.Fatal("plan did not preserve valid unmanaged array tables")
	}
}

func TestCodexAdapterPlanRejectsInvalidSpecs(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	spec := codexRegistrationSpec(adapter)
	spec.DisplayName = "other-name"
	_, err := adapter.plan(context.Background(), spec, clientRegistrationConfig{})
	if !errors.Is(err, errClientRegistrationInvalidSpec) {
		t.Fatalf("custom name returned %v, want invalid spec", err)
	}

	spec = codexRegistrationSpec(adapter)
	spec.Transport = MCPTransportStreamableHTTP
	_, err = adapter.plan(context.Background(), spec, clientRegistrationConfig{})
	if !errors.Is(err, errClientRegistrationUnsupportedTransport) {
		t.Fatalf("HTTP transport returned %v, want unsupported transport", err)
	}
}

func TestCodexAdapterInspectAndVerifyReportConfigOnly(t *testing.T) {
	adapter, configPath, _, _ := newTestCodexAdapter(t, true)
	mutation, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{})
	if err != nil {
		t.Fatalf("plan returned error: %v", err)
	}
	if err := adapter.writeConfig(context.Background(), clientRegistrationConfig{}, mutation.config); err != nil {
		t.Fatalf("writeConfig returned error: %v", err)
	}

	snapshot, err := adapter.inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect returned error: %v", err)
	}
	if snapshot.status.Registration != "registered" || snapshot.status.EvidenceSource != "config_observed" {
		t.Fatalf("unexpected inspect status: %#v", snapshot.status)
	}
	statusOverclaimsEvidence := snapshot.status.Connection != "unverified" ||
		snapshot.status.ToolCall != "unverified" || snapshot.status.Version != ""
	if statusOverclaimsEvidence {
		t.Fatalf("inspect overclaimed evidence: %#v", snapshot.status)
	}
	verified, err := adapter.verify(context.Background())
	if err != nil {
		t.Fatalf("verify returned error: %v", err)
	}
	if verified.Registration != "registered" || verified.ToolCall != "unverified" {
		t.Fatalf("unexpected verify status: %#v", verified)
	}
	if configPath == "" {
		t.Fatal("test did not use a temporary profile path")
	}
}

func TestCodexAdapterAbsentCLIDoesNotReadSettings(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, err := newCodexClientRegistrationAdapterWithOptions(codexClientRegistrationAdapterOptions{
		configPath:    configPath,
		appExecutable: filepath.Join(dir, "harness.exe"),
	})
	if err != nil {
		t.Fatalf("constructor returned error: %v", err)
	}

	snapshot, err := adapter.inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect read settings despite absent CLI: %v", err)
	}
	cliNotObserved := snapshot.status.Installed == "installed_cli_not_observed"
	registrationNotObserved := snapshot.status.Registration == "not_observed"
	evidenceNotObserved := snapshot.status.EvidenceSource == "not_observed"
	if !cliNotObserved || !registrationNotObserved || !evidenceNotObserved {
		t.Fatalf("unexpected absent-CLI status: %#v", snapshot.status)
	}
	if snapshot.config.exists || len(snapshot.config.bytes) != 0 {
		t.Fatal("absent CLI snapshot exposed settings")
	}
	verified, err := adapter.verify(context.Background())
	if err != nil {
		t.Fatalf("verify read settings despite absent CLI: %v", err)
	}
	verifiedRegistrationNotObserved := verified.Registration == mcpClientRegistrationNotObserved
	verifiedEvidenceNotObserved := verified.EvidenceSource == mcpClientEvidenceNotObserved
	if !verifiedRegistrationNotObserved || !verifiedEvidenceNotObserved {
		t.Fatalf("unexpected absent-CLI verification status: %#v", verified)
	}
	_, err = adapter.plan(context.Background(), codexRegistrationSpec(adapter), clientRegistrationConfig{})
	if !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("plan returned %v, want client unavailable", err)
	}
}

func TestCodexConfigPathUsesOnlyAnExistingCODEXHOME(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("CODEX_HOME", profile)
	path, err := codexConfigPath()
	if err != nil {
		t.Fatalf("resolve existing CODEX_HOME: %v", err)
	}
	if path != filepath.Join(profile, codexConfigFileName) {
		t.Fatalf("config path = %q, want path under temporary profile", path)
	}

	missingProfile := filepath.Join(t.TempDir(), "missing")
	t.Setenv("CODEX_HOME", missingProfile)
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve user home: %v", err)
	}
	path, err = codexConfigPath()
	if err != nil {
		t.Fatalf("resolve fallback config path: %v", err)
	}
	want := filepath.Join(userHome, ".codex", codexConfigFileName)
	if path != want {
		t.Fatalf("config path = %q, want fallback path", path)
	}
}

func TestCodexAdapterWriteConfigUsesExactCASAndAtomicReplacement(t *testing.T) {
	adapter, configPath, _, _ := newTestCodexAdapter(t, true)
	original := []byte("# before\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := clientRegistrationConfig{exists: true, bytes: []byte("# after\n")}
	expected := clientRegistrationConfig{exists: true, bytes: append([]byte(nil), original...)}
	var err error

	if err := os.WriteFile(configPath, []byte("# changed externally\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = adapter.writeConfig(context.Background(), expected, replacement)
	if !errors.Is(err, errClientRegistrationStaleSettings) {
		t.Fatalf("stale write returned %v, want stale settings", err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "# changed externally\n" {
		t.Fatal("stale write replaced a newer config")
	}

	expected = clientRegistrationConfig{exists: true, bytes: current}
	if err := adapter.writeConfig(context.Background(), expected, replacement); err != nil {
		t.Fatalf("current write returned error: %v", err)
	}
	current, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(replacement.bytes) {
		t.Fatal("atomic replacement did not write the expected bytes")
	}
}

func TestCodexAdapterRejectsOversizedAndMalformedSettings(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	oversized := clientRegistrationConfig{exists: true, bytes: make([]byte, codexMaxConfigBytes+1)}
	_, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), oversized)
	if !errors.Is(err, errClientRegistrationConfigTooLarge) {
		t.Fatalf("oversized config returned %v, want too large", err)
	}
	malformedBytes := "[mcp_servers.local-agent-harness\ncommand = \"unterminated\n"
	malformed := clientRegistrationConfig{exists: true, bytes: []byte(malformedBytes)}
	_, err = adapter.plan(context.Background(), codexRegistrationSpec(adapter), malformed)
	if !errors.Is(err, errCodexSettingsUnreadable) {
		t.Fatalf("malformed config returned %v, want safe unreadable error", err)
	}
}

func TestCodexAdapterPublicStatusDoesNotExposeConfigBytes(t *testing.T) {
	adapter, _, _, _ := newTestCodexAdapter(t, true)
	canary := "synthetic-config-canary"
	config := clientRegistrationConfig{exists: true, bytes: []byte("# " + canary + "\n")}
	mutation, err := adapter.plan(context.Background(), codexRegistrationSpec(adapter), config)
	if err != nil {
		t.Fatalf("plan returned error: %v", err)
	}
	status, err := adapter.statusForConfig(mutation.config)
	if err != nil {
		t.Fatalf("status returned error: %v", err)
	}
	publicBytes, err := json.Marshal(struct {
		Status MCPClientStatus
		Plan   MCPClientRegistrationPlan
	}{
		Status: status,
		Plan:   MCPClientRegistrationPlan{ClientID: codexClientID, Changes: mutation.changes},
	})
	if err != nil {
		t.Fatal(err)
	}
	containsCanary := strings.Contains(string(publicBytes), canary)
	containsConfigPath := strings.Contains(string(publicBytes), adapter.configPath)
	containsAppPath := strings.Contains(string(publicBytes), adapter.appExecutable)
	if containsCanary || containsConfigPath || containsAppPath {
		t.Fatal("public DTO contained config data or a local path")
	}
	if len(mutation.changes) != 1 || mutation.changes[0] != mcpRegistrationChangeAdded {
		t.Fatalf("unexpected public change codes: %#v", mutation.changes)
	}
}

func TestCodexAdapterControllerBackupApplyAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}

	appExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codexExecutable := filepath.Join(root, "codex.cmd")
	if err := os.WriteFile(codexExecutable, []byte("synthetic CLI fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(profile, codexConfigFileName)
	original := readCodexFixture(t)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := newCodexClientRegistrationAdapterWithOptions(codexClientRegistrationAdapterOptions{
		configPath:      configPath,
		appExecutable:   appExecutable,
		codexExecutable: codexExecutable,
	})
	if err != nil {
		t.Fatalf("adapter constructor returned error: %v", err)
	}
	backupStore, err := newClientRegistrationBackupStoreAt(filepath.Join(root, "backups"))
	if err != nil {
		t.Fatalf("backup store constructor returned error: %v", err)
	}
	controller, err := newMCPClientRegistrationController(mcpClientRegistrationControllerOptions{
		adapters:    []clientRegistrationAdapter{adapter},
		backupStore: backupStore,
	})
	if err != nil {
		t.Fatalf("controller constructor returned error: %v", err)
	}
	spec := MCPRegistrationSpec{
		ClientID:    codexClientID,
		DisplayName: codexManagedServerName,
		Transport:   MCPTransportStdio,
		Command:     appExecutable,
		Args:        []string{"--mcp"},
		Scope:       []string{"user"},
	}

	plan, err := controller.PlanClientRegistration(ctx, spec)
	if err != nil {
		t.Fatalf("plan returned error: %v", err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0] != mcpRegistrationChangeAdded {
		t.Fatalf("unexpected plan changes: %#v", plan.Changes)
	}
	receipt, err := controller.BackupClientRegistration(ctx, codexClientID)
	if err != nil {
		t.Fatalf("backup returned error: %v", err)
	}
	status, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision)
	if err != nil {
		t.Fatalf("apply returned error: %v", err)
	}
	if status.Registration != mcpClientRegistrationRegistered || status.Backup != mcpClientBackupApplied {
		t.Fatalf("unexpected applied status: %#v", status)
	}
	if status.Connection != mcpClientConnectionUnverified || status.ToolCall != mcpClientConnectionUnverified {
		t.Fatalf("apply overclaimed runtime evidence: %#v", status)
	}

	applied, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(applied, original) || !bytes.Contains(applied, []byte("[mcp_servers.local-agent-harness]")) {
		t.Fatal("apply failed to preserve existing settings and add the managed entry")
	}
	if _, err := controller.ApplyClientRegistration(ctx, plan.ID, plan.BaseRevision); err != nil {
		t.Fatalf("idempotent apply returned error: %v", err)
	}
	appliedAgain, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(applied, appliedAgain) {
		t.Fatal("idempotent apply changed the Codex settings")
	}

	status, err = controller.RestoreClientRegistration(ctx, codexClientID, receipt.ID)
	if err != nil {
		t.Fatalf("restore returned error: %v", err)
	}
	if status.Registration != mcpClientRegistrationNotRegistered || status.Backup != mcpClientBackupRestored {
		t.Fatalf("unexpected restored status: %#v", status)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatal("restore did not return the exact original Codex settings")
	}
}

func newTestCodexAdapter(t *testing.T, installed bool) (*codexClientRegistrationAdapter, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	appExecutable := filepath.Join(dir, "local-agent-harness.exe")
	if err := os.WriteFile(appExecutable, []byte("synthetic executable fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	codexExecutable := ""
	if installed {
		codexExecutable = filepath.Join(dir, "codex.cmd")
		if err := os.WriteFile(codexExecutable, []byte("synthetic CLI fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(profile, codexConfigFileName)
	adapter, err := newCodexClientRegistrationAdapterWithOptions(codexClientRegistrationAdapterOptions{
		configPath:      configPath,
		appExecutable:   appExecutable,
		codexExecutable: codexExecutable,
	})
	if err != nil {
		t.Fatalf("constructor returned error: %v", err)
	}
	return adapter, configPath, appExecutable, codexExecutable
}

func codexRegistrationSpec(adapter *codexClientRegistrationAdapter) MCPRegistrationSpec {
	return MCPRegistrationSpec{
		ClientID:    codexClientID,
		DisplayName: codexManagedServerName,
		Transport:   MCPTransportStdio,
		Command:     adapter.appExecutable,
		Args:        []string{"--mcp"},
		Scope:       []string{"user"},
	}
}

func readCodexFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "client_codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
