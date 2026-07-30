package llm

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gogen/internal/debuglog"
	"gogen/internal/modelinfo"

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
	// modelsCatalogTimeout bounds /v1/models for ListModels / UI catalog.
	// Separate from the long SSE stream idle timeout so a hung catalog
	// endpoint cannot pin startup or the web WS loop for minutes.
	modelsCatalogTimeout = 8 * time.Second
	// modelsLimitLookupTimeout bounds the /v1/models attempt inside
	// ModelContextLimit. Local llama.cpp answers in milliseconds; a hung
	// remote catalog must fail fast so models.dev / heuristics can run.
	// ListModels keeps modelsCatalogTimeout.
	modelsLimitLookupTimeout = 1500 * time.Millisecond
	// propsProbeTimeout bounds GET /props for chat_template_caps discovery.
	propsProbeTimeout = 1500 * time.Millisecond
)

// propsHTTPClient is a plain short-timeout client for capability probes.
// Intentionally not the SSE client (idle read deadlines are stream-oriented).
var propsHTTPClient = &http.Client{Timeout: propsProbeTimeout}

// isOpencodeURL reports whether baseURL points to an OpenCode endpoint that
// should also expose the Go model family at openCodeGoBaseURL.
func isOpencodeURL(baseURL string) bool {
	return strings.Contains(baseURL, "opencode.ai")
}

type OpenAIProvider struct {
	client           openai.Client  // primary streaming client
	catalogClient    *openai.Client // non-stream /v1/models (non-OpenCode)
	zenClient        *openai.Client // OpenCode Zen streaming
	zenCatalogClient *openai.Client // OpenCode Zen catalog
	goClient         *openai.Client // OpenCode Go streaming
	goCatalogClient  *openai.Client // OpenCode Go catalog
	model            string
	baseURL          string
	apiKey           string
	modelClient      map[string]*openai.Client // model ID → streaming client routing
	// promptCacheKey scopes provider-side prompt caching (defaults to none).
	promptCacheKey param.Opt[string]
	// modelInfo resolves context limits from models.dev (optional enrichment).
	modelInfo *modelinfo.Resolver

	modelsMu       sync.RWMutex
	modelsCache    []openai.Model
	modelsCachedAt time.Time // zero means no successful cache entry
	modelsFetch    *modelsFetch

	// propsCaps caches llama.cpp GET /props chat_template_caps. Invalidated
	// on SetModel because router/multi-model hosts may change the template.
	propsMu                sync.Mutex
	propsChecked           bool
	propsPreserveReasoning bool

	// preserveReasoningMode: auto (default, probe /props), on, off.
	preserveReasoningMode string

	// thinkingLevel controls reasoning_effort. Empty means omit (no thinking).
	thinkingLevel string
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
	query := func(catalog, stream *openai.Client) result {
		if catalog == nil {
			catalog = stream
		}
		var models []openai.Model
		routing := make(map[string]*openai.Client)
		pager := catalog.Models.ListAutoPaging(ctx)
		for pager.Next() {
			if ctx.Err() != nil {
				return result{models: models, routing: routing, err: ctx.Err()}
			}
			m := pager.Current()
			models = append(models, m)
			if stream != nil {
				routing[m.ID] = stream
			}
		}
		return result{models: models, routing: routing, err: pager.Err()}
	}

	if p.zenClient != nil && p.goClient != nil {
		var zenRes, goRes result
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			zenRes = query(p.zenCatalogClient, p.zenClient)
		}()
		go func() {
			defer wg.Done()
			goRes = query(p.goCatalogClient, p.goClient)
		}()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-done:
		}

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

	res := query(p.catalogClient, &p.client)
	if len(res.models) == 0 && res.err != nil {
		return nil, nil, res.err
	}
	return res.models, res.routing, nil
}

func NewOpenAIProvider(apiKey string, model string, baseURL string, workingDir string) *OpenAIProvider {
	streamOpts := []option.RequestOption{
		option.WithHTTPClient(newSSEHTTPClient()),
	}
	catalogOpts := []option.RequestOption{
		option.WithHTTPClient(newCatalogHTTPClient()),
	}
	if apiKey != "" {
		streamOpts = append(streamOpts, option.WithAPIKey(apiKey))
		catalogOpts = append(catalogOpts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		streamOpts = append(streamOpts, option.WithBaseURL(baseURL))
		catalogOpts = append(catalogOpts, option.WithBaseURL(baseURL))
	}
	p := &OpenAIProvider{
		client:      openai.NewClient(streamOpts...),
		model:       model,
		baseURL:     baseURL,
		apiKey:      apiKey,
		modelClient: make(map[string]*openai.Client),
		modelInfo:   modelinfo.NewResolver(modelinfo.CachePath(workingDir)),
	}
	p.modelInfo.Warm() // non-blocking; populate cache before first limit lookup
	if isOpencodeURL(baseURL) {
		newClients := func(url string) (stream, catalog *openai.Client) {
			sopts := []option.RequestOption{
				option.WithHTTPClient(newSSEHTTPClient()),
				option.WithBaseURL(url),
			}
			copts := []option.RequestOption{
				option.WithHTTPClient(newCatalogHTTPClient()),
				option.WithBaseURL(url),
			}
			if apiKey != "" {
				sopts = append(sopts, option.WithAPIKey(apiKey))
				copts = append(copts, option.WithAPIKey(apiKey))
			}
			s := openai.NewClient(sopts...)
			c := openai.NewClient(copts...)
			return &s, &c
		}
		p.zenClient, p.zenCatalogClient = newClients(openCodeZenBaseURL)
		p.goClient, p.goCatalogClient = newClients(openCodeGoBaseURL)
	} else {
		c := openai.NewClient(catalogOpts...)
		p.catalogClient = &c
	}
	return p
}

// SetModelInfoCacheDir points the models.dev disk cache at
// <dir>/.gogen/models.json. Used when the agent switches projects.
func (p *OpenAIProvider) SetModelInfoCacheDir(dir string) {
	if p == nil || p.modelInfo == nil {
		return
	}
	p.modelInfo.SetCachePath(modelinfo.CachePath(dir))
	p.modelInfo.Warm()
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

// SetPreserveReasoningMode controls whether chat_template_kwargs.preserve_reasoning
// is sent for self-hosted endpoints: auto (probe /props), on, or off.
func (p *OpenAIProvider) SetPreserveReasoningMode(mode string) {
	if p == nil {
		return
	}
	p.preserveReasoningMode = normalizePreserveReasoningMode(mode)
}

// WarmPreserveReasoning eagerly probes the /props endpoint (in auto mode)
// during startup so the result is cached before the first chat-completion
// request, avoiding a 1.5 s inline timeout on the critical path.
// Only the default ("auto") mode probes; "on" and "off" skip the probe.
func (p *OpenAIProvider) WarmPreserveReasoning() {
	if p == nil || normalizePreserveReasoningMode(p.preserveReasoningMode) != "auto" {
		return
	}
	go p.templateSupportsPreserveReasoning(context.Background())
}

func normalizePreserveReasoningMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on", "1", "true", "yes":
		return "on"
	case "off", "0", "false", "no":
		return "off"
	default:
		return "auto"
	}
}

// ProjectPromptCacheKey returns a stable, short hash of the working directory
// for use as an OpenAI prompt_cache_key. This keeps provider-side cache hits
// scoped per-project without leaking paths.
func ProjectPromptCacheKey(workingDir string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(workingDir))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], h.Sum64())
	return hex.EncodeToString(b[:])
}

// applyChatCompletionExtras sets llama.cpp-style kwargs that keep prompt-cache
// prefixes stable across multi-turn agent loops.
//
//   - mode "off": never send
//   - mode "on": send when a custom base URL is set (skip /props probe)
//   - mode "auto" (default): send only if GET /props reports
//     chat_template_caps.supports_preserve_reasoning
//
// Empty base URL is the default OpenAI API and never gets kwargs.
func (p *OpenAIProvider) applyChatCompletionExtras(ctx context.Context, params *openai.ChatCompletionNewParams) {
	if p == nil || params == nil {
		return
	}
	switch normalizePreserveReasoningMode(p.preserveReasoningMode) {
	case "off":
		return
	case "on":
		if strings.TrimSpace(p.baseURL) == "" {
			return
		}
	default: // auto
		if !p.templateSupportsPreserveReasoning(ctx) {
			return
		}
	}
	params.SetExtraFields(map[string]any{
		"chat_template_kwargs": map[string]any{
			"preserve_reasoning": true,
		},
	})
}

// applyThinkingLevel sets the reasoning_effort field on chat completion params
// when a non-empty thinking level is configured. Empty level means omit the
// parameter entirely (no thinking/reasoning requested from the API).
func (p *OpenAIProvider) applyThinkingLevel(_ context.Context, params *openai.ChatCompletionNewParams) {
	if p == nil || params == nil {
		return
	}
	p.modelsMu.RLock()
	level := p.thinkingLevel
	p.modelsMu.RUnlock()
	if level == "" || level == "off" {
		return
	}
	// Map Gogen thinking levels to OpenAI reasoning_effort.
	// OpenAI supports: low, medium, high.
	switch level {
	case "low", "minimal":
		params.ReasoningEffort = shared.ReasoningEffortLow
	case "medium":
		params.ReasoningEffort = shared.ReasoningEffortMedium
	case "high", "xhigh", "max":
		params.ReasoningEffort = shared.ReasoningEffortHigh
	}
}

// templateSupportsPreserveReasoning probes llama.cpp GET /props once and caches
// chat_template_caps.supports_preserve_reasoning. Failures / missing caps → false.
func (p *OpenAIProvider) templateSupportsPreserveReasoning(ctx context.Context) bool {
	p.propsMu.Lock()
	if p.propsChecked {
		v := p.propsPreserveReasoning
		p.propsMu.Unlock()
		return v
	}
	p.propsMu.Unlock()

	supported := p.probePreserveReasoning(ctx)

	p.propsMu.Lock()
	// Another goroutine may have filled the cache while we probed.
	if !p.propsChecked {
		p.propsChecked = true
		p.propsPreserveReasoning = supported
	}
	v := p.propsPreserveReasoning
	p.propsMu.Unlock()
	return v
}

func (p *OpenAIProvider) invalidatePropsCaps() {
	p.propsMu.Lock()
	p.propsChecked = false
	p.propsPreserveReasoning = false
	p.propsMu.Unlock()
}

// probePreserveReasoning GETs /props and reads supports_preserve_reasoning.
// Returns false on any error, non-200, or missing capability key.
func (p *OpenAIProvider) probePreserveReasoning(ctx context.Context) bool {
	propsURL := llamaPropsURL(p.baseURL)
	if propsURL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, propsProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, propsURL, nil)
	if err != nil {
		return false
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := propsHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	return parsePreserveReasoningCap(body)
}

func parsePreserveReasoningCap(body []byte) bool {
	var props struct {
		ChatTemplateCaps map[string]bool `json:"chat_template_caps"`
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return false
	}
	return props.ChatTemplateCaps["supports_preserve_reasoning"]
}

// llamaPropsURL maps an OpenAI-compatible base URL (.../v1) to llama.cpp GET /props.
func llamaPropsURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	path := strings.TrimSuffix(u.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	u.Path = path + "/props"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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

type streamAccumulator struct {
	fullContent      strings.Builder
	fullRefusal      strings.Builder
	fullReasoning    string
	lastFinishReason string
	streamUsage      *Usage
	tcAccums         []tcAccum
	tcIndexMap       map[int]int
	extras           extraFieldAccums
	streamDone       bool
	drainAfterDone   int
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{
		tcIndexMap: make(map[int]int),
		extras:     newExtraFieldAccums(),
	}
}

func (a *streamAccumulator) processChunk(chunk openai.ChatCompletionChunk, onToken, onThinking func(string), h *StreamHandlers) bool {
	if u := usageFromOpenAI(chunk.Usage); u != nil {
		a.streamUsage = u
	}
	if a.streamDone {
		a.drainAfterDone++
		return a.drainAfterDone >= 8
	}
	if len(chunk.Choices) == 0 {
		return false
	}

	choice := chunk.Choices[0]
	delta := choice.Delta
	a.extras.addFromDelta(delta, onThinking, &a.fullReasoning)

	if delta.Content != "" {
		a.fullContent.WriteString(delta.Content)
		onToken(delta.Content)
	}
	if delta.Refusal != "" {
		a.fullRefusal.WriteString(delta.Refusal)
	}
	if len(delta.ToolCalls) > 0 {
		for _, tc := range delta.ToolCalls {
			var idx int
			a.tcAccums, idx = mergeToolCallDelta(tc, a.tcAccums, a.tcIndexMap)
			tacc := &a.tcAccums[idx]
			if tc.Function.Name != "" {
				emitToolCallStart(tacc.Index, tacc, h)
			}
			if tc.Function.Arguments != "" {
				emitToolCallStart(tacc.Index, tacc, h)
				emitToolCallArgsDelta(tacc.Index, tacc, tc.Function.Arguments, h)
			}
		}
	}

	if choice.FinishReason != "" {
		switch choice.FinishReason {
		case "tool_calls":
			a.lastFinishReason = choice.FinishReason
			a.streamDone = true
		case "stop":
			if a.fullContent.Len() > 0 || a.fullRefusal.Len() > 0 || len(a.tcAccums) > 0 {
				a.lastFinishReason = choice.FinishReason
				a.streamDone = true
			}
		case "length", "content_filter":
			a.lastFinishReason = choice.FinishReason
			a.streamDone = true
		default:
			a.lastFinishReason = choice.FinishReason
			a.streamDone = true
		}
	} else if len(a.tcAccums) > 0 && toolAccumsStreamComplete(a.tcAccums) && deltaIsTerminalToolSignal(delta, true) {
		a.lastFinishReason = "tool_calls"
		return true
	}
	return false
}

func emitToolCallStart(tcIdx int, acc *tcAccum, h *StreamHandlers) {
	if acc.Started || acc.Name == "" || h.OnToolCallStart == nil {
		return
	}
	acc.Started = true
	h.OnToolCallStart(tcIdx, acc.ID, acc.Name)
}

func emitToolCallArgsDelta(tcIdx int, acc *tcAccum, argsDelta string, h *StreamHandlers) {
	if argsDelta == "" || h.OnToolCallArgsDelta == nil {
		return
	}
	h.OnToolCallArgsDelta(tcIdx, acc.ID, acc.Name, argsDelta)
}

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

func (a *streamAccumulator) buildResult() (*StreamResult, error) {
	var toolCalls []ToolCall
	for _, acc := range a.tcAccums {
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

	if len(toolCalls) == 0 && (a.fullReasoning != "" || a.fullContent.Len() > 0) {
		extractedCalls := extractToolCallsFromText(a.fullReasoning + a.fullContent.String())
		if len(extractedCalls) > 0 {
			toolCalls = extractedCalls
		}
	}

	content := a.fullContent.String()

	if a.lastFinishReason == "" && (content != "" || a.fullRefusal.Len() > 0 || a.fullReasoning != "" || len(a.tcAccums) > 0) {
		if len(a.tcAccums) > 0 {
			a.lastFinishReason = "tool_calls"
		} else {
			a.lastFinishReason = "stop"
		}
	}

	if a.lastFinishReason == "" && content == "" && a.fullRefusal.Len() == 0 && a.fullReasoning == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("stream ended without finish_reason")
	}

	return &StreamResult{
		Content:   content,
		Reasoning: a.fullReasoning,
		Refusal:   a.fullRefusal.String(),
		ToolCalls: toolCalls,
		Usage:     a.streamUsage,
	}, nil
}

func (p *OpenAIProvider) ModelContextLimit(ctx context.Context) (int, error) {
	// Resolution order: warm in-memory cache → models.dev (disk-cached, no
	// network) → brief /v1/models probe → 128k default. Network catalog
	// uses a short timeout so a hung remote host cannot stall restore or
	// context refresh; local servers still win when they answer quickly
	// with n_ctx etc.
	ctx, cancel := context.WithTimeout(ctx, modelsLimitLookupTimeout)
	defer cancel()

	// 1. Warm in-memory cache: parse provider JSON with no I/O.
	p.modelsMu.RLock()
	cached, cacheOK := p.cachedModelsLocked()
	if cacheOK {
		cached = append([]openai.Model(nil), cached...)
	}
	p.modelsMu.RUnlock()
	if cacheOK {
		if lim, ok := p.contextLimitFromModels(cached); ok {
			return lim, nil
		}
	}

	// 2. models.dev registry: disk-cached, resolves instantly with no network.
	modelName := p.currentModel()
	if limit, _ := p.lookupModelsDevLimit(modelName); limit > 0 {
		return limit, nil
	}

	// 3. Brief /v1/models probe (fallback when models.dev has no entry).
	//    Local LLMs typically answer in << 1s; a hung remote host is bounded
	//    by the outer modelsLimitLookupTimeout context.
	models, _ := p.listModels(ctx)
	if lim, ok := p.contextLimitFromModels(models); ok {
		return lim, nil
	}
	// 4. Fallback — return the default.
	return 128000, nil
}

// contextLimitFromModels applies sole-model auto-select and provider JSON
// context fields. ok is false when no positive limit was found.
func (p *OpenAIProvider) contextLimitFromModels(models []openai.Model) (int, bool) {
	if len(models) == 1 {
		sole := models[0]
		p.modelsMu.Lock()
		if sole.ID != "" {
			p.model = sole.ID
		}
		p.modelsMu.Unlock()
		if limit := parseContextLimitFromJSON(sole.RawJSON()); limit > 0 {
			return limit, true
		}
	}
	modelName := p.currentModel()
	for _, model := range models {
		if model.ID != modelName {
			continue
		}
		if limit := parseContextLimitFromJSON(model.RawJSON()); limit > 0 {
			return limit, true
		}
	}
	return 0, false
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
		limit, cost := p.resolveContextLimit(m.RawJSON(), m.ID)
		info := ModelInfo{
			ID:           m.ID,
			ContextLimit: limit,
			Current:      m.ID == current,
		}
		if m.ID == current && cost != nil {
			info.InputPricePer1M = cost.Input
			info.OutputPricePer1M = cost.Output
			info.CachedPricePer1M = cost.CacheRead
		}
		out = append(out, info)
	}
	return out, nil
}

// lookupModelsDevLimit queries the models.dev registry by base URL + model ID
// for both context limit and pricing in a single pass. OpenCode dual endpoints
// are both tried so zen and go models resolve.
func (p *OpenAIProvider) lookupModelsDevLimit(modelID string) (int, *modelinfo.Cost) {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return 0, nil
	}
	urls := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(u string) {
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	add(p.baseURL)
	if isOpencodeURL(p.baseURL) {
		add(openCodeZenBaseURL)
		add(openCodeGoBaseURL)
	}
	for _, u := range urls {
		lim, cost, err := p.modelInfo.Resolve(u, modelID)
		if err == nil {
			var c *modelinfo.Cost
			if cost != nil && (cost.Input > 0 || cost.Output > 0) {
				c = cost
			}
			if lim.Context > 0 {
				return lim.Context, c
			}
			if c != nil {
				return 0, c
			}
		}
	}
	return 0, nil
}

func (p *OpenAIProvider) SetModel(id string) error {
	p.modelsMu.Lock()
	p.model = id
	p.modelsMu.Unlock()
	// Template caps can change with the model on multi-model / router hosts.
	p.invalidatePropsCaps()
	return nil
}

// SetThinkingLevel sets the reasoning_effort level. Empty string means omit
// the parameter (the model will not use reasoning/thinking).
func (p *OpenAIProvider) SetThinkingLevel(level string) {
	p.modelsMu.Lock()
	p.thinkingLevel = level
	p.modelsMu.Unlock()
}

// resolveContextLimit resolves context limit and pricing. Provider JSON is
// tried first for the limit, but models.dev is always consulted for cost
// data since provider JSON never includes pricing.
func (p *OpenAIProvider) resolveContextLimit(rawJSON, modelID string) (int, *modelinfo.Cost) {
	// Always look up models.dev for pricing (provider JSON never has it).
	var cost *modelinfo.Cost
	devLimit, devCost := p.lookupModelsDevLimit(modelID)
	if devCost != nil {
		cost = devCost
	}
	// Prefer provider JSON limit, then models.dev, then default.
	if limit := parseContextLimitFromJSON(rawJSON); limit > 0 {
		return limit, cost
	}
	if devLimit > 0 {
		return devLimit, cost
	}
	return 128000, cost
}

// ModelPricing returns per-token pricing (USD per 1M tokens) for the given
// model from the models.dev registry. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelPricing(modelID string) (input, output, cached float64, ok bool) {
	_, cost := p.lookupModelsDevLimit(modelID)
	if cost != nil {
		return cost.Input, cost.Output, cost.CacheRead, true
	}
	return 0, 0, 0, false
}
