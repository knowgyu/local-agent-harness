package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServerListsDraftAndSSHToolsAfterRuntimeInitialization(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	registrationAdapter := newReadyFakeAdapter("codex", []byte(`{}`), []byte(`{"mcpServers":{}}`))
	registration := newFakeRegistrationController(t, registrationAdapter, nil, defaultClientRegistrationPlanTTL)
	controller := newSetupDraftControllerWithQueue(newSetupDraftConfigStore(configPath), &fakeSetupDraftQueueStore{})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	app := &app{configPath: configPath, setupDrafts: controller, clientRegistration: registration}
	if err := app.initializeRuntimeControllersWith(nil, registration, executable); err != nil {
		t.Fatal(err)
	}
	if app.ssh == nil {
		t.Fatal("runtime initialization did not construct the SSH controller")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := app.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect production MCP server to synthetic transport: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("connect synthetic client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
	})
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	tools := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}
	for _, name := range []string{mcpToolRegisteredSSHTargets, mcpToolRunSSHOperation, "submit_setup_draft"} {
		if tools[name] == nil {
			t.Fatalf("initialized MCP server omitted %q", name)
		}
	}
	if got := schemaText(t, tools[mcpToolRunSSHOperation].InputSchema); got == "" || containsAny(got, `"host"`, `"url"`, `"command"`, `"credential"`, `"endpoint"`) {
		t.Fatalf("SSH tool schema exposes a host, endpoint, command or credential field: %s", got)
	}
	if got := schemaText(t, tools["submit_setup_draft"].InputSchema); got == "" || containsAny(got, `"credential_ref"`, `"command"`, `"host"`, `"endpoint"`) {
		t.Fatalf("setup draft tool schema exposes forbidden configuration material: %s", got)
	}
}

func schemaText(t *testing.T, schema any) string {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("encode tool input schema: %v", err)
	}
	return string(encoded)
}

func containsAny(value string, forbidden ...string) bool {
	for _, item := range forbidden {
		if strings.Contains(value, item) {
			return true
		}
	}
	return false
}
