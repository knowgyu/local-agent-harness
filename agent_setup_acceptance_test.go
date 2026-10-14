package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var agentSetupSmokeRoot = flag.String("lah-agent-setup-root", "", "task-owned directory for the opt-in mock MCP fixture")

func newAgentSetupFixture(t *testing.T, root string) (*app, *atomic.Int32) {
	t.Helper()
	ref := "cred:" + strings.Repeat("a", 32)
	path := filepath.Join(root, "settings.json")
	if err := writeConfig(path, config{
		Version:       configVersion,
		GitHubTargets: []target{{ID: "github:" + strings.Repeat("1", 32), Name: "Mock GitHub", Origin: "https://setup.example.invalid", Repository: "demo/widget", SecretRef: ref}},
	}); err != nil {
		t.Fatal(err)
	}
	requests := &atomic.Int32{}
	client := &http.Client{Transport: githubFirstUseAcceptanceRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://setup.example.invalid/api/v3/repos/demo/widget" || r.Header.Get("Authorization") != "Bearer synthetic-setup-token" {
			return nil, errors.New("fixture rejected an unexpected service request")
		}
		requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"name":"widget","full_name":"demo/widget","private":true,"default_branch":"main"}`))}, nil
	})}
	a := &app{configPath: path, client: client, secrets: &githubFirstUseAcceptanceSecrets{values: map[string][]byte{ref: []byte("synthetic-setup-token")}}}
	a.setupDrafts = newSetupDraftControllerWithQueue(newSetupDraftConfigStore(path), &fakeSetupDraftQueueStore{})
	return a, requests
}

func TestAgentSetupMockReadSurvivesRestartWithoutPromotingRegistration(t *testing.T) {
	a, requests := newAgentSetupFixture(t, t.TempDir())
	for _, clientName := range []string{"codex-protocol-fixture", "claude-protocol-fixture"} {
		t.Run(clientName, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			server, err := a.mcpServer().Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			client, err := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "fixture"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(); _ = server.Wait() }()
			listed, err := client.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range listed.Tools {
				readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
				switch tool.Name {
				case "github_repository", "setup_status":
					if !readOnly {
						t.Fatalf("read-only tool %s lacks client metadata", tool.Name)
					}
				case "submit_setup_draft", "registered_target_connection_test", "jenkins_run_registered_job", "python_run_registered_task", "run_ssh_operation":
					if readOnly {
						t.Fatalf("operation tool %s is advertised as read-only", tool.Name)
					}
				}
			}
			beforeRequests := requests.Load()
			guide, err := client.ReadResource(ctx, &mcp.ReadResourceParams{URI: agentSetupGuideURI})
			canonical, readErr := uiFiles.ReadFile("docs/AGENT_SETUP.md")
			if err != nil || readErr != nil || len(guide.Contents) != 1 || guide.Contents[0].Text != string(canonical) || requests.Load() != beforeRequests {
				t.Fatal("installed setup guide was not the passive canonical document")
			}
			status := func() setupStatusDTO {
				t.Helper()
				result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "setup_status", Arguments: map[string]any{}})
				if err != nil || result.IsError {
					t.Fatalf("setup status failed: %v", err)
				}
				var value setupStatusDTO
				if len(result.Content) != 1 {
					t.Fatal("setup status did not return one bounded result")
				}
				text, ok := result.Content[0].(*mcp.TextContent)
				if !ok || json.Unmarshal([]byte(text.Text), &value) != nil {
					t.Fatal("setup status was not JSON")
				}
				return value
			}
			before := status()
			if len(before.Targets) != 1 || before.ClientAttribution != "not_observed" {
				t.Fatal("status invented a target or client identity")
			}
			if requests.Load() == 0 && before.Targets[0].FirstRead.State != firstReadStateNotObserved {
				t.Fatal("registration was promoted to a successful read")
			}
			read, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "github_repository", Arguments: map[string]any{"target": "Mock GitHub"}})
			if err != nil || read.IsError {
				t.Fatalf("mock service read failed: %v", err)
			}
			after := status()
			if after.Targets[0].FirstRead.State != firstReadStateObserved || after.Targets[0].FirstRead.Source != "mcp_observed" || after.Targets[0].ConnectionTest.State != "not_tested" {
				t.Fatal("actual read evidence was lost or confused with a connection test")
			}
		})
		// A new app controller must read durable evidence, without another service request.
		a.onboarding = nil
	}
	if requests.Load() != 2 {
		t.Fatal("passive status caused an extra service request")
	}
}

// This opt-in helper lets real CLI clients exercise only an in-memory fake service.
func TestAgentSetupMockStdioHelper(t *testing.T) {
	root := os.Getenv("LAH_AGENT_SETUP_SMOKE_ROOT")
	if *agentSetupSmokeRoot != "" {
		root = *agentSetupSmokeRoot
	}
	if root == "" {
		t.Skip("opt in with a task-owned LAH_AGENT_SETUP_SMOKE_ROOT")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	a, requests := newAgentSetupFixture(t, root)
	err := a.mcpServer().Run(context.Background(), &mcp.StdioTransport{MaxLineLength: 1 << 20})
	result := struct {
		ServiceReads int32 `json:"mock_service_reads"`
		TransportOK  bool  `json:"transport_ok"`
	}{requests.Load(), err == nil}
	encoded, _ := json.Marshal(result)
	if err := os.WriteFile(filepath.Join(root, "fixture-result.json"), encoded, 0o600); err != nil {
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
	// Do not append Go's test-runner output to the MCP protocol stream.
	os.Exit(0)
}
