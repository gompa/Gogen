package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// wsHandleProviderSave upserts a registered OpenAI-compatible provider
// profile from the settings modal (provider_save). The change applies to
// new sessions (provider factory reads the workspace list) AND live
// sessions (SetProfiles sweep), persists the config with secrets, and
// broadcasts the fresh provider list to every tab.
func wsHandleProviderSave(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleProviderSave(ws, msg)
}

// wsHandleProviderDelete removes a registered provider profile
// (provider_delete). The implicit default profile (legacy config fields)
// cannot be deleted; sessions whose selected model was served by the
// removed profile are re-validated so they never silently route to the
// wrong endpoint.
func wsHandleProviderDelete(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleProviderDelete(ws, msg)
}

// wsHandleTestProvider runs the connectivity + catalog test against a
// THROWAWAY provider built from the request (test_provider) — never
// registered, never wired to a session. The reply is a provider_test
// message carrying ok/latency/models/error.
func wsHandleTestProvider(s *Server, ws *wsConn, r *http.Request, pane **sessionRuntime, target *sessionRuntime, msg WSMessage, holder *userTermHolder) {
	s.handleTestProvider(ws, msg)
}

func (s *Server) handleProviderSave(ws *wsConn, msg WSMessage) {
	op := msg.ProviderOp
	if op == nil || strings.TrimSpace(op.Name) == "" {
		writeNoticeError(ws, "provider", "Error: provider name is required")
		return
	}
	name := strings.TrimSpace(op.Name)
	if strings.EqualFold(name, "default") {
		// The default profile is EDITABLE (base URL / key / model — the
		// legacy config fields) but never deletable. A blank key keeps the
		// stored one (accidental-key-wipe guard); a blank base URL means the
		// official OpenAI endpoint. The model edit applies LIVE: the
		// workspace default is updated so new sessions seed from it (the
		// base URL / key already applied live via SetProfiles).
		if op.BaseURL != "" && !validHTTPURL(op.BaseURL) {
			writeNoticeError(ws, "provider", fmt.Sprintf("Error: invalid base URL %q (want http(s))", op.BaseURL))
			return
		}
		r := s.ws.GetRuntimeConfig()
		r.OpenAIURL = strings.TrimSpace(op.BaseURL)
		if op.APIKey != "" {
			r.OpenAIKey = op.APIKey
		}
		if op.Model != "" {
			r.OpenAIModel = op.Model
			s.ws.SetDefaultModel(op.Model)
		}
		s.ws.SetRuntimeConfig(r)
		// Refresh live session providers (SetProfiles rebuilds the default
		// profile's clients), persist with secrets, broadcast.
		s.applyProviderList(ws, s.ws.GetOpenAIProviders())
		return
	}
	if op.BaseURL != "" && !validHTTPURL(op.BaseURL) {
		writeNoticeError(ws, "provider", fmt.Sprintf("Error: invalid base URL %q (want http(s))", op.BaseURL))
		return
	}
	baseURL := strings.TrimSpace(op.BaseURL)

	providers := s.ws.GetOpenAIProviders()
	idx := -1
	for i, p := range providers {
		if p.Name == name {
			idx = i
			break
		}
	}
	entry := config.OpenAIProviderConfig{
		Name:    name,
		BaseURL: baseURL,
		Model:   strings.TrimSpace(op.Model),
	}
	if idx >= 0 {
		// A blank apiKey on an existing profile keeps the stored key.
		entry.APIKey = providers[idx].APIKey
		if op.APIKey != "" {
			entry.APIKey = op.APIKey
		}
		providers[idx] = entry
	} else {
		entry.APIKey = op.APIKey
		providers = append(providers, entry)
	}
	s.applyProviderList(ws, providers)
}

func (s *Server) handleProviderDelete(ws *wsConn, msg WSMessage) {
	op := msg.ProviderOp
	if op == nil || strings.TrimSpace(op.Name) == "" {
		writeNoticeError(ws, "provider", "Error: provider name is required")
		return
	}
	name := strings.TrimSpace(op.Name)
	if strings.EqualFold(name, "default") {
		writeNoticeError(ws, "provider", "Error: \"default\" is the built-in profile from the legacy config fields and cannot be deleted")
		return
	}
	providers := s.ws.GetOpenAIProviders()
	idx := -1
	for i, p := range providers {
		if p.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeNoticeError(ws, "provider", fmt.Sprintf("Error: provider %q is not registered", name))
		return
	}
	providers = append(providers[:idx], providers[idx+1:]...)
	s.applyProviderList(ws, providers)
	// Sessions whose selected model was served by the removed profile must
	// not silently route to the wrong endpoint: re-validate each session's
	// current model against the refreshed catalog and clear it when gone.
	go s.revalidateSessionModels()
}

// applyProviderList installs the new registered provider list: the
// workspace accessor is updated (new sessions seed from it), every live
// session provider is refreshed via SetProfiles, the effective config is
// persisted WITH secrets (provider keys are user-entered — decided policy),
// and a config broadcast syncs every tab. Runs off the read loop (file
// write + sweep).
func (s *Server) applyProviderList(ws *wsConn, providers []config.OpenAIProviderConfig) {
	s.ws.SetOpenAIProviders(providers)
	go func() {
		s.applyProviderProfilesToAll()
		if s.config != nil {
			s.persistConfigForced(s.effectiveConfig())
		}
		s.broadcastConfigAll()
	}()
}

// applyProviderProfilesToAll refreshes every live session provider with the
// workspace's current registered profile list. SetProfiles swaps the client
// sets and invalidates the catalog cache; the next ListModels/turn
// re-fetches every endpoint. No turn locks needed — all provider state is
// internally synchronized.
func (s *Server) applyProviderProfilesToAll() {
	profiles := s.ws.providerProfiles()
	for _, id := range s.registry.activeIDs() {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		if p, ok := rt.agent.Provider.(*llm.OpenAIProvider); ok {
			_ = p.SetProfiles(profiles)
		}
	}
}

// revalidateSessionModels clears the current model of any session whose
// model is no longer listed by its (refreshed) provider, mirroring
// ValidateRestoredModel: clear, then let sole-model auto-select fill the
// gap when the provider serves exactly one model. Runs off the read loop;
// ListModels is bounded (modelsCatalogTimeout) and single-flighted, and
// catalog errors keep the model (fail-open, like recheckRestoredModel).
func (s *Server) revalidateSessionModels() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range s.registry.activeIDs() {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		model := rt.agent.CurrentModel()
		if model == "" {
			continue
		}
		models, err := rt.agent.ListModels(ctx)
		if err != nil {
			continue
		}
		found := false
		for _, m := range models {
			if m.ID == model {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if p, ok := rt.agent.Provider.(*llm.OpenAIProvider); ok {
			_ = p.SetModel("")
		}
		_, _ = rt.agent.Provider.ModelContextLimit(ctx) // sole-model auto-select
		s.pushConfigForAgent(rt.agent)
	}
}

func (s *Server) handleTestProvider(ws *wsConn, msg WSMessage) {
	op := msg.ProviderOp
	if op == nil {
		writeNoticeError(ws, "provider", "Error: missing provider test request")
		return
	}
	req := *op
	// Testing a registered profile by name uses its STORED credentials (the
	// client only knows apiKeySet, never the key itself); the add-form test
	// carries the endpoint to test directly.
	if req.Name != "" {
		for _, p := range s.ws.GetOpenAIProviders() {
			if p.Name == req.Name {
				if req.BaseURL == "" {
					req.BaseURL = p.BaseURL
				}
				if req.APIKey == "" {
					req.APIKey = p.APIKey
				}
				if req.Model == "" {
					req.Model = p.Model
				}
				break
			}
		}
	}
	if req.BaseURL != "" && !validHTTPURL(req.BaseURL) {
		writeNoticeError(ws, "provider", fmt.Sprintf("Error: invalid base URL %q (want http(s))", req.BaseURL))
		return
	}
	go func() {
		start := time.Now()
		reply := func(res ProviderTestResult) {
			_ = ws.writeJSON(WSMessage{Type: "provider_test", ProviderTest: &res})
		}
		prov, err := s.providerTestBuilder(req, s.ws.GetWorkingDir())
		if err != nil {
			reply(ProviderTestResult{Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()})
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		models, err := prov.ListModels(ctx)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			reply(ProviderTestResult{Error: err.Error(), LatencyMs: latency})
			return
		}
		reply(ProviderTestResult{
			OK:        true,
			LatencyMs: latency,
			Models:    s.modelEntries(models),
		})
	}()
}

// validHTTPURL reports whether v parses as an http(s) URL with a host.
func validHTTPURL(v string) bool {
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
