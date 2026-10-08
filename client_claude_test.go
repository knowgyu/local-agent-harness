package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestClaudeRegistrationAdapterInspectAndPlan(t *testing.T) {
	t.Parallel()

	profile := t.TempDir()
	configPath := filepath.Join(profile, claudeRegistrationFileName)
	fixture, err := os.ReadFile(filepath.Join("testdata", "client_claude", "user-config-unrelated.json"))
	if err != nil {
		t.Fatal("read Claude fixture")
	}
	if err := os.WriteFile(configPath, fixture, 0o600); err != nil {
		t.Fatal("write temporary Claude profile")
	}
	appPath := filepath.Join(profile, "local-agent-harness-test.exe")
	adapter := newInstalledClaudeTestAdapter(configPath, appPath)
	ctx := context.Background()

	initial, err := adapter.inspect(ctx)
	if err != nil {
		t.Fatalf("inspect temporary profile: %v", err)
	}
	if initial.status.Installed != "installed" || initial.status.Registration != "not_registered" {
		t.Fatalf("unexpected initial status: %+v", initial.status)
	}
	if initial.status.Connection != "unverified" || initial.status.ToolCall != "unverified" || initial.status.EvidenceSource != "config_observed" {
		t.Fatalf("unexpected evidence status: %+v", initial.status)
	}
	statusJSON, err := json.Marshal(initial.status)
	if err != nil {
		t.Fatalf("marshal adapter status: %v", err)
	}
	if bytes.Contains(statusJSON, []byte("c02-private-config-canary")) || bytes.Contains(statusJSON, []byte(configPath)) || bytes.Contains(statusJSON, []byte(appPath)) {
		t.Fatal("status exposed config data or a local path")
	}

	mutation, err := adapter.plan(ctx, claudeTestRegistrationSpec(appPath), initial.config)
	if err != nil {
		t.Fatalf("plan registration: %v", err)
	}
	if mutation.changes == nil || len(mutation.changes) != 1 || mutation.changes[0] != mcpRegistrationChangeAdded {
		t.Fatalf("plan returned an unsafe change summary: %#v", mutation.changes)
	}
	if err := adapter.writeConfig(ctx, initial.config, mutation.config); err != nil {
		t.Fatalf("apply registration to temporary profile: %v", err)
	}

	updatedBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read updated temporary profile")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(updatedBytes, &root); err != nil {
		t.Fatalf("decode updated temporary profile: %v", err)
	}
	var theme string
	if err := json.Unmarshal(root["theme"], &theme); err != nil || theme != "dark" {
		t.Fatalf("unrelated user setting was not preserved: %q, %v", theme, err)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(root["mcpServers"], &servers); err != nil {
		t.Fatalf("decode updated server entries: %v", err)
	}
	if _, ok := servers["existing-server"]; !ok {
		t.Fatal("unmanaged MCP server was removed")
	}
	if !claudeEntryMatches(servers[mcpClientRegistrationDisplayName], claudeMCPServerEntry{
		Type:    "stdio",
		Command: appPath,
		Args:    []string{"--mcp"},
	}) {
		t.Fatal("Claude registration does not contain the fixed stdio command")
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(root["projects"], &projects); err != nil || len(projects) != 1 {
		t.Fatalf("unrelated project settings were not preserved: %v", err)
	}

	verified, err := adapter.verify(ctx)
	if err != nil {
		t.Fatalf("verify saved config: %v", err)
	}
	if verified.Registration != "registered" || verified.Connection != "unverified" || verified.ToolCall != "unverified" {
		t.Fatalf("verification overstated the evidence: %+v", verified)
	}

	idempotent, err := adapter.plan(ctx, claudeTestRegistrationSpec(appPath), mutation.config)
	if err != nil {
		t.Fatalf("plan idempotent registration: %v", err)
	}
	if len(idempotent.changes) != 1 || idempotent.changes[0] != mcpRegistrationChangeUnchanged {
		t.Fatalf("idempotent plan returned an unexpected change: %#v", idempotent.changes)
	}
	if !bytes.Equal(idempotent.config.bytes, mutation.config.bytes) {
		t.Fatal("idempotent plan changed config bytes")
	}
}

func TestClaudeRegistrationAdapterRejectsNameConflictAndAmbiguousJSON(t *testing.T) {
	t.Parallel()

	appPath := filepath.Join(t.TempDir(), "local-agent-harness-test.exe")
	adapter := newInstalledClaudeTestAdapter(filepath.Join(t.TempDir(), claudeRegistrationFileName), appPath)
	tests := []struct {
		name   string
		config clientRegistrationConfig
		want   error
	}{
		{
			name: "occupied name",
			config: clientRegistrationConfig{
				exists: true,
				bytes:  []byte(`{"mcpServers":{"local-agent-harness":{"type":"stdio","command":"another-app.exe","args":["--mcp"]}}}`),
			},
			want: errClientRegistrationNameConflict,
		},
		{
			name: "duplicate entry key",
			config: clientRegistrationConfig{
				exists: true,
				bytes:  []byte(`{"mcpServers":{"local-agent-harness":{"type":"stdio","command":"one.exe","args":["--mcp"]},"local-agent-harness":{"type":"stdio","command":"two.exe","args":["--mcp"]}}}`),
			},
			want: errClaudeRegistrationConfigInvalid,
		},
		{
			name: "duplicate root key",
			config: clientRegistrationConfig{
				exists: true,
				bytes:  []byte(`{"theme":"light","theme":"dark"}`),
			},
			want: errClaudeRegistrationConfigInvalid,
		},
		{
			name: "duplicate project scope override",
			config: clientRegistrationConfig{
				exists: true,
				bytes:  []byte(`{"projects":{"C:\\synthetic":{"mcpServers":{"local-agent-harness":{"type":"stdio","command":"other.exe","args":[]}}}}}`),
			},
			want: errClientRegistrationNameConflict,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := adapter.plan(context.Background(), claudeTestRegistrationSpec(appPath), test.config)
			if !errors.Is(err, test.want) {
				t.Fatalf("plan error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestClaudeRegistrationAdapterDisablesPlanForUnsupportedConfigShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		bytes []byte
	}{
		{name: "mcpServers is not an object", bytes: []byte(`{"mcpServers":[]}`)},
		{name: "null mcpServers", bytes: []byte(`{"mcpServers":null}`)},
		{name: "malformed projects", bytes: []byte(`{"projects":[]}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), claudeRegistrationFileName)
			if err := os.WriteFile(configPath, test.bytes, 0o600); err != nil {
				t.Fatal("write unsupported temporary profile")
			}
			appPath := filepath.Join(t.TempDir(), "local-agent-harness-test.exe")
			adapter := newInstalledClaudeTestAdapter(configPath, appPath)
			current, err := adapter.inspect(context.Background())
			if err != nil {
				t.Fatalf("inspect unsupported profile: %v", err)
			}
			if current.status.Registration != mcpClientRegistrationNotObserved || current.status.EvidenceSource != mcpClientEvidenceNotObserved {
				t.Fatalf("unsupported profile was not safely reported as unobserved: %+v", current.status)
			}
			if _, err := adapter.plan(context.Background(), claudeTestRegistrationSpec(appPath), current.config); err == nil {
				t.Fatal("plan accepted an unsupported configuration shape")
			}
			actual, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal("read unsupported profile after rejected plan")
			}
			if !bytes.Equal(actual, test.bytes) {
				t.Fatal("rejected plan changed the unsupported profile")
			}
		})
	}
}

func TestClaudeRegistrationAdapterWriteConfigUsesExactCompareAndSwap(t *testing.T) {
	t.Parallel()

	profile := t.TempDir()
	configPath := filepath.Join(profile, claudeRegistrationFileName)
	appPath := filepath.Join(profile, "local-agent-harness-test.exe")
	adapter := newInstalledClaudeTestAdapter(configPath, appPath)
	expected := clientRegistrationConfig{exists: true, bytes: []byte(`{"theme":"before"}`)}
	if err := os.WriteFile(configPath, expected.bytes, 0o600); err != nil {
		t.Fatal("write expected config")
	}
	replacement := clientRegistrationConfig{exists: true, bytes: []byte(`{"theme":"after"}`)}

	changed := clientRegistrationConfig{exists: true, bytes: []byte(`{"theme":"other-writer"}`)}
	if err := os.WriteFile(configPath, changed.bytes, 0o600); err != nil {
		t.Fatal("simulate an external settings change")
	}
	if err := adapter.writeConfig(context.Background(), expected, replacement); !errors.Is(err, errClientRegistrationStaleSettings) {
		t.Fatalf("write after external change = %v, want stale settings", err)
	}
	actual, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read config after rejected write")
	}
	if !bytes.Equal(actual, changed.bytes) {
		t.Fatal("stale write overwrote the external settings change")
	}

	if err := adapter.writeConfig(context.Background(), changed, replacement); err != nil {
		t.Fatalf("write with matching exact bytes: %v", err)
	}
	actual, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read config after successful write")
	}
	if !bytes.Equal(actual, replacement.bytes) {
		t.Fatal("atomic write did not persist the replacement bytes")
	}

	if err := adapter.writeConfig(context.Background(), clientRegistrationConfig{}, expected); !errors.Is(err, errClientRegistrationStaleSettings) {
		t.Fatalf("write with a stale expected absence = %v, want stale settings", err)
	}
	actual, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read config after stale absence check")
	}
	if !bytes.Equal(actual, replacement.bytes) {
		t.Fatal("existence mismatch overwrote the current settings")
	}

	if err := adapter.writeConfig(context.Background(), replacement, clientRegistrationConfig{}); err != nil {
		t.Fatalf("restore a previously absent config: %v", err)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored config still exists or stat failed: %v", err)
	}
}

func TestClaudeRegistrationAdapterDisablesWhenCLIOrConfigLocationIsUnobserved(t *testing.T) {
	t.Parallel()

	configPath := t.TempDir()
	missing := newClaudeRegistrationAdapterWithOptions(claudeRegistrationOptions{
		configPath: configPath,
		lookupExecutable: func(string) (string, error) {
			return "", exec.ErrNotFound
		},
	})
	status, err := missing.verify(context.Background())
	if err != nil {
		t.Fatalf("inspect absent CLI: %v", err)
	}
	if status.Installed != "installed_cli_not_observed" || status.Registration != "not_observed" || status.EvidenceSource != "not_observed" {
		t.Fatalf("absent CLI status is not safely unobserved: %+v", status)
	}
	if status.Connection != "unverified" || status.ToolCall != "unverified" || status.Backup != "not_created" {
		t.Fatalf("absent CLI status overstated its evidence: %+v", status)
	}
	if _, err := missing.plan(context.Background(), claudeTestRegistrationSpec(filepath.Join(t.TempDir(), "app.exe")), clientRegistrationConfig{}); !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("plan without CLI = %v, want unavailable", err)
	}

	customConfigDir := newClaudeRegistrationAdapterWithOptions(claudeRegistrationOptions{
		configPath: configPath,
		lookupExecutable: func(string) (string, error) {
			return "claude", nil
		},
		currentExecutable: func() (string, error) {
			return filepath.Join(t.TempDir(), "app.exe"), nil
		},
		configDir: func() string {
			return t.TempDir()
		},
	})
	customStatus, err := customConfigDir.verify(context.Background())
	if err != nil {
		t.Fatalf("inspect custom config directory: %v", err)
	}
	if customStatus.Installed != "installed" || customStatus.Registration != "not_observed" || customStatus.EvidenceSource != "not_observed" {
		t.Fatalf("custom config directory was not safely unobserved: %+v", customStatus)
	}
	if _, err := customConfigDir.plan(context.Background(), claudeTestRegistrationSpec(filepath.Join(t.TempDir(), "app.exe")), clientRegistrationConfig{}); !errors.Is(err, errClientRegistrationClientUnavailable) {
		t.Fatalf("plan with custom config directory = %v, want unavailable", err)
	}
}

func TestClaudeRegistrationAdapterRejectsUnsupportedRegistrationSpecs(t *testing.T) {
	t.Parallel()

	appPath := filepath.Join(t.TempDir(), "local-agent-harness-test.exe")
	adapter := newInstalledClaudeTestAdapter(filepath.Join(t.TempDir(), claudeRegistrationFileName), appPath)
	tests := []struct {
		name string
		edit func(*MCPRegistrationSpec)
		want error
	}{
		{
			name: "http transport",
			edit: func(spec *MCPRegistrationSpec) {
				spec.Transport = MCPTransportStreamableHTTP
				spec.Endpoint = "http://127.0.0.1:49321/mcp"
			},
			want: errClientRegistrationUnsupportedTransport,
		},
		{
			name: "alternate name",
			edit: func(spec *MCPRegistrationSpec) {
				spec.DisplayName = "other-server"
			},
			want: errClientRegistrationInvalidSpec,
		},
		{
			name: "non-user scope",
			edit: func(spec *MCPRegistrationSpec) {
				spec.Scope = []string{"project"}
			},
			want: errClientRegistrationInvalidSpec,
		},
		{
			name: "different executable",
			edit: func(spec *MCPRegistrationSpec) {
				spec.Command = filepath.Join(t.TempDir(), "different.exe")
			},
			want: errClientRegistrationInvalidSpec,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := claudeTestRegistrationSpec(appPath)
			test.edit(&spec)
			_, err := adapter.plan(context.Background(), spec, clientRegistrationConfig{})
			if !errors.Is(err, test.want) {
				t.Fatalf("plan error = %v, want %v", err, test.want)
			}
		})
	}
}

func newInstalledClaudeTestAdapter(configPath, appPath string) *claudeRegistrationAdapter {
	return newClaudeRegistrationAdapterWithOptions(claudeRegistrationOptions{
		configPath: configPath,
		lookupExecutable: func(string) (string, error) {
			return "claude", nil
		},
		currentExecutable: func() (string, error) {
			return appPath, nil
		},
		configDir: func() string {
			return ""
		},
	})
}

func claudeTestRegistrationSpec(appPath string) MCPRegistrationSpec {
	return MCPRegistrationSpec{
		ClientID:    mcpClientIDClaude,
		DisplayName: mcpClientRegistrationDisplayName,
		Transport:   MCPTransportStdio,
		Command:     appPath,
		Args:        []string{"--mcp"},
		Scope:       []string{"user"},
	}
}
