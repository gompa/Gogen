package agent

import "gogen/internal/contextmgr"

// sessionMemory groups the session's deterministic, model-independent memory
// of what it did and what it can still act on (see
// contextmgr.SessionState). It is the cumulative source of the state manifest
// compaction appends to a summary, so a background job id created twenty
// calls ago still survives every later compaction instead of decaying through
// the model's recaps.
type sessionMemory struct {
	// SessionState is owned by the turn goroutine, like Messages: it is
	// folded during compaction under the session's turn lock and never read
	// lock-free elsewhere. A nil value is created lazily by
	// compactionSessionState, so agents built without NewAgent stay usable.
	SessionState *contextmgr.SessionState
}

// newSessionState returns a fresh deterministic session state for a session.
//
// The file-mutation classification is passed IN from the tool registry
// (FSMutatingToolNames, the MutatesFS flag set), the same way the manager is
// handed Tools/AllowedTools for the summarization request: the layer that owns
// the registry decides which tools write to the working tree, so contextmgr
// never keeps a second hardcoded list that could drift. A tool added to the
// registry with MutatesFS is then recorded as "modified" in the manifest with
// no edit outside this package.
func newSessionState() *contextmgr.SessionState {
	return contextmgr.NewSessionStateWithMutatingTools(FSMutatingToolNames())
}

// compactionSessionState returns the session's state, creating it on first
// use. Every construction path goes through newSessionState, so a lazily
// created state (an Agent built without NewAgent) carries the same registry
// classification as an eagerly created one; Reset keeps it (see
// SessionState.Reset).
func (a *Agent) compactionSessionState() *contextmgr.SessionState {
	if a == nil {
		return nil
	}
	if a.SessionState == nil {
		a.SessionState = newSessionState()
	}
	return a.SessionState
}
