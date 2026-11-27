package tui

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// The paste failures reported from real terminals: a paste did nothing while a
// turn was running, and a lost paste end marker left the input decoder stuck so
// the next paste arrived prefixed with the abandoned buffer. Each has a
// regression below.

// pasteTestModel is the smallest Model that can receive a paste: an input-
// focused composer with a real textarea.
func pasteTestModel() *Model {
	m := &Model{focus: FocusInput, keys: DefaultKeyMap}
	m.textarea = textarea.New()
	m.textarea.Focus()
	return m
}

// TestPasteReachesTheComposer pins where a paste is accepted. The composer is
// editable while a turn or a compaction runs (Enter queues the message), so a
// paste must land there too; a modal and the sessions panel keep swallowing it.
func TestPasteReachesTheComposer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Model)
		want  string
	}{
		{name: "idle composer", setup: func(*Model) {}, want: "pasted text"},
		{name: "while a turn streams", setup: func(m *Model) { m.streaming = true }, want: "pasted text"},
		{name: "while compaction runs", setup: func(m *Model) { m.compacting = true }, want: "pasted text"},
		{
			name:  "transcript focused takes the paste",
			setup: func(m *Model) { m.focus = FocusViewport },
			want:  "pasted text",
		},
		{name: "modal open swallows it", setup: func(m *Model) { m.modal = ModalHelp }, want: ""},
		{
			// A VISIBLE panel owns the pointer/keys, so it owns the paste too.
			name: "sessions panel swallows it",
			setup: func(m *Model) {
				m.focus = FocusSidebar
				m.sidebarVisible = true
				m.sidebarWidth = 30
				m.width = 120
			},
			want: "",
		},
		{
			// Narrow terminal: the panel is auto-hidden, so handleKeyMsg falls
			// back to the composer — the paste must follow, or typing works there
			// and pasting silently does not.
			name: "auto-hidden sessions panel falls back to the composer",
			setup: func(m *Model) {
				m.focus = FocusSidebar
				m.sidebarVisible = true
				m.sidebarWidth = 30
				m.width = 40 // below the panel's minimum main-column width
			},
			want: "pasted text",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := pasteTestModel()
			tc.setup(m)
			if _, _ = m.Update(tea.PasteMsg{Content: "pasted text"}); m.textarea.Value() != tc.want {
				t.Fatalf("composer = %q, want %q", m.textarea.Value(), tc.want)
			}
			if tc.want != "" && m.modal == ModalNone && m.focus != FocusInput {
				t.Fatalf("focus = %v, want the composer after an accepted paste", m.focus)
			}
		})
	}
}

// TestPasteKeepsTheComposerContents proves the paste is inserted at the cursor
// of an existing draft rather than replacing it.
func TestPasteKeepsTheComposerContents(t *testing.T) {
	m := pasteTestModel()
	m.textarea.SetValue("start ")
	m.textarea.CursorEnd()
	if _, _ = m.Update(tea.PasteMsg{Content: "end"}); m.textarea.Value() != "start end" {
		t.Fatalf("composer = %q, want %q", m.textarea.Value(), "start end")
	}
}

// TestPasteRecoversFromALostEndMarker pins the recovery path: once the decoder
// is stuck, the next completed paste is delivered as one PasteMsg containing
// the stale buffer, the start marker, and the text the user just pasted. Only
// the text after the last marker may reach the composer.
func TestPasteRecoversFromALostEndMarker(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "stale buffer before the marker",
			content: "abandoned bytes" + pasteStartMarker + "the text I meant",
			want:    "the text I meant",
		},
		{
			name:    "two lost pastes then the real one",
			content: "first" + pasteStartMarker + "second" + pasteStartMarker + "third",
			want:    "third",
		},
		{
			name:    "ordinary content is untouched",
			content: "no markers here",
			want:    "no markers here",
		},
		{
			name:    "a literal 200~ without ESC is not a marker",
			content: "a[200~b",
			want:    "a[200~b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := pasteTestModel()
			if _, _ = m.Update(tea.PasteMsg{Content: tc.content}); m.textarea.Value() != tc.want {
				t.Fatalf("composer = %q, want %q", m.textarea.Value(), tc.want)
			}
		})
	}
}
