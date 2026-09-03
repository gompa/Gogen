package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gogen/internal/debuglog"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// clientForModel returns the openai.Client that should serve the currently
// selected model.  When modelClient has been populated by a ListModels call
// the lookup is cheap; otherwise it does a one-time catalog fetch to
// populate the cache.
//
// OpenCode note: the gateways do not implement GET /models/{model}, so a
// per-model probe can never tell the Zen endpoint from the Go endpoint —
// discovery must build routing from the full /models lists instead. Go
// models take precedence over Zen models (see fetchModels).
func (p *OpenAIProvider) clientForModel(ctx context.Context) *openai.Client {
	p.modelsMu.RLock()
	if p.modelClient != nil {
		if c, ok := p.modelClient[p.model]; ok {
			p.modelsMu.RUnlock()
			return c
		}
	}
	model := p.model
	p.modelsMu.RUnlock()

	// Catalog-based discovery (OpenCode, or multi-profile providers): a
	// bounded listModels fetch populates modelClient with the exact model →
	// endpoint mapping. The modelsCache TTL and the modelsFetch single-flight
	// make repeat misses cheap, and the failure backoff (modelsFetchFailedAt)
	// stops a dead endpoint from being re-probed on every request. Do not
	// hold modelsMu across network I/O; bound the fetch so a hung endpoint
	// cannot stall the first chat request indefinitely. The caller's context
	// is threaded through so Ctrl+C aborts an in-flight catalog fetch instead
	// of waiting out the timeout. OpenCode is detected from the CURRENT
	// profile set (hasOpenCodeProfile), so discovery stays live-correct
	// after SetProfiles.
	if p.hasMultipleProfiles() || p.hasOpenCodeProfile() {
		if !p.catalogFetchOnBackoff() {
			fetchCtx, cancel := context.WithTimeout(ctx, modelsCatalogTimeout)
			_, _ = p.listModels(fetchCtx)
			cancel()
		}
		p.modelsMu.RLock()
		c, ok := p.modelClient[model]
		p.modelsMu.RUnlock()
		if ok {
			return c
		}
		// Model is not listed on any endpoint — fall through to the
		// last-known owner, then the deterministic fallback (models.dev,
		// then the default client).
	}

	chosen, ownerName := p.ownerClientForModel(model)
	if chosen == nil {
		chosen = p.inferOpenCodeEndpoint(model)
	}
	if chosen == nil {
		chosen = p.fallbackClient()
	}

	p.modelsMu.Lock()
	if p.modelClient == nil {
		p.modelClient = make(map[string]*openai.Client)
	}
	// Another goroutine may have filled this in while we were discovering.
	if c, ok := p.modelClient[model]; ok {
		p.modelsMu.Unlock()
		return c
	}
	p.modelClient[model] = chosen
	// Record the owner for a fallback resolved through the ownership record
	// (it may have come from the shared registry, not this provider's own
	// record): the next catalog merge must apply the sticky-ownership rule
	// to this model instead of treating it as ownerless and letting a
	// surviving endpoint's same-ID model steal it.
	if ownerName != "" {
		p.modelOwner[model] = ownerName
	}
	p.modelsMu.Unlock()
	return chosen
}

// inferOpenCodeEndpoint picks the client that should serve model on OpenCode
// without a catalog fetch, using the models.dev registry (in-memory/disk
// cached; never blocks on the network). Go takes precedence: a model listed
// on both the Go and Zen catalogs routes to the Go endpoint. Resolution is
// profile-derived: the CURRENT registered OpenCode profiles' zen/go clients
// are used, so SetProfiles swapping the endpoint set never routes through
// stale construction-time clients. Returns nil when nothing can be
// determined (cold registry, model unknown, or no OpenCode profile is
// registered), in which case the caller falls back to the default client.
func (p *OpenAIProvider) inferOpenCodeEndpoint(model string) *openai.Client {
	if p.modelInfo == nil || model == "" {
		return nil
	}
	p.modelsMu.RLock()
	profiles := p.profiles
	p.modelsMu.RUnlock()
	for _, prof := range profiles {
		if prof.zenStream == nil || prof.goStream == nil {
			continue // not an OpenCode profile
		}
		if _, _, _, _, err := p.modelInfo.Resolve(openCodeGoBaseURL, model); err == nil {
			return prof.goStream
		}
		if _, _, _, _, err := p.modelInfo.Resolve(openCodeZenBaseURL, model); err == nil {
			return prof.zenStream
		}
	}
	return nil
}

// ownerClientForModel returns the stream client of the profile that last
// served model (and that profile's name), when the profile is still
// registered: the provider's own sticky-ownership record (modelOwner, from
// its successful catalog fetches) first, then the process-shared
// OwnerRegistry (owners learned by sibling providers — a fresh session or
// subagent provider inherits routing knowledge it cannot gain itself while
// its owning endpoint is down). This runs BEFORE the models.dev inference
// and the default-client fallback: a model the user's local endpoint served
// must keep going there while that endpoint is unreachable, not be re-homed
// to the default profile (which may be a remote gateway that does not serve
// it). Returns a nil client when no owner is known or the owner profile is
// no longer registered, in which case the caller falls through to the
// deterministic fallback.
func (p *OpenAIProvider) ownerClientForModel(model string) (*openai.Client, string) {
	if model == "" {
		return nil, ""
	}
	p.modelsMu.RLock()
	name, ok := p.modelOwner[model]
	profiles := p.profiles
	reg := p.ownerRegistry
	p.modelsMu.RUnlock()
	if !ok && reg != nil {
		name, ok = reg.Owner(model)
	}
	if !ok {
		return nil, ""
	}
	for _, prof := range profiles {
		if prof.name != name {
			continue
		}
		if prof.zenStream != nil && prof.goStream != nil {
			// OpenCode profile: pick the twin the registry says serves the
			// model (a Go-subscription model must not hit the Zen endpoint);
			// a cold registry falls back to the profile's configured
			// endpoint.
			if p.modelInfo != nil {
				if _, _, _, _, err := p.modelInfo.Resolve(openCodeGoBaseURL, model); err == nil {
					return prof.goStream, name
				}
				if _, _, _, _, err := p.modelInfo.Resolve(openCodeZenBaseURL, model); err == nil {
					return prof.zenStream, name
				}
			}
			return prof.stream, name
		}
		return prof.stream, name
	}
	return nil, ""
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
			if !m.HasImages() {
				chatMessages = append(chatMessages, openai.UserMessage(m.Content))
				continue
			}
			// Vision input: build a multi-part content array (text + one
			// image_url part per image). The text part is omitted entirely
			// when the message has no text, mirroring how the provider
			// expects a pure-image prompt.
			parts := make([]openai.ChatCompletionContentPartUnionParam, 0, 1+len(m.Images))
			if m.Content != "" {
				parts = append(parts, openai.TextContentPart(m.Content))
			}
			for _, img := range m.Images {
				if img.DataURL == "" {
					continue
				}
				detail := img.Detail
				if detail == "" {
					detail = "auto"
				}
				parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
					URL:    img.DataURL,
					Detail: detail,
				}))
			}
			if len(parts) == 0 {
				// All images were empty; fall back to plain text so the
				// request still sends something coherent.
				chatMessages = append(chatMessages, openai.UserMessage(m.Content))
				continue
			}
			chatMessages = append(chatMessages, openai.UserMessage(parts))
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
	// Fast path: ArgsStr was already validated (and trimmed) by
	// StabilizeToolCallArgs or session restore, so re-running json.Valid —
	// once per historical tool call on every API request — is pure overhead.
	// The flag is only ever set before the message is published, never by
	// this serializer (which runs outside the stats lock on shared
	// stabilized ToolCalls), so a valid flag is always trustworthy.
	if tc.ArgsJSONValid {
		return tc.ArgsStr
	}
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
			"", map[string]any{
				"name":       tc.Name,
				"id":         tc.ID,
				"argsStr":    tc.ArgsStr,
				"argsStrLen": len(tc.ArgsStr),
			})
	} else if len(tc.Args) > 0 {
		debuglog.Write("llm/tool_args", "toolCallArgumentsJSON: ArgsStr empty, re-marshaling from map",
			"", map[string]any{
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
func marshalToolArgsJSON(args map[string]any) string {
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
// so provider/history bytes are not rewritten mid-session. Records whether
// the resulting ArgsStr is the exact trimmed valid wire bytes in
// ArgsJSONValid, so the wire serializer can skip re-validating stabilized
// tool calls on every request.
func StabilizeToolCallArgs(tc *ToolCall) {
	_ = toolCallArgumentsJSON(tc)
	s := strings.TrimSpace(tc.ArgsStr)
	tc.ArgsJSONValid = s != "" && json.Valid([]byte(s))
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
	resp, err := p.clientForModel(ctx).Chat.Completions.New(ctx, params)

	if err != nil {
		// wrapContextWindowError classifies a context-window refusal so the
		// agent run loop can recover in-loop (forced compaction + retry)
		// instead of aborting the turn.
		return Response{}, wrapContextWindowError(fmt.Errorf("openai api error: %w", err))
	}

	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("no choices returned")
	}

	var toolCalls []ToolCall
	for _, tc := range resp.Choices[0].Message.ToolCalls {
		var args map[string]any
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
		Model:     resp.Model,
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
	stream := p.clientForModel(ctx).Chat.Completions.NewStreaming(ctx, params)
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

	// reasoningStopGrace bounds the wait after a reasoning-only stop: a
	// two-phase stream (reasoning → content) legitimately continues with
	// content, but a provider that sends the stop and then holds the
	// connection open without [DONE] would otherwise block the read for the
	// full idle timeout. The timer is armed once the stop is seen and
	// re-armed on every subsequent chunk while still pending (so an active
	// but slow thinking stream is never cut short); it is disarmed the
	// moment content/refusal/tool-calls resume. Closing the stream from the
	// timer unblocks Next() promptly (same mechanism as the ctx watcher).
	grace := reasoningStopGrace()
	var stopGrace *time.Timer
	for stream.Next() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stop := acc.processChunk(stream.Current(), onToken, onThinking, h); stop {
			break
		}
		if acc.stopPending && grace > 0 {
			if stopGrace == nil {
				stopGrace = time.AfterFunc(grace, func() {
					acc.graceExpired.Store(true)
					_ = stream.Close()
				})
			} else {
				stopGrace.Reset(grace)
			}
		} else if stopGrace != nil {
			stopGrace.Stop()
			stopGrace = nil
		}
	}
	if stopGrace != nil {
		stopGrace.Stop()
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := stream.Err(); err != nil {
		if acc.graceExpired.Load() {
			// The stream was closed by the reasoning-stop grace timer, not
			// by a real failure: return the accumulated reasoning as a
			// complete response (the provider held the connection open
			// instead of sending [DONE]) rather than triggering the
			// non-streaming fallback, which would re-request the turn.
			return acc.buildResult()
		}
		return p.handleStreamFallback(ctx, messages, allowedTools, tools, h, err, acc)
	}

	return acc.buildResult()
}

// handleStreamFallback is called when a streaming error occurs. It attempts a
// non-streaming fallback to recover partial results.
func (p *OpenAIProvider) handleStreamFallback(ctx context.Context, messages []Message, allowedTools map[string]struct{}, tools []Tool, h *StreamHandlers, streamErr error, acc *streamAccumulator) (*StreamResult, error) {
	// Deliberately NOT firing OnStreamEnd / OnRecoverPartialStream here: the
	// agent loop owns those and fires them exactly once when it finalizes the
	// round (finishStreamUI on the content path, the tool-call branch + the
	// PartialStream check on the tool path). Firing them here too would
	// double-deliver stream_end frames / streamRoundEndMsg on every stream
	// failure; the error path that runs when the fallback also fails already
	// finalizes the round itself. h came from GenerateResponseStream, which
	// ran ensureStreamCallbacks, so the callbacks used below are non-nil.
	resp, fbErr := p.GenerateResponse(ctx, messages, allowedTools, tools)
	if fbErr != nil {
		// Classify the STREAM error (the primary cause): a context-window
		// refusal that also fails the fallback must reach the agent run
		// loop still classified, so it recovers in-loop instead of
		// aborting the turn.
		return nil, fmt.Errorf("stream error: %w (non-streaming fallback also failed: %v)", wrapContextWindowError(streamErr), fbErr)
	}
	// Re-render only the suffix beyond what the failed stream already
	// emitted, so the live bubble does not show the recovered text twice (a
	// retry of the same request typically re-generates the same opening, and
	// the client already rendered the partial stream). The StreamResult below
	// still carries the complete recovery for persistence — only the live
	// re-render is trimmed.
	reasoning := trimRecoveredText(acc.fullReasoning.String(), resp.Reasoning)
	content := trimRecoveredText(acc.fullContent.String(), resp.Content)
	refusal := trimRecoveredText(acc.fullRefusal.String(), resp.Refusal)
	if reasoning != "" {
		h.OnThinkingToken(reasoning)
	}
	if content != "" {
		h.OnToken(content)
	} else if refusal != "" {
		h.OnToken(refusal)
	}
	// The non-streaming fallback may omit the model field; the failed stream
	// already reported one (router endpoints resolve aliases server-side).
	model := resp.Model
	if model == "" {
		model = acc.model
	}
	return &StreamResult{
		Content:       resp.Content,
		Reasoning:     resp.Reasoning,
		Refusal:       resp.Refusal,
		ToolCalls:     resp.ToolCalls,
		Usage:         resp.Usage,
		PartialStream: len(acc.tcAccums) > 0 || acc.fullContent.Len() > 0 || acc.fullRefusal.Len() > 0 || acc.extras.textLen() > 0,
		Model:         model,
	}, nil
}

// trimRecoveredText drops the portion of a recovered response the client
// already saw streamed. It trims only when the streamed text is an exact
// byte prefix of the recovered text; a divergent re-generation is emitted in
// full so two different answers are never spliced into one bubble. Byte-wise
// comparison is safe: both strings reached this point via JSON decoding, so
// they are valid UTF-8 and a byte prefix of one is also a rune-aligned
// prefix.
func trimRecoveredText(streamed, recovered string) string {
	if streamed == "" || recovered == "" {
		return recovered
	}
	n := len(streamed)
	if n > len(recovered) {
		n = len(recovered)
	}
	if recovered[:n] != streamed {
		return recovered // no exact-prefix overlap: status quo
	}
	return recovered[n:]
}
