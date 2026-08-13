package server

import (
	"fmt"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/treesitter"
)

// runtimeConfigFields are the config option names the runtime-config branch
// accepts (client→server "config" with ConfigFields). A name in the list
// means the corresponding WSMessage value is applied — including explicit
// empty/zero values.
var runtimeConfigFields = map[string]bool{
	"commandSafety": true, "commandAllowlist": true, "deleteApproval": true,
	"commandSandbox": true, "commandTimeoutSecs": true,
	"contextLimit": true, "compactThreshold": true, "compactKeepRecentMessages": true,
	"maxToolResultBytes": true, "compactReserveTokens": true,
	"webFetch": true, "webSearch": true, "webSearchBackend": true, "webSearchApiKey": true,
	"webAllowedDomains": true, "webFetchMode": true,
	"treesitter": true, "treesitterLangs": true, "preserveReasoning": true,
	"sessionMaxCount": true, "sessionMaxAgeDays": true, "webApprovalHoldSecs": true,
	"webBind": true, "webAllowedOrigins": true, "webAuthToken": true,
	"webTLSCertFile": true, "webTLSKeyFile": true, "webMaxActiveSessions": true,
	"mcp":              true,
	"subagentModel":    true,
	"boardStartPrompt": true, "systemPrompt": true, "subagentPrompt": true,
}

// maxPromptTemplateLen caps the configurable prompt templates (settings
// modal). The RENDERED prompts can exceed this via ticket content; the cap
// bounds the templates themselves.
const maxPromptTemplateLen = 8192

// restartStagedFields are the options that cannot apply to the running
// process: they are persisted and take effect on the next start (A0b). The
// value is the config-key name shown to the user (toast + banner).
var restartStagedFields = map[string]string{
	"webBind":              "web_bind",
	"webAllowedOrigins":    "web_allowed_origins",
	"webAuthToken":         "web_auth_token",
	"webTLSCertFile":       "web_tls_cert_file",
	"webTLSKeyFile":        "web_tls_key_file",
	"webMaxActiveSessions": "web_max_active_sessions",
	"mcp":                  "mcp",
}

// handleWSRuntimeConfig applies the settings-modal config options: each name
// in msg.ConfigFields names a value to apply (explicit empty/zero values are
// legal). Live-applied options update the runtime target immediately
// (executor, process globals, per-session context managers, session store)
// AND the workspace runtime overlay (source of truth for the push, new
// sessions, and persistence); restart-staged options only update the
// overlay. Any invalid value rejects the whole request; on success the
// effective config is persisted and every tab gets a fresh config push.
func (s *Server) handleWSRuntimeConfig(ws *wsConn, msg WSMessage) {
	if len(msg.ConfigFields) == 0 {
		return
	}
	seen := make(map[string]bool, len(msg.ConfigFields))
	for _, name := range msg.ConfigFields {
		if !runtimeConfigFields[name] {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: unknown config option %q", name))
			return
		}
		if seen[name] {
			writeNoticeError(ws, "settings", fmt.Sprintf("Error: config option %q listed twice", name))
			return
		}
		seen[name] = true
	}
	set := func(name string) bool { return seen[name] }

	r := s.ws.GetRuntimeConfig()
	var restartChanged []string
	for name := range seen {
		switch name {
		case "commandSafety":
			mode := strings.ToLower(strings.TrimSpace(msg.CommandSafetyMode))
			if mode != "blocklist" && mode != "allowlist" && mode != "off" {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid commandSafety %q (want blocklist, allowlist, or off)", msg.CommandSafetyMode))
				return
			}
			r.CommandSafetyMode = mode
		case "commandAllowlist":
			r.CommandAllowlist = msg.CommandAllowlist
		case "deleteApproval":
			mode := strings.ToLower(strings.TrimSpace(msg.DeleteApproval))
			if mode != "required" && mode != "off" {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid deleteApproval %q (want required or off)", msg.DeleteApproval))
				return
			}
			r.DeleteApproval = mode
		case "commandSandbox":
			mode := strings.ToLower(strings.TrimSpace(msg.CommandSandbox))
			if mode != "off" && mode != "bwrap" {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid commandSandbox %q (want off or bwrap)", msg.CommandSandbox))
				return
			}
			r.CommandSandbox = mode
		case "commandTimeoutSecs":
			if msg.CommandTimeoutSecs < 0 {
				writeNoticeError(ws, "settings", "Error: commandTimeoutSecs must be >= 0")
				return
			}
			r.CommandTimeoutSecs = msg.CommandTimeoutSecs
		case "contextLimit":
			if msg.ContextLimitConfig < 0 {
				writeNoticeError(ws, "settings", "Error: contextLimit must be >= 0")
				return
			}
			r.ContextLimit = msg.ContextLimitConfig
		case "compactThreshold":
			if msg.CompactThreshold < 0 || msg.CompactThreshold > 1 {
				writeNoticeError(ws, "settings", "Error: compactThreshold must be between 0 and 1")
				return
			}
			r.CompactThreshold = msg.CompactThreshold
		case "sessionMaxAgeDays":
			// -1 = "keep sessions forever" (the store's retention sentinel);
			// the merge path preserves it so it survives a restart.
			v := configValueFor(name, msg)
			if v < -1 {
				writeNoticeError(ws, "settings", "Error: sessionMaxAgeDays must be >= -1 (-1 = keep sessions forever)")
				return
			}
			applyConfigInt(&r, name, v)
		case "compactKeepRecentMessages", "maxToolResultBytes", "compactReserveTokens", "sessionMaxCount", "webApprovalHoldSecs", "webMaxActiveSessions":
			v := configValueFor(name, msg)
			if v < 0 {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: %s must be >= 0", name))
				return
			}
			applyConfigInt(&r, name, v)
		case "webFetch":
			if err := applyOnOff(&r.WebFetch, "webFetch", msg.WebFetch, ws); err != nil {
				return
			}
		case "webSearch":
			if err := applyOnOff(&r.WebSearch, "webSearch", msg.WebSearch, ws); err != nil {
				return
			}
		case "treesitter":
			if err := applyOnOff(&r.TreeSitter, "treesitter", msg.TreeSitter, ws); err != nil {
				return
			}
		case "webSearchBackend":
			v := strings.ToLower(strings.TrimSpace(msg.WebSearchBackend))
			if v != "" && v != "brave" {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid webSearchBackend %q (want brave or empty)", msg.WebSearchBackend))
				return
			}
			r.WebSearchBackend = v
		case "webSearchApiKey":
			r.WebSearchAPIKey = msg.WebSearchAPIKey
		case "webAllowedDomains", "webFetchMode", "treesitterLangs", "preserveReasoning", "webBind", "webAllowedOrigins", "webAuthToken", "webTLSCertFile", "webTLSKeyFile":
			applyConfigString(&r, name, stringValueFor(name, msg))
		case "subagentModel":
			// The default model for spawned subagents; empty clears back to
			// "inherit the parent's model". Any model id string is accepted —
			// catalog validation is fail-open at spawn time (the spawner
			// falls back to the workspace default when unselectable).
			raw := ""
			if msg.SubagentModel != nil {
				raw = *msg.SubagentModel
			}
			v := strings.TrimSpace(raw)
			if raw != "" && v == "" {
				writeNoticeError(ws, "settings", "Error: subagentModel must be a model id or empty (inherit)")
				return
			}
			r.SubagentModel = v
		case "mcp":
			v := strings.ToLower(strings.TrimSpace(msg.MCP))
			if _, ok := parseOnOff(v); !ok {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid mcp %q (want on or off)", msg.MCP))
				return
			}
			r.MCP = v
		case "boardStartPrompt", "systemPrompt", "subagentPrompt":
			raw := stringValueFor(name, msg)
			v := strings.TrimSpace(raw)
			if len(v) > maxPromptTemplateLen {
				writeNoticeError(ws, "settings", fmt.Sprintf("Error: %s exceeds %d characters", name, maxPromptTemplateLen))
				return
			}
			// Saving the default text verbatim stores nothing (no bake-in):
			// resolution applies the same rule to hand-edited files/env.
			switch name {
			case "boardStartPrompt":
				r.BoardStartPrompt = agent.NormalizePromptTemplate(v, agent.DefaultBoardStartPrompt)
			case "systemPrompt":
				r.SystemPrompt = agent.NormalizePromptTemplate(v, agent.DefaultSystemPromptTemplate())
			case "subagentPrompt":
				r.SubagentPrompt = agent.NormalizePromptTemplate(v, agent.DefaultSubagentPrompt)
			}
		}
		if display, ok := restartStagedFields[name]; ok {
			restartChanged = append(restartChanged, display)
		}
	}
	s.ws.SetRuntimeConfig(r)

	// Apply to the runtime targets (live tier only).
	if set("commandSafety") || set("commandAllowlist") {
		s.ws.Exec.SetCommandGuard(r.CommandSafetyMode, agent.ParseAllowlist(r.CommandAllowlist))
	}
	if set("deleteApproval") {
		s.ws.Exec.SetDeleteApproval(!strings.EqualFold(r.DeleteApproval, "off"))
	}
	if set("commandSandbox") {
		s.ws.Exec.SetSandbox(r.CommandSandbox)
	}
	if set("commandTimeoutSecs") {
		s.ws.Exec.SetCommandTimeout(time.Duration(r.CommandTimeoutSecs) * time.Second)
	}
	if set("webFetch") || set("webFetchMode") || set("webAllowedDomains") {
		agent.ConfigureWebFetch(parseOnOffValue(r.WebFetch), r.WebFetchMode, r.WebAllowedDomains)
	}
	if set("webSearch") {
		agent.ConfigureWebSearchEnabled(parseOnOffValue(r.WebSearch))
	}
	if set("webSearchBackend") || set("webSearchApiKey") {
		agent.ConfigureWebSearch(r.WebSearchBackend, r.WebSearchAPIKey)
	}
	if set("treesitter") || set("treesitterLangs") {
		treesitter.Configure(parseOnOffValue(r.TreeSitter), r.TreeSitterLangs)
	}
	if set("preserveReasoning") {
		s.applyPreserveReasoningToAll(r.PreserveReasoning)
	}
	if set("contextLimit") || set("compactThreshold") || set("compactKeepRecentMessages") || set("maxToolResultBytes") || set("compactReserveTokens") {
		s.applyContextSettingsToAll(r, set)
	}
	if set("sessionMaxCount") || set("sessionMaxAgeDays") {
		if s.ws.Store != nil {
			s.ws.Store.SetRetention(r.SessionMaxCount, r.SessionMaxAgeDays)
			s.pruneSessions()
		}
	}
	if set("systemPrompt") {
		// Live for every agent (subagents included): the next turn's system
		// message resolves the configured template.
		agent.ConfigureSystemPrompt(r.SystemPrompt)
	}
	if set("subagentPrompt") {
		// Live for every spawner (web + TUI): jobs are wrapped at spawn time.
		agent.ConfigureSubagentPrompt(r.SubagentPrompt)
	}

	// Persist + broadcast off the read loop (file write + fan-out). A staged
	// web_auth_token is user-entered in the UI, so the write forces secrets
	// (0600) like provider saves — the token must survive the restart it is
	// staged for. Restart-staged changes get a success notice listing them.
	forceSecrets := set("webAuthToken")
	go func() {
		if s.config != nil {
			if forceSecrets {
				s.persistConfigForced(s.effectiveConfig())
			} else {
				s.persistConfig(s.effectiveConfig())
			}
		}
		s.broadcastConfigAll()
	}()
	if len(restartChanged) > 0 {
		writeNotice(ws, "settings", true, "Saved — restart gogen for these to take effect: "+strings.Join(restartChanged, ", "))
	}
}

// applyOnOff validates an on/off value and stores it; it writes the error
// notice and returns non-nil on invalid input.
func applyOnOff(dst *string, name, v string, ws *wsConn) error {
	on, ok := parseOnOff(v)
	if !ok {
		writeNoticeError(ws, "settings", fmt.Sprintf("Error: invalid %s %q (want on or off)", name, v))
		return fmt.Errorf("invalid %s", name)
	}
	*dst = onOff(on)
	return nil
}

// applyConfigString writes a string runtime-config field by name.
func applyConfigString(r *config.Config, name, v string) {
	switch name {
	case "commandAllowlist":
		r.CommandAllowlist = v
	case "webAllowedDomains":
		r.WebAllowedDomains = v
	case "webFetchMode":
		r.WebFetchMode = v
	case "treesitterLangs":
		r.TreeSitterLangs = v
	case "preserveReasoning":
		r.PreserveReasoning = v
	case "webBind":
		r.WebBind = v
	case "webAllowedOrigins":
		r.WebAllowedOrigins = v
	case "webAuthToken":
		r.WebAuthToken = v
	case "webTLSCertFile":
		r.WebTLSCertFile = v
	case "webTLSKeyFile":
		r.WebTLSKeyFile = v
	}
}

// applyConfigInt writes an int runtime-config field by name.
func applyConfigInt(r *config.Config, name string, v int) {
	switch name {
	case "compactKeepRecentMessages":
		r.CompactKeepRecentMessages = v
	case "maxToolResultBytes":
		r.MaxToolResultBytes = v
	case "compactReserveTokens":
		r.CompactReserveTokens = v
	case "sessionMaxCount":
		r.SessionMaxCount = v
	case "sessionMaxAgeDays":
		r.SessionMaxAgeDays = v
	case "webApprovalHoldSecs":
		r.WebApprovalHoldSecs = v
	case "webMaxActiveSessions":
		r.WebMaxActiveSessions = v
	}
}

// configValueFor returns the int runtime-config value for a validated name.
func configValueFor(name string, msg WSMessage) int {
	switch name {
	case "compactKeepRecentMessages":
		return msg.CompactKeepRecentMessages
	case "maxToolResultBytes":
		return msg.MaxToolResultBytes
	case "compactReserveTokens":
		return msg.CompactReserveTokens
	case "sessionMaxCount":
		return msg.SessionMaxCount
	case "sessionMaxAgeDays":
		return msg.SessionMaxAgeDays
	case "webApprovalHoldSecs":
		return msg.WebApprovalHoldSecs
	case "webMaxActiveSessions":
		return msg.WebMaxActiveSessions
	}
	return 0
}

// stringValueFor returns the string runtime-config value for a validated
// name.
func stringValueFor(name string, msg WSMessage) string {
	switch name {
	case "commandSafety":
		return msg.CommandSafetyMode
	case "commandAllowlist":
		return msg.CommandAllowlist
	case "deleteApproval":
		return msg.DeleteApproval
	case "commandSandbox":
		return msg.CommandSandbox
	case "webFetch":
		return msg.WebFetch
	case "webSearch":
		return msg.WebSearch
	case "webSearchBackend":
		return msg.WebSearchBackend
	case "webSearchApiKey":
		return msg.WebSearchAPIKey
	case "webAllowedDomains":
		return msg.WebAllowedDomains
	case "webFetchMode":
		return msg.WebFetchMode
	case "treesitter":
		return msg.TreeSitter
	case "treesitterLangs":
		return msg.TreeSitterLangs
	case "preserveReasoning":
		return msg.PreserveReasoning
	case "webBind":
		return msg.WebBind
	case "webAllowedOrigins":
		return msg.WebAllowedOrigins
	case "webAuthToken":
		return msg.WebAuthToken
	case "webTLSCertFile":
		return msg.WebTLSCertFile
	case "webTLSKeyFile":
		return msg.WebTLSKeyFile
	case "boardStartPrompt":
		return msg.BoardStartPrompt
	case "systemPrompt":
		return msg.SystemPrompt
	case "subagentPrompt":
		return msg.SubagentPrompt
	}
	return ""
}

// parseOnOffValue renders an on/off string as a boolean (parseOnOff with a
// validated input never fails).
func parseOnOffValue(v string) bool {
	on, _ := parseOnOff(v)
	return on
}

// applyContextSettingsToAll pushes the live context settings to every live
// session's context manager, merging per field: only the options named by
// apply are taken from r, everything else keeps the session's CURRENT
// values (each manager's SettingsSnapshot). A wholesale replace would
// clobber per-session state — most notably a restored session's resolved
// ContextLimit (set via SetContextLimit on restore, which is not a manual
// limit): changing only compact_threshold must not reset a restored
// session's limit back to provider resolution. All Manager methods are
// internally synchronized, so no turn locks are needed.
func (s *Server) applyContextSettingsToAll(r config.Config, apply func(name string) bool) {
	for _, id := range s.registry.activeIDs() {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		if rt.agent.Context == nil {
			continue
		}
		next := rt.agent.Context.SettingsSnapshot()
		if apply("contextLimit") {
			next.ContextLimit = r.ContextLimit
		}
		if apply("compactThreshold") {
			next.CompactThreshold = r.CompactThreshold
		}
		if apply("compactKeepRecentMessages") {
			next.CompactKeepRecentMessages = r.CompactKeepRecentMessages
		}
		if apply("maxToolResultBytes") {
			next.MaxToolResultBytes = r.MaxToolResultBytes
		}
		if apply("compactReserveTokens") {
			next.CompactReserveTokens = r.CompactReserveTokens
		}
		rt.agent.Context.UpdateSettings(next)
	}
}

// applyPreserveReasoningToAll pushes the preserve_reasoning mode to every
// live session's OpenAI provider.
func (s *Server) applyPreserveReasoningToAll(mode string) {
	for _, id := range s.registry.activeIDs() {
		rt, ok := s.registry.get(id)
		if !ok {
			continue
		}
		if p, ok := rt.agent.Provider.(*llm.OpenAIProvider); ok {
			p.SetPreserveReasoningMode(mode)
		}
	}
}

// restartPendingFields lists restart-staged settings whose staged value
// differs from the running startup config (the modal's "restart to take
// effect" banner).
func (s *Server) restartPendingFields() []string {
	if s.config == nil {
		return nil
	}
	r := s.ws.GetRuntimeConfig()
	var out []string
	pick := func(name, staged, running string) {
		if staged != running {
			out = append(out, name)
		}
	}
	pick("web_bind", r.WebBind, s.config.WebBind)
	pick("web_allowed_origins", r.WebAllowedOrigins, s.config.WebAllowedOrigins)
	pick("web_auth_token", r.WebAuthToken, s.config.WebAuthToken)
	pick("web_tls_cert_file", r.WebTLSCertFile, s.config.WebTLSCertFile)
	pick("web_tls_key_file", r.WebTLSKeyFile, s.config.WebTLSKeyFile)
	pick("mcp", r.MCP, s.config.MCP)
	if r.WebMaxActiveSessions != s.config.WebMaxActiveSessions {
		out = append(out, "web_max_active_sessions")
	}
	return out
}
