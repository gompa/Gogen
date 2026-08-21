package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"

	"gogen/internal/onoff"
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
	if on, ok := onoff.Parse(mode); ok {
		if on {
			return "on"
		}
		return "off"
	}
	return "auto"
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
		// The live default endpoint (SetProfiles may have replaced it): the
		// kwargs must reach the endpoint the request actually goes to.
		if strings.TrimSpace(p.defaultBaseURL()) == "" {
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
// current model has no models.dev entry and no llama.cpp capability probe has
// derived a set (unknown/self-hosted endpoints). It is the most common
// accepted set in the registry: the low/medium/high triple is the mode of
// models.dev's per-model effort values (accepted by ~80%+ of the
// effort-enabled models; high alone by 99%).
var DefaultReasoningEfforts = []string{"low", "medium", "high"}

// applyThinkingLevel sets the reasoning_effort field on chat completion params
// when a non-empty thinking level is configured. Empty or "off" means omit the
// parameter entirely (no reasoning/effort requested from the API).
//
// The configured value is sent verbatim — never translated — but only when the
// current model accepts it. The accepted set comes from the models.dev
// registry when the model is known (empty for toggle/budget-only models), from
// a llama.cpp capability probe when one has completed, else
// DefaultReasoningEfforts. A stored value the model does not accept is omitted
// rather than rewritten (policy B: it stays stored, inactive, and re-activates
// if a later model accepts it).
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
// toggle-only/budget-only models), else the llama.cpp /props-derived set when
// one has been probed, else DefaultReasoningEfforts. Never blocks — registry
// lookups and the derived cache are in-memory map reads.
func (p *OpenAIProvider) acceptedReasoningEfforts() []string {
	return p.ModelReasoningEfforts(p.currentModel())
}

// templateSupportsPreserveReasoning probes llama.cpp GET /props once per
// endpoint and caches chat_template_caps.supports_preserve_reasoning. The
// probe hits the CURRENT model's owning profile (with multiple registered
// providers, /props must be asked of the endpoint that actually serves the
// request). Failures / missing caps → false.
func (p *OpenAIProvider) templateSupportsPreserveReasoning(ctx context.Context) bool {
	baseURL := p.propsBaseURLForCurrentModel()
	p.propsMu.Lock()
	if p.propsChecked && p.propsBaseURL == baseURL {
		v := p.propsPreserveReasoning
		p.propsMu.Unlock()
		return v
	}
	p.propsMu.Unlock()

	supported := p.probePreserveReasoning(ctx, baseURL)

	p.propsMu.Lock()
	// Another goroutine may have filled the cache while we probed.
	if !p.propsChecked || p.propsBaseURL != baseURL {
		p.propsChecked = true
		p.propsBaseURL = baseURL
		p.propsPreserveReasoning = supported
	}
	v := p.propsPreserveReasoning
	p.propsMu.Unlock()
	return v
}

// propsBaseURLForModel returns the base URL of the endpoint that serves
// modelID: the model's owning profile when the catalog has been fetched, else
// the default profile's base URL.
func (p *OpenAIProvider) propsBaseURLForModel(modelID string) string {
	p.modelsMu.RLock()
	info := p.modelProfile[modelID]
	p.modelsMu.RUnlock()
	if info.baseURL != "" {
		return info.baseURL
	}
	return p.defaultBaseURL()
}

// propsBaseURLForCurrentModel returns the base URL of the endpoint that
// serves the currently selected model (see propsBaseURLForModel).
func (p *OpenAIProvider) propsBaseURLForCurrentModel() string {
	return p.propsBaseURLForModel(p.currentModel())
}

func (p *OpenAIProvider) invalidatePropsCaps() {
	p.propsMu.Lock()
	p.propsChecked = false
	p.propsBaseURL = ""
	p.propsPreserveReasoning = false
	p.propsMu.Unlock()
}

// probePreserveReasoning GETs /props on baseURL and reads
// supports_preserve_reasoning. Returns false on any error, non-200, or
// missing capability key.
func (p *OpenAIProvider) probePreserveReasoning(ctx context.Context, baseURL string) bool {
	propsURL := llamaPropsURL(baseURL)
	if propsURL == "" {
		return false
	}
	status, body, err := p.propsRequest(ctx, http.MethodGet, propsURL, "")
	if err != nil || status != http.StatusOK {
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
	base := llamaEndpointBase(baseURL)
	if base == "" {
		return ""
	}
	return base + "/props"
}
