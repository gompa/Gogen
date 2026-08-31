package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// dynamicCatalog is a /models server with a mutable model list and a
// failure switch: setDown makes every request fail (like an endpoint that
// went down mid-session), set replaces the listed models.
type dynamicCatalog struct {
	mu     sync.Mutex
	models []string
	down   bool
}

func (d *dynamicCatalog) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		d.mu.Lock()
		down := d.down
		models := append([]string(nil), d.models...)
		d.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		data := make([]map[string]any, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

func (d *dynamicCatalog) set(models ...string) {
	d.mu.Lock()
	d.models = models
	d.mu.Unlock()
}

func (d *dynamicCatalog) setDown(down bool) {
	d.mu.Lock()
	d.down = down
	d.mu.Unlock()
}

// invalidateCatalogForTest forces the next listModels to re-fetch every
// endpoint (bypassing the TTL cache and the failure backoff).
func (p *OpenAIProvider) invalidateCatalogForTest() {
	p.modelsMu.Lock()
	p.modelsCache = nil
	p.modelsCachedAt = time.Time{}
	p.modelsFetchFailedAt = time.Time{}
	p.modelsMu.Unlock()
}

// TestStickyRoutingKeepsDownedOwner pins the reported bug: a model the user
// runs on a secondary (local) endpoint must NOT be re-homed to the default
// profile when the local endpoint stops answering and the default happens to
// list a same-ID model. The downed owner keeps its entry; once the owner is
// back and no longer lists the model (it moved), the surviving lister takes
// over.
func TestStickyRoutingKeepsDownedOwner(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "local-model", t.TempDir(), nil)

	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	wantDef := p.profiles[0].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("local-model must route to the local endpoint while it is up")
	}

	// The default gateway starts listing the SAME model ID (a collision)
	// and the local endpoint goes down.
	def.set("remote-model", "local-model")
	loc.setDown(true)
	p.invalidateCatalogForTest()
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("downed owner displaced: a surviving gateway's same-ID model is a collision, not ownership")
	}
	p.modelsMu.RLock()
	owner := p.modelOwner["local-model"]
	p.modelsMu.RUnlock()
	if owner != "local" {
		t.Fatalf("modelOwner[local-model] = %q, want local", owner)
	}

	// The owner comes back and no longer lists the model (it moved): the
	// surviving lister takes over.
	loc.setDown(false)
	loc.set()
	p.invalidateCatalogForTest()
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != wantDef {
		t.Fatal("once the owner is up and no longer lists the model, the earliest surviving lister must take over")
	}
}

// TestFailoverToCoListedEndpoint pins the other direction: a model listed by
// BOTH endpoints (owner = the first profile, per precedence) must fail over
// to the co-listed endpoint when the owner goes down — the model is
// genuinely available there — and then stay put (no re-homing flap) when
// the owner returns.
func TestFailoverToCoListedEndpoint(t *testing.T) {
	def := &dynamicCatalog{models: []string{"shared-model"}}
	loc := &dynamicCatalog{models: []string{"shared-model", "local-only"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "shared-model", t.TempDir(), nil)

	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	wantDef := p.profiles[0].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantDef {
		t.Fatal("both endpoints up: the first profile (default) wins the duplicate, as before")
	}

	// The owner (default) goes down: fail over to the co-listed endpoint.
	def.setDown(true)
	p.invalidateCatalogForTest()
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("owner down: a co-listed endpoint must take over (the model is genuinely available there)")
	}

	// The owner returns: the model stays on the endpoint serving it (no
	// silent re-homing back to the first-listing profile).
	def.setDown(false)
	p.invalidateCatalogForTest()
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("owner back: the model must stay on the endpoint that serves it (no re-homing flap)")
	}
}

// TestSetProfilesKeepsOwnerRouting pins the settings-save path: SetProfiles
// rebuilds the client sets and clears the routing cache while the owner is
// down — the owner record must survive (the profile is still registered), so
// the model keeps routing to its owner instead of falling back to the
// default profile.
func TestSetProfilesKeepsOwnerRouting(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	profiles := []ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}
	p := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}

	loc.setDown(true)
	// A provider settings save rebuilds the profile set while the owner is
	// down (modelClient cleared, owner record must survive).
	if err := p.SetProfiles(profiles); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantLocal := p.profiles[1].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("after SetProfiles with the owner down, the model must keep routing to its owner")
	}

	// Removing the owner profile drops the record: the model falls through
	// to the default client (the documented fallback; re-validation then
	// surfaces the gap to the user).
	if err := p.SetProfiles([]ProviderProfile{{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"}}); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantDef := p.profiles[0].stream
	_, ownerKept := p.modelOwner["local-model"]
	p.modelsMu.RUnlock()
	if ownerKept {
		t.Fatal("owner record for a removed profile must be dropped")
	}
	if got := p.clientForModel(t.Context()); got != wantDef {
		t.Fatal("model of a removed profile must fall back to the default client")
	}
}

// TestOwnerRegistrySharedAcrossProviders pins the cross-provider inheritance:
// a FRESH provider (a new session or subagent) whose owning endpoint is down
// cannot learn the owner from its own catalog merge — the shared record
// (published by a sibling provider) must steer it to the owner instead of
// the default profile.
func TestOwnerRegistrySharedAcrossProviders(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	profiles := []ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}
	reg := NewOwnerRegistry()
	p1 := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	p1.SetOwnerRegistry(reg)
	if _, err := p1.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The owner goes down; a fresh provider has no routing of its own.
	loc.setDown(true)
	p1.modelsMu.RLock()
	wantLocal1 := p1.profiles[1].stream
	p1.modelsMu.RUnlock()
	if got := p1.clientForModel(t.Context()); got != wantLocal1 {
		t.Fatal("the publishing provider must keep routing to its (down) owner")
	}

	p2 := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	p2.SetOwnerRegistry(reg)
	p2.modelsMu.RLock()
	wantLocal2 := p2.profiles[1].stream
	p2.modelsMu.RUnlock()
	if got := p2.clientForModel(t.Context()); got != wantLocal2 {
		t.Fatal("fresh provider must route the model to its shared-record owner, not the default profile")
	}

	// The owner returns: the fresh provider's own fetch now maps it.
	loc.setDown(false)
	p2.invalidateCatalogForTest()
	if got := p2.clientForModel(t.Context()); got != wantLocal2 {
		t.Fatal("after the owner returns, routing must still resolve to it")
	}
}

// TestRegistryFallbackSurvivesCollisionMerge pins the follow-up to the
// shared-registry path: a fresh provider that resolved a model through the
// shared record (owner down) must keep it on the owner when its next catalog
// merge sees a surviving endpoint list the same ID — the fallback resolution
// records the owner in the provider's own record, so the sticky rule applies
// to the merge.
func TestRegistryFallbackSurvivesCollisionMerge(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	profiles := []ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}
	reg := NewOwnerRegistry()
	p1 := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	p1.SetOwnerRegistry(reg)
	if _, err := p1.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Owner down; the fresh provider resolves through the shared record.
	loc.setDown(true)
	p2 := NewOpenAIProviderWithProfiles(profiles, "local-model", t.TempDir(), nil)
	p2.SetOwnerRegistry(reg)
	p2.modelsMu.RLock()
	wantLocal := p2.profiles[1].stream
	p2.modelsMu.RUnlock()
	if got := p2.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("fresh provider must resolve the model through the shared record")
	}

	// The gateway starts listing the SAME ID; force p2's next merge to run
	// (drop the cached entry) — it must not steal the model.
	def.set("remote-model", "local-model")
	p2.modelsMu.Lock()
	delete(p2.modelClient, "local-model")
	p2.modelsMu.Unlock()
	p2.invalidateCatalogForTest()
	if got := p2.clientForModel(t.Context()); got != wantLocal {
		t.Fatal("collision on the surviving gateway must not displace the shared-record owner")
	}
}

// TestUnknownModelWithoutOwnerFallsBackToDefault pins the unchanged behavior
// for a model no endpoint lists and no owner record covers: the default
// profile still serves it (custom/router models on the default endpoint).
func TestUnknownModelWithoutOwnerFallsBackToDefault(t *testing.T) {
	def := &dynamicCatalog{models: []string{"remote-model"}}
	loc := &dynamicCatalog{models: []string{"local-model"}}
	defSrv := httptest.NewServer(def.handler())
	locSrv := httptest.NewServer(loc.handler())
	t.Cleanup(defSrv.Close)
	t.Cleanup(locSrv.Close)

	p := NewOpenAIProviderWithProfiles([]ProviderProfile{
		{Name: "default", BaseURL: defSrv.URL, APIKey: "k1"},
		{Name: "local", BaseURL: locSrv.URL, APIKey: "k2"},
	}, "mystery-model", t.TempDir(), nil)
	if _, err := p.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.modelsMu.RLock()
	wantDef := p.profiles[0].stream
	p.modelsMu.RUnlock()
	if got := p.clientForModel(t.Context()); got != wantDef {
		t.Fatal("unknown model with no known owner must fall back to the default profile (unchanged behavior)")
	}
}

// TestOwnerRegistryNilSafe pins the nil-receiver contract: a standalone
// provider (no shared record) must behave exactly as before.
func TestOwnerRegistryNilSafe(t *testing.T) {
	var r *OwnerRegistry
	r.Record("m", "p")
	if _, ok := r.Owner("m"); ok {
		t.Fatal("nil registry must report no owner")
	}
	r = NewOwnerRegistry()
	r.Record("m", "p")
	if got, ok := r.Owner("m"); !ok || got != "p" {
		t.Fatalf("Owner = %q, %v; want p, true", got, ok)
	}
}
