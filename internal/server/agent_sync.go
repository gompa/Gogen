package server

// Agent locking on the web Server:
//
//   turnMu  — exclusive "turn" ownership across WebSocket clients (one mutator).
//   agentMu — brief snapshots of Agent memory for concurrent readers
//             (list_sessions, connect history, context stats, config).
//
// Lock order: always acquire turnMu before agentMu. Never take turnMu while
// holding agentMu. Readers that do not mutate may take agentMu.RLock alone.
//
// Never hold agentMu across provider, tool, or disk I/O (ListModels,
// SelectModel, StreamProcessInput, SessionStore.List, ContextStats
// tokenization). That pinned WS connect/history behind /v1/models, session
// listing, and the full LLM turn — the slow --web startup/population bug.
// turnMu already serializes mutators for the duration of a turn.
//
// FS browser paths use Executor.GetWorkingDir (its own mutex) so they do not
// need agentMu and can proceed during an active turn.

func (s *Server) lockAgentRead(fn func()) {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	fn()
}

func (s *Server) lockAgentWrite(fn func()) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	fn()
}
