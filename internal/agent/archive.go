package agent

import (
	"log"
	"time"

	"gogen/internal/llm"
	"gogen/internal/session"
)

// ArchiveEntry is one line of a session's archive sidecar (Phase 5):
// content that was shadowed out of the live history. The sidecar is an
// append-only JSONL file written by the session store (see
// session.Store.AppendArchive); the agent only produces entries. The
// canonical definition lives in internal/session; this alias keeps
// existing references compiling.
type ArchiveEntry = session.ArchiveEntry

// ArchiveAppender is optionally implemented by a SessionPersister: it
// appends shadowed content to the session's archive sidecar (Phase 5). The
// agent type-asserts to it instead of widening SessionPersister, so test
// doubles and stores without archive support keep working unchanged.
type ArchiveAppender interface {
	AppendArchive(workingDir, id string, entry ArchiveEntry) error
}

// archiveShadowedMessage appends a shadowed message to the session's
// archive sidecar (Phase 5). Best-effort: it returns false (and logs) when
// persistence is off, the store does not implement ArchiveAppender, or the
// write fails — the caller reports the outcome in the in-band notice so a
// failed archive is never silent.
func (a *Agent) archiveShadowedMessage(idx int, msg llm.Message, tokens int) bool {
	appender, ok := a.SessionStore.(ArchiveAppender)
	if !ok || a.SessionID == "" {
		return false
	}
	entry := ArchiveEntry{
		TS:      time.Now().UTC(),
		Kind:    "condensed_message",
		Index:   idx,
		Role:    msg.Role,
		Tokens:  tokens,
		Content: msg.Content,
		Model:   msg.Model,
	}
	if err := appender.AppendArchive(a.WorkingDir, a.SessionID, entry); err != nil {
		log.Printf("archive sidecar write failed (session %s): %v", a.SessionID, err)
		return false
	}
	return true
}
