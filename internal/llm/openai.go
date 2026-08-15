package llm

import (
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"gogen/internal/config"
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

// ProviderProfile is one registered OpenAI-compatible API endpoint: a base
// URL, an optional API key, and an optional default model. The first entry
// is the default profile (fallback client + new-session model seed); later
// entries are additional registered providers whose models are aggregated
// into the model catalog and routed per model (clientForModel).
type ProviderProfile struct {
	Name    string
	BaseURL string
	APIKey  string
	Model   string
}

// providerProfile is the internal endpoint set: the openai clients for one
// registered provider. An OpenCode profile additionally carries its zen/go
// twin clients; non-OpenCode profiles carry a single stream + catalog pair.
type providerProfile struct {
	name       string
	baseURL    string
	apiKey     string
	model      string
	stream     *openai.Client
	catalog    *openai.Client
	zenStream  *openai.Client
	zenCatalog *openai.Client
	goStream   *openai.Client
	goCatalog  *openai.Client
}

// modelProfileInfo records which registered provider profile serves a model:
// the profile name (model-picker grouping) and its base URL (models.dev and
// /props resolution).
type modelProfileInfo struct {
	name    string
	baseURL string
}

type OpenAIProvider struct {
	client           openai.Client  // primary streaming client (default profile)
	catalogClient    *openai.Client // non-stream /v1/models (default profile, non-OpenCode)
	zenClient        *openai.Client // OpenCode Zen streaming (default profile)
	zenCatalogClient *openai.Client // OpenCode Zen catalog (default profile)
	goClient         *openai.Client // OpenCode Go streaming (default profile)
	goCatalogClient  *openai.Client // OpenCode Go catalog (default profile)
	model            string
	baseURL          string
	apiKey           string
	modelClient      map[string]*openai.Client // model ID → streaming client routing
	// profiles is the registered endpoint set built by the constructors;
	// profiles[0] is the default profile whose clients alias the legacy
	// client fields above. Nil for direct-constructed providers (tests):
	// those use the legacy fields only.
	profiles []*providerProfile
	// profilesGen is bumped by SetProfiles. A catalog fetch started before
	// the swap records the generation it began with; if the generation
	// changed when it completes, its results came from the OLD endpoint set
	// and are discarded instead of polluting the cache/routing with stale
	// models (the next fetch hits the new endpoints).
	profilesGen int
	// modelProfile records which profile serves each catalog model (model ID
	// → profile name + base URL), populated by listModels alongside the
	// model cache. Used for picker grouping, models.dev resolution, and the
	// /props probe target.
	modelProfile map[string]modelProfileInfo
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
	// The cache is keyed by endpoint (propsBaseURL): with multiple
	// registered profiles, the probe must hit the CURRENT model's owning
	// endpoint.
	propsMu                sync.Mutex
	propsChecked           bool
	propsBaseURL           string
	propsPreserveReasoning bool

	// effortsDerived caches per-model reasoning-effort options derived from
	// llama.cpp /props (+ /apply-template) capability probes (see
	// ProbeReasoningEfforts). Keyed by model ID; entries record the endpoint
	// they were derived from. Cleared by SetProfiles (the endpoint set
	// changed); NOT cleared by SetModel — each model is probed once and the
	// result stays valid across switches back to it.
	effortsMu      sync.Mutex
	effortsDerived map[string]derivedEfforts

	// preserveReasoningMode: auto (default, probe /props), on, off.
	preserveReasoningMode string

	// thinkingLevel controls reasoning_effort. Empty means omit (no thinking).
	thinkingLevel string
}

// ProviderProfiles builds the registered OpenAI-compatible endpoint list for
// provider construction: the implicit "default" profile from the legacy
// config fields first, then the additional registered providers in order
// (profile order is the duplicate-model-ID precedence — the default profile
// wins on collisions). Shared by setup.go, the TUI subagent spawner, and
// the web workspace provider factory so every path aggregates the same
// endpoint set from the same source values.
func ProviderProfiles(openAIKey, openAIModel, openAIURL string, providers []config.OpenAIProviderConfig) []ProviderProfile {
	profiles := []ProviderProfile{{
		Name:    "default",
		BaseURL: openAIURL,
		APIKey:  openAIKey,
		Model:   openAIModel,
	}}
	for _, p := range providers {
		profiles = append(profiles, ProviderProfile{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
	}
	return profiles
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
	profiles := []ProviderProfile{{Name: "default", BaseURL: baseURL, APIKey: apiKey, Model: model}}
	return NewOpenAIProviderWithProfiles(profiles, model, workingDir, resolver)
}

// NewOpenAIProviderWithProfiles builds a multi-endpoint provider:
// profiles[0] is the default profile (fallback client, new-session model
// seed); every registered profile's catalog is aggregated into one model
// list, and each model routes to its owning profile's endpoint
// (clientForModel). A nil resolver builds a private one rooted at
// workingDir.
func NewOpenAIProviderWithProfiles(profiles []ProviderProfile, model string, workingDir string, resolver *modelinfo.Resolver) *OpenAIProvider {
	if resolver == nil {
		resolver = modelinfo.NewResolver(modelinfo.CachePath(workingDir))
	}
	built := make([]*providerProfile, 0, len(profiles))
	for _, pr := range profiles {
		name := strings.TrimSpace(pr.Name)
		if name == "" {
			name = "default"
		}
		built = append(built, buildProfileClients(providerProfile{
			name:    name,
			baseURL: strings.TrimSpace(pr.BaseURL),
			apiKey:  pr.APIKey,
			model:   pr.Model,
		}))
	}
	return newOpenAIProvider(built, model, resolver)
}

// buildProfileClients constructs the stream/catalog client pair (plus the
// zen/go twins for OpenCode URLs) for one registered provider profile.
func buildProfileClients(prof providerProfile) *providerProfile {
	streamOpts := []option.RequestOption{
		option.WithHTTPClient(newSSEHTTPClient()),
	}
	catalogOpts := []option.RequestOption{
		option.WithHTTPClient(newCatalogHTTPClient()),
	}
	if prof.apiKey != "" {
		streamOpts = append(streamOpts, option.WithAPIKey(prof.apiKey))
		catalogOpts = append(catalogOpts, option.WithAPIKey(prof.apiKey))
	}
	if prof.baseURL != "" {
		streamOpts = append(streamOpts, option.WithBaseURL(prof.baseURL))
		catalogOpts = append(catalogOpts, option.WithBaseURL(prof.baseURL))
	}
	stream := openai.NewClient(streamOpts...)
	prof.stream = &stream
	if isOpencodeURL(prof.baseURL) {
		newClients := func(url string) (stream, catalog *openai.Client) {
			sopts := []option.RequestOption{
				option.WithHTTPClient(newSSEHTTPClient()),
				option.WithBaseURL(url),
			}
			copts := []option.RequestOption{
				option.WithHTTPClient(newCatalogHTTPClient()),
				option.WithBaseURL(url),
			}
			if prof.apiKey != "" {
				sopts = append(sopts, option.WithAPIKey(prof.apiKey))
				copts = append(copts, option.WithAPIKey(prof.apiKey))
			}
			s := openai.NewClient(sopts...)
			c := openai.NewClient(copts...)
			return &s, &c
		}
		prof.zenStream, prof.zenCatalog = newClients(openCodeZenBaseURL)
		prof.goStream, prof.goCatalog = newClients(openCodeGoBaseURL)
	} else {
		c := openai.NewClient(catalogOpts...)
		prof.catalog = &c
	}
	return &prof
}

// newOpenAIProvider assembles the provider from its profile client sets.
// The default profile's (profiles[0]) clients alias the legacy client
// fields so the fallback path and direct-construction tests keep working;
// profiles[0].stream is re-pointed at the copied p.client value so the
// fallback client and routing always agree for constructor-built providers.
func newOpenAIProvider(profiles []*providerProfile, model string, resolver *modelinfo.Resolver) *OpenAIProvider {
	p := &OpenAIProvider{
		model:          model,
		modelClient:    make(map[string]*openai.Client),
		modelInfo:      resolver,
		profiles:       profiles,
		effortsDerived: make(map[string]derivedEfforts),
	}
	if len(profiles) > 0 {
		def := profiles[0]
		p.baseURL = def.baseURL
		p.apiKey = def.apiKey
		p.client = *def.stream
		p.catalogClient = def.catalog
		p.zenClient = def.zenStream
		p.zenCatalogClient = def.zenCatalog
		p.goClient = def.goStream
		p.goCatalogClient = def.goCatalog
		def.stream = &p.client
	}
	p.modelInfo.Warm() // non-blocking; populate cache before first limit lookup
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

// SetProfiles replaces the registered provider set at runtime (the web
// settings modal's provider_save/provider_delete). The profile client sets
// are rebuilt and the catalog/routing caches are invalidated so the next
// ListModels re-fetches every endpoint. The current model is kept: it stays
// routable when its profile is still registered, otherwise clientForModel
// falls back to the default client and requireModelSelected re-validates it
// on the next turn.
//
// The legacy client fields (client, catalogClient, zen/go twins) are NOT
// rewritten: constructor-built providers route through the profile set, and
// the legacy fields only serve the direct-construction (test) shape where
// profiles is nil. Similarly p.baseURL/p.apiKey stay frozen at construction;
// per-model lookups resolve through modelProfile, which the next fetch
// repopulates.
func (p *OpenAIProvider) SetProfiles(profiles []ProviderProfile) error {
	if p == nil {
		return nil
	}
	built := make([]*providerProfile, 0, len(profiles))
	for _, pr := range profiles {
		name := strings.TrimSpace(pr.Name)
		if name == "" {
			name = "default"
		}
		built = append(built, buildProfileClients(providerProfile{
			name:    name,
			baseURL: strings.TrimSpace(pr.BaseURL),
			apiKey:  pr.APIKey,
			model:   pr.Model,
		}))
	}
	p.invalidatePropsCaps()
	p.invalidateReasoningEfforts()
	p.modelsMu.Lock()
	p.profiles = built
	p.profilesGen++
	p.modelsCache = nil
	p.modelsCachedAt = time.Time{}
	p.modelProfile = nil
	p.modelClient = make(map[string]*openai.Client)
	p.modelsFetchFailedAt = time.Time{}
	p.modelsMu.Unlock()
	return nil
}

// defaultBaseURL returns the base URL of the default profile (profiles[0])
// for constructor-built providers, or the legacy frozen p.baseURL for the
// direct-constructed (test) shape. Live-correct after SetProfiles: the
// legacy fields are intentionally not rewritten, but the /props probe,
// models.dev resolution, and the preserve_reasoning kwargs gate must
// resolve against the CURRENT default endpoint, not the construction-time
// one. For the single-profile constructor path profiles[0].baseURL equals
// p.baseURL, so this is a no-op there.
func (p *OpenAIProvider) defaultBaseURL() string {
	p.modelsMu.RLock()
	profiles := p.profiles
	p.modelsMu.RUnlock()
	if len(profiles) > 0 {
		return profiles[0].baseURL
	}
	return p.baseURL
}

// fallbackClient returns the client that serves models with no known
// routing: the default profile's stream client for constructor-built
// providers, or the legacy primary client (direct-constructed shape).
func (p *OpenAIProvider) fallbackClient() *openai.Client {
	p.modelsMu.RLock()
	profiles := p.profiles
	p.modelsMu.RUnlock()
	if len(profiles) > 0 {
		return profiles[0].stream
	}
	return &p.client
}

// hasMultipleProfiles reports whether more than one registered provider
// endpoint is configured. Sole-model auto-select only applies to the single
// default endpoint: with additional profiles the aggregate catalog is a
// merge of several endpoints and "sole model" is not meaningful.
func (p *OpenAIProvider) hasMultipleProfiles() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return len(p.profiles) > 1
}

// hasOpenCodeProfile reports whether any registered profile is an OpenCode
// endpoint (has zen/go twin clients). Profile-derived — NOT the legacy
// frozen zenClient field — so it stays live-correct after SetProfiles: a
// provider switched to or from OpenCode must drive catalog discovery and
// endpoint inference from the CURRENT endpoint set, never stale
// construction-time clients.
func (p *OpenAIProvider) hasOpenCodeProfile() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	for _, prof := range p.profiles {
		if prof.zenStream != nil && prof.goStream != nil {
			return true
		}
	}
	return false
}

// profileFor returns the registered provider profile name that serves
// modelID ("default" when the catalog has not been fetched or the model is
// unknown). The web model picker groups models by this name.
func (p *OpenAIProvider) profileFor(modelID string) string {
	p.modelsMu.RLock()
	info := p.modelProfile[modelID]
	p.modelsMu.RUnlock()
	if info.name != "" {
		return info.name
	}
	return "default"
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
// toggle/budget-only models, meaning no effort control); else the set derived
// from a llama.cpp capability probe when one has completed (empty when the
// endpoint reported no effort support); else DefaultReasoningEfforts. Never
// blocks — the models.dev registry and the derived cache are in-memory map
// reads; the probe itself runs off the hot path via ProbeReasoningEfforts.
// This is the same set applyThinkingLevel uses to gate the wire value.
func (p *OpenAIProvider) ModelReasoningEfforts(modelID string) []string {
	efforts, _ := p.reasoningEffortResolution(modelID)
	return efforts
}

// ReasoningEffortUnsupported reports whether the model definitively has NO
// reasoning-effort control: a known models.dev entry with an empty accepted
// set (toggle/budget-only), or a llama.cpp capability probe that reported no
// support — with no source reporting support. Unknown or not-yet-probed
// models are NOT unsupported (callers keep the DefaultReasoningEfforts
// fallback). Never blocks. The client hides the thinking chips in this case.
func (p *OpenAIProvider) ReasoningEffortUnsupported(modelID string) bool {
	_, unsupported := p.reasoningEffortResolution(modelID)
	return unsupported
}

// reasoningEffortResolution resolves the accepted reasoning-effort set and
// the definitive no-support flag for one model. Precedence:
//
//  1. The models.dev accepted set when the model is known and non-empty.
//  2. The llama.cpp capability-probe set when probed and non-empty (a runtime
//     template that references reasoning_effort wins over a registry entry
//     that declared none).
//  3. "unsupported" when a source positively reported no effort control
//     (registry known with an empty set, or a probe reporting no support)
//     and no source reported support.
//  4. DefaultReasoningEfforts otherwise (unknown / not yet probed).
//
// Never blocks: registry and derived-cache reads are in-memory map lookups.
func (p *OpenAIProvider) reasoningEffortResolution(modelID string) (efforts []string, unsupported bool) {
	if p == nil || modelID == "" {
		return DefaultReasoningEfforts, false
	}
	registryKnown := false
	var registryEfforts []string
	if p.modelInfo != nil {
		for _, u := range p.modelsDevURLsFor(modelID) {
			if _, _, efforts, _, err := p.modelInfo.Resolve(u, modelID); err == nil {
				registryKnown = true
				registryEfforts = efforts
				break
			}
		}
	}
	if registryKnown && len(registryEfforts) > 0 {
		return append([]string(nil), registryEfforts...), false
	}
	p.effortsMu.Lock()
	d, ok := p.effortsDerived[modelID]
	p.effortsMu.Unlock()
	if ok && d.probed && len(d.efforts) > 0 {
		return append([]string(nil), d.efforts...), false
	}
	if (registryKnown && len(registryEfforts) == 0) || (ok && d.probed && len(d.efforts) == 0) {
		return nil, true
	}
	return DefaultReasoningEfforts, false
}

// ModelDescription returns the models.dev description for the given model, or
// "" when the model is unknown. Only cached map lookup — never blocks.
func (p *OpenAIProvider) ModelDescription(modelID string) string {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return ""
	}
	for _, u := range p.modelsDevURLsFor(modelID) {
		if _, _, _, desc, err := p.modelInfo.Resolve(u, modelID); err == nil {
			return desc
		}
	}
	return ""
}
