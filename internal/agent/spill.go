package agent

import (
	"strings"
	"sync/atomic"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	"gogen/internal/spill"
)

// spillEnabled gates the spill feature (config: output_spill, default on).
// When off, oversized tool output takes the legacy plain-cap truncation
// path and no spill files are created. Package-global like the other
// Configure* seams (web fetch, prompts): main.go applies it at startup from
// the merged config, and the web server's runtime-config handler applies
// live changes. Lifecycle paths that must keep working regardless of the
// gate (fork locator repointing, session-dir cleanup) do NOT check it.
var spillEnabled atomic.Bool

func init() { spillEnabled.Store(true) }

// ConfigureOutputSpill sets whether oversized tool output is spilled to the
// session's spill dir (head/tail preview + locator) instead of the legacy
// plain-cap truncation.
func ConfigureOutputSpill(enabled bool) {
	spillEnabled.Store(enabled)
}

// OutputSpillEnabled reports the current spill gate state (config:
// output_spill). Exported so the runtime-config tests can verify the live
// application landed on the runtime target.
func OutputSpillEnabled() bool {
	return spillEnabled.Load()
}

// Spill storage: oversized tool output is persisted to a private,
// session-scoped directory (<workingDir>/.gogen/spill/session-<id>, named
// after the raw session id like .gogen/sessions/<id>.json so users can
// find it) and the inline result becomes a head/tail preview + locator the
// model can retrieve on demand with read_file/search_code. Both seams are
// best-effort — any failure falls back to the plain truncation path, so a
// spill problem can never turn a capped result into a tool error.
//
// Lifecycle: the session store removes a session's spill dir wherever it
// destroys the session's persisted state (user delete, prune, nested
// cascade) — see spill.RemoveSessionDir. Fork and /new deliberately keep
// the old session's spill: a forked transcript copies the parent's locator
// lines verbatim, and those paths must stay valid until the PARENT is
// deleted (after which the child keeps its head/tail preview text but the
// on-demand retrieval of the middle is gone).

// spillStore returns the spill store for the agent's current working dir,
// or nil when spilling is unavailable (no working dir or no session id —
// bare NewAgent sessions, e.g. most tests, keep the legacy cap behavior).
// The store is stateless and rebuilt per call, so a working-dir change is
// picked up immediately.
func (a *Agent) spillStore() *spill.Store {
	if !spillEnabled.Load() {
		return nil
	}
	if a.WorkingDir == "" || a.SessionID == "" {
		return nil
	}
	return spill.NewStore(a.WorkingDir)
}

// RepointSpillLocators rewrites the spill locator lines of a forked
// transcript so they reference the CHILD's spill dir instead of the
// original session's: ForkMessages copies the original's messages
// verbatim — locators included — and the session store removes the
// original's spill files when that session is deleted, which would leave
// the child's on-demand retrieval dangling. Each referenced file is
// hardlinked into the child's spill dir (spill.RepointFile) under the same
// name, and the copied locator text is rewritten to the new path. The scan
// is tool results only: those are the locator lines this feature wrote.
// Anything that cannot be repointed (missing file, path outside the spill
// root, link failure) keeps its original locator — best-effort, matching
// the rest of the spill feature; the head/tail preview survives in the
// transcript regardless. Called on every fork path (TUI ForkSession, web
// sessionFork, subagent continuation fork) with the child's session id.
func (a *Agent) RepointSpillLocators(msgs []llm.Message, toSessionID string) {
	if a.WorkingDir == "" || toSessionID == "" {
		return
	}
	for i := range msgs {
		if msgs[i].Role != "tool" || !strings.Contains(msgs[i].Content, spill.LocatorMarker) {
			continue
		}
		// RewriteLocators replaces the parsed locator SPANS: the rewrite can
		// never land on a copy of the same path text elsewhere in the result
		// (where a first-match strings.Replace did, leaving the real locator
		// pointing at the parent's dir and dangling once it was deleted).
		msgs[i].Content = spill.RewriteLocators(msgs[i].Content, func(oldPath string) (string, bool) {
			newPath, err := spill.RepointFile(a.WorkingDir, oldPath, toSessionID)
			if err != nil || newPath == oldPath {
				return "", false
			}
			return newPath, true
		})
	}
}

// commandSpillTarget returns a lazily-opened spill target for one
// foreground command's output, or nil when spilling is unavailable. The
// executor opens the file only on the first byte past the in-memory cap,
// so commands that fit the cap never touch the disk.
func (a *Agent) commandSpillTarget(tool string) *spill.Target {
	store := a.spillStore()
	if store == nil {
		return nil
	}
	return store.NewTarget(a.SessionID, tool)
}

// capToolResult caps an oversized tool result, spilling the full output to
// the session's spill dir first so nothing is lost: the stored result
// becomes spill.Preview — a rune-safe head/tail within the context cap,
// plus a locator ("full output saved to <path>") and a retrieval hint.
// Falls back to the plain cap (TruncateToolResult) when spilling is
// unavailable or the save fails, and for results already truncated
// upstream: those carry the standard marker and their data beyond the
// upstream cap no longer exists, so there is nothing left to spill.
//
// a.Context must be non-nil.
func (a *Agent) capToolResult(tool, result string) string {
	limit := a.Context.SettingsSnapshot().MaxToolResultBytes
	if limit <= 0 || len(result) <= limit || alreadyPartial(result) {
		return a.Context.TruncateToolResult(result)
	}
	store := a.spillStore()
	if store == nil {
		return a.Context.TruncateToolResult(result)
	}
	path, err := store.Save(a.SessionID, tool, []byte(result))
	if err != nil {
		// Best-effort: keep the exact pre-spill truncation behavior.
		return a.Context.TruncateToolResult(result)
	}
	return spill.Preview(result, limit, path, int64(len(result)))
}

// alreadyPartial reports whether a tool result was already cut upstream, so
// re-spilling it would only persist the cut text (the documented rule: such a
// result falls back to the plain cap):
//
//   - a spill preview: the body carries an intact locator line naming the
//     saved file (spill.LocatorPaths is a strict structural parse);
//   - a plain cap: the standard truncation marker TERMINATES the body — the
//     shape every cap producer writes (the context manager's tool-result cap,
//     the executor's command cap, and the per-tool footers in
//     search/explore/web_fetch/references).
//
// The shape matters: a body that merely CONTAINS the marker text — a file
// quoting it, a session snapshot, a log line — was not truncated, and
// treating it as one silently downgraded the result to a head-only cut
// instead of the head + locator + tail spill preview this seam exists to
// deliver.
func alreadyPartial(result string) bool {
	return contextmgr.HasTruncatedTail(result) || len(spill.LocatorPaths(result)) > 0
}
