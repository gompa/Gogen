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
	mcpCallTimeout = 30 * time.Second
	// mcpInitTimeout bounds initialize + tools/list during NewManager so a
	// hung MCP stdio server cannot stall process startup for a full
	// mcpCallTimeout per server.
	mcpInitTimeout         = 5 * time.Second
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

// Client is a single MCP stdio connection with an async read loop.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu         sync.Mutex
	writeMu    sync.Mutex
	nextID     atomic.Int64
	pending    map[int64]chan jsonRPCResponse
	closed     chan struct{}
	closeOnce  sync.Once
	readerDone chan struct{}
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
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

// ValidServers returns entries that have both a name and a command.
// Incomplete stubs (mcp: on with placeholder objects) are dropped so
// NewManager is never asked to spawn empty exec.Command values.
func ValidServers(servers []config.MCPServerConfig) []config.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	out := make([]config.MCPServerConfig, 0, len(servers))
	for _, s := range servers {
		if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Command) == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NewManager starts configured MCP servers and builds a tool registry.
func NewManager(servers []config.MCPServerConfig) (*Manager, error) {
	reg := &Registry{tools: make(map[string]toolEntry)}
	m := &Manager{reg: reg}
	for _, s := range ValidServers(servers) {
		c, err := startClient(s)
		if err != nil {
			continue
		}
		initCtx, cancel := context.WithTimeout(context.Background(), mcpInitTimeout)
		err = c.initialize(initCtx)
		if err != nil {
			cancel()
			_ = c.Close()
			continue
		}
		tools, err := c.listTools(initCtx)
		cancel()
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
	// Discard stderr so MCP noise does not pollute the TUI/CLI.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdoutPipe,
		pending:    make(map[int64]chan jsonRPCResponse),
		closed:     make(chan struct{}),
		readerDone: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.readerDone)
	reader := bufio.NewReader(c.stdout)
	skipped := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.failPending(err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
			skipped = 0
		}
		c.mu.Unlock()
		if !ok {
			skipped++
			if skipped > mcpMaxSkippedResponses {
				c.failPending(fmt.Errorf("mcp: too many unmatched responses; server may be broken"))
				return
			}
			continue
		}
		select {
		case ch <- resp:
		default:
		}
	}
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- jsonRPCResponse{Error: &jsonRPCError{Message: err.Error()}}:
		default:
		}
		delete(c.pending, id)
	}
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
	return c.notify("notifications/initialized", map[string]interface{}{})
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
	for i, part := range result.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text)
	}
	out := b.String()
	if result.IsError {
		return out, fmt.Errorf("mcp tool error")
	}
	return out, nil
}

func (c *Client) notify(method string, params interface{}) error {
	select {
	case <-c.closed:
		return fmt.Errorf("mcp client closed")
	default:
	}
	req := jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()

	select {
	case <-c.closed:
		return nil, fmt.Errorf("mcp client closed")
	default:
	}

	id := c.nextID.Add(1)
	ch := make(chan jsonRPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	idCopy := id
	req := jsonRPCRequest{JSONRPC: "2.0", ID: &idCopy, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("mcp client closed")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp rpc error: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// Close shuts down the MCP client process.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
		// Wait for the reader to exit before touching pending under mu, so we
		// never contend with readLoop.failPending on the same mutex.
		<-c.readerDone
		c.failPending(fmt.Errorf("mcp client closed"))
	})
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
