package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// clientForModel returns the openai.Client that should serve the currently
// selected model.  When modelClient has been populated by a ListModels call
// the lookup is cheap; otherwise it does a one-time discovery probe against
// both endpoints to populate the cache.
func (p *OpenAIProvider) clientForModel() *openai.Client {
	p.modelsMu.RLock()
	if p.modelClient != nil {
		if c, ok := p.modelClient[p.model]; ok {
			p.modelsMu.RUnlock()
			return c
		}
	}
	model := p.model
	p.modelsMu.RUnlock()

	// Discovery: probe Zen first, then Go (deterministic order).
	// Do not hold modelsMu across network I/O. Bound probes so a hung
	// OpenCode endpoint cannot stall the first chat request indefinitely.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), modelsCatalogTimeout)
	defer probeCancel()
	var chosen *openai.Client
	if p.zenClient != nil {
		catalog := p.zenCatalogClient
		if catalog == nil {
			catalog = p.zenClient
		}
		_, err := catalog.Models.Get(probeCtx, model)
		if err == nil {
			chosen = p.zenClient
		}
	}
	if chosen == nil && p.goClient != nil {
		catalog := p.goCatalogClient
		if catalog == nil {
			catalog = p.goClient
		}
		_, err := catalog.Models.Get(probeCtx, model)
		if err == nil {
			chosen = p.goClient
		}
	}
	if chosen == nil {
		chosen = &p.client
	}

	p.modelsMu.Lock()
	if p.modelClient == nil {
		p.modelClient = make(map[string]*openai.Client)
	}
	// Another goroutine may have filled this in while we were probing.
	if c, ok := p.modelClient[model]; ok {
		p.modelsMu.Unlock()
		return c
	}
	p.modelClient[model] = chosen
	p.modelsMu.Unlock()
	return chosen
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
			// Always build an explicit assistant param so reasoning_content /
			// refusal round-trip on the wire. Folding them into Content would
			// diverge from the original completion bytes and bust prompt-cache
			// prefixes on providers that emit those fields.
			asst := openai.ChatCompletionAssistantMessageParam{}
			if m.Content != "" {
				asst.Content.OfString = param.NewOpt(m.Content)
			}
			if m.Refusal != "" {
				asst.Refusal = param.NewOpt(m.Refusal)
			}
			if m.Reasoning != "" {
				asst.SetExtraFields(map[string]any{
					"reasoning_content": m.Reasoning,
				})
			}
			for i := range m.ToolCalls {
				asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallParam{
					ID: m.ToolCalls[i].ID,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      m.ToolCalls[i].Name,
						Arguments: toolCallArgumentsJSON(&m.ToolCalls[i]),
					},
				})
			}
			chatMessages = append(chatMessages, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
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
// Prefer the raw ArgsStr from the model so re-sends match the bytes that
// established the prompt-cache prefix. Accepts a pointer so the exact wire
// bytes can be pinned in tc.ArgsStr for all future turns.
//
// encoding/json already sorts map keys, so a remarsal fallback is
// deterministic — but it still usually differs from the provider's original
// ArgsStr (spacing/key order), which is why pinning matters.
func toolCallArgumentsJSON(tc *ToolCall) string {
	if s := strings.TrimSpace(tc.ArgsStr); s != "" && json.Valid([]byte(s)) {
		// Pin the exact bytes we send so history stays aligned with the wire.
		if tc.ArgsStr != s {
			tc.ArgsStr = s
		}
		return tc.ArgsStr
	}

	// Remarsal for the wire only. Never overwrite a non-empty ArgsStr — even
	// when invalid/truncated — so later turns keep the original history bytes
	// and the drift detector can still see the provider fragment.
	hadArgsStr := tc.ArgsStr != ""
	if hadArgsStr {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr invalid, remarsaling without overwrite",
			"", map[string]interface{}{
				"name":       tc.Name,
				"id":         tc.ID,
				"argsStr":    tc.ArgsStr,
				"argsStrLen": len(tc.ArgsStr),
			})
	} else if len(tc.Args) > 0 {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr empty, re-marshaling from map",
			"", map[string]interface{}{
				"name": tc.Name,
				"id":   tc.ID,
			})
	}

	marshaled := marshalToolArgsJSON(tc.Args)
	if !hadArgsStr {
		tc.ArgsStr = marshaled
	}
	return marshaled
}

// marshalToolArgsJSON encodes tool args without HTML escaping so remarsaled
// bytes stay closer to typical provider JSON (`<` not `\u003c`).
func marshalToolArgsJSON(args map[string]interface{}) string {
	if args == nil {
		return "{}"
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return "{}"
	}
	// Encoder always appends a trailing newline; strip it for stable ArgsStr.
	return strings.TrimSuffix(buf.String(), "\n")
}

// StabilizeToolCallArgs pins ArgsStr to the bytes that will be sent on the
// wire when ArgsStr is empty. Non-empty ArgsStr (valid or not) is left alone
// so provider/history bytes are not rewritten mid-session.
func StabilizeToolCallArgs(tc *ToolCall) {
	_ = toolCallArgumentsJSON(tc)
}

func (p *OpenAIProvider) GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool) (Response, error) {
	chatMessages := p.messagesToChat(messages)
	model := p.currentModel()
	params := openai.ChatCompletionNewParams{
		Messages: chatMessages,
		Tools:    toolsToOpenAI(tools, allowedTools),
		Model:    model,
	}
	if p.promptCacheKey.Valid() {
		params.PromptCacheKey = p.promptCacheKey
	}
	p.applyChatCompletionExtras(ctx, &params)
	p.applyThinkingLevel(ctx, &params)
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
	// Keep content/reasoning/refusal separate. Providers that emit
	// reasoning_content or refusal expect those fields echoed back; stuffing
	// them into Content changes the wire bytes and busts prompt-cache prefixes.
	display := content
	if display == "" {
		display = reasoning
	}
	if msg.Refusal != "" && display == "" {
		display = msg.Refusal
	}
	logNonStreamResponse(model, "non-stream", content, msg.Refusal, display, extras, toolCalls, usageFromOpenAI(resp.Usage))
	return Response{
		Content:   content,
		Reasoning: reasoning,
		Refusal:   msg.Refusal,
		ToolCalls: toolCalls,
		Usage:     usageFromOpenAI(resp.Usage),
	}, nil
}

func (p *OpenAIProvider) GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool, h *StreamHandlers) (*StreamResult, error) {
	h = ensureStreamCallbacks(h)
	onToken := h.OnToken
	onThinking := h.OnThinkingToken

	chatMessages := p.messagesToChat(messages)
	model := p.currentModel()
	params := openai.ChatCompletionNewParams{
		Messages: chatMessages,
		Tools:    toolsToOpenAI(tools, allowedTools),
		Model:    model,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}
	if p.promptCacheKey.Valid() {
		params.PromptCacheKey = p.promptCacheKey
	}
	p.applyChatCompletionExtras(ctx, &params)
	p.applyThinkingLevel(ctx, &params)
	stream := p.clientForModel().Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	// stream.Next() can block on the response body even after ctx cancel if
	// the transport is slow to abort. Closing the stream from a watcher
	// unblocks Next promptly (important for --web Ctrl+C).

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-stopWatch:
		}
	}()

	acc := newStreamAccumulator()

	for stream.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stop := acc.processChunk(stream.Current(), onToken, onThinking, h); stop {
			break
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := stream.Err(); err != nil {
		return p.handleStreamFallback(ctx, messages, allowedTools, tools, h, err, acc)
	}

	return acc.buildResult()
}

// handleStreamFallback is called when a streaming error occurs. It attempts a
// non-streaming fallback to recover partial results.
func (p *OpenAIProvider) handleStreamFallback(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool, h *StreamHandlers, streamErr error, acc *streamAccumulator) (*StreamResult, error) {
	if h.OnStreamEnd != nil {
		h.OnStreamEnd()
	}
	if h.OnRecoverPartialStream != nil {
		h.OnRecoverPartialStream()
	}
	resp, fbErr := p.GenerateResponse(ctx, messages, allowedTools, tools)
	if fbErr != nil {
		return nil, fmt.Errorf("stream error: %w (non-streaming fallback also failed: %v)", streamErr, fbErr)
	}
	if resp.Reasoning != "" && h.OnThinkingToken != nil {
		h.OnThinkingToken(resp.Reasoning)
	}
	if resp.Content != "" && h.OnToken != nil {
		h.OnToken(resp.Content)
	} else if resp.Refusal != "" && h.OnToken != nil {
		h.OnToken(resp.Refusal)
	}
	return &StreamResult{
		Content:       resp.Content,
		Reasoning:     resp.Reasoning,
		Refusal:       resp.Refusal,
		ToolCalls:     resp.ToolCalls,
		Usage:         resp.Usage,
		PartialStream: len(acc.tcAccums) > 0 || acc.fullContent.Len() > 0 || acc.fullRefusal.Len() > 0 || acc.extras.textLen() > 0,
	}, nil
}
