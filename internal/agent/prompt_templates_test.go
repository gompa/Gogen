package agent

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- board item accessor + agent link ---

func TestBoardItemAccessor(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("T", "D", "low", "user"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "T" || item.Status != "backlog" {
		t.Fatalf("item = %+v", item)
	}
	if _, err := m.Item("99"); err == nil {
		t.Fatal("missing item should error")
	}
}

func TestBoardAgentLink(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("T", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim("1", "ticket #1: T"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AttachAgent("1", "sess123", "ticket #1: T"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Assignee != "ticket #1: T" || item.AgentSessionID != "sess123" || item.Status != "in_progress" {
		t.Fatalf("linked item = %+v", item)
	}
	show, err := m.Show("1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "Agent session: sess123") {
		t.Fatalf("show missing agent session: %q", show)
	}
	data, err := os.ReadFile(m.itemPath("1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "agentSession") {
		t.Fatal("agentSession not persisted in the ticket file")
	}
	// ResetAgent clears the link and moves the ticket back to backlog.
	if _, err := m.ResetAgent("1", "user"); err != nil {
		t.Fatal(err)
	}
	item, _ = m.Item("1")
	if item.Assignee != "" || item.AgentSessionID != "" || item.Status != "backlog" {
		t.Fatalf("reset item = %+v", item)
	}
	// Resetting a clean item is a no-op, and re-claiming works afterwards.
	if _, err := m.ResetAgent("1", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim("1", "ticket #1: T"); err != nil {
		t.Fatalf("re-claim after reset: %v", err)
	}
}

// --- ticket prompt ---

func TestTicketPromptDefault(t *testing.T) {
	item := &BoardItem{
		ID:          "3",
		Title:       "Fix the parser",
		Description: "make go test pass",
		Priority:    "high",
	}
	p := TicketPrompt(item, "")
	for _, want := range []string{
		"board ticket #3: Fix the parser",
		"make go test pass",
		"Priority: high",
		"don't claim it again",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Ticket log context") {
		t.Errorf("fresh ticket must not include activity context:\n%s", p)
	}
}

func TestTicketPromptEmptyFields(t *testing.T) {
	p := TicketPrompt(&BoardItem{ID: "1", Title: "T"}, "")
	if !strings.Contains(p, "Priority: none") {
		t.Errorf("empty priority should render as none:\n%s", p)
	}
}

func TestTicketPromptContext(t *testing.T) {
	item := &BoardItem{ID: "1", Title: "T", Activity: []BoardActivity{
		{Text: "created"},
		{Text: "claimed"},
		{Text: "moved to in_progress"},
		{Text: "blocked: waiting for API key"},
		{Text: "confirmed with @jane: use v2"},
		{Text: "marked done"},
	}}
	p := TicketPrompt(item, "")
	for _, want := range []string{"Ticket log context", "blocked: waiting for API key", "confirmed with @jane: use v2"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	for _, noise := range []string{"- created", "- claimed", "- moved to in_progress", "- marked done"} {
		if strings.Contains(p, noise) {
			t.Errorf("transition noise %q leaked into the prompt:\n%s", noise, p)
		}
	}
}

func TestTicketPromptCustomTemplate(t *testing.T) {
	item := &BoardItem{ID: "7", Title: "X", Description: "desc"}
	custom := "Custom: {id} / {title} / {description} / {priority} / {context}"
	if got := TicketPrompt(item, custom); got != "Custom: 7 / X / desc / none / " {
		t.Fatalf("custom prompt = %q", got)
	}
	// A template equal to the built-in default resolves back to the default
	// (the default text must never be baked in).
	if got := TicketPrompt(item, DefaultBoardStartPrompt); got != TicketPrompt(item, "") {
		t.Fatal("template equal to the default must resolve to the default")
	}
}

func TestActivityContextFilter(t *testing.T) {
	allNoise := []BoardActivity{
		{Text: "created"}, {Text: "claimed"}, {Text: "moved to in_review"},
		{Text: "marked done"}, {Text: "   "},
	}
	if got := activityContext(allNoise); len(got) != 0 {
		t.Fatalf("all-transition log = %v, want empty", got)
	}
	var many []BoardActivity
	for i := 0; i < 7; i++ {
		many = append(many, BoardActivity{Text: "note " + string(rune('a'+i))})
	}
	many = append(many, BoardActivity{Text: strings.Repeat("x", 400)})
	got := activityContext(many)
	if len(got) != 5 {
		t.Fatalf("capped entries = %d, want 5", len(got))
	}
	if got[0] != "note d" {
		t.Fatalf("cap should keep the LAST 5 entries, got %v", got)
	}
	if utf8.RuneCountInString(got[4]) != maxActivityContextLineLen+1 || !strings.HasSuffix(got[4], "…") {
		t.Fatalf("long entry not truncated: runes=%d", utf8.RuneCountInString(got[4]))
	}
}

// resetPromptConfigForTest clears both configurable prompt templates back to
// the built-in defaults. Tests that configure them must register it via
// t.Cleanup so the package-global prompt config cannot leak between tests
// (test order independence).
func resetPromptConfigForTest() {
	ConfigureSystemPrompt("")
	ConfigureSubagentPrompt("")
}

// --- subagent job wrapper ---

func TestFormatSubagentJob(t *testing.T) {
	t.Cleanup(resetPromptConfigForTest)
	job := "fix the bug"
	def := FormatSubagentJob(job)
	if !strings.Contains(def, job) || !strings.Contains(def, "subagent") {
		t.Fatalf("default wrapper missing job: %q", def)
	}
	ConfigureSubagentPrompt("Work on: {job}")
	if got := FormatSubagentJob(job); got != "Work on: fix the bug" {
		t.Fatalf("custom wrapper = %q", got)
	}
	ConfigureSubagentPrompt(DefaultSubagentPrompt)
	if got := FormatSubagentJob(job); got != def {
		t.Fatal("template equal to the default must resolve to the built-in")
	}
}

// --- system prompt ---

func TestSystemPromptConfigured(t *testing.T) {
	t.Cleanup(resetPromptConfigForTest)
	def := SystemPrompt("/w")
	if !strings.Contains(def, "/w") || !strings.Contains(def, "GoGen") {
		t.Fatalf("default system prompt broken: %q", def[:60])
	}
	ConfigureSystemPrompt("Custom agent in {working_dir}")
	if got := SystemPrompt("/x"); got != "Custom agent in /x" {
		t.Fatalf("configured system prompt = %q", got)
	}
	// The default text is never baked in: configuring it verbatim resolves
	// back to the built-in default.
	ConfigureSystemPrompt(DefaultSystemPromptTemplate())
	if got := SystemPrompt("/w"); got != def {
		t.Fatal("template equal to the default must resolve to the built-in")
	}
	// An OLD (different) default text is a pinned custom prompt.
	ConfigureSystemPrompt(strings.ReplaceAll(def, "GoGen", "Gogen"))
	got := SystemPrompt("/w")
	if got == def || !strings.Contains(got, "Gogen") {
		t.Fatal("old default text should be treated as a pinned custom prompt")
	}
}
