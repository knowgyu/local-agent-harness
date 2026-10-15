package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var setupStatusInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "inspect_clients": {"type": "boolean"}
  }
}`)

var setupStatusOutputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "client_attribution", "configuration_state", "observed_at", "configuration_revision", "targets"],
  "properties": {
    "schema_version": {"type": "integer", "const": 1},
    "client_attribution": {"type": "string", "const": "not_observed"},
    "configuration_state": {"type": "string"},
    "observed_at": {"type": "string", "format": "date-time"},
    "configuration_revision": {"type": "string"},
    "targets": {"type": "array", "items": {"type": "object"}},
    "service_bundles": {"type": "array", "items": {"type": "object"}},
    "clients": {"type": "array", "items": {"type": "object"}}
  }
}`)

type setupStatusRequest struct {
	InspectClients bool `json:"inspect_clients"`
}

type setupStatusDTO struct {
	SchemaVersion      int                  `json:"schema_version"`
	ClientAttribution  string               `json:"client_attribution"`
	ConfigurationState string               `json:"configuration_state"`
	ObservedAt         time.Time            `json:"observed_at"`
	ConfigurationRev   string               `json:"configuration_revision"`
	Targets            []readinessTargetDTO `json:"targets"`
	ServiceBundles     []readinessBundleDTO `json:"service_bundles,omitempty"`
	Clients            []readinessClientDTO `json:"clients,omitempty"`
}

func registerSetupStatusMCPTool(server *mcp.Server, a *app) {
	if server == nil || a == nil {
		return
	}
	server.AddTool(&mcp.Tool{
		Name:         "setup_status",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		Title:        "Read local setup status",
		Description:  "Read compact local setup status. Returns configuration state, revision, observed time, targets, and configured service bundles. Client details are omitted unless inspect_clients is true. By default it reads saved settings and retained receipt metadata. It may create or secure protected evidence-store directories on first use; it does not write settings or receipt data, contact services, or load credentials. A reported MCP read matches the current saved target definition; the calling client is not observed or attributed.",
		InputSchema:  setupStatusInputSchema,
		OutputSchema: setupStatusOutputSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		request, err := decodeSetupStatusRequest(req)
		if err != nil {
			return setupStatusToolError(), nil
		}
		response := a.setupStatus(ctx, request)
		encoded, err := json.Marshal(response)
		if err != nil || len(encoded) > readinessMaximumResponseSize {
			response = setupStatusUnavailable()
			encoded, _ = json.Marshal(response)
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
			StructuredContent: response,
		}, nil
	})
}

func decodeSetupStatusRequest(req *mcp.CallToolRequest) (setupStatusRequest, error) {
	if req == nil || len(req.Params.Arguments) > 4096 {
		return setupStatusRequest{}, errors.New("invalid setup status request")
	}
	if len(bytes.TrimSpace(req.Params.Arguments)) == 0 {
		return setupStatusRequest{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	decoder.DisallowUnknownFields()
	var request setupStatusRequest
	if err := decoder.Decode(&request); err != nil {
		return setupStatusRequest{}, errors.New("invalid setup status request")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return setupStatusRequest{}, errors.New("invalid setup status request")
	}
	return request, nil
}

func (a *app) setupStatus(ctx context.Context, request setupStatusRequest) setupStatusDTO {
	controller, _ := a.ensureOnboardingController()
	handler := &readinessStatusHandler{
		configPath:    a.configPath,
		registrations: a.clientRegistration,
		onboarding:    controller,
	}
	status, ok := handler.snapshot(ctx, readinessSnapshotOptions{
		inspectClients:     request.InspectClients,
		setupDraftRevision: true,
	})
	if !ok {
		return setupStatusFromReadiness(readinessUnavailableStatus(), false)
	}
	return setupStatusFromReadiness(status, request.InspectClients)
}

func setupStatusFromReadiness(status readinessStatusDTO, includeClients bool) setupStatusDTO {
	response := setupStatusDTO{
		SchemaVersion:      1,
		ClientAttribution:  "not_observed",
		ConfigurationState: status.State,
		ObservedAt:         status.ObservedAt,
		ConfigurationRev:   status.ConfigurationRev,
		Targets:            status.Targets,
		ServiceBundles:     status.ServiceBundles,
	}
	if includeClients {
		response.Clients = status.Clients
	}
	return response
}

func setupStatusUnavailable() setupStatusDTO {
	return setupStatusFromReadiness(readinessUnavailableStatus(), false)
}

func setupStatusToolError() *mcp.CallToolResult {
	result := &mcp.CallToolResult{}
	result.SetError(errors.New("The setup status request is invalid."))
	return result
}
