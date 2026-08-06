package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
)

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
//
// Gogen exposes more levels (off/minimal/low/medium/high/xhigh/max) than the
// reasoning_effort values most models accept. The widely-supported set is
// low/medium/high (o1/o3/o4-mini and most OpenAI-compatible endpoints); only
// newer GPT-5.x-class models add minimal/xhigh/max. To stay compatible with
// the majority of models, the extra levels are folded onto the common set:
// minimal → low, xhigh/max → high.
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
	switch level {
	case "minimal", "low":
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
