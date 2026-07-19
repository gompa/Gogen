package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

const (
	openCodeZenBaseURL = "https://opencode.ai/zen/v1/"
	openCodeGoBaseURL = "https://opencode.ai/zen/go/v1/"
)

// isOpencodeURL reports whether baseURL points to an OpenCode endpoint that
// should also expose the Go model family at openCodeGoBaseURL.
func isOpencodeURL(baseURL string) bool {
	return strings.Contains(baseURL, "opencode.ai")
}

type OpenAIProvider struct {
	client      openai.Client            // primary (user-configured; fallback for non-OpenCode)
	zenClient   *openai.Client           // OpenCode Zen endpoint
	goClient    *openai.Client           // OpenCode Go endpoint
	model       string
	modelClient map[string]*openai.Client // model ID → client routing
	// promptCacheKey scopes provider-side prompt caching (defaults to none).
	promptCacheKey param.Opt[string]
}

func (p *OpenAIProvider) ModelName() string {
	return p.model
}

func (p *OpenAIProvider) listModels(ctx context.Context) []openai.Model {
	p.modelClient = make(map[string]*openai.Client)
	var models []openai.Model

	query := func(c *openai.Client) {
		pager := c.Models.ListAutoPaging(ctx)
		for pager.Next() {
			m := pager.Current()
			models = append(models, m)
			p.modelClient[m.ID] = c
		}
	}

	if p.zenClient != nil && p.goClient != nil {
		// OpenCode: query both endpoints independently.
		query(p.zenClient)
		query(p.goClient)
	} else {
		// Non-OpenCode: just the primary client.
		query(&p.client)
	}
	return models
}

func NewOpenAIProvider(apiKey string, model string, baseURL string) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(newSSEHTTPClient()),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	p := &OpenAIProvider{
		client:      openai.NewClient(opts...),
		model:       model,
		modelClient: make(map[string]*openai.Client),
	}
	if isOpencodeURL(baseURL) {
		newClient := func(url string) *openai.Client {
			c := openai.NewClient(
				option.WithAPIKey(apiKey),
				option.WithHTTPClient(newSSEHTTPClient()),
				option.WithBaseURL(url),
			)
			return &c
		}
		p.zenClient = newClient(openCodeZenBaseURL)
		p.goClient = newClient(openCodeGoBaseURL)
	}
	return p
}

// SetPromptCacheKey sets a stable key for provider-side prompt caching
// (maps to the OpenAI prompt_cache_key parameter). An empty key disables.
// Use a value derived from the working directory to keep cache hits
// scoped per-project while avoiding cross-user leakage.
func (p *OpenAIProvider) SetPromptCacheKey(key string) {
	if key == "" { p.promptCacheKey = param.Opt[string]{}; return }
	p.promptCacheKey = param.NewOpt(key)
}

// clientForModel returns the openai.Client that should serve the currently
// selected model.  When modelClient has been populated by a ListModels call
// the lookup is cheap; otherwise it does a one-time discovery probe against
// both endpoints to populate the cache.
func (p *OpenAIProvider) clientForModel() *openai.Client {
	if p.modelClient == nil {
		p.modelClient = make(map[string]*openai.Client)
	}
	if c, ok := p.modelClient[p.model]; ok {
		return c
	}
	// Discovery: probe Zen first, then Go (deterministic order).
	if p.zenClient != nil {
		_, err := p.zenClient.Models.Get(context.Background(), p.model)
		if err == nil {
			p.modelClient[p.model] = p.zenClient
			return p.zenClient
		}
	}
	if p.goClient != nil {
		_, err := p.goClient.Models.Get(context.Background(), p.model)
		if err == nil {
			p.modelClient[p.model] = p.goClient
			return p.goClient
		}
	}
	// Fallback to primary.
	p.modelClient[p.model] = &p.client
	return &p.client
}

func toolsToOpenAI(tools []Tool, allowed map[string]struct{}) []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		if allowed != nil {
			if _, ok := allowed[t.Name]; !ok {
				continue
			}
		}
		out = append(out, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  shared.FunctionParameters(t.Parameters),
			},
		})
	}
	return out
}

func (p *OpenAIProvider) messagesToChat(messages []Message) []openai.ChatCompletionMessageParamUnion {
	chatMessages := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "system":
			chatMessages = append(chatMessages, openai.SystemMessage(m.Content))
		case "user":
			chatMessages = append(chatMessages, openai.UserMessage(m.Content))
		case "assistant":
			if len(m.ToolCalls) > 0 {
				asst := openai.ChatCompletionAssistantMessageParam{}
				if m.Content != "" {
					asst.Content.OfString = param.NewOpt(m.Content)
				}
				for _, tc := range m.ToolCalls {
					asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: toolCallArgumentsJSON(tc),
						},
					})
				}
				chatMessages = append(chatMessages, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
			} else {
				chatMessages = append(chatMessages, openai.AssistantMessage(m.Content))
			}
		case "tool":
			toolCallID := m.ToolCallID
			if toolCallID == "" {
				toolCallID = "unknown"
			}
			chatMessages = append(chatMessages, openai.ToolMessage(m.Content, toolCallID))
		}
	}
	return chatMessages
}

// toolCallArgumentsJSON returns provider-stable tool argument JSON.
// Prefer the raw ArgsStr from the model so re-sends match the prior completion bytes.
func toolCallArgumentsJSON(tc ToolCall) string {
	if s := strings.TrimSpace(tc.ArgsStr); s != "" && json.Valid([]byte(s)) {
		return tc.ArgsStr
	}

	// Falling back to re-marshaling breaks byte-level prompt-cache prefix
	// stability. Log this so cache misses can be traced to a root cause.
	if tc.ArgsStr != "" {
		// ArgsStr was set but invalid JSON — indicates a provider bug or
		// a message that was hand-constructed without preserving ArgsStr.
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr invalid, re-marshaling",
			"", map[string]interface{}{
				"name":    tc.Name,
				"id":      tc.ID,
				"argsStr": tc.ArgsStr,
			})
	} else if len(tc.Args) > 0 {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr empty, re-marshaling from map",
			"", map[string]interface{}{
				"name": tc.Name,
				"id":   tc.ID,
			})
	}

	if tc.Args == nil {
		return "{}"
	}
	argsJSON, err := json.Marshal(tc.Args)
	if err != nil {
		return "{}"
	}
	return string(argsJSON)
}

func (p *OpenAIProvider) GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool) (Response, error) {
	chatMessages := p.messagesToChat(messages)
	params := openai.ChatCompletionNewParams{
		Messages: chatMessages,
		Tools:    toolsToOpenAI(tools, allowedTools),
		Model:    p.model,
	}
	if p.promptCacheKey.Valid() {
		params.PromptCacheKey = p.promptCacheKey
	}
	resp, err := p.clientForModel().Chat.Completions.New(ctx, params)

	if err != nil {
		return Response{}, fmt.Errorf("openai api error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("no choices returned")
	}

	var toolCalls []ToolCall
	for _, tc := range resp.Choices[0].Message.ToolCalls {
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return Response{}, fmt.Errorf("failed to unmarshal tool call arguments: %w", err)
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:      tc.ID,
			Name:    tc.Function.Name,
			Args:    args,
			ArgsStr: tc.Function.Arguments,
		})
	}

	content := resp.Choices[0].Message.Content
	msg := resp.Choices[0].Message
	extras := extraFieldsFromMessage(msg)
	reasoning := primaryDisplayFromExtrasMap(extras)
	display := content
	if display == "" {
		display = reasoning
	}
	if msg.Refusal != "" && display == "" {
		display = msg.Refusal
	}
	logNonStreamResponse(p.model, "non-stream", content, msg.Refusal, display, extras, toolCalls, usageFromOpenAI(resp.Usage))
	return Response{
		Content:   display,
		Reasoning: reasoning,
		ToolCalls: toolCalls,
		Usage:     usageFromOpenAI(resp.Usage),
	}, nil
}

func (p *OpenAIProvider) GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool, h *StreamHandlers) (*StreamResult, error) {
	if h == nil {
		h = &StreamHandlers{}
	}
	onToken := h.OnToken
	if onToken == nil {
		onToken = func(string) {}
	}
	onThinking := h.OnThinkingToken
	if onThinking == nil {
		onThinking = func(string) {}
	}

	chatMessages := p.messagesToChat(messages)
	params := openai.ChatCompletionNewParams{
		Messages: chatMessages,
		Tools:    toolsToOpenAI(tools, allowedTools),
		Model:    p.model,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}
	if p.promptCacheKey.Valid() {
		params.PromptCacheKey = p.promptCacheKey
	}
	stream := p.clientForModel().Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	var fullContent string
	var fullRefusal string
	var lastFinishReason string
	var streamUsage *Usage
	var tcAccums []tcAccum
	tcIndexMap := make(map[int]int)
	extras := newExtraFieldAccums()

	emitToolCallStart := func(tcIdx int, acc *tcAccum) {
		if acc.Started || acc.Name == "" || h.OnToolCallStart == nil {
			return
		}
		acc.Started = true
		h.OnToolCallStart(tcIdx, acc.ID, acc.Name)
	}

	emitToolCallArgsDelta := func(tcIdx int, acc *tcAccum, argsDelta string) {
		if argsDelta == "" || h.OnToolCallArgsDelta == nil {
			return
		}
		h.OnToolCallArgsDelta(tcIdx, acc.ID, acc.Name, argsDelta)
	}

	streamDone := false
	drainAfterDone := 0
	for stream.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		chunk := stream.Current()
		// Always capture usage — with include_usage it often arrives in a
		// trailing chunk after finish_reason (empty choices).
		if u := usageFromOpenAI(chunk.Usage); u != nil {
			streamUsage = u
		}
		if streamDone {
			// Drain a few trailer chunks for usage, then stop (some local
			// servers keep the HTTP stream open after finish_reason).
			drainAfterDone++
			if drainAfterDone >= 8 {
				break
			}
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta
		extras.addFromDelta(delta, onThinking)

		if delta.Content != "" {
			fullContent += delta.Content
			onToken(delta.Content)
		}
		if delta.Refusal != "" {
			fullRefusal += delta.Refusal
		}
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				var idx int
				tcAccums, idx = mergeToolCallDelta(tc, tcAccums, tcIndexMap)
				acc := &tcAccums[idx]
				if tc.Function.Name != "" {
					emitToolCallStart(acc.Index, acc)
				}
				if tc.Function.Arguments != "" {
					emitToolCallStart(acc.Index, acc)
					emitToolCallArgsDelta(acc.Index, acc, tc.Function.Arguments)
				}
			}
		}

		if choice.FinishReason != "" {
			switch choice.FinishReason {
			case "tool_calls":
				lastFinishReason = choice.FinishReason
				streamDone = true
			case "stop":
				// llama.cpp emits spurious stop while reasoning is still in
				// progress; only treat stop as terminal once real
				// content/refusal/tool-calls have arrived. A reasoning-carrying
				// delta is NOT sufficient — the spurious stop often rides on the
				// same chunk as a reasoning token.
				if fullContent != "" || fullRefusal != "" || len(tcAccums) > 0 {
					lastFinishReason = choice.FinishReason
					streamDone = true
				}
			case "length", "content_filter":
				lastFinishReason = choice.FinishReason
				streamDone = true
			default:
				lastFinishReason = choice.FinishReason
				streamDone = true
			}
		} else if len(tcAccums) > 0 && toolAccumsStreamComplete(tcAccums) && deltaIsTerminalToolSignal(delta, true) {
			// llama.cpp often sends an empty {} delta after tool args but keeps
			// the HTTP connection open without [DONE] or finish_reason. Only
			// treat it as terminal once the accumulated args are actually
			// complete JSON, otherwise an empty keepalive between arg
			// fragments would end the stream prematurely.
			lastFinishReason = "tool_calls"
			break
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := stream.Err(); err != nil {
		if h.OnStreamEnd != nil {
			h.OnStreamEnd()
		}
		if h.OnRecoverPartialStream != nil {
			h.OnRecoverPartialStream()
		}
		resp, fbErr := p.GenerateResponse(ctx, messages, allowedTools, tools)
		if fbErr != nil {
			return nil, fmt.Errorf("stream error: %w (non-streaming fallback also failed: %v)", err, fbErr)
		}
		if resp.Reasoning != "" {
			onThinking(resp.Reasoning)
		}
		if resp.Content != "" && (resp.Reasoning == "" || resp.Content != resp.Reasoning) {
			onToken(resp.Content)
		}
		return &StreamResult{
			Content:       resp.Content,
			ToolCalls:     resp.ToolCalls,
			Usage:         resp.Usage,
			PartialStream: len(tcAccums) > 0 || fullContent != "" || extras.textLen() > 0,
		}, nil
	}

	var toolCalls []ToolCall
	for _, acc := range tcAccums {
		if acc.Name == "" {
			continue
		}
		var args map[string]interface{}
		var argsErr string
		if strings.TrimSpace(acc.ArgsStr) == "" {
			args = map[string]interface{}{}
		} else {
			parsed, parseErr := parseToolCallArgs(acc.ArgsStr)
			if parseErr != nil {
				args = map[string]interface{}{}
				argsErr = parseErr.Error()
			} else {
				args = parsed
			}
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		toolCalls = append(toolCalls, ToolCall{
			Index:     acc.Index,
			ID:        acc.ID,
			Name:      acc.Name,
			Args:      args,
			ArgsStr:   acc.ArgsStr,
			ArgsError: argsErr,
		})
	}

	content := fullContent
	if content == "" && fullRefusal != "" {
		content = fullRefusal
	}
	if content == "" {
		content = extras.primaryDisplayText()
	}

	if lastFinishReason == "" && (content != "" || len(tcAccums) > 0) {
		if len(tcAccums) > 0 {
			lastFinishReason = "tool_calls"
		} else {
			lastFinishReason = "stop"
		}
	}

	if lastFinishReason == "" && content == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("stream ended without finish_reason")
	}

	return &StreamResult{
		Content:   content,
		ToolCalls: toolCalls,
		Usage:     streamUsage,
	}, nil
}

func (p *OpenAIProvider) ModelContextLimit(ctx context.Context) (int, error) {
	models := p.listModels(ctx)

	if len(models) == 1 {
		sole := models[0]
		if sole.ID != "" {
			p.model = sole.ID
		}
		if limit := parseContextLimitFromJSON(sole.RawJSON()); limit > 0 {
			return limit, nil
		}
	}

	for _, model := range models {
		if model.ID != p.model {
			continue
		}
		if limit := parseContextLimitFromJSON(model.RawJSON()); limit > 0 {
			return limit, nil
		}
	}

	// Try the model's known client first, then the other endpoint as a
	// fallback (important when one endpoint is temporarily unreachable).
	clients := []*openai.Client{p.clientForModel()}
	if p.zenClient != nil && p.goClient != nil {
		known := p.clientForModel()
		if known == p.zenClient {
			clients = append(clients, p.goClient)
		} else if known == p.goClient {
			clients = append(clients, p.zenClient)
		} else {
			clients = append(clients, p.zenClient, p.goClient)
		}
	}
	for _, c := range clients {
		model, err := c.Models.Get(ctx, p.model)
		if err != nil {
			continue
		}
		if limit := parseContextLimitFromJSON(model.RawJSON()); limit > 0 {
			return limit, nil
		}
	}

	if limit := inferContextLimitFromModelName(p.model); limit > 0 {
		return limit, nil
	}

	return resolveContextLimit("", p.model), nil
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models := p.listModels(ctx)
	out := make([]ModelInfo, 0, len(models))
	current := p.model
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:           m.ID,
			ContextLimit: resolveContextLimit(m.RawJSON(), m.ID),
			Current:      m.ID == current,
		})
	}
	return out, nil
}

func (p *OpenAIProvider) SetModel(id string) error {
	p.model = id
	return nil
}

// resolveContextLimit tries JSON fields first, then falls back to model-name inference.
func resolveContextLimit(rawJSON, modelID string) int {
	if limit := parseContextLimitFromJSON(rawJSON); limit > 0 {
		return limit
	}
	if limit := inferContextLimitFromModelName(modelID); limit > 0 {
		return limit
	}
	return 128000
}
