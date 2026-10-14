package main

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const agentSetupGuideURI = "lah://guides/agent-setup"

func registerAgentSetupGuide(server *mcp.Server) {
	server.AddResource(&mcp.Resource{URI: agentSetupGuideURI, Name: "Agent setup guide", MIMEType: "text/markdown"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			guide, err := uiFiles.ReadFile("docs/AGENT_SETUP.md")
			if err != nil {
				return nil, errors.New("agent setup guide is unavailable")
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: agentSetupGuideURI, MIMEType: "text/markdown", Text: string(guide)}}}, nil
		})
}
