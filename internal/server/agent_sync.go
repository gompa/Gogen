package server

// Agent locking on the web Server:
//
//   turnMu  — exclusive "turn" ownership across WebSocket clients (one mutator).
//   agentMu — protects Agent memory against concurrent readers (list_sessions,
//             connect history, context stats) while a turn is mutating state.
//
// Lock order: always acquire turnMu before agentMu. Never take turnMu while
// holding agentMu. Readers that do not mutate may take agentMu.RLock alone.
//
// Stream turns hold agentMu for the full StreamProcessInput call. That is
// intentional: Messages/session state mutate continuously during the LLM
// turn, and releasing early would race with RLock readers under -race.
// turnMu already serializes mutators; agentMu additionally excludes readers
// for the turn duration (list_sessions / connect stats wait until the turn
// ends). Do not "narrow" this lock without an alternate snapshot protocol.
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
