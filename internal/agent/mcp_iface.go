package agent

import (
	"context"

	"gogen/internal/llm"
)

// MCPToolRegistry exposes MCP tools to the agent.
type MCPToolRegistry interface {
	Definitions() []llm.Tool
	ToolNames() map[string]struct{}
	CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error)
}
