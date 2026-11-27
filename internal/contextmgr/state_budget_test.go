package contextmgr

import (
	"strings"
	"testing"

	"gogen/internal/llm"
)

// TestRenderStateBudgetIsAPrefix pins the budget cut as a STRICT PREFIX: once
// a manifest line does not fit, the rest of that section and every
// lower-priority section are omitted — a later, shorter line must not be
// rendered below the gap. The old loop kept scanning the section, so a
// three-entry section rendered entries 1 and 3 with entry 2 missing and put the
// "N more state entries omitted" note under an entry that was not the tail.
func TestRenderStateBudgetIsAPrefix(t *testing.T) {
	m := NewManager(nil, DefaultSettings())

	// Three operations in one "operations:" section, rendered newest first
	// (Seq desc): a short line, a LONG line that will not fit, then another
	// short line that would fit if the scan continued past the gap.
	st := NewSessionState()
	foldOp := func(seqTool string, seq int, args map[string]any, result string) {
		t.Helper()
		callID := "c" + seqTool
		st.Fold([]llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: callID, Name: seqTool, Args: args}}},
			{Role: "tool", ToolCallID: callID, Content: result},
		})
	}
	foldOp("git", 1, map[string]any{"command": "status"}, "clean")
	foldOp("execute_command", 2, map[string]any{"command": strings.Repeat("x", 90)}, "ok")
	foldOp("background_job", 3, map[string]any{"action": "status", "job_id": "j1"}, "running")

	sections := st.stateSections()
	if len(sections) != 1 || sections[0].title != "operations:" || len(sections[0].lines) != 3 {
		t.Fatalf("setup: sections = %+v, want one operations section with 3 entries", sections)
	}
	lines := sections[0].lines

	base := m.EstimateTokens([]llm.Message{{Role: "assistant", Content: StateHeader + "\n" + StateFooter}})
	cost := func(s string) int {
		return m.EstimateTokens([]llm.Message{{Role: "assistant", Content: s}})
	}
	header := cost(sections[0].title)
	l1, l2, l3 := lines[0], lines[1], lines[2]
	if cost(l2) <= cost(l3) {
		t.Fatalf("setup: the long entry (%d tokens) must cost more than the short one (%d)", cost(l2), cost(l3))
	}
	// Fits the header and the first entry, plus exactly enough slack for the
	// third entry — so the old "keep scanning" loop rendered it.
	budget := base + header + cost(l1) + cost(l3)

	block := m.renderState(st, budget)
	if block == "" {
		t.Fatal("renderState returned an empty manifest")
	}
	if !strings.Contains(block, l1) {
		t.Fatalf("the first entry was dropped:\n%s", block)
	}
	if !strings.Contains(block, "2 more state entries omitted") {
		t.Fatalf("omission note does not count the two dropped entries:\n%s", block)
	}
	for _, dropped := range []string{l2, l3} {
		if strings.Contains(block, dropped) {
			t.Fatalf("entry %q rendered after the budget cut (the block is not a section prefix):\n%s", dropped, block)
		}
	}
}

// TestRenderStateBudgetOmitsWholeSection pins the other side of the same rule:
// a section whose FIRST entry does not fit never renders its title alone, and
// the note counts its entries (a title is not an entry).
func TestRenderStateBudgetOmitsWholeSection(t *testing.T) {
	m := NewManager(nil, DefaultSettings())
	st := NewSessionState()
	st.Fold([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "execute_command", Args: map[string]any{"command": strings.Repeat("y", 90)}},
			{ID: "c2", Name: "execute_command", Args: map[string]any{"command": strings.Repeat("z", 90)}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "tool", ToolCallID: "c2", Content: "ok"},
	})

	// Big enough to pass the minimum-budget guard, far too small for a section
	// title plus one entry.
	block := m.renderState(st, 48)
	for _, op := range st.stateSections()[0].lines {
		if strings.Contains(block, op) {
			t.Fatalf("an entry rendered although nothing fits:\n%s", block)
		}
	}
	if strings.Contains(block, "operations:") {
		t.Fatalf("a section title rendered without any entry:\n%s", block)
	}
	if !strings.Contains(block, "2 more state entries omitted") {
		t.Fatalf("omission note does not count both entries (a title is not an entry):\n%s", block)
	}
}
