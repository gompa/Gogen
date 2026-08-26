package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"gogen/internal/llm"
)

// groupHeaderText composes exactly what buildModelRows emits for a provider
// group header (same vars, so it holds under NO_COLOR too).
func groupHeaderText(name string) string {
	return "  " + ansiCyanOn + name + ansiReset
}

func TestBuildModelRows(t *testing.T) {
	tests := []struct {
		name        string
		list        []llm.ModelInfo
		start       int
		end         int
		cursor      int
		wantHeaders []string // in order, one per emitted group header
		wantNumbers []int    // 1-based numbers that must appear as rows
	}{
		{
			name: "groups consecutive providers under styled headers",
			list: []llm.ModelInfo{
				{ID: "gpt-4o", Provider: "default"},
				{ID: "gpt-4o-mini", Provider: "default"},
				{ID: "llama3", Provider: "ollama"},
			},
			start: 0, end: 3, cursor: 0,
			wantHeaders: []string{"default", "ollama"},
			wantNumbers: []int{1, 2, 3},
		},
		{
			name: "empty provider tag falls back to default",
			list: []llm.ModelInfo{
				{ID: "m1"},
				{ID: "m2", Provider: ""},
				{ID: "m3", Provider: "default"},
			},
			start: 0, end: 3, cursor: 0,
			wantHeaders: []string{"default"},
			wantNumbers: []int{1, 2, 3},
		},
		{
			name: "resuming mid-group emits no duplicate header",
			list: []llm.ModelInfo{
				{ID: "a1", Provider: "p"}, {ID: "a2", Provider: "p"}, {ID: "a3", Provider: "p"},
				{ID: "a4", Provider: "p"}, {ID: "a5", Provider: "p"},
			},
			start: 2, end: 5, cursor: 3,
			wantHeaders: nil,
			wantNumbers: []int{3, 4, 5},
		},
		{
			name: "headers appear for groups starting within the window",
			list: []llm.ModelInfo{
				{ID: "x1", Provider: "aa"}, {ID: "x2", Provider: "aa"},
				{ID: "y1", Provider: "bb"}, {ID: "y2", Provider: "bb"},
				{ID: "z1", Provider: "cc"},
			},
			start: 2, end: 5, cursor: 2,
			wantHeaders: []string{"bb", "cc"},
			wantNumbers: []int{3, 4, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := buildModelRows(tt.list, tt.start, tt.end, tt.cursor)

			var gotHeaders []string
			for _, r := range rows {
				if !r.preStyled {
					continue
				}
				for _, prov := range tt.wantHeaders {
					if r.text == groupHeaderText(prov) {
						gotHeaders = append(gotHeaders, prov)
						break
					}
				}
			}
			if fmt.Sprint(gotHeaders) != fmt.Sprint(tt.wantHeaders) {
				t.Fatalf("group headers = %v, want %v", gotHeaders, tt.wantHeaders)
			}

			for _, n := range tt.wantNumbers {
				want := fmt.Sprintf("  %2d. ", n)
				found := false
				for _, r := range rows {
					if !r.preStyled && strings.HasPrefix(r.text, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("model number %d missing from rendered rows", n)
				}
			}
		})
	}
}

func TestBuildModelRowsHighlightsCursor(t *testing.T) {
	list := []llm.ModelInfo{
		{ID: "a", Provider: "p"},
		{ID: "b", Provider: "q"},
		{ID: "c", Provider: "q", Current: true},
	}
	rows := buildModelRows(list, 0, 3, 2)

	highlighted := 0
	for _, r := range rows {
		if r.preStyled {
			continue
		}
		if r.highlight {
			highlighted++
			if !strings.HasSuffix(r.text, "c") && !strings.Contains(r.text, " c") {
				t.Fatalf("highlighted row is not the cursor model: %q", r.text)
			}
		}
	}
	if highlighted != 1 {
		t.Fatalf("got %d highlighted rows, want exactly 1", highlighted)
	}
}

func TestBuildModelRowsContextAndCurrentSuffix(t *testing.T) {
	list := []llm.ModelInfo{{ID: "glm-5", Provider: "z", ContextLimit: 128000, Current: true}}
	rows := buildModelRows(list, 0, 1, 0)

	var entry *styleLine
	for i := range rows {
		if !rows[i].preStyled {
			entry = &rows[i]
		}
	}
	if entry == nil {
		t.Fatal("no model entry row rendered")
	}
	want := "   1. glm-5  (context: 128000 tokens) *"
	if entry.text != want {
		t.Fatalf("entry row = %q, want %q", entry.text, want)
	}
}

func TestRenderModelsModalShowsGroups(t *testing.T) {
	m := &Model{
		modelList: []llm.ModelInfo{
			{ID: "gpt-4o", Provider: "default", ContextLimit: 128000, Current: true},
			{ID: "llama3", Provider: "ollama", ContextLimit: 32768},
		},
		modelCursor: 0,
		height:      40,
	}
	out := m.renderModelsModal()
	if !strings.Contains(out, groupHeaderText("default")) ||
		!strings.Contains(out, groupHeaderText("ollama")) {
		t.Fatalf("modal output missing provider group headers:\n%s", out)
	}
	// Numbering stays aligned with /models <n> across group boundaries:
	// the second model keeps number 2 even though it sits under a new header.
	if !strings.Contains(out, "   2. llama3") {
		t.Fatalf("second model lost its continuous numbering:\n%s", out)
	}
}

func TestRenderModelsModalEmpty(t *testing.T) {
	m := &Model{}
	out := m.renderModelsModal()
	if !strings.Contains(out, "No models available.") {
		t.Fatalf("empty modal output unexpected:\n%s", out)
	}
}

// The picker's width has three regimes: the fixed preferred default for
// short catalogs (no hugging), growth to fit the biggest model name when
// the terminal is wide enough, and truncation under the terminal clamp
// when it is not. The probe measures the WHOLE list, so scrolling between
// windows that include/exclude the longest entry must never resize the box.
func TestRenderModelsModalStableWidth(t *testing.T) {
	boxWidth := func(t *testing.T, list []llm.ModelInfo, termW int) int {
		t.Helper()
		m := &Model{modelList: list, width: termW, height: 40}
		out := m.renderModelsModal()
		lines := strings.Split(out, "\n")
		top := lipgloss.Width(lines[0])
		for i, l := range lines {
			if w := lipgloss.Width(l); w != top {
				t.Fatalf("ragged modal box at row %d: border %d vs row %d", i, top, w)
			}
		}
		return top
	}

	short := []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}}
	longID := "anthropic/claude-sonnet-4-5-20250929-with-a-very-long-provider-qualified-identifier"
	long := []llm.ModelInfo{{ID: longID, Provider: "default", ContextLimit: 200000}}

	t.Run("short ids get the preferred width", func(t *testing.T) {
		if got := boxWidth(t, short, 100); got != modelsModalInnerWidth+2 {
			t.Fatalf("box = %d cells, want the preferred %d", got, modelsModalInnerWidth+2)
		}
	})

	t.Run("biggest name fits when the terminal is wide enough", func(t *testing.T) {
		const termW = 200
		want := lipgloss.Width(modelEntryText(0, long[0])) + 6 // padding + borders
		if got := boxWidth(t, long, termW); got != want {
			t.Fatalf("box = %d cells, want %d (the longest entry fully visible)", got, want)
		}
		m := &Model{modelList: long, width: termW, height: 40}
		if plain := stripANSI(m.renderModelsModal()); !strings.Contains(plain, longID+"  (context: 200000 tokens)") {
			t.Fatal("wide terminal truncated an id that fits")
		}
	})

	t.Run("names wider than the screen grow to the clamp and truncate there", func(t *testing.T) {
		// limit = 100-6 inner -> total 96; the natural width exceeds it.
		if got := boxWidth(t, long, 100); got != 96 {
			t.Fatalf("box = %d cells, want the clamped %d", got, 100-4)
		}
		m := &Model{modelList: long, width: 100, height: 40}
		plain := stripANSI(m.renderModelsModal())
		// The 85-char id itself still fits inside the clamped row; what
		// must be cut is the trailing context suffix beyond the budget.
		if !strings.Contains(plain, longID) {
			t.Fatal("clamped row lost an id that still fits within the clamp")
		}
		if strings.Contains(plain, "(context: 200000 tokens)") {
			t.Fatal("row content beyond the terminal clamp was not truncated")
		}
	})

	t.Run("width does not change while scrolling past the long entry", func(t *testing.T) {
		list := make([]llm.ModelInfo, 0, 30)
		for i := 0; i < 29; i++ {
			list = append(list, llm.ModelInfo{ID: fmt.Sprintf("short-model-%d", i)})
		}
		list = append(list, llm.ModelInfo{ID: longID, ContextLimit: 200000})
		atTop := boxWidth(t, list, 120) // window [0..27): excludes index 29
		m := &Model{modelList: list, modelCursor: 29, width: 120, height: 40}
		bottom := lipgloss.Width(strings.Split(m.renderModelsModal(), "\n")[0]) // includes index 29
		if atTop != bottom {
			t.Fatalf("box resized while scrolling: %d -> %d cells", atTop, bottom)
		}
	})

	t.Run("empty catalog matches the populated picker width", func(t *testing.T) {
		m := &Model{width: 100, height: 40}
		if got := lipgloss.Width(m.renderModelsModal()); got != modelsModalInnerWidth+2 {
			t.Fatalf("empty-catalog box = %d cells, want %d", got, modelsModalInnerWidth+2)
		}
	})

	t.Run("narrow terminal still clamps", func(t *testing.T) {
		// limit = 50-6 inner -> total 46; preferred 58 must give way.
		if got := boxWidth(t, short, 50); got > 46 {
			t.Fatalf("box = %d cells in a 50-col terminal, want <= 46", got)
		}
	})
}
