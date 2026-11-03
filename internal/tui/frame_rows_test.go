package tui

import (
	"fmt"
	"strings"
	"testing"

	"gogen/internal/agent"
)

// The inline renderer owns every row of a terminal-height frame, so a single
// section that renders one row MORE than it was budgeted scrolls the screen by
// one: the renderer's cell diff no longer matches the real screen and the
// frame shows duplicated/ghost rows (the composer and status bar appearing
// twice) until something forces a full repaint.
//
// The sections that interpolate OUTSIDE text — the status bar, the busy
// progress strip, the sessions panel — took that text verbatim. Error strings
// from a provider, the store, a tool, or a command's output arrive with
// embedded newlines, and cellWidth SUMS a multi-line value, so the row passed
// its width check and then rendered as two rows. Likewise a bare TAB: the
// frame's measure counts one cell while the terminal expands it to the next
// tab stop and soft-wraps the row.
//
// singleRow flattens such a value to one row. These tests pin the frame shape
// end to end and the helper's contract.

// frameRows returns the frame's row count, failing when it does not match the
// terminal height (the invariant the inline renderer depends on).
func checkFrameRows(t *testing.T, m *Model, label string) {
	t.Helper()
	rows := strings.Split(m.View().Content, "\n")
	if len(rows) != m.height {
		t.Fatalf("%s: frame has %d rows, want exactly the terminal's %d", label, len(rows), m.height)
	}
	for i, row := range rows {
		if w := cellWidth(row); w > m.width {
			t.Fatalf("%s: row %d is %d cells wide, want <= %d: %q", label, i, w, m.width, row)
		}
	}
}

func TestSingleRow(t *testing.T) {
	const styled = "\x1b[38;2;136;136;136mdim\x1b[0m"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "plain text", "plain text"},
		{"styled text keeps its escapes", styled, styled},
		{"newline becomes a space", "first\nsecond", "first second"},
		{"crlf collapses", "first\r\nsecond", "first  second"},
		{"tab becomes a space", "a\tb", "a b"},
		{"bare CR becomes a space", "50%\r100%", "50% 100%"},
		{"DEL and C1 become spaces", "a\x7fb\u009bc", "a b c"},
		{"styles survive next to a newline", "a\n" + styled, "a " + styled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := singleRow(tc.in); got != tc.want {
				t.Fatalf("singleRow(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("result never contains a row break", func(t *testing.T) {
		got := singleRow("a\nb\rc\td\x1b[31mred\x1b[0m")
		for _, r := range got {
			if isRowBreakingRune(r) {
				t.Fatalf("singleRow left a row-breaking rune in %q", got)
			}
		}
	})
}

// TestStatusBarStaysOneRow is the reported symptom: a turn or compaction error
// carries a newline into the status line ("✗ <label>: <err>"), the bar renders
// two rows for a one-row budget, and the frame ends up a row taller than the
// terminal — the duplicate input band.
func TestStatusBarStaysOneRow(t *testing.T) {
	multiLine := "✗ provider error: 500 Internal Server Error\nbody:\n{\"error\": \"overloaded\"}"
	for _, width := range []int{24, 40, 80, 120} {
		for _, height := range []int{4, 8, 24} {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
				m := newSidebarTestModel(width)
				m.sidebarVisible = false
				m.SetSize(width, height)

				m.statusMsg = multiLine
				if got := len(strings.Split(m.renderStatusBar(), "\n")); got != 1 {
					t.Fatalf("status bar rendered %d rows, want 1", got)
				}
				if got := cellWidth(m.renderStatusBar()); got > m.mainWidth() {
					t.Fatalf("status bar is %d cells, want <= %d", got, m.mainWidth())
				}
				checkFrameRows(t, m, "statusMsg")

				// The context indicator shares the bar's one-row budget.
				m.statusMsg = ""
				m.contextLine = "context: 12k / 200k (6%)\nstray line"
				checkFrameRows(t, m, "contextLine")
			})
		}
	}
}

// TestWorkingDirAndModelStaySingleRow covers the bar's other interpolated
// text: a working directory (or model name) carrying a control character used
// to split the bottom frame row in two.
func TestWorkingDirAndModelStaySingleRow(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	m.SetSize(80, 24)
	m.agent.WorkingDir = "/tmp/weird\tdir\nsecond-line"
	checkFrameRows(t, m, "working dir")
}

// TestBusyStripStaysOneRow pins the progress strip's one-row budget: a tool
// name, retry reason, or queued suffix that carries a newline must not wrap
// the strip into the composer's rows (the band's total height is fixed while
// streaming — see SetSize).
func TestBusyStripStaysOneRow(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	m.SetSize(80, 24)
	m.streaming = true
	m.spinner = newProgressSpinner()
	m.progressPhase = progressTool
	m.progressLabel = "running tool\nthat broke its name"
	m.relayout()

	if got := len(strings.Split(m.renderProgressInput(), "\n")); got != 1 {
		t.Fatalf("progress strip rendered %d rows, want 1:\n%q", got, m.renderProgressInput())
	}
	checkFrameRows(t, m, "progress strip")

	// The compacting strip goes through the same choke point.
	m.streaming = false
	m.compacting = true
	m.progressLabel = "compacting history\nagain"
	m.relayout()
	if got := len(strings.Split(m.renderCompactingInput(), "\n")); got != 1 {
		t.Fatalf("compacting strip rendered %d rows, want 1:\n%q", got, m.renderCompactingInput())
	}
	checkFrameRows(t, m, "compacting strip")
}

// TestSidebarRowStaysOneRow pins the panel's rows: the block is padded to the
// chat column's height and joined beside it, so one label that splits in two
// makes the whole frame a row taller than the terminal.
func TestSidebarRowStaysOneRow(t *testing.T) {
	m := dragModel(t)
	m.SetSize(100, 24)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	// A rename may keep a newline (labels derived from a message do not), and
	// the working dir is printed verbatim too.
	m.lives.Add(&agent.Agent{SessionID: "bg-newline"}, "label with\nnewline")
	m.agent.WorkingDir = "/tmp/dir\nwith-newline"
	checkFrameRows(t, m, "sidebar rows")
}

// TestFrameRowsExactAtEveryHeight covers the layout's row budget across
// terminal heights, including the degenerate ones: below 4 rows the divider is
// given up (showDivider) so the frame still fits, and below 3 the frame is
// clamped (clampFrameRows) — it must never exceed the terminal.
func TestFrameRowsExactAtEveryHeight(t *testing.T) {
	for height := 1; height <= 30; height++ {
		for _, streaming := range []bool{false, true} {
			for _, sidebar := range []bool{false, true} {
				name := fmt.Sprintf("h=%d streaming=%v sidebar=%v", height, streaming, sidebar)
				t.Run(name, func(t *testing.T) {
					m := dragModel(t)
					m.SetSize(80, height)
					if sidebar {
						m.sidebarVisible = true
						m.sidebarWidth = defaultSidebarWidth
						m.SetSize(80, height)
					} else {
						m.sidebarVisible = false
					}
					m.chatLines = []string{"You: hi", "GoGen: hello"}
					m.setViewportContent()
					if streaming {
						m.streaming = true
						m.spinner = newProgressSpinner()
						m.progressPhase = progressThinking
					}
					m.relayout()

					rows := strings.Split(m.renderMainColumn(), "\n")
					if len(rows) > height {
						t.Fatalf("frame has %d rows in a %d-row terminal (the inline renderer scrolls and desyncs)", len(rows), height)
					}
					// From 3 rows up the chrome is arrangeable, so the frame
					// must fill the terminal exactly.
					if height >= 3 && len(rows) != height {
						t.Fatalf("frame has %d rows in a %d-row terminal, want exactly %d", len(rows), height, height)
					}
				})
			}
		}
	}
}

// TestShowDividerDropsOnTinyTerminal pins the divider's degenerate case: it is
// the row the layout gives up when the terminal cannot hold viewport, divider,
// input band and status bar.
func TestShowDividerDropsOnTinyTerminal(t *testing.T) {
	for height, want := range map[int]bool{1: false, 2: false, 3: false, 4: true, 10: true} {
		m := dragModel(t)
		m.SetSize(60, height)
		if got := m.showDivider(); got != want {
			t.Fatalf("showDivider() at height %d = %v, want %v", height, got, want)
		}
	}
}
