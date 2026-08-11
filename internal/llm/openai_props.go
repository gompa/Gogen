package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
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

// DefaultReasoningEfforts is the reasoning_effort value set used when the
// current model has no models.dev entry (unknown/self-hosted endpoints). It is
// the most common accepted set in the registry: the low/medium/high triple is
// the mode of models.dev's per-model effort values (accepted by ~80%+ of the
// effort-enabled models; high alone by 99%).
var DefaultReasoningEfforts = []string{"low", "medium", "high"}

// applyThinkingLevel sets the reasoning_effort field on chat completion params
// when a non-empty thinking level is configured. Empty or "off" means omit the
// parameter entirely (no reasoning/effort requested from the API).
//
// The configured value is sent verbatim — never translated — but only when the
// current model accepts it. The accepted set comes from the models.dev
// registry when the model is known (empty for toggle/budget-only models);
// unknown/self-hosted models fall back to DefaultReasoningEfforts. A stored
// value the model does not accept is omitted rather than rewritten (policy B:
// it stays stored, inactive, and re-activates if a later model accepts it).
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
	if !slices.Contains(p.acceptedReasoningEfforts(), level) {
		return // stored value not accepted by this model → omit (policy B)
	}
	params.ReasoningEffort = shared.ReasoningEffort(level)
}

// acceptedReasoningEfforts returns the effective reasoning-effort options for
// the current model: its models.dev accepted set when known (empty for
// toggle-only/budget-only models), else DefaultReasoningEfforts. Never blocks
// — registry lookups are in-memory/disk-cached map lookups.
func (p *OpenAIProvider) acceptedReasoningEfforts() []string {
	return p.ModelReasoningEfforts(p.currentModel())
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
