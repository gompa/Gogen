package contextmgr

import (
	"fmt"
	"regexp"
	"testing"

	"gogen/internal/llm"
)

// TestFoldSubagentNoticeClassifiesOnVerb pins the classification of a subagent
// terminal notice on its STRUCTURAL verb. The notice text leads with the
// child's label, so the historical keyword scan read "failed" out of a label
// like "retry failed tests" and recorded a child that had finished as failed —
// the model then believed a result it could still fetch was gone.
func TestFoldSubagentNoticeClassifiesOnVerb(t *testing.T) {
	// The terminal notice the spawner emits, and the older wordings that are
	// already inside persisted summaries.
	terminal := func(id, label, verb string) string {
		return SubagentNoticePrefix + id + "] " + label +
			fmt.Sprintf(" (%s). Use get_agent_result(agent_id=%q) for the full result.", verb, id)
	}
	tests := []struct {
		name   string
		notice string
		id     string
		start  string // the start notice that created the handle
		want   string
	}{
		{
			name:   "finished child labelled with failed",
			notice: terminal("sub-1", "retry failed tests", "finished"),
			id:     "sub-1",
			start:  "Subagent sub-1 started in background",
			want:   stateStatusDone,
		},
		{
			name:   "failed child labelled with finished",
			notice: terminal("sub-2", "finished the parser", "failed"),
			id:     "sub-2",
			start:  "Subagent sub-2 started in background",
			want:   stateStatusFailed,
		},
		{
			name:   "plain finished",
			notice: terminal("sub-3", "scout", "finished"),
			id:     "sub-3",
			start:  "Subagent sub-3 started in background",
			want:   stateStatusDone,
		},
		{
			name:   "label carrying both keywords",
			notice: terminal("sub-4", "fix (failed) tests then finished", "finished"),
			id:     "sub-4",
			start:  "Subagent sub-4 started in background",
			want:   stateStatusDone,
		},
		{
			// A notice an older gogen wrote: no result pointer, so the verb is
			// not positional and the keyword fallback must keep the old answer.
			name:   "legacy wording keeps its keyword classification",
			notice: SubagentNoticePrefix + "sub-5] finished (done). Use get_agent_result for the full result.",
			id:     "sub-5",
			start:  "Subagent sub-5 started in background",
			want:   stateStatusDone,
		},
		{
			// A reply notice carries the LABEL, never the id, so it must not
			// touch any handle — even when it mentions "failed".
			name:   "reply notice with a failed label changes nothing",
			notice: SubagentNoticePrefix + "retry failed tests] the parser is in internal/parse",
			id:     "sub-6",
			start:  "Subagent sub-6 started in background",
			want:   stateStatusLive,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := NewSessionState()
			st.Fold([]llm.Message{
				{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "c1", Name: "subagent", Args: map[string]any{"job": "j"},
				}}},
				{Role: "tool", ToolCallID: "c1", Content: tc.start},
			})
			if h := st.handles[stateKindAgent+"\x00"+tc.id]; h == nil {
				t.Fatalf("setup: no handle for %q", tc.id)
			}
			st.Fold([]llm.Message{{Role: "user", Content: tc.notice}})
			h := st.handles[stateKindAgent+"\x00"+tc.id]
			if h == nil {
				t.Fatalf("handle %q disappeared", tc.id)
			}
			if h.Status != tc.want {
				t.Fatalf("status after %q = %q, want %q", tc.notice, h.Status, tc.want)
			}
			if len(st.handles) != 1 {
				t.Fatalf("a notice created a handle from its label: %+v", st.handles)
			}
		})
	}
}

// TestStateIDRegexesTolerateLongerIDs pins the id patterns against a longer id:
// the trailing \b meant the old 16-rune cap did not shorten the match, it
// removed it (no word boundary at 16 inside a 24-char id), so a future id
// length would silently drop every job/terminal handle. A 16-char id keeps
// matching exactly the same text.
func TestStateIDRegexesTolerateLongerIDs(t *testing.T) {
	const short = "0123456789abcdef"                       // 16, today's length
	const long = "0123456789abcdef01234567"                // 24
	const tooLong = "0123456789abcdef0123456789abcdef0123" // 36
	tests := []struct {
		name  string
		re    *regexp.Regexp
		id    string
		in    string
		match bool
	}{
		{name: "job id at today's length", re: stateJobIDRe, id: "job-" + short, in: "Started background job job-" + short + ".", match: true},
		{name: "job id longer than 16", re: stateJobIDRe, id: "job-" + long, in: "Started background job job-" + long + ".", match: true},
		{name: "tjob id longer than 16", re: stateJobIDRe, id: "tjob-" + long, in: "terminal send tjob-" + long + " started", match: true},
		{name: "job id at the new cap", re: stateJobIDRe, id: "job-" + tooLong, in: "Started background job job-" + tooLong + ".", match: true},
		{name: "terminal id at today's length", re: stateTerminalIDRe, id: "term-" + short, in: "Opened terminal term-" + short + ".", match: true},
		{name: "terminal id longer than 16", re: stateTerminalIDRe, id: "term-" + long, in: "Opened terminal term-" + long + ".", match: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueMatches(tc.re, tc.in)
			if tc.match {
				if len(got) != 1 || got[0] != tc.id {
					t.Fatalf("matches = %v, want exactly [%s]", got, tc.id)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("matches = %v, want none", got)
			}
		})
	}
}
