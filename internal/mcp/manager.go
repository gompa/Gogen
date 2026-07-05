package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
)

const (
	mcpCallTimeout         = 30 * time.Second
	mcpMaxSkippedResponses = 100
)

var sanitizeRE = regexp.MustCompile(`[^a-z0-9_]+`)

// Registry aggregates MCP tools for the agent.
type Registry struct {
	tools map[string]toolEntry
}

type toolEntry struct {
	server string
	tool   string
	schema llm.Tool
	client *Client
}

// Client is a single MCP stdio connection.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID atomic.Int64
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Manager owns MCP server processes.
type Manager struct {
	clients []*Client
	reg     *Registry
}

// NewManager starts configured MCP servers and builds a tool registry.
func NewManager(servers []config.MCPServerConfig) (*Manager, error) {
	reg := &Registry{tools: make(map[string]toolEntry)}
	m := &Manager{reg: reg}
	for _, s := range servers {
		c, err := startClient(s)
		if err != nil {
			continue
		}
		if err := c.initialize(context.Background()); err != nil {
			_ = c.Close()
			continue
		}
		tools, err := c.listTools(context.Background())
		if err != nil {
			_ = c.Close()
			continue
		}
		for _, tool := range tools {
			extName := ExternalToolName(s.Name, tool.Name)
			reg.tools[extName] = toolEntry{
				server: s.Name,
				tool:   tool.Name,
				schema: tool,
				client: c,
			}
		}
		m.clients = append(m.clients, c)
	}
	return m, nil
}

func startClient(s config.MCPServerConfig) (*Client, error) {
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Client{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
	}, nil
}

func (c *Client) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "gogen",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return err
	}
	_, err = c.notify("notifications/initialized", map[string]interface{}{})
	return err
}

func (c *Client) listTools(ctx context.Context) ([]llm.Tool, error) {
	raw, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var result struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	out := make([]llm.Tool, 0, len(result.Tools))
	for _, t := range result.Tools {
		params := t.InputSchema
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return out, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	raw, err := c.call(ctx, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw), nil
	}
	var b strings.Builder
	for i, c := range result.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Text)
	}
	out := b.String()
	if result.IsError {
		return out, fmt.Errorf("mcp tool error")
	}
	return out, nil
}

func (c *Client) notify(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID.Add(1)
	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	return nil, nil
}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID.Add(1)
	req := jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	skipped := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytesTrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			skipped++
			if skipped > mcpMaxSkippedResponses {
				return nil, fmt.Errorf("mcp: too many skipped responses (%d); server may be broken", skipped)
			}
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp rpc error: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func bytesTrimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}

// Close shuts down the MCP client process.
func (c *Client) Close() error {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	return nil
}

// Close shuts down all MCP clients.
func (m *Manager) Close() error {
	for _, c := range m.clients {
		_ = c.Close()
	}
	return nil
}

// Registry returns the MCP tool registry.
func (m *Manager) Registry() *Registry {
	return m.reg
}

// ExternalToolName builds the LLM-visible MCP tool name.
func ExternalToolName(server, tool string) string {
	return "mcp_" + sanitize(server) + "_" + sanitize(tool)
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = sanitizeRE.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "x"
	}
	return s
}

// Definitions implements agent.MCPToolRegistry.
func (r *Registry) Definitions() []llm.Tool {
	if r == nil || len(r.tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]llm.Tool, 0, len(r.tools))
	for _, name := range names {
		t := r.tools[name].schema
		t.Name = name
		out = append(out, t)
	}
	return out
}

// ToolNames implements agent.MCPToolRegistry.
func (r *Registry) ToolNames() map[string]struct{} {
	if r == nil {
		return nil
	}
	out := make(map[string]struct{}, len(r.tools))
	for name := range r.tools {
		out[name] = struct{}{}
	}
	return out
}

// CallTool implements agent.MCPToolRegistry.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if r == nil {
		return "", fmt.Errorf("mcp registry not configured")
	}
	entry, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown mcp tool: %s", name)
	}
	return entry.client.CallTool(ctx, entry.tool, args)
}
