package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

const (
	openCodeZenBaseURL = "https://opencode.ai/zen/v1/"
	openCodeGoBaseURL  = "https://opencode.ai/zen/go/v1/"
	// modelsCacheTTL avoids repeated full catalog fetches during startup
	// (ValidateRestoredModel → ListModels + ModelContextLimit) and /models flows.
	modelsCacheTTL = 60 * time.Second
	// modelsCatalogTimeout bounds /v1/models and related catalog lookups.
	// Separate from the long SSE stream idle timeout so a hung catalog
	// endpoint cannot pin startup or the web WS loop for minutes.
	modelsCatalogTimeout = 8 * time.Second
)

// isOpencodeURL reports whether baseURL points to an OpenCode endpoint that
// should also expose the Go model family at openCodeGoBaseURL.
func isOpencodeURL(baseURL string) bool {
	return strings.Contains(baseURL, "opencode.ai")
}

type OpenAIProvider struct {
	client      openai.Client  // primary (user-configured; fallback for non-OpenCode)
	zenClient   *openai.Client // OpenCode Zen endpoint
	goClient    *openai.Client // OpenCode Go endpoint
	model       string
	modelClient map[string]*openai.Client // model ID → client routing
	// promptCacheKey scopes provider-side prompt caching (defaults to none).
	promptCacheKey param.Opt[string]

	modelsMu       sync.RWMutex
	modelsCache    []openai.Model
	modelsCachedAt time.Time // zero means no successful cache entry
	modelsFetch    *modelsFetch
}

type modelsFetch struct {
	done   chan struct{}
	models []openai.Model
	err    error
}

func (p *OpenAIProvider) ModelName() string {
	return p.currentModel()
}

func (p *OpenAIProvider) currentModel() string {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return p.model
}

func (p *OpenAIProvider) cachedModelsLocked() ([]openai.Model, bool) {
	if p.modelsCachedAt.IsZero() || time.Since(p.modelsCachedAt) >= modelsCacheTTL {
		return nil, false
	}
	return p.modelsCache, true
}

func (p *OpenAIProvider) listModels(ctx context.Context) ([]openai.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
	defer cancel()

	p.modelsMu.RLock()
	if models, ok := p.cachedModelsLocked(); ok {
		out := append([]openai.Model(nil), models...)
		p.modelsMu.RUnlock()
		return out, nil
	}
	if f := p.modelsFetch; f != nil {
		p.modelsMu.RUnlock()
		return waitModelsFetch(ctx, f)
	}
	p.modelsMu.RUnlock()

	p.modelsMu.Lock()
	if models, ok := p.cachedModelsLocked(); ok {
		out := append([]openai.Model(nil), models...)
		p.modelsMu.Unlock()
		return out, nil
	}
	if f := p.modelsFetch; f != nil {
		p.modelsMu.Unlock()
		return waitModelsFetch(ctx, f)
	}
	f := &modelsFetch{done: make(chan struct{})}
	p.modelsFetch = f
	p.modelsMu.Unlock()

	models, routing, err := p.fetchModels(ctx)

	p.modelsMu.Lock()
	f.models, f.err = models, err
	if err == nil {
		p.modelsCache = models
		p.modelsCachedAt = time.Now()
		if routing != nil {
			p.modelClient = routing
		}
	}
	p.modelsFetch = nil
	close(f.done)
	p.modelsMu.Unlock()
	if err != nil {
		return nil, err
	}
	return append([]openai.Model(nil), models...), nil
}

func waitModelsFetch(ctx context.Context, f *modelsFetch) ([]openai.Model, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return append([]openai.Model(nil), f.models...), nil
	}
}

// fetchModels loads the model catalog from the provider. OpenCode zen and go
// endpoints are queried in parallel.
func (p *OpenAIProvider) fetchModels(ctx context.Context) ([]openai.Model, map[string]*openai.Client, error) {
	type result struct {
		models  []openai.Model
		routing map[string]*openai.Client
		err     error
	}
	query := func(c *openai.Client) result {
		var models []openai.Model
		routing := make(map[string]*openai.Client)
		pager := c.Models.ListAutoPaging(ctx)
		for pager.Next() {
			m := pager.Current()
			models = append(models, m)
			routing[m.ID] = c
		}
		return result{models: models, routing: routing, err: pager.Err()}
	}

	if p.zenClient != nil && p.goClient != nil {
		var zenRes, goRes result
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			zenRes = query(p.zenClient)
		}()
		go func() {
			defer wg.Done()
			goRes = query(p.goClient)
		}()
		wg.Wait()

		routing := make(map[string]*openai.Client, len(zenRes.routing)+len(goRes.routing))
		models := make([]openai.Model, 0, len(zenRes.models)+len(goRes.models))
		models = append(models, zenRes.models...)
		for id, c := range zenRes.routing {
			routing[id] = c
		}
		models = append(models, goRes.models...)
		for id, c := range goRes.routing {
			routing[id] = c
		}
		var errs []error
		if zenRes.err != nil {
			errs = append(errs, zenRes.err)
		}
		if goRes.err != nil {
			errs = append(errs, goRes.err)
		}
		if len(models) == 0 && len(errs) > 0 {
			return nil, nil, errors.Join(errs...)
		}
		return models, routing, nil
	}

	res := query(&p.client)
	if len(res.models) == 0 && res.err != nil {
		return nil, nil, res.err
	}
	return res.models, res.routing, nil
}

func NewOpenAIProvider(apiKey string, model string, baseURL string) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithHTTPClient(newSSEHTTPClient()),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
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
			nopts := []option.RequestOption{
				option.WithHTTPClient(newSSEHTTPClient()),
				option.WithBaseURL(url),
			}
			if apiKey != "" {
				nopts = append(nopts, option.WithAPIKey(apiKey))
			}
			c := openai.NewClient(nopts...)
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
	if key == "" {
		p.promptCacheKey = param.Opt[string]{}
		return
	}
	p.promptCacheKey = param.NewOpt(key)
}

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
		_, err := p.zenClient.Models.Get(probeCtx, model)
		if err == nil {
			chosen = p.zenClient
		}
	}
	if chosen == nil && p.goClient != nil {
		_, err := p.goClient.Models.Get(probeCtx, model)
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

	// Falling back to re-marshaling can diverge from provider ArgsStr bytes.
	// Log this so cache misses can be traced to a root cause.
	if tc.ArgsStr != "" {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr invalid, re-marshaling",
			"", map[string]interface{}{
				"name":    tc.Name,
				"id":      tc.ID,
				"argsStr": tc.ArgsStr,
			})
	} else if len(tc.Args) > 0 {
		// ArgsStr was empty despite having parsed args — this happens
		// when ToolCalls are constructed manually. Marshal once and pin.
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr empty, re-marshaling from map",
			"", map[string]interface{}{
				"name": tc.Name,
				"id":   tc.ID,
			})
	}

	if tc.Args == nil {
		tc.ArgsStr = "{}"
		return tc.ArgsStr
	}
	argsJSON, err := json.Marshal(tc.Args)
	if err != nil {
		tc.ArgsStr = "{}"
		return tc.ArgsStr
	}
	tc.ArgsStr = string(argsJSON)
	return tc.ArgsStr
}

// StabilizeToolCallArgs pins ArgsStr to the bytes that will be sent on the
// wire. Call when appending tool calls to history so token estimates and
// later re-sends share one stable prefix.
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
	stream := p.clientForModel().Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	var fullContent string
	var fullRefusal string
	var fullReasoning string
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
		extras.addFromDelta(delta, onThinking, &fullReasoning)

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
		if resp.Content != "" {
			onToken(resp.Content)
		} else if resp.Refusal != "" {
			onToken(resp.Refusal)
		}
		return &StreamResult{
			Content:       resp.Content,
			Reasoning:     resp.Reasoning,
			Refusal:       resp.Refusal,
			ToolCalls:     resp.ToolCalls,
			Usage:         resp.Usage,
			PartialStream: len(tcAccums) > 0 || fullContent != "" || fullRefusal != "" || extras.textLen() > 0,
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

	// Fallback: if no tool calls were extracted from the stream deltas,
	// try to find embedded tool call patterns in the reasoning/content text.
	if len(toolCalls) == 0 && (fullReasoning != "" || fullContent != "") {
		extractedCalls := extractToolCallsFromText(fullReasoning + fullContent)
		if len(extractedCalls) > 0 {
			toolCalls = extractedCalls
		}
	}

	// Keep content/reasoning/refusal separate so re-sends match the original
	// completion shape (required for provider prompt-cache prefix hits).
	content := fullContent

	if lastFinishReason == "" && (content != "" || fullRefusal != "" || fullReasoning != "" || len(tcAccums) > 0) {
		if len(tcAccums) > 0 {
			lastFinishReason = "tool_calls"
		} else {
			lastFinishReason = "stop"
		}
	}

	if lastFinishReason == "" && content == "" && fullRefusal == "" && fullReasoning == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("stream ended without finish_reason")
	}

	return &StreamResult{
		Content:   content,
		Reasoning: fullReasoning,
		Refusal:   fullRefusal,
		ToolCalls: toolCalls,
		Usage:     streamUsage,
	}, nil
}

func (p *OpenAIProvider) ModelContextLimit(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
	defer cancel()

	models, _ := p.listModels(ctx)

	p.modelsMu.Lock()
	if len(models) == 1 {
		sole := models[0]
		if sole.ID != "" {
			p.model = sole.ID
		}
		p.modelsMu.Unlock()
		if limit := parseContextLimitFromJSON(sole.RawJSON()); limit > 0 {
			return limit, nil
		}
	} else {
		p.modelsMu.Unlock()
	}

	modelName := p.currentModel()
	for _, model := range models {
		if model.ID != modelName {
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
		model, err := c.Models.Get(ctx, modelName)
		if err != nil {
			continue
		}
		if limit := parseContextLimitFromJSON(model.RawJSON()); limit > 0 {
			return limit, nil
		}
	}

	if limit := inferContextLimitFromModelName(modelName); limit > 0 {
		return limit, nil
	}

	return resolveContextLimit("", modelName), nil
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.listModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(models))
	current := p.currentModel()
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
	p.modelsMu.Lock()
	p.model = id
	p.modelsMu.Unlock()
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
