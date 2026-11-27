package contextmgr

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gogen/internal/llm"
)

// This file implements the deterministic session-state manifest that
// compaction appends to a summary. It replaces the old executed-call roster:
// instead of one line per WIRE tool call (with a call id that has no consumer
// after the pair is summarized away), the manifest is a cumulative projection
// of what the session can still act on and what it already tried:
//
//   - live/recoverable handles: background jobs, subagents, terminals, and
//     spill/archive locators (the ids tools actually take as arguments),
//   - failed operations and their one-line evidence (the "do not retry"
//     record a model recap reliably loses),
//   - files touched (deduplicated by path),
//   - collapsed operation outcomes (deduplicated by tool + salient argument).
//
// The manifest is model-independent: it is folded from tool results and
// notices by deterministic extractors, never written by the summarizer. It is
// cumulative across compactions in one session because the SessionState
// carries forward between them, and it is fenced so an older manifest inside
// a summarized summary can be parsed back (restore/fork) and stripped from a
// summarization request.
//
// Cache discipline: the manifest is materialized only when a compaction
// writes a summary (the prefix is already being rewritten), never per turn.
// A SessionState is owned by the turn goroutine, like Agent.Messages: writes
// happen during compaction under the session's turn lock, and no lock-free
// reader touches it.

// StateHeader and StateFooter fence the deterministic session-state block.
// The fence makes the block parseable (restore/fork seed the state from the
// previous summary) and strippable (a summarization request must not feed the
// old machine text back to the model).
const (
	StateHeader = "[Session state — deterministic, regenerated at compaction]"
	StateFooter = "[/Session state]"
)

// State handle kinds. They mirror the argument tools take, not the tool that
// produced the result: a "job" is addressable by background_job, an "agent"
// by send_message/get_agent_result, a "terminal" by the terminal tool, and a
// "spill" path by read_file.
const (
	stateKindJob      = "job"
	stateKindAgent    = "agent"
	stateKindTerminal = "terminal"
	stateKindSpill    = "spill"
)

// Handle statuses. Only live and recoverable entries are rendered: a finished
// job or closed terminal is not actionable, and the model can no longer do
// anything with it.
const (
	stateStatusLive        = "live"
	stateStatusRecoverable = "recoverable"
	stateStatusDone        = "done"
	stateStatusFailed      = "failed"
)

const (
	stateOutcomeOK     = "ok"
	stateOutcomeFailed = "failed"

	// maxStateTokens bounds the rendered manifest. The effective budget is
	// also capped at a fraction of the summarized region (stateBudget), so a
	// tiny middle never gets a manifest bigger than what it replaces.
	maxStateTokens = 500

	// Map bounds. The manifest is bounded by rendering, but the cumulative
	// maps must not grow for the session's lifetime: a long session can make
	// thousands of calls. prune keeps the actionable/newest entries.
	maxStateHandles    = 128
	maxStateOperations = 192
	maxStateFiles      = 192
)

// StateHandle is one addressable resource the session created.
type StateHandle struct {
	Kind   string // job, agent, terminal, spill
	Key    string // job-bg1, sub-7, term-1a2b, /path/to/spill.log
	Status string // live, recoverable, done, failed
	Label  string // short "tool key" descriptor, for the manifest line
	// Seq is the fold order; imported entries carry 0 (older than anything
	// folded in this process) and never override a newer entry.
	Seq int
}

// StateOperation is one deduplicated operation outcome.
type StateOperation struct {
	Tool     string
	Key      string // canonical salient argument (command, path, pattern, ...)
	Outcome  string // ok, failed
	Evidence string // first line of the result (or the error)
	Count    int
	Seq      int
}

// StateFile is one path the session read or modified.
type StateFile struct {
	Path   string
	Change string // modified, read
	Count  int
	Seq    int
}

// SessionState is the session-scoped deterministic memory. It is a value the
// agent owns; compaction folds each summarized span into it and renders the
// result into the summary.
type SessionState struct {
	handles    map[string]*StateHandle
	operations map[string]*StateOperation
	files      map[string]*StateFile
	// folded records the tool call ids already folded, so the shrinking-tail
	// retry loop (keep, keep/2, 1, 0) can fold overlapping spans without
	// double-counting operations.
	folded map[string]struct{}
	// mutating is the touched-files classification (see
	// stateDefaultMutatingTools): the tools whose calls record a "modified"
	// file entry. It is configuration, not conversation state — Reset leaves
	// it alone, and nil (a zero-value struct) means the builtin default.
	mutating map[string]struct{}
	seq      int
}

// NewSessionState returns an empty state that classifies the builtin write
// tools from this package's default list. It is for callers that own no tool
// registry (a Manager used directly, tests, embedders); the agent uses
// NewSessionStateWithMutatingTools so its registry stays the single source of
// truth.
func NewSessionState() *SessionState {
	return &SessionState{mutating: newMutatingToolSet(stateDefaultMutatingTools)}
}

// NewSessionStateWithMutatingTools returns an empty state that classifies
// exactly names as file-mutating, plus the documented foreign (MCP) spellings.
// names is the caller's tool-registry MutatesFS set — the agent passes
// agent.FSMutatingToolNames() — so a new write tool is classified the moment it
// is flagged in the registry and this package never keeps a second list.
func NewSessionStateWithMutatingTools(names []string) *SessionState {
	return &SessionState{mutating: newMutatingToolSet(names)}
}

func (s *SessionState) ensure() {
	if s.handles == nil {
		s.handles = make(map[string]*StateHandle)
	}
	if s.operations == nil {
		s.operations = make(map[string]*StateOperation)
	}
	if s.files == nil {
		s.files = make(map[string]*StateFile)
	}
	if s.folded == nil {
		s.folded = make(map[string]struct{})
	}
}

// Reset clears the state. Called when the conversation is replaced wholesale
// (new session, restore, fork): the entries describe the OLD conversation.
//
// The injected mutating classification (mutating) is configuration, not
// conversation state, so it survives: a reset session keeps classifying edits
// as modified instead of silently falling back to treating every call as a
// read.
func (s *SessionState) Reset() {
	if s == nil {
		return
	}
	s.handles = nil
	s.operations = nil
	s.files = nil
	s.folded = nil
	s.seq = 0
}

// Fold folds messages into the state. It is idempotent for tool calls (the
// folded set is keyed by call id), so overlapping spans from the shrinking-tail
// retry are safe. Summaries carrying a state block are imported first, so a
// restored or forked history seeds the cumulative memory instead of starting
// empty.
func (s *SessionState) Fold(messages []llm.Message) {
	if s == nil {
		return
	}
	s.ensure()
	calls := make(map[string]llm.ToolCall, len(messages))
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if _, seen := calls[tc.ID]; !seen {
				calls[tc.ID] = tc
			}
		}
	}
	for _, msg := range messages {
		if strings.Contains(msg.Content, StateHeader) {
			s.importBlock(msg.Content)
		}
		switch msg.Role {
		case "tool":
			if msg.ToolCallID == "" {
				continue
			}
			if _, done := s.folded[msg.ToolCallID]; done {
				continue
			}
			tc, known := calls[msg.ToolCallID]
			if !known {
				// The call itself is outside this span (a region split can cut a
				// pair, and Fold is exported). A zero ToolCall names no tool and
				// carries no arguments, so folding it recorded an "unknown" entry
				// with an empty key in both the handles and the operation ledger —
				// manifest lines nothing can explain. The id stays UNFOLDED so a
				// later span that does carry the call folds the pair correctly.
				continue
			}
			s.folded[msg.ToolCallID] = struct{}{}
			s.foldCall(tc, msg.Content)
		case "user":
			s.foldNotice(msg.Content)
		}
	}
	s.prune()
}

// Empty reports whether the state has anything worth rendering.
func (s *SessionState) Empty() bool {
	if s == nil {
		return true
	}
	for _, h := range s.handles {
		if h.Status == stateStatusLive || h.Status == stateStatusRecoverable {
			return false
		}
	}
	if len(s.operations) > 0 || len(s.files) > 0 {
		return false
	}
	return true
}

// foldCall records one executed tool call: its operation outcome, the paths it
// touched, and the handles its result created or updated.
func (s *SessionState) foldCall(tc llm.ToolCall, result string) {
	s.seq++
	tool := tc.Name
	if tool == "" {
		tool = "unknown"
	}
	key := operationKey(tool, tc.Args)
	outcome := stateOutcomeOK
	evidence := firstStateLine(result)
	if strings.HasPrefix(strings.TrimSpace(result), "Error:") ||
		strings.HasPrefix(strings.TrimSpace(result), "error:") {
		outcome = stateOutcomeFailed
	}
	opKey := tool + "\x00" + key
	if op, ok := s.operations[opKey]; ok {
		op.Count++
		op.Outcome = outcome
		op.Evidence = truncateStateLine(evidence, 120)
		op.Seq = s.seq
	} else {
		s.operations[opKey] = &StateOperation{
			Tool:     tool,
			Key:      key,
			Outcome:  outcome,
			Evidence: truncateStateLine(evidence, 120),
			Count:    1,
			Seq:      s.seq,
		}
	}
	change := s.stateChangeClass(tool)
	paths, fromResult := stateResultFiles(tool, result)
	if !fromResult {
		paths = stateTouchedFiles(tool, tc.Args)
	}
	for _, path := range paths {
		if f, ok := s.files[path]; ok {
			f.Count++
			if change == "modified" {
				f.Change = change
			}
			f.Seq = s.seq
			continue
		}
		s.files[path] = &StateFile{Path: path, Change: change, Count: 1, Seq: s.seq}
	}
	s.foldResultHandles(tool, key, result)
}

// foldNotice applies an in-band lifecycle notice (the append-only channel that
// keeps the model current between compactions).
func (s *SessionState) foldNotice(content string) {
	if strings.HasPrefix(content, JobNoticePrefix) {
		m := stateJobNoticeRe.FindStringSubmatch(content)
		if m == nil {
			return
		}
		code, _ := strconv.Atoi(m[2])
		status := stateStatusDone
		if code != 0 {
			status = stateStatusFailed
		}
		s.seq++
		s.upsertHandle(stateKindJob, m[1], status, "")
		if status == stateStatusFailed {
			// A non-zero background exit is a negative constraint: keep it in
			// the operation ledger too, where the failed section renders it and
			// the parser can merge it back after a restore.
			key := m[1]
			opKey := "background_job\x00" + key
			evidence := fmt.Sprintf("exited with code %d", code)
			if op, ok := s.operations[opKey]; ok {
				op.Outcome = stateOutcomeFailed
				op.Evidence = evidence
				op.Seq = s.seq
			} else {
				s.operations[opKey] = &StateOperation{
					Tool: "background_job", Key: key, Outcome: stateOutcomeFailed,
					Evidence: evidence, Count: 1, Seq: s.seq,
				}
			}
		}
		return
	}
	if !strings.HasPrefix(content, SubagentNoticePrefix) {
		return
	}
	m := stateSubagentNoticeRe.FindStringSubmatch(content)
	if m == nil {
		return
	}
	text := strings.ToLower(m[2])
	status := ""
	switch subagentTerminalVerb(text) {
	case "finished":
		status = stateStatusDone
	case "failed":
		status = stateStatusFailed
	case "interrupted":
		status = stateStatusLive
	}
	if status == "" {
		// Not a terminal notice (a reply, a "no reply", or a notice written by
		// an older gogen whose tail differs): keep the historical keyword
		// scan, so a notice already inside a persisted summary classifies
		// exactly as it did when it was folded.
		switch {
		case strings.Contains(text, "failed"):
			status = stateStatusFailed
		case strings.Contains(text, "finished"):
			status = stateStatusDone
		case strings.Contains(text, "interrupted"), strings.Contains(text, "no reply"):
			// An interrupted child stays continuable; it is still addressable.
			status = stateStatusLive
		default:
			return
		}
	}
	s.seq++
	// Reply notices are labelled with the child's LABEL, not its id, so only
	// update an existing handle. The terminal notice uses the id.
	if h, ok := s.handles[stateKindAgent+"\x00"+m[1]]; ok {
		h.Status = status
		h.Seq = s.seq
	}
}

// subagentTerminalVerb returns the structural status verb of a TERMINAL
// subagent notice, and "" when the text is not one.
//
// A terminal notice reads "<label> (finished). Use
// get_agent_result(agent_id=...)", so the verb has a POSITION: the word in
// parentheses immediately before the result pointer. It must not be found by
// scanning the whole text — the text leads with the child's LABEL, and a child
// labelled "retry failed tests" that finished was recorded as failed, telling
// the model (after a compaction) that a subagent had died when its result was
// one tool call away.
func subagentTerminalVerb(text string) string {
	const suffix = "). use get_agent_result(agent_id="
	i := strings.Index(text, suffix)
	if i < 0 {
		return ""
	}
	j := strings.LastIndexByte(text[:i], '(')
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(text[j+1 : i])
}

// foldResultHandles extracts handles from one tool result. Only entries with a
// status cue are created: a job id quoted in prose is not a live handle.
func (s *SessionState) foldResultHandles(tool, opKey, result string) {
	// The label is DISPLAY text (model guidance), never an address, so it is
	// capped: the handle KEY is lossless, but an uncapped label built from a
	// multi-hundred-character command would push the "- job "job1": …" line
	// past the manifest budget, and renderState drops whole lines — losing the
	// live id the label exists to explain.
	label := truncateStateLine(strings.TrimSpace(tool+" "+quoteState(opKey)), 120)
	for _, id := range uniqueMatches(stateJobIDRe, result) {
		line := lineContaining(result, id)
		if status := jobLineStatus(line); status != "" {
			s.upsertHandle(stateKindJob, id, status, label)
		}
	}
	for _, id := range uniqueMatches(stateTerminalIDRe, result) {
		line := lineContaining(result, id)
		status := ""
		switch {
		case strings.Contains(line, "Opened terminal"):
			status = stateStatusLive
		case strings.Contains(line, "Closed terminal"):
			status = stateStatusDone
		}
		if status != "" {
			s.upsertHandle(stateKindTerminal, id, status, label)
		}
	}
	for _, m := range stateSubagentStartRe.FindAllStringSubmatch(result, -1) {
		status := stateStatusLive
		switch m[2] {
		case "finished":
			status = stateStatusDone
		case "failed":
			status = stateStatusFailed
		}
		s.upsertHandle(stateKindAgent, m[1], status, label)
	}
	for _, m := range stateSpillRe.FindAllStringSubmatch(result, -1) {
		s.upsertHandle(stateKindSpill, strings.TrimSpace(m[1]), stateStatusRecoverable, label)
	}
}

// upsertHandle creates or updates one handle. A terminal status always wins
// over live (a job that finished must read done); labels are kept from the
// first mention so a later status poll does not overwrite "execute_command
// \"make build\"" with "background_job \"status job-bg1\"".
func (s *SessionState) upsertHandle(kind, key, status, label string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	id := kind + "\x00" + key
	h, ok := s.handles[id]
	if !ok {
		s.handles[id] = &StateHandle{Kind: kind, Key: key, Status: status, Label: label, Seq: s.seq}
		return
	}
	if h.Status == stateStatusLive || h.Status == stateStatusRecoverable || status == stateStatusFailed {
		h.Status = status
	}
	if h.Label == "" {
		h.Label = label
	}
	h.Seq = s.seq
}

// importBlock parses a previously rendered state block. Imported entries carry
// Seq 0, so they never override a live entry folded in this process (Seq > 0).
// A transcript can hold SEVERAL blocks — a deep-dive session summary plus the
// progress summaries written after it — and they are folded oldest first, so a
// later block is the fresher snapshot and wins over an earlier one. Counts
// merge by max, which keeps a truncated or older manifest from lowering a
// cumulative count.
func (s *SessionState) importBlock(content string) {
	i := strings.Index(content, StateHeader)
	if i < 0 {
		return
	}
	block := content[i+len(StateHeader):]
	if j := strings.Index(block, StateFooter); j >= 0 {
		block = block[:j]
	}
	s.ensure()
	section := ""
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		switch line {
		case "live:", "recoverable:", "failed:", "files:", "operations:":
			section = strings.TrimSuffix(line, ":")
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		switch section {
		case "live", "recoverable":
			kind, rest, ok := cutStateToken(line)
			if !ok {
				continue
			}
			key, tail, ok := cutQuotedState(rest)
			if !ok {
				continue
			}
			label := strings.TrimSpace(strings.TrimPrefix(tail, ":"))
			status := stateStatusLive
			if section == "recoverable" {
				status = stateStatusRecoverable
			}
			id := kind + "\x00" + key
			if h, exists := s.handles[id]; exists {
				// An entry folded in this process is live state and keeps its
				// status; between two imported blocks the later one wins.
				if h.Seq == 0 {
					h.Status = status
					if label != "" {
						h.Label = label
					}
				}
				continue
			}
			s.handles[id] = &StateHandle{Kind: kind, Key: key, Status: status, Label: label}
		case "failed", "operations":
			tool, rest, ok := cutStateToken(line)
			if !ok {
				continue
			}
			key, tail, ok := cutQuotedState(rest)
			if !ok {
				continue
			}
			count := 1
			evidence := ""
			if change, n, ok := parseStateCounted(tail); ok {
				_ = change
				count = n
			} else {
				evidence = strings.TrimSpace(strings.TrimPrefix(tail, ":"))
			}
			outcome := stateOutcomeOK
			if section == "failed" {
				outcome = stateOutcomeFailed
			}
			s.importOperation(tool, key, outcome, evidence, count)
		case "files":
			path, tail, ok := cutQuotedState(line)
			if !ok {
				continue
			}
			change, count, ok := parseStateCounted(tail)
			if !ok {
				continue
			}
			s.importFile(path, change, count)
		}
	}
}

// importOperation merges one parsed operation. The outcome and evidence come
// from the newest block that describes the operation (a later block is the
// fresher snapshot; a live entry keeps what the fold recorded), and the count
// is the largest seen so a truncated manifest cannot lower it.
func (s *SessionState) importOperation(tool, key, outcome, evidence string, count int) {
	opKey := tool + "\x00" + key
	if op, ok := s.operations[opKey]; ok {
		if op.Seq == 0 {
			op.Outcome = outcome
			op.Evidence = evidence
		}
		if count > op.Count {
			op.Count = count
		}
		return
	}
	s.operations[opKey] = &StateOperation{Tool: tool, Key: key, Outcome: outcome, Evidence: evidence, Count: count}
}

// importFile merges one parsed file entry. "modified" is monotone (the fold
// never downgrades a file to "read"), so it wins over "read" whichever block it
// came from; a live entry keeps its own change class.
func (s *SessionState) importFile(path, change string, count int) {
	if f, ok := s.files[path]; ok {
		if f.Seq == 0 && (change == "modified" || f.Change == "") {
			f.Change = change
		}
		if count > f.Count {
			f.Count = count
		}
		return
	}
	s.files[path] = &StateFile{Path: path, Change: change, Count: count}
}

// prune bounds the cumulative maps. Actionable entries are kept first, then
// the newest; the rest are dropped. Counts are not lost for a key that stays.
func (s *SessionState) prune() {
	// The folded set only needs to cover overlapping spans from the current
	// compaction retry sequence (keep, keep/2, 1, 0), not the whole session.
	// Reset it rather than growing one string per call forever.
	if len(s.folded) > 4096 {
		s.folded = make(map[string]struct{})
	}
	if len(s.handles) > maxStateHandles {
		type entry struct {
			id string
			h  *StateHandle
		}
		all := make([]entry, 0, len(s.handles))
		for id, h := range s.handles {
			all = append(all, entry{id, h})
		}
		rank := func(h *StateHandle) int {
			switch h.Status {
			case stateStatusLive, stateStatusRecoverable:
				return 0
			default:
				return 1
			}
		}
		sort.Slice(all, func(i, j int) bool {
			ri, rj := rank(all[i].h), rank(all[j].h)
			if ri != rj {
				return ri < rj
			}
			return all[i].h.Seq > all[j].h.Seq
		})
		for _, e := range all[maxStateHandles:] {
			delete(s.handles, e.id)
		}
	}
	if len(s.operations) > maxStateOperations {
		type entry struct {
			id string
			op *StateOperation
		}
		all := make([]entry, 0, len(s.operations))
		for id, op := range s.operations {
			all = append(all, entry{id, op})
		}
		sort.Slice(all, func(i, j int) bool {
			fi, fj := all[i].op.Outcome == stateOutcomeFailed, all[j].op.Outcome == stateOutcomeFailed
			if fi != fj {
				return fi
			}
			return all[i].op.Seq > all[j].op.Seq
		})
		for _, e := range all[maxStateOperations:] {
			delete(s.operations, e.id)
		}
	}
	if len(s.files) > maxStateFiles {
		type entry struct {
			id string
			f  *StateFile
		}
		all := make([]entry, 0, len(s.files))
		for id, f := range s.files {
			all = append(all, entry{id, f})
		}
		sort.Slice(all, func(i, j int) bool {
			fi, fj := all[i].f.Change == "modified", all[j].f.Change == "modified"
			if fi != fj {
				return fi
			}
			return all[i].f.Seq > all[j].f.Seq
		})
		for _, e := range all[maxStateFiles:] {
			delete(s.files, e.id)
		}
	}
}

// stateSection is one priority-ordered manifest section.
type stateSection struct {
	title string
	lines []string
}

// stateSections renders the state in actionability order: live handles,
// recoverable locators, failures, touched files, then collapsed operations.
func (s *SessionState) stateSections() []stateSection {
	if s == nil {
		return nil
	}
	var live, recoverable, failed, files, operations []string
	for _, h := range sortedHandles(s.handles) {
		switch h.Status {
		case stateStatusLive:
			live = append(live, fmt.Sprintf("- %s %s: %s", h.Kind, quoteState(h.Key), h.Label))
		case stateStatusRecoverable:
			recoverable = append(recoverable, fmt.Sprintf("- %s %s: %s", h.Kind, quoteState(h.Key), h.Label))
		}
	}
	var failedOps, okOps []*StateOperation
	for _, op := range s.operations {
		if op.Outcome == stateOutcomeFailed {
			failedOps = append(failedOps, op)
		} else {
			okOps = append(okOps, op)
		}
	}
	// Seq is the recency order. Imported entries all carry Seq 0, so the
	// tool/key tiebreaker keeps the rendered manifest deterministic (a
	// deterministic replacement is what the smaller-summary guard and the
	// summary change detector rely on).
	sort.Slice(failedOps, func(i, j int) bool {
		if failedOps[i].Seq != failedOps[j].Seq {
			return failedOps[i].Seq > failedOps[j].Seq
		}
		if failedOps[i].Tool != failedOps[j].Tool {
			return failedOps[i].Tool < failedOps[j].Tool
		}
		return failedOps[i].Key < failedOps[j].Key
	})
	sort.Slice(okOps, func(i, j int) bool {
		if okOps[i].Seq != okOps[j].Seq {
			return okOps[i].Seq > okOps[j].Seq
		}
		if okOps[i].Tool != okOps[j].Tool {
			return okOps[i].Tool < okOps[j].Tool
		}
		return okOps[i].Key < okOps[j].Key
	})
	for _, op := range failedOps {
		evidence := op.Evidence
		if evidence == "" {
			evidence = "failed"
		}
		failed = append(failed, fmt.Sprintf("- %s %s: %s", op.Tool, quoteState(op.Key), truncateStateLine(evidence, 120)))
	}
	for _, op := range okOps {
		operations = append(operations, fmt.Sprintf("- %s %s (ok, %dx)", op.Tool, quoteState(op.Key), op.Count))
	}
	var fileList []*StateFile
	for _, f := range s.files {
		fileList = append(fileList, f)
	}
	sort.Slice(fileList, func(i, j int) bool {
		mi, mj := fileList[i].Change == "modified", fileList[j].Change == "modified"
		if mi != mj {
			return mi
		}
		if fileList[i].Seq != fileList[j].Seq {
			return fileList[i].Seq > fileList[j].Seq
		}
		return fileList[i].Path < fileList[j].Path
	})
	for _, f := range fileList {
		files = append(files, fmt.Sprintf("- %s (%s, %dx)", quoteState(f.Path), f.Change, f.Count))
	}
	var out []stateSection
	for _, sec := range []stateSection{
		{"live:", live},
		{"recoverable:", recoverable},
		{"failed:", failed},
		{"files:", files},
		{"operations:", operations},
	} {
		if len(sec.lines) > 0 {
			out = append(out, sec)
		}
	}
	return out
}

func sortedHandles(handles map[string]*StateHandle) []*StateHandle {
	out := make([]*StateHandle, 0, len(handles))
	for _, h := range handles {
		if h.Status != stateStatusLive && h.Status != stateStatusRecoverable {
			continue
		}
		out = append(out, h)
	}
	kindRank := func(k string) int {
		switch k {
		case stateKindJob:
			return 0
		case stateKindAgent:
			return 1
		case stateKindTerminal:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := kindRank(out[i].Kind), kindRank(out[j].Kind)
		if ri != rj {
			return ri < rj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// renderState renders the manifest into budget tokens. It returns "" when
// there is nothing to render or the budget is too small to say anything.
//
// The budget cut is a STRICT PREFIX over the priority-ordered sections: the
// first line of the first section that no longer fits ends the manifest, and
// every later line and lower-priority section is omitted (and counted in the
// tail note, which counts ENTRIES — a section title is not an entry, so the
// number is exactly how many manifest lines are missing). A section is
// therefore always rendered whole or as a prefix of itself, never with a hole.
//
// The alternative — keep scanning a section after an over-budget line and
// render the later shorter ones — was incoherent: the manifest showed a
// low-priority line while hiding a higher-priority one that just missed, and
// the "N more omitted" note sat below entries that were NOT the tail of the
// section.
//
// The tail note itself is rendered OUTSIDE the budget, deliberately: it is a
// bounded bookkeeping line that reports entries were dropped, so trimming a
// real entry to pay for it would hide the very information it exists to
// disclose. The block can therefore exceed budget by that one line.
func (m *Manager) renderState(state *SessionState, budget int) string {
	if state == nil || state.Empty() || budget < 48 {
		return ""
	}
	sections := state.stateSections()
	if len(sections) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(StateHeader)
	b.WriteByte('\n')
	used := m.EstimateTokens([]llm.Message{{Role: "assistant", Content: StateHeader + "\n" + StateFooter}})
	omitted := 0
	stopped := false
	for _, sec := range sections {
		if stopped {
			omitted += len(sec.lines)
			continue
		}
		header := m.EstimateTokens([]llm.Message{{Role: "assistant", Content: sec.title}})
		first := m.EstimateTokens([]llm.Message{{Role: "assistant", Content: sec.lines[0]}})
		// A section title never renders alone: if its first entry does not
		// fit, the whole section (and every lower-priority section) is
		// omitted.
		if used+header+first > budget {
			stopped = true
			omitted += len(sec.lines)
			continue
		}
		b.WriteString(sec.title)
		b.WriteByte('\n')
		used += header
		for i, line := range sec.lines {
			cost := m.EstimateTokens([]llm.Message{{Role: "assistant", Content: line}})
			if used+cost > budget {
				// Strict prefix: the first line that does not fit ends this
				// section too. A later, shorter line must not sneak in below
				// the gap — the rendered block has to be the section's prefix
				// for the omission note to describe what is really missing.
				stopped = true
				omitted += len(sec.lines) - i
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
			used += cost
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- … %d more state entries omitted\n", omitted)
	}
	b.WriteString(StateFooter)
	return b.String()
}

// stateBudget is the manifest budget for a summarized region: at most
// maxStateTokens, and at most a quarter of the region it replaces. The
// quarter leaves the model recap three quarters of the region, so the framed
// summary (recap + manifest) stays comfortably under the smaller-summary
// guard even when the recap is long.
func stateBudget(regionTokens int) int {
	budget := regionTokens / 4
	if budget > maxStateTokens {
		budget = maxStateTokens
	}
	return budget
}

// StripStateBlock removes a deterministic state block from content, leaving
// any text before and after it (a summary note can be appended after the
// block). A content without a block is returned unchanged.
func StripStateBlock(content string) string {
	i := strings.Index(content, StateHeader)
	if i < 0 {
		return content
	}
	before := strings.TrimRight(content[:i], "\n")
	j := strings.Index(content[i:], StateFooter)
	if j < 0 {
		return before
	}
	after := strings.TrimLeft(content[i+j+len(StateFooter):], "\n")
	switch {
	case before == "":
		return after
	case after == "":
		return before
	default:
		return before + "\n\n" + after
	}
}

// stripStateBlocks returns a copy of msgs with state blocks stripped, or the
// input slice when nothing changed.
func stripStateBlocks(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for i, msg := range msgs {
		stripped := StripStateBlock(msg.Content)
		if stripped == msg.Content {
			continue
		}
		if out == nil {
			out = append([]llm.Message(nil), msgs...)
		}
		out[i].Content = stripped
	}
	if out == nil {
		return msgs
	}
	return out
}

// operationKey returns the canonical salient argument for a tool call. It is
// the dedupe key for the operation ledger: two reads of one file collapse to
// one entry with a count, and a re-run command shows its latest outcome.
func operationKey(tool string, args map[string]any) string {
	// The per-tool cases mirror each tool's schema in internal/agent/tools.go;
	// the names below are the ones the model actually sends. A tool's `path`
	// is NOT a safe generic fallback: for the search family it is a directory
	// SCOPE, so every search in one tree collapsed into a single ledger entry.
	switch tool {
	case "execute_command", "bash", "run_command", "shell", "bash_persistent":
		return argStateText(args, "command")
	case "background_job":
		return joinStateTexts(argStateText(args, "action"), argStateText(args, "job_id"))
	case "subagent", "subagent_fork":
		return argStateText(args, "job")
	case "terminal":
		return joinStateTexts(argStateText(args, "action"), argStateText(args, "session_id"))
	// Nested MCP exposure is three gateway tools, not one (internal/agent/
	// mcp_tool.go): mcp_call/mcp_describe are identified by the server tool
	// they act on, mcp_list is one catalog query. The old single `mcp` case
	// matched nothing, so every mcp_call folded to the same empty key.
	case "mcp_list":
		return argStateText(args, "query")
	case "mcp_describe", "mcp_call":
		return argStateText(args, "tool")
	case "search_code", "glob":
		return joinStateTexts(argStateText(args, "pattern"), argStateText(args, "path"))
	case "find_symbol":
		return joinStateTexts(argStateText(args, "kind"), argStateText(args, "symbol"), argStateText(args, "path"))
	case "call_graph":
		return joinStateTexts(argStateText(args, "symbol"), argStateText(args, "direction"))
	case "find_file":
		return joinStateTexts(argStateText(args, "name"), argStateText(args, "path"))
	case "read_files":
		return joinStateTexts(argStateTexts(args, "paths")...)
	// git_stage's only argument is the path list (empty = stage everything).
	// Without this case every staging call folded into ONE operation entry with
	// an empty key, so "git_stage [a.go]" and "git_stage [b.go]" collapsed and
	// the ledger could not say what had been staged.
	case "git_stage":
		return joinStateTexts(argStateTexts(args, "paths")...)
	case "read_image":
		return argStateText(args, "path")
	case "git":
		return joinStateTexts(argStateText(args, "action"), argStateText(args, "ref"), argStateText(args, "path"))
	case "todo":
		return joinStateTexts(argStateText(args, "action"), argStateText(args, "id"))
	}
	if v := argStateText(args, "path"); v != "" {
		return v
	}
	if v := argStateText(args, "file"); v != "" {
		return v
	}
	for _, k := range []string{"file_path", "pattern", "query", "url", "job", "agent_id", "symbol", "name", "action", "message"} {
		if v := argStateText(args, k); v != "" {
			return v
		}
	}
	return ""
}

// argStateTexts renders a string-list argument (read_files paths, git_stage
// paths) as individual values. Both []string and the []any a JSON decode
// produces are accepted.
func argStateTexts(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	var raw []any
	switch v := args[key].(type) {
	case []string:
		raw = make([]any, 0, len(v))
		for _, s := range v {
			raw = append(raw, s)
		}
	case []any:
		raw = v
	default:
		if s := argStateText(args, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s := argStateValue(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// argStateText renders one argument value, or "" when it is absent or blank.
func argStateText(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok {
		return ""
	}
	return argStateValue(v)
}

// argStateValue renders one argument value as a token for a KEY.
//
// The value is deliberately NOT whitespace-folded and NOT length-capped: an
// operation/file key is the dedupe identity of an entry AND it is parsed back
// out of the manifest on restore/fork, so rewriting it renames the entry. The
// old fold+cap did exactly that — a >100-rune path re-imported with an
// ellipsis in it (a path that does not exist) and a command with a double
// space re-imported as a different key. Single-line safety comes from
// quoteState (strconv.Quote escapes newlines and quotes), and FITTING the
// manifest comes from renderState's budget: a line too long for the budget is
// omitted and counted in the "N more state entries omitted" tail, which is
// truthful, while a silently rewritten key was not.
//
// EV/display text (evidence, handle labels) is still capped by
// truncateStateLine; only keys are lossless.
func argStateValue(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprint(v)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

func joinStateTexts(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// stateFileArg names, per BUILTIN tool, the argument holding the file(s) a call
// touched — the input of the manifest's files section. Each entry mirrors the
// tool's schema in internal/agent/tools.go.
//
// A tool whose `path` is a directory SCOPE (search_code, glob, find_symbol,
// find_file, list_files, call_graph, rename_symbol, git) maps to "" on purpose:
// recording the scope claimed the session had opened a DIRECTORY as a file, and
// the section exists to answer "what did I change" (the higher-priority
// modification class) and "what did I already read". An empty entry is listed
// rather than omitted so the exclusion is a decision, not an accident.
var stateFileArg = map[string]string{
	"read_file":        "path",
	"read_image":       "path",
	"read_files":       "paths",
	"list_definitions": "path",
	"write_file":       "path",
	"replace_in_file":  "path",
	"delete":           "path",
	"download_file":    "path",
	"git_blame":        "file",
	// patch_file's schema carries only the diff, but a model that names the
	// file it is patching must still get a "modified" entry for it.
	"patch_file": "path",

	// Directory-scope tools: no file entry.
	"search_code":   "",
	"glob":          "",
	"find_symbol":   "",
	"find_file":     "",
	"list_files":    "",
	"call_graph":    "",
	"rename_symbol": "",
	"git":           "",
	"repo_overview": "",

	// show_diff's `path` is an optional file OR DIRECTORY (absent = the whole
	// working tree), so the argument is not the set of files shown — recording
	// it claimed the session had opened a directory as a file. Its touched
	// files come from the RESULT instead (stateResultFiles).
	"show_diff": "",

	// git_stage changes the INDEX, not the working tree: the file's content is
	// unchanged, so a "modified" entry would be false, and a "read" entry
	// would claim the model already saw the file's content. The staging is
	// recorded where it belongs — in the operation ledger, keyed by its paths
	// (operationKey) — and an existing "modified" entry the file already has
	// stays modified.
	"git_stage": "",
}

// stateResultFiles derives the touched files of a tool whose files are only
// knowable from its RESULT, reporting false for every other tool (those stay
// on the argument table, stateTouchedFiles).
//
// show_diff is that one case: its `path` argument is an optional file OR
// DIRECTORY (absent = the whole working tree) and the implementation runs
// `git diff -- <path>`, so the argument names a SCOPE, not the files shown —
// a directory landed in the files section as if it were a file, as did a path
// with no changes at all. The diff itself names what was shown, and reading
// the transcript (never the filesystem) keeps the fold deterministic: the same
// transcript always folds to the same manifest, on any machine, for any state
// of the working tree.
func stateResultFiles(tool, result string) ([]string, bool) {
	if tool == "show_diff" {
		return diffStatePaths(result), true
	}
	return nil, false
}

// diffStatePaths returns the file paths a unified diff shows, first-seen order,
// deduplicated.
//
// The `diff --git a/<path> b/<path>` header is the primary source: it names
// both sides, so a rename records the old AND the new path (both were acted
// on), and a created/deleted file names one real side. A diff without those
// headers falls back to the `--- a/…` / `+++ b/…` pair. `/dev/null` is the
// absent side of a create/delete, not a path. Git C-quotes a path with unusual
// bytes ("a/pa th.txt"), so a quoted token is unquoted; a header that does not
// split into two parsable tokens is SKIPPED rather than recorded half-parsed.
//
// Paths are recorded exactly as git printed them — relative to the REPOSITORY
// root, which for a working dir inside a repo (or a repo checked out above it)
// is not the same frame as the other tools' working-dir-relative paths. That is
// deliberate: resolving them would need filesystem I/O (and a repo probe) in a
// fold that must depend only on the transcript. For the same reason the fold
// never asks whether a path exists: a rename records both sides (the old path
// no longer exists), and a diff of a deleted file records the deleted file.
func diffStatePaths(result string) []string {
	seen := make(map[string]struct{})
	var out []string
	// side is the a/ or b/ prefix the token carries ("" = a plain unified
	// diff, whose paths are bare); a token that does not carry it is kept
	// verbatim, because a file may legitimately be named "a/x.go".
	add := func(token, side string) {
		path := trimDiffPathToken(token, side)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sawHeader := false
	for _, line := range strings.Split(result, "\n") {
		rest, ok := strings.CutPrefix(line, "diff --git ")
		if !ok {
			continue
		}
		oldPath, newPath, ok := cutDiffPair(rest)
		if !ok {
			continue
		}
		sawHeader = true
		add(oldPath, "a")
		add(newPath, "b")
	}
	if sawHeader {
		return out
	}
	// No git header: a plain unified diff, whose ---/+++ paths are bare.
	for _, line := range strings.Split(result, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			add(strings.TrimPrefix(line, "--- "), "")
		case strings.HasPrefix(line, "+++ "):
			add(strings.TrimPrefix(line, "+++ "), "")
		}
	}
	return out
}

// cutDiffPair splits the two path tokens of a "diff --git" header.
func cutDiffPair(s string) (oldPath, newPath string, ok bool) {
	oldPath, rest, ok := cutDiffToken(s)
	if !ok {
		return "", "", false
	}
	newPath, tail, ok := cutDiffToken(rest)
	if !ok || strings.TrimSpace(tail) != "" {
		return "", "", false
	}
	return oldPath, newPath, true
}

// cutDiffToken splits one path token off s, honoring git's C-quoted form
// (which is what lets a path with a space be one token).
func cutDiffToken(s string) (token, rest string, ok bool) {
	s = strings.TrimLeft(s, " ")
	if s == "" {
		return "", "", false
	}
	if s[0] != '"' {
		// The last token of a header has no trailing space: the whole remainder
		// is the token (strings.Cut would report "not found" and drop it).
		if token, rest, ok = strings.Cut(s, " "); !ok {
			return s, "", true
		}
		return token, rest, true
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case '"':
			return s[:i+1], s[i+1:], true
		}
	}
	return "", "", false
}

// trimDiffPathToken normalizes one path token from a diff header: unquote a
// C-quoted token, drop the side prefix git adds (side is "a" or "b"; "" keeps
// the path bare), drop the trailing "\t<timestamp>" a plain `diff -u` appends,
// and map /dev/null (the absent side of a create or delete) to no path at all.
func trimDiffPathToken(token, side string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, `"`) {
		if unquoted, err := strconv.Unquote(token); err == nil {
			token = unquoted
		}
	}
	if i := strings.IndexByte(token, '\t'); i >= 0 {
		token = token[:i]
	}
	if prefix := side + "/"; side != "" && strings.HasPrefix(token, prefix) {
		token = token[len(prefix):]
	}
	if strings.TrimSpace(token) == "" || token == "/dev/null" {
		return ""
	}
	return token
}

// stateTouchedFiles returns the file paths one call touched: none, one, or
// several (read_files takes a list). A tool outside the table — an MCP server's
// own tool — keeps the historical path/file/file_path guess, because its schema
// is unknown here.
func stateTouchedFiles(tool string, args map[string]any) []string {
	if arg, known := stateFileArg[tool]; known {
		if arg == "" {
			return nil
		}
		if arg == "paths" {
			return argStateTexts(args, "paths")
		}
		if v := argStateText(args, arg); v != "" {
			return []string{v}
		}
		return nil
	}
	for _, k := range []string{"path", "file", "file_path"} {
		if v := argStateText(args, k); v != "" {
			return []string{v}
		}
	}
	return nil
}

// The touched-files section classifies a call as MODIFIED or read. The set of
// file-mutating tools is NOT declared here any more: it is injected at
// construction by the layer that owns the tool registry
// (NewSessionStateWithMutatingTools, fed with agent.FSMutatingToolNames), so
// there is no second copy of the list to drift from the MutatesFS flag — a
// write tool added to the registry is classified correctly with no edit in this
// package. The old hardcoded map meant a missing entry rendered an edit as
// "(read, Nx)", so the model resuming from a compaction summary believed it had
// only inspected a file it had actually rewritten and re-read or re-edited
// blindly.
//
// stateDefaultMutatingTools is the builtin default, used by NewSessionState
// (and by a zero-value SessionState) for callers with no registry: direct
// Manager users, tests, embedders.
var stateDefaultMutatingTools = []string{
	"write_file",
	"replace_in_file",
	"patch_file",
	"delete",
	"download_file",
	"rename_symbol",
}

// stateForeignMutatingTools are NOT builtin tools: they are foreign (MCP)
// spellings of a patch/move, which mutate the tree the same way. No Go registry
// knows them and their schemas are unknown here, so they are added to every
// classification instead of being injected.
var stateForeignMutatingTools = []string{"apply_patch", "move_file", "rename"}

// defaultMutatingToolSet is the classification for a state that carries none
// (a zero-value SessionState): the builtin default plus the foreign spellings.
var defaultMutatingToolSet = newMutatingToolSet(stateDefaultMutatingTools)

// newMutatingToolSet builds the classification: names (the caller's registry
// MutatesFS set) plus the foreign spellings.
func newMutatingToolSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names)+len(stateForeignMutatingTools))
	for _, name := range names {
		if name != "" {
			out[name] = struct{}{}
		}
	}
	for _, name := range stateForeignMutatingTools {
		out[name] = struct{}{}
	}
	return out
}

// stateChangeClass classifies a tool as file-mutating or file-reading for the
// touched-files section. A state built without a constructor (a zero value, or
// an embedder's struct literal) falls back to the builtin default set rather
// than calling every edit a read.
func (s *SessionState) stateChangeClass(tool string) string {
	set := s.mutating
	if set == nil {
		set = defaultMutatingToolSet
	}
	if _, ok := set[tool]; ok {
		return "modified"
	}
	return "read"
}

// firstStateLine returns the first non-empty line of s, trimmed.
func firstStateLine(s string) string {
	for len(s) > 0 {
		line := s
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// truncateStateLine collapses newlines and caps s at max runes (rune-safe).
func truncateStateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// quoteState renders a key as a quoted, single-line token. The manifest is
// parsed back (restore/fork), so the key format must be unambiguous even when
// a command or path contains spaces or quotes — and LOSSLESS: a handle key is
// an address the model is told to reuse (a spill locator path goes straight to
// read_file), so a shortened one re-imports as a path that does not exist.
//
// Quoting is what makes it single-line (Quote escapes a newline to "\n"), so
// no whitespace folding or length cap belongs here. Fitting the line is
// renderState's job: a line that would blow the token budget is dropped and
// counted in the "N more state entries omitted" tail, which is truthful;
// silently rewriting the key was not. Operation and file keys reach this
// point uncapped too (argStateValue), so an over-long key is never rendered
// corrupt — the whole line is either rendered verbatim or omitted and
// counted.
func quoteState(s string) string {
	return strconv.Quote(s)
}

// cutStateToken splits the first space-separated token off s.
func cutStateToken(s string) (token, rest string, ok bool) {
	token, rest, ok = strings.Cut(s, " ")
	return token, strings.TrimSpace(rest), ok
}

// cutQuotedState splits a leading quoted token off s and unquotes it.
func cutQuotedState(s string) (value, rest string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			v, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", false
			}
			return v, strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

var stateCountedRe = regexp.MustCompile(`^\(([a-z]+), (\d+)x\)$`)

// parseStateCounted parses the "(change, Nx)" tail of a rendered line.
func parseStateCounted(s string) (change string, count int, ok bool) {
	m := stateCountedRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 1 {
		return "", 0, false
	}
	return m[1], n, true
}

var (
	// The id suffixes are bounded generously rather than at today's 16 chars:
	// the trailing \b means an id LONGER than the cap did not match a prefix,
	// it did not match AT ALL (a 20-char id hit no boundary at 16), so a future
	// longer id silently lost its handle. Ids of 16 chars match exactly as
	// before.
	stateJobIDRe          = regexp.MustCompile(`\b(tjob|job)-[0-9a-z]{2,64}\b`)
	stateTerminalIDRe     = regexp.MustCompile(`\bterm-[0-9a-z]{2,64}\b`)
	stateJobNoticeRe      = regexp.MustCompile(`^\[job\] (\S+) \(.*\) exited with code (-?\d+)`)
	stateSubagentStartRe  = regexp.MustCompile(`\bSubagent ([A-Za-z0-9_.:-]{1,64}) (started in background|is still running|finished|failed|interrupted)\b`)
	stateSubagentNoticeRe = regexp.MustCompile(`^\[subagent ([^\]]+)\] (.*)`)
	stateSpillRe          = regexp.MustCompile(`full output saved to ([^\n]+?) — use read_file`)
)

// jobLineStatus classifies one line that mentions a job id.
func jobLineStatus(line string) string {
	switch {
	case strings.Contains(line, "Started background job"),
		strings.Contains(line, "Started background terminal send"),
		strings.Contains(line, "is RUNNING"):
		return stateStatusLive
	case strings.Contains(line, "FINISHED"),
		strings.Contains(line, "exited with code"),
		strings.Contains(line, "CANCELLED"):
		return stateStatusDone
	case strings.Contains(line, "FAILED"):
		return stateStatusFailed
	}
	return ""
}

// lineContaining returns the first line of s that mentions needle.
func lineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// uniqueMatches returns the distinct matches of re in s, in order.
func uniqueMatches(re *regexp.Regexp, s string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, m := range re.FindAllString(s, -1) {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}
