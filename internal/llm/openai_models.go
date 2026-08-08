package llm

import (
	"context"
	"errors"
	"sync"
	"time"

	"gogen/internal/config"
	"gogen/internal/modelinfo"

	"github.com/openai/openai-go"
)

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
			// Merge, don't replace: clientForModel caches fallback entries for
			// models absent from the catalog (unknown/custom models resolved
			// via models.dev or the primary client). A wholesale replacement
			// would drop those entries and force a fresh lookup + inference on
			// every request after each catalog refresh. Catalog entries win
			// for IDs they cover; entries for models a future catalog removes
			// linger until the model reappears (they only point at the
			// endpoint that used to serve it, so this is a minor risk).
			if p.modelClient == nil {
				p.modelClient = routing
			} else {
				for id, c := range routing {
					p.modelClient[id] = c
				}
			}
		}
		p.modelsFetchFailedAt = time.Time{}
	} else {
		// Record the failure so clientForModel's backoff gate skips re-probing
		// a dead catalog on every chat request.
		p.modelsFetchFailedAt = time.Now()
	}
	p.modelsFetch = nil
	close(f.done)
	p.modelsMu.Unlock()
	if err != nil {
		return nil, err
	}
	return append([]openai.Model(nil), models...), nil
}

// catalogFetchOnBackoff reports whether a failed catalog fetch is still in
// its backoff window, so a dead OpenCode endpoint is not re-probed on every
// chat request. A fetch that failed moments ago costs one bounded attempt
// per modelsFetchBackoff window instead of one per request.
func (p *OpenAIProvider) catalogFetchOnBackoff() bool {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return !p.modelsFetchFailedAt.IsZero() &&
		time.Since(p.modelsFetchFailedAt) < modelsFetchBackoff
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
		// Go takes precedence over Zen: for models listed on both endpoints
		// keep the Go entry in both the merged list and the routing map, so
		// what /models shows and where the request actually goes always
		// agree. A Go-subscription model must never be sent to the Zen
		// endpoint, which rejects it as unsupported.
		seen := make(map[string]struct{}, len(goRes.models))
		for _, m := range goRes.models {
			models = append(models, m)
			seen[m.ID] = struct{}{}
		}
		for _, m := range zenRes.models {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			models = append(models, m)
		}
		for id, c := range goRes.routing {
			routing[id] = c
		}
		for id, c := range zenRes.routing {
			if _, goWins := routing[id]; goWins {
				continue
			}
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
	return config.DefaultContextLimit, nil
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
	return config.DefaultContextLimit, cost
}
