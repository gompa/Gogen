package llm

import (
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"gogen/internal/modelinfo"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
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

// modelsFetchBackoff gates re-fetching after a failed catalog fetch: a dead
// OpenCode endpoint costs one bounded attempt per window instead of one per
// chat request. A var (not const) so tests can shrink it, mirroring how
// modelsCacheTTL governs cache staleness.
var modelsFetchBackoff = 30 * time.Second

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
	// modelsFetchFailedAt is set when a catalog fetch fails; clientForModel
	// skips re-fetching until modelsFetchBackoff has elapsed.
	modelsFetchFailedAt time.Time
	modelsFetch         *modelsFetch

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

func NewOpenAIProvider(apiKey string, model string, baseURL string, workingDir string) *OpenAIProvider {
	return NewOpenAIProviderWithResolver(apiKey, model, baseURL, workingDir, nil)
}

// NewOpenAIProviderWithResolver is NewOpenAIProvider with an optional shared
// models.dev resolver. The multi-session web server passes one resolver per
// workspace so N per-session providers do not issue N parallel catalog
// fetches or race N atomic writes to the same .gogen/models.json cache file.
// A nil resolver builds a private one (single-session/TUI behavior).
func NewOpenAIProviderWithResolver(apiKey string, model string, baseURL string, workingDir string, resolver *modelinfo.Resolver) *OpenAIProvider {
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
	if resolver == nil {
		resolver = modelinfo.NewResolver(modelinfo.CachePath(workingDir))
	}
	p := &OpenAIProvider{
		client:      openai.NewClient(streamOpts...),
		model:       model,
		baseURL:     baseURL,
		apiKey:      apiKey,
		modelClient: make(map[string]*openai.Client),
		modelInfo:   resolver,
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

// SetModel updates the provider model ID and invalidates template capability
// caches that may differ across models.
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

// ModelPricing returns per-token pricing (USD per 1M tokens) for the given
// model from the models.dev registry. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelPricing(modelID string) (input, output, cached float64, ok bool) {
	_, cost, _, _ := p.lookupModelsDevInfo(modelID)
	if cost != nil {
		return cost.Input, cost.Output, cost.CacheRead, true
	}
	return 0, 0, 0, false
}

// ModelReasoningEfforts returns the effective reasoning-effort options for the
// given model: its models.dev accepted set when the model is known (empty for
// toggle/budget-only models, meaning no effort control), else
// DefaultReasoningEfforts. Only cached map lookup — never blocks. This is the
// same set applyThinkingLevel uses to gate the wire value.
func (p *OpenAIProvider) ModelReasoningEfforts(modelID string) []string {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return DefaultReasoningEfforts
	}
	for _, u := range p.modelsDevURLs() {
		if _, _, efforts, _, err := p.modelInfo.Resolve(u, modelID); err == nil {
			return efforts // known model: accepted set (possibly empty)
		}
	}
	return DefaultReasoningEfforts // unknown model
}

// ModelDescription returns the models.dev description for the given model, or
// "" when the model is unknown. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelDescription(modelID string) string {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return ""
	}
	for _, u := range p.modelsDevURLs() {
		if _, _, _, desc, err := p.modelInfo.Resolve(u, modelID); err == nil {
			return desc
		}
	}
	return ""
}
