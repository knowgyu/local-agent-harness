package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var setupDraftSubmissionSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["base_revision", "sources", "missing", "connections", "bundles", "ssh_targets"],
  "properties": {
    "base_revision": {"type": "string", "maxLength": 128},
    "sources": {"type": "array", "minItems": 1, "maxItems": 8, "items": {"type": "object", "additionalProperties": false, "required": ["kind", "label", "confidence"], "properties": {"kind": {"type": "string"}, "label": {"type": "string"}, "confidence": {"type": "string"}}}},
    "missing": {"type": "array", "items": {"type": "string"}},
    "connections": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["kind", "action", "name"], "properties": {"kind": {"type": "string", "enum": ["github", "jenkins", "harbor", "dashboard"]}, "action": {"type": "string", "enum": ["add", "update", "reuse"]}, "existing_id": {"type": "string"}, "name": {"type": "string"}, "origin": {"type": "string"}, "repository": {"type": "string"}, "username": {"type": "string"}, "project": {"type": "string"}, "job_path": {"type": "string"}, "environment": {"type": "string"}, "namespace": {"type": "string"}, "base_url": {"type": "string"}, "credential_name": {"type": "string"}}}},
    "bundles": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["name", "repository_ids", "environments"], "properties": {"name": {"type": "string"}, "repository_ids": {"type": "array", "items": {"type": "string"}}, "environments": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {"name": {"type": "string"}, "jenkins_target_id": {"type": "string"}, "harbor_target_id": {"type": "string"}, "dashboard_target_id": {"type": "string"}, "dashboard_namespace": {"type": "string"}, "dashboard_deployment": {"type": "string"}}}}}}},
    "ssh_targets": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["name", "alias", "operations"], "properties": {"name": {"type": "string"}, "alias": {"type": "string"}, "operations": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["id", "name", "summary", "program", "fixed_args", "risk", "parameters", "approval"], "properties": {"id": {"type": "string"}, "name": {"type": "string"}, "summary": {"type": "string"}, "program": {"type": "string"}, "fixed_args": {"type": "array", "items": {"type": "string"}}, "risk": {"type": "string", "enum": ["read_only", "state_changing", "destructive"]}, "parameters": {"type": "array", "items": {"type": "object", "additionalProperties": false, "required": ["name", "type", "required"], "properties": {"name": {"type": "string"}, "type": {"type": "string", "enum": ["string", "integer", "boolean"]}, "required": {"type": "boolean"}, "description": {"type": "string"}}}}, "approval": {"type": "object", "additionalProperties": false, "required": ["mode", "scope"], "properties": {"mode": {"type": "string", "enum": ["read_only", "per_call_confirmation"]}, "revision": {"type": "string"}, "scope": {"type": "array", "items": {"type": "string"}}}, "revision": {"type": "string"}}}}}}}}
  }
}`)

func registerSetupDraftMCPTool(server *mcp.Server, controller SetupDraftController) {
	if server == nil || controller == nil {
		return
	}
	server.AddTool(&mcp.Tool{
		Name:        "submit_setup_draft",
		Title:       "Submit a setup draft for local review",
		Description: "Submit proposed registered targets, service bundles, and fixed SSH operation definitions for review in the local UI. This only creates a short-lived draft. It does not save settings, read credential values, access Credential Manager, contact services, or run SSH. A person must open the local UI and explicitly approve the current reviewed proposal before it is applied. Use only existing registered scope and user-supplied names; never include secret values, credential references, arbitrary commands, or arbitrary hosts in the proposal.",
		InputSchema: setupDraftSubmissionSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req == nil {
			return setupDraftToolError(errSetupDraftPayload), nil
		}
		submission, err := decodeSetupDraftSubmissionJSON(req.Params.Arguments)
		if err != nil {
			return setupDraftToolError(errSetupDraftPayload), nil
		}
		review, err := controller.SubmitSetupDraft(ctx, submission)
		if err != nil {
			return setupDraftToolError(err), nil
		}
		encoded, err := json.Marshal(review)
		if err != nil {
			return setupDraftToolError(errSetupDraftStore), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil
	})
}

func setupDraftToolError(err error) *mcp.CallToolResult {
	message := "The setup draft was rejected. No settings were changed."
	switch {
	case errors.Is(err, errSetupDraftCapacity), errors.Is(err, errSetupDraftQueueCapacity):
		message = "The local setup draft review queue is full. Review or discard a pending draft before submitting another."
	case errors.Is(err, errSetupDraftStale):
		message = "Settings changed while the setup draft was being prepared. Refresh the local UI and submit a new draft."
	case errors.Is(err, errSetupDraftNoChange):
		message = "The setup draft does not change registered settings."
	case errors.Is(err, errSetupDraftMissing):
		message = "The setup draft needs local information before it can be approved. Review the local UI."
	}
	result := &mcp.CallToolResult{}
	result.SetError(errors.New(message))
	return result
}
