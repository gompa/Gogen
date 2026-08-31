package llm

import "sync"

// OwnerRegistry is a shared, process-scoped record of which registered
// provider profile last served each model ID. Every provider in a scope
// (a web workspace, or a TUI process) shares one registry: each provider
// publishes the owners it learns from successful catalog fetches, and a
// provider whose own routing for a model is unknown — a fresh session or
// subagent provider whose owning endpoint is down, so its own catalog merge
// cannot map the model — consults the record before falling back to the
// default profile. Without it, a model the user runs on a secondary (e.g.
// local) endpoint would be re-homed to the default profile — possibly a
// remote gateway that does not serve it — the moment the owning endpoint
// stops answering.
//
// A recorded owner only steers the fallback: the consulting provider must
// still have a profile registered under that name (checked at use time), so
// records survive SetProfiles harmlessly and stop applying once the profile
// is gone.
//
// Entries are last-known, not live: they are never pruned. That is
// deliberate — the record routes a model to the endpoint that last served
// it, which is exactly where the user's model lives while that endpoint is
// unreachable; per-provider routing self-corrects on the owner's next
// successful fetch.
//
// Lock ordering: providers may call Record while holding their modelsMu
// (the listModels merge); Owner is always called WITHOUT holding modelsMu
// (ownerClientForModel releases it first). Nothing takes a registry lock
// before a provider modelsMu, so the order modelsMu → registry.mu is
// acyclic.
type OwnerRegistry struct {
	mu     sync.RWMutex
	owners map[string]string // model ID → profile name
}

// NewOwnerRegistry builds an empty shared owner record.
func NewOwnerRegistry() *OwnerRegistry {
	return &OwnerRegistry{owners: make(map[string]string)}
}

// Record notes that profileName serves modelID (from a successful catalog
// fetch or sticky routing decision). Nil-safe; empty arguments are ignored.
func (r *OwnerRegistry) Record(modelID, profileName string) {
	if r == nil || modelID == "" || profileName == "" {
		return
	}
	r.mu.Lock()
	r.owners[modelID] = profileName
	r.mu.Unlock()
}

// Owner returns the last profile recorded as serving modelID. Nil-safe.
func (r *OwnerRegistry) Owner(modelID string) (string, bool) {
	if r == nil || modelID == "" {
		return "", false
	}
	r.mu.RLock()
	name, ok := r.owners[modelID]
	r.mu.RUnlock()
	return name, ok
}
