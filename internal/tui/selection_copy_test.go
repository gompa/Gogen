package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"gogen/internal/modelinfo"
)

// The copy path is the one that shipped: drag to select in the transcript —
// the TUI reports mouse motion (View's MouseModeCellMotion), so the highlight
// is the app's, not the terminal's — then press ctrl+shift+c. These tests pin
// that path end to end instead of assuming it: the chord has to reach the key
// handler and the clipboard, and the SELECTION has to still be there when it
// does.
//
// That last part is where it broke. Nothing was wrong with the write itself;
// the selection was gone by the time the key was pressed, because
// setViewportContent dropped it unconditionally and that path runs on every
// round boundary and every mid-line streaming write — so a selection made
// while a turn streamed was wiped a moment later ("sometimes I select text and
// copy does nothing"). handleKeyMsg then hit the empty branch, which returned
// without a word, making the failure look like a dead keybinding.

// TestMain neutralizes the local clipboard for the whole package: the real
// writer shells out to wl-copy/xclip/xsel, which must never run (nor touch the
// developer's clipboard) during `go test`. Tests that observe a write stub it.
// It also takes the whole package offline with respect to the models.dev
// registry: providers built by these tests otherwise each started a ~6 MB
// catalog download (see modelinfo.Offline).
func TestMain(m *testing.M) {
	writeClipboard = func(string) error { return errors.New("clipboard disabled in tests") }
	modelinfo.Offline()
	os.Exit(m.Run())
}

// stubClipboard replaces the local clipboard writer for one test and returns a
// pointer to the recorded writes.
func stubClipboard(t *testing.T, err error) *[]string {
	t.Helper()
	var got []string
	prev := writeClipboard
	writeClipboard = func(s string) error {
		got = append(got, s)
		return err
	}
	t.Cleanup(func() { writeClipboard = prev })
	return &got
}

// copyTestModel builds a sized model with one transcript row to select.
func copyTestModel(t *testing.T) *Model {
	t.Helper()
	m := newSidebarTestModel(80)
	m.sidebarVisible = false
	// NewModel installs the keymap; the bare test constructor does not, and
	// without it no binding can match.
	m.keys = DefaultKeyMap
	m.SetSize(80, 24)
	m.chatLines = []string{"hello world", "second line"}
	m.setViewportContent()
	return m
}

// dragSelect runs a real press → motion → release over content cells 0..5 of
// the first row (ViewportStyle pads one cell on the left, so terminal column 1
// is content column 0).
func dragSelect(m *Model) {
	m.handleMouseMsg(tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseMotionMsg{X: 6, Y: 0, Button: tea.MouseLeft})
	m.handleMouseMsg(tea.MouseReleaseMsg{X: 6, Y: 0, Button: tea.MouseLeft})
}

// pressCopy presses the copy chord through the real key handler.
func pressCopy(m *Model) tea.Cmd {
	_, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift})
	return cmd
}

func TestCopyChordAfterDrag(t *testing.T) {
	writes := stubClipboard(t, nil)
	m := copyTestModel(t)

	dragSelect(m)
	if got := m.getSelectedText(); got != "hello" {
		t.Fatalf("drag selected %q, want hello", got)
	}
	if cmd := pressCopy(m); cmd != nil {
		t.Fatal("a local clipboard write must not need the OSC52 fallback")
	}
	if len(*writes) != 1 || (*writes)[0] != "hello" {
		t.Fatalf("clipboard writes = %v, want exactly [hello]", *writes)
	}
	if !strings.Contains(m.statusMsg, "Copied 5 chars") {
		t.Fatalf("status = %q, want the copy confirmation", m.statusMsg)
	}
	if m.selection != nil {
		t.Fatal("a successful copy clears the highlight (it has served its purpose)")
	}
}

// TestCopyChordSurvivesRebuilds is the regression for the reported "I can
// select text but cannot copy it". Each step is a real call site that runs the
// full re-wrap (setViewportContent) while a turn is being rendered — and each
// of them used to drop the selection on the floor, because that function
// cleared it unconditionally even when the rows it covered were untouched.
func TestCopyChordSurvivesRebuilds(t *testing.T) {
	steps := []struct {
		name string
		run  func(m *Model)
	}{
		{"round boundary", func(m *Model) { m.handleStreamRoundEnd() }},
		{"mid-line streaming write (growing tool diff)", func(m *Model) {
			// idx 1 of 3: not the last line, so this is the full-rebuild path
			// (the last line goes through the incremental append funnel).
			m.chatLines = append(m.chatLines, "third line")
			m.commitChatLineWrite(1, false, "second line, rewritten mid-stream")
		}},
		{"re-wrap at the same width", func(m *Model) { m.setViewportContent() }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			writes := stubClipboard(t, nil)
			m := copyTestModel(t)
			m.streamAssistantLine = -1 // the real constructor's initial value
			dragSelect(m)

			step.run(m)

			if got := m.getSelectedText(); got != "hello" {
				t.Fatalf("selection lost by %s (selected text %q)", step.name, got)
			}
			pressCopy(m)
			if len(*writes) != 1 || (*writes)[0] != "hello" {
				t.Fatalf("clipboard writes after %s = %v, want exactly [hello]", step.name, *writes)
			}
		})
	}
}

// TestCopyChordSurvivesAppends pins the append paths, which never cleared the
// selection (buildFromPrefix) and must not start to: a token batch or a notice
// appended below the selection is the normal case while a turn streams.
func TestCopyChordSurvivesAppends(t *testing.T) {
	steps := []struct {
		name string
		run  func(m *Model)
	}{
		{"streamed token batch", func(m *Model) { m.handleStreamToken("streaming token") }},
		{"appended chat line", func(m *Model) { m.appendChatLine("System: a notice mid-turn") }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			writes := stubClipboard(t, nil)
			m := copyTestModel(t)
			m.streamAssistantLine = -1
			dragSelect(m)

			step.run(m)

			if got := m.getSelectedText(); got != "hello" {
				t.Fatalf("selection lost by %s (selected text %q)", step.name, got)
			}
			pressCopy(m)
			if len(*writes) != 1 || (*writes)[0] != "hello" {
				t.Fatalf("clipboard writes after %s = %v, want exactly [hello]", step.name, *writes)
			}
		})
	}
}

func TestCopyChordWithNoSelection(t *testing.T) {
	writes := stubClipboard(t, nil)
	m := copyTestModel(t)

	pressCopy(m)

	if len(*writes) != 0 {
		t.Fatalf("clipboard written with no selection: %v", *writes)
	}
	if !strings.Contains(m.statusMsg, "No selection") {
		t.Fatalf("status = %q, want the empty-selection hint (the old silent branch made this look like a dead key)", m.statusMsg)
	}
}

func TestCopyChordFallsBackToOSC52(t *testing.T) {
	stubClipboard(t, errors.New("no clipboard utility available"))
	m := copyTestModel(t)

	dragSelect(m)
	cmd := pressCopy(m)

	if cmd == nil {
		t.Fatal("a failed local copy must return the OSC52 fallback command")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("the fallback command produced no message")
	}
	if !strings.Contains(m.statusMsg, "terminal clipboard") {
		t.Fatalf("status = %q, want the terminal-clipboard report", m.statusMsg)
	}
	// OSC52 is fire-and-forget (no delivery ack): the selection stays so a
	// retry is one keystroke away.
	if m.selection == nil || !m.selection.Active {
		t.Fatal("the fallback must keep the selection for a retry")
	}
}

// TestCopyChordMatchingShapes pins how the chord is recognized. Key.String()
// returns Key.Text verbatim when a terminal fills it in, so matching that string
// alone missed the chord in the shape uv produces for a shifted letter; the
// keystroke form is matched too. A bare ctrl+c is NOT the chord — a terminal
// without kitty/modifyOtherKeys reporting cannot disambiguate it and the app
// must keep treating it as cancel.
func TestCopyChordMatchingShapes(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{"enhanced reporting (kitty / modifyOtherKeys)", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift}, true},
		{"terminal filled in Text", tea.KeyPressMsg{Code: 'c', ShiftedCode: 'C', Text: "C", Mod: tea.ModCtrl | tea.ModShift}, true},
		{"legacy ctrl+c byte stays cancel", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, false},
		{"plain c types", tea.KeyPressMsg{Code: 'c', Text: "c"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{keys: DefaultKeyMap}
			if got := m.matchesCopyChord(tc.msg); got != tc.want {
				t.Fatalf("matchesCopyChord(String=%q Keystroke=%q) = %v, want %v",
					tc.msg.String(), tc.msg.Keystroke(), got, tc.want)
			}
			// The two roles must never both match: the legacy shape has to
			// stay cancel, or the keystroke matching above would have stolen
			// ctrl+c.
			if key.Matches(tc.msg, DefaultKeyMap.CancelTurn) && tc.want {
				t.Fatalf("CancelTurn and CopySelection both match %q", tc.msg.Keystroke())
			}
		})
	}
}
