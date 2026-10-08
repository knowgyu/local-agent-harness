package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeminiCLISettingsPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "synthetic-home")
	path, err := geminiCLISettingsPath(home)
	if err != nil {
		t.Fatal(err)
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(absoluteHome, ".gemini", "settings.json")
	if path != want {
		t.Fatalf("settings path mismatch: got %q, want %q", path, want)
	}
}

func TestGeminiCLIAdapterUnobservedWhenCLIIsMissing(t *testing.T) {
	configPath := t.TempDir()
	adapter := newGeminiCLIRegistrationAdapterForPath(
		configPath,
		filepath.Join(t.TempDir(), "lah-test.exe"),
		func(string) (string, error) { return "", errors.New("synthetic not found") },
	)

	snapshot, err := adapter.inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := geminiCLIUnobservedStatus()
	if snapshot.status != want {
		t.Fatalf("unobserved status mismatch: got %#v, want %#v", snapshot.status, want)
	}
	if snapshot.config.exists || len(snapshot.config.bytes) != 0 {
		t.Fatal("settings were read although the CLI was not observed")
	}

	spec := geminiTestRegistrationSpec(adapter)
	if _, err := adapter.plan(context.Background(), spec, clientRegistrationConfig{}); !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("plan error = %v, want client unavailable", err)
	}
	if err := adapter.writeConfig(context.Background(), clientRegistrationConfig{}, clientRegistrationConfig{}); !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("write error = %v, want client unavailable", err)
	}
}

func TestGeminiCLIAdapterMissingSettingsIsObservedWithoutCreatingIt(t *testing.T) {
	adapter, configPath := newGeminiTestAdapter(t)
	status, err := adapter.verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Registration != "not_registered" || status.EvidenceSource != "config_observed" {
		t.Fatalf("unexpected empty-settings state: %#v", status)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verify created the user settings file: %v", err)
	}
}

func TestGeminiCLIAdapterPlanPreservesUnmanagedSettingsAndWritesUserRegistration(t *testing.T) {
	adapter, configPath := newGeminiTestAdapter(t)
	fixture, err := os.ReadFile(filepath.Join("testdata", "client_gemini", "settings-base.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	expected := clientRegistrationConfig{exists: true, bytes: fixture}

	mutation, err := adapter.plan(context.Background(), geminiTestRegistrationSpec(adapter), expected)
	if err != nil {
		t.Fatal(err)
	}
	if !sameGeminiStrings(mutation.changes, []string{mcpRegistrationChangeAdded}) {
		t.Fatalf("changes = %#v, want registration_added", mutation.changes)
	}
	if !mutation.config.exists || len(mutation.config.bytes) == 0 {
		t.Fatal("plan did not produce settings")
	}
	assertGeminiUnmanagedSettingsPreserved(t, fixture, mutation.config.bytes)
	assertGeminiRegistrationEntry(t, mutation.config.bytes, adapter.command)

	if err := adapter.writeConfig(context.Background(), expected, mutation.config); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(mutation.config.bytes) {
		t.Fatal("settings bytes differ from planned replacement")
	}

	status, err := adapter.verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Registration != "registered" || status.Connection != "unverified" || status.ToolCall != "unverified" {
		t.Fatalf("unexpected verification state: %#v", status)
	}
	if status.Version != "" || status.EvidenceSource != "config_observed" {
		t.Fatalf("unexpected version or evidence source: %#v", status)
	}
	assertGeminiPublicStatusDoesNotExposeSettings(t, status, configPath, "vscode")
}

func TestGeminiCLIAdapterNoOpAndNameConflict(t *testing.T) {
	adapter, configPath := newGeminiTestAdapter(t)
	base := clientRegistrationConfig{}
	added, err := adapter.plan(context.Background(), geminiTestRegistrationSpec(adapter), base)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := adapter.plan(context.Background(), geminiTestRegistrationSpec(adapter), added.config)
	if err != nil {
		t.Fatal(err)
	}
	if !sameGeminiStrings(unchanged.changes, []string{mcpRegistrationChangeUnchanged}) {
		t.Fatalf("changes = %#v, want registration_unchanged", unchanged.changes)
	}
	if !geminiConfigsEqual(unchanged.config, added.config) {
		t.Fatal("no-op plan changed the exact settings bytes")
	}

	if err := adapter.writeConfig(context.Background(), clientRegistrationConfig{}, added.config); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.writeConfig(context.Background(), added.config, added.config); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("idempotent write changed the settings file")
	}

	conflicts := []struct {
		name       string
		command    string
		extraField string
	}{
		{name: "different command", command: `C:\old\lah.exe`},
		{name: "trust field", command: adapter.command, extraField: "trust"},
		{name: "environment field", command: adapter.command, extraField: "env"},
		{name: "disabled field", command: adapter.command, extraField: "disabled"},
	}
	for _, conflict := range conflicts {
		t.Run(conflict.name, func(t *testing.T) {
			contents, err := geminiConflictSettings(conflict.command, conflict.extraField)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.plan(
				context.Background(),
				geminiTestRegistrationSpec(adapter),
				clientRegistrationConfig{exists: true, bytes: contents},
			); !errors.Is(err, errClientRegistrationNameConflict) {
				t.Fatalf("conflict error = %v, want name conflict", err)
			}
		})
	}
}

func TestGeminiCLIAdapterRejectsInvalidOrAmbiguousSettings(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "duplicate top-level key", content: `{"ui":{},"ui":{}}`},
		{name: "duplicate nested key", content: `{"ui":{"theme":"dark","theme":"light"}}`},
		{name: "duplicate server name", content: `{"mcpServers":{"one":{},"one":{}}}`},
		{name: "wrong server map type", content: `{"mcpServers":[]}`},
		{name: "trailing value", content: `{} {}`},
		{name: "invalid JSON", content: `{"mcpServers":`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			adapter, configPath := newGeminiTestAdapter(t)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := adapter.inspect(context.Background())
			if !errors.Is(err, errGeminiCLISettingsInvalid) {
				t.Fatalf("inspect error = %v, want fixed invalid-settings error", err)
			}
			if strings.Contains(err.Error(), configPath) || strings.Contains(err.Error(), testCase.content) {
				t.Fatal("settings error exposed a path or config value")
			}
		})
	}
}

func TestGeminiCLIAdapterBoundsSettingsAndRejectsStaleWrites(t *testing.T) {
	t.Run("oversized settings", func(t *testing.T) {
		adapter, configPath := newGeminiTestAdapter(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		contents := make([]byte, geminiCLIRegistrationMaxBytes+1)
		if err := os.WriteFile(configPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.inspect(context.Background()); !errors.Is(err, errClientRegistrationConfigTooLarge) {
			t.Fatalf("inspect error = %v, want config too large", err)
		}
	})

	t.Run("stale expected bytes", func(t *testing.T) {
		adapter, configPath := newGeminiTestAdapter(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		original := []byte(`{"ui":{"theme":"dark"}}`)
		externalChange := []byte(`{"ui":{"theme":"light"}}`)
		replacement := []byte(`{"ui":{"theme":"dark"},"mcpServers":{}}`)
		if err := os.WriteFile(configPath, externalChange, 0o600); err != nil {
			t.Fatal(err)
		}
		err := adapter.writeConfig(
			context.Background(),
			clientRegistrationConfig{exists: true, bytes: original},
			clientRegistrationConfig{exists: true, bytes: replacement},
		)
		if !errors.Is(err, errClientRegistrationStaleSettings) {
			t.Fatalf("write error = %v, want stale settings", err)
		}
		written, readErr := os.ReadFile(configPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(written) != string(externalChange) {
			t.Fatal("stale write changed externally updated settings")
		}
	})

	t.Run("stale no-op", func(t *testing.T) {
		adapter, configPath := newGeminiTestAdapter(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		expected := []byte(`{"ui":{"theme":"dark"}}`)
		current := []byte(`{"ui":{"theme":"light"}}`)
		if err := os.WriteFile(configPath, current, 0o600); err != nil {
			t.Fatal(err)
		}
		err := adapter.writeConfig(
			context.Background(),
			clientRegistrationConfig{exists: true, bytes: expected},
			clientRegistrationConfig{exists: true, bytes: expected},
		)
		if !errors.Is(err, errClientRegistrationStaleSettings) {
			t.Fatalf("no-op write error = %v, want stale settings", err)
		}
		written, readErr := os.ReadFile(configPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(written) != string(current) {
			t.Fatal("stale no-op changed settings")
		}
	})

	t.Run("stale existence", func(t *testing.T) {
		adapter, configPath := newGeminiTestAdapter(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		current := []byte(`{"ui":{"theme":"light"}}`)
		if err := os.WriteFile(configPath, current, 0o600); err != nil {
			t.Fatal(err)
		}
		replacement := []byte(`{"ui":{"theme":"dark"}}`)
		err := adapter.writeConfig(
			context.Background(),
			clientRegistrationConfig{},
			clientRegistrationConfig{exists: true, bytes: replacement},
		)
		if !errors.Is(err, errClientRegistrationStaleSettings) {
			t.Fatalf("write error = %v, want stale settings", err)
		}
		written, readErr := os.ReadFile(configPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(written) != string(current) {
			t.Fatal("stale existence mismatch changed settings")
		}
	})

	t.Run("restore absent original file", func(t *testing.T) {
		adapter, configPath := newGeminiTestAdapter(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		original := []byte(`{"mcpServers":{"local-agent-harness":{"command":"synthetic","args":["--mcp"]}}}`)
		if err := os.WriteFile(configPath, original, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := adapter.writeConfig(
			context.Background(),
			clientRegistrationConfig{exists: true, bytes: original},
			clientRegistrationConfig{},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("settings file still exists after restore: %v", err)
		}
	})
}

func TestGeminiCLIAdapterRejectsOutOfScopeSpecs(t *testing.T) {
	adapter, _ := newGeminiTestAdapter(t)
	base := geminiTestRegistrationSpec(adapter)
	tests := []struct {
		name string
		edit func(*MCPRegistrationSpec)
		want error
	}{
		{name: "custom display name", edit: func(spec *MCPRegistrationSpec) { spec.DisplayName = "other" }},
		{name: "wrong scope", edit: func(spec *MCPRegistrationSpec) { spec.Scope = []string{"project"} }},
		{
			name: "HTTP transport",
			edit: func(spec *MCPRegistrationSpec) { spec.Transport = MCPTransportStreamableHTTP },
			want: errClientRegistrationUnsupportedTransport,
		},
		{name: "endpoint", edit: func(spec *MCPRegistrationSpec) { spec.Endpoint = "http://127.0.0.1:49321/mcp" }},
		{name: "wrong command", edit: func(spec *MCPRegistrationSpec) { spec.Command = filepath.Join(t.TempDir(), "other.exe") }},
		{name: "extra arguments", edit: func(spec *MCPRegistrationSpec) { spec.Args = []string{"--mcp", "--token", "synthetic"} }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			spec := base
			spec.Args = append([]string(nil), base.Args...)
			spec.Scope = append([]string(nil), base.Scope...)
			testCase.edit(&spec)
			want := testCase.want
			if want == nil {
				want = errClientRegistrationInvalidSpec
			}
			if _, err := adapter.plan(context.Background(), spec, clientRegistrationConfig{}); !errors.Is(err, want) {
				t.Fatalf("plan error = %v, want %v", err, want)
			}
		})
	}
}

func newGeminiTestAdapter(t *testing.T) (*geminiCLIRegistrationAdapter, string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), ".gemini", "settings.json")
	command, err := filepath.Abs(filepath.Join(t.TempDir(), "lah-test.exe"))
	if err != nil {
		t.Fatal(err)
	}
	adapter := newGeminiCLIRegistrationAdapterForPath(
		configPath,
		command,
		func(string) (string, error) { return "synthetic-gemini-cli", nil },
	)
	return adapter, configPath
}

func geminiTestRegistrationSpec(adapter *geminiCLIRegistrationAdapter) MCPRegistrationSpec {
	return MCPRegistrationSpec{
		ClientID:    mcpClientIDGemini,
		DisplayName: mcpClientRegistrationDisplayName,
		Transport:   MCPTransportStdio,
		Command:     adapter.command,
		Args:        []string{"--mcp"},
		Scope:       []string{"user"},
	}
}

func assertGeminiUnmanagedSettingsPreserved(t *testing.T, before []byte, after []byte) {
	t.Helper()
	var oldRoot map[string]json.RawMessage
	var newRoot map[string]json.RawMessage
	if err := json.Unmarshal(before, &oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &newRoot); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ui", "mcp", "general", "security"} {
		if !sameGeminiJSONValue(oldRoot[key], newRoot[key]) {
			t.Fatalf("unmanaged top-level setting %q changed", key)
		}
	}
	var oldServers map[string]json.RawMessage
	var newServers map[string]json.RawMessage
	if err := json.Unmarshal(oldRoot["mcpServers"], &oldServers); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(newRoot["mcpServers"], &newServers); err != nil {
		t.Fatal(err)
	}
	if !sameGeminiJSONValue(oldServers["other-server"], newServers["other-server"]) {
		t.Fatal("unmanaged MCP server entry changed")
	}
}

func assertGeminiRegistrationEntry(t *testing.T, contents []byte, command string) {
	t.Helper()
	var settings struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(contents, &settings); err != nil {
		t.Fatal(err)
	}
	entry := settings.MCPServers[mcpClientRegistrationDisplayName]
	if entry.Command != command || !sameGeminiStrings(entry.Args, []string{"--mcp"}) {
		t.Fatalf("Gemini registration entry mismatch: %#v", entry)
	}
}

func assertGeminiPublicStatusDoesNotExposeSettings(
	t *testing.T,
	status MCPClientStatus,
	configPath string,
	canary string,
) {
	t.Helper()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{configPath, canary, "lah-test.exe"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public status exposed %q", forbidden)
		}
	}
}

func sameGeminiJSONValue(left json.RawMessage, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftEncoded, leftErr := json.Marshal(leftValue)
	rightEncoded, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && string(leftEncoded) == string(rightEncoded)
}

func geminiConflictSettings(command string, extraField string) ([]byte, error) {
	entry := map[string]any{
		"command": command,
		"args":    []string{"--mcp"},
	}
	switch extraField {
	case "env":
		entry[extraField] = map[string]string{"TEST_SECRET": "synthetic"}
	case "disabled":
		entry[extraField] = true
	case "trust":
		entry[extraField] = false
	}
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			mcpClientRegistrationDisplayName: entry,
		},
	})
}
