package llm

import (
	"context"
	"errors"
	"log"
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
	gen := p.profilesGen
	p.modelsMu.Unlock()

	models, routing, profileFor, err := p.fetchModelsWithProfiles(ctx)

	p.modelsMu.Lock()
	f.models, f.err = models, err
	if err == nil {
		if p.profilesGen == gen {
			p.modelsCache = models
			p.modelsCachedAt = time.Now()
			p.modelProfile = profileFor
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
		}
		// Stale fetch (SetProfiles swapped the endpoint set while this
		// catalog fetch was in flight): the result came from the OLD
		// endpoints and is NOT cached — concurrent waiters get the snapshot
		// once, and the next call re-fetches the new endpoint set.
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

// fetchModels loads the merged model catalog from every registered provider
// endpoint. It is the 3-value wrapper kept for direct callers/tests;
// fetchModelsWithProfiles is the full implementation.
func (p *OpenAIProvider) fetchModels(ctx context.Context) ([]openai.Model, map[string]*openai.Client, error) {
	models, routing, _, err := p.fetchModelsWithProfiles(ctx)
	return models, routing, err
}

// fetchModelsWithProfiles loads the merged catalog from every registered
// provider endpoint: each profile's catalog is queried in parallel (an
// OpenCode profile queries its zen and go endpoints in parallel with
// Go-over-Zen precedence, exactly like the single-endpoint shape), then the
// per-profile lists are merged in PROFILE ORDER — the first profile wins on
// duplicate model IDs (the default profile first), so what /models shows and
// where the request actually goes always agree. The returned profileFor map
// records each model's owning profile (name + base URL) for picker grouping
// and per-endpoint models.dev / props resolution.
func (p *OpenAIProvider) fetchModelsWithProfiles(ctx context.Context) ([]openai.Model, map[string]*openai.Client, map[string]modelProfileInfo, error) {
	type result struct {
		models  []openai.Model
		routing map[string]*openai.Client
		err     error
	}
	query := func(qctx context.Context, catalog, stream *openai.Client) result {
		if catalog == nil {
			catalog = stream
		}
		var models []openai.Model
		routing := make(map[string]*openai.Client)
		pager := catalog.Models.ListAutoPaging(qctx)
		for pager.Next() {
			if qctx.Err() != nil {
				return result{models: models, routing: routing, err: qctx.Err()}
			}
			m := pager.Current()
			models = append(models, m)
			if stream != nil {
				routing[m.ID] = stream
			}
		}
		return result{models: models, routing: routing, err: pager.Err()}
	}

	// queryProfile fetches one profile's endpoint set. An OpenCode profile
	// queries zen + go in parallel; Go takes precedence over Zen for models
	// listed on both (a Go-subscription model must never be sent to the Zen
	// endpoint, which rejects it as unsupported).
	queryProfile := func(qctx context.Context, prof *providerProfile) result {
		if prof.zenStream == nil || prof.goStream == nil {
			return query(qctx, prof.catalog, prof.stream)
		}
		var zenRes, goRes result
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			zenRes = query(qctx, prof.zenCatalog, prof.zenStream)
		}()
		go func() {
			defer wg.Done()
			goRes = query(qctx, prof.goCatalog, prof.goStream)
		}()
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-qctx.Done():
			return result{err: qctx.Err()}
		case <-done:
		}

		routing := make(map[string]*openai.Client, len(zenRes.routing)+len(goRes.routing))
		models := make([]openai.Model, 0, len(zenRes.models)+len(goRes.models))
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
			return result{err: errors.Join(errs...)}
		}
		return result{models: models, routing: routing}
	}

	profiles := p.resolvedProfiles()
	results := make([]result, len(profiles))
	var wg sync.WaitGroup
	for i, prof := range profiles {
		wg.Add(1)
		go func(i int, prof *providerProfile) {
			defer wg.Done()
			// Per-profile deadline: a hung endpoint (offline host that never
			// refuses the connection) must not hold the picker — or the
			// first chat request — hostage for the whole fetch budget while
			// the other providers already answered. The merge below keeps
			// every catalog that completed within this window.
			qctx, cancel := context.WithTimeout(ctx, profileCatalogTimeout)
			defer cancel()
			results[i] = queryProfile(qctx, prof)
		}(i, prof)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// Wait for every profile query to settle — including after the fetch
	// budget expires: an offline or hanging provider must not discard the
	// catalogs that already succeeded (the early ctx.Done() exit here used
	// to empty the whole model picker over a single dead endpoint). The
	// in-flight queries observe ctx cancellation and each catalog HTTP
	// client carries its own timeout, so this wait is bounded; the merge
	// below keeps every profile that completed and only reports an error
	// when ALL profiles failed.
	<-done

	models := make([]openai.Model, 0)
	routing := make(map[string]*openai.Client)
	profileFor := make(map[string]modelProfileInfo)
	seen := make(map[string]struct{})
	var errs []error
	for i, prof := range profiles {
		res := results[i]
		if res.err != nil {
			errs = append(errs, res.err)
			continue
		}
		// Add this profile's models and their routing in one pass: the
		// routing entry for a winning model must come from the SAME profile
		// that contributed it (an earlier profile's duplicate wins).
		for _, m := range res.models {
			if _, dup := seen[m.ID]; dup {
				continue // earlier profile wins
			}
			seen[m.ID] = struct{}{}
			models = append(models, m)
			profileFor[m.ID] = modelProfileInfo{name: prof.name, baseURL: prof.baseURL}
			if c, ok := res.routing[m.ID]; ok {
				routing[m.ID] = c
			}
		}
	}
	if len(models) == 0 && len(errs) > 0 {
		return nil, nil, nil, errors.Join(errs...)
	}
	if len(errs) > 0 {
		// Partial failure: serve the catalogs that succeeded and say which
		// providers dropped out — the picker must not go empty because one
		// endpoint is offline.
		log.Printf("model catalog: %d of %d provider(s) failed, serving %d models: %v",
			len(errs), len(profiles), len(models), errors.Join(errs...))
	}
	return models, routing, profileFor, nil
}

// resolvedProfiles returns the endpoint set to query: the registered
// profiles when set (constructor-built), else a single default profile
// synthesized from the legacy client fields (direct-constructed providers in
// tests).
func (p *OpenAIProvider) resolvedProfiles() []*providerProfile {
	p.modelsMu.RLock()
	profiles := p.profiles
	p.modelsMu.RUnlock()
	if len(profiles) > 0 {
		return profiles
	}
	return []*providerProfile{{
		name:       "default",
		baseURL:    p.baseURL,
		apiKey:     p.apiKey,
		model:      p.model,
		stream:     &p.client,
		catalog:    p.catalogClient,
		zenStream:  p.zenClient,
		zenCatalog: p.zenCatalogClient,
		goStream:   p.goClient,
		goCatalog:  p.goCatalogClient,
	}}
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
	if limit, _, _, _ := p.lookupModelsDevInfo(modelName); limit > 0 {
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
// context fields. ok is false when no positive limit was found. Sole-model
// auto-select only applies to the single default endpoint: with additional
// registered profiles the aggregate list is a merge of several catalogs and
// "sole model" is not meaningful.
func (p *OpenAIProvider) contextLimitFromModels(models []openai.Model) (int, bool) {
	if len(models) == 1 && !p.hasMultipleProfiles() {
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
		limit, cost, efforts, desc := p.resolveContextLimit(m.RawJSON(), m.ID)
		info := ModelInfo{
			ID:               m.ID,
			ContextLimit:     limit,
			Current:          m.ID == current,
			Provider:         p.profileFor(m.ID),
			Description:      desc,
			ReasoningEfforts: efforts,
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

// modelsDevURLsFor returns the models.dev base URLs to try for modelID: the
// owning profile's URLs first (so a model listed on a second endpoint
// resolves against its own registry entry), falling back to the default
// profile's URLs when the catalog has not been fetched.
func (p *OpenAIProvider) modelsDevURLsFor(modelID string) []string {
	p.modelsMu.RLock()
	info := p.modelProfile[modelID]
	p.modelsMu.RUnlock()
	if info.baseURL != "" {
		return modelsDevURLsForBaseURL(info.baseURL)
	}
	return modelsDevURLsForBaseURL(p.defaultBaseURL())
}

// modelsDevURLsForBaseURL is the shared URL derivation for one endpoint: the
// base URL itself, plus the OpenCode zen/go endpoints when it is an OpenCode
// URL. Deduplicated.
func modelsDevURLsForBaseURL(baseURL string) []string {
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
	add(baseURL)
	if isOpencodeURL(baseURL) {
		add(openCodeZenBaseURL)
		add(openCodeGoBaseURL)
	}
	return urls
}

// lookupModelsDevInfo queries the models.dev registry by base URL + model ID
// for context limit, pricing, accepted reasoning-effort values, and the model
// description in a single pass. OpenCode dual endpoints are both tried so zen
// and go models resolve. Only models.dev entries with a usable limit or cost
// are returned; the loop keeps trying the remaining URLs otherwise, matching
// the previous behavior.
func (p *OpenAIProvider) lookupModelsDevInfo(modelID string) (int, *modelinfo.Cost, []string, string) {
	if p == nil || p.modelInfo == nil || modelID == "" {
		return 0, nil, nil, ""
	}
	for _, u := range p.modelsDevURLsFor(modelID) {
		lim, cost, efforts, desc, err := p.modelInfo.Resolve(u, modelID)
		if err != nil {
			continue
		}
		var c *modelinfo.Cost
		if cost != nil && (cost.Input > 0 || cost.Output > 0) {
			c = cost
		}
		if lim.Context > 0 {
			return lim.Context, c, efforts, desc
		}
		if c != nil {
			return 0, c, efforts, desc
		}
	}
	return 0, nil, nil, ""
}

// resolveContextLimit resolves context limit, pricing, accepted
// reasoning-effort values, and the model description. Provider JSON is tried
// first for the limit, but models.dev is always consulted for cost, efforts,
// and description since provider JSON never includes them.
func (p *OpenAIProvider) resolveContextLimit(rawJSON, modelID string) (int, *modelinfo.Cost, []string, string) {
	// Always look up models.dev for pricing/efforts/description (provider JSON
	// never has them).
	var cost *modelinfo.Cost
	var efforts []string
	var desc string
	devLimit, devCost, devEfforts, devDesc := p.lookupModelsDevInfo(modelID)
	if devCost != nil {
		cost = devCost
	}
	if len(devEfforts) > 0 {
		efforts = devEfforts
	}
	if devDesc != "" {
		desc = devDesc
	}
	// Prefer provider JSON limit, then models.dev, then default.
	if limit := parseContextLimitFromJSON(rawJSON); limit > 0 {
		return limit, cost, efforts, desc
	}
	if devLimit > 0 {
		return devLimit, cost, efforts, desc
	}
	return config.DefaultContextLimit, cost, efforts, desc
}
