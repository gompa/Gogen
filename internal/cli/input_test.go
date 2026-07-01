package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseKeyArrowSequences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want keyKind
	}{
		{"up CSI", "\x1b[A", keyUp},
		{"down CSI", "\x1b[B", keyDown},
		{"right CSI", "\x1b[C", keyRight},
		{"left CSI", "\x1b[D", keyLeft},
		{"up SS3", "\x1bOA", keyUp},
		{"down SS3", "\x1bOB", keyDown},
		{"right SS3", "\x1bOC", keyRight},
		{"left SS3", "\x1bOD", keyLeft},
		{"enter", "\r", keyEnter},
		{"backspace", "\x7f", keyBackspace},
		{"tab", "\t", keyTab},
		{"printable", "a", keyRune},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, _, err := parseKey(bytes.NewReader([]byte(tt.in)))
			if err != nil {
				t.Fatalf("parseKey() error = %v", err)
			}
			if kind != tt.want {
				t.Fatalf("parseKey() kind = %v, want %v", kind, tt.want)
			}
		})
	}
}

func TestParseKeyPrintableRune(t *testing.T) {
	kind, ch, err := parseKey(bytes.NewReader([]byte("z")))
	if err != nil {
		t.Fatalf("parseKey() error = %v", err)
	}
	if kind != keyRune || ch != 'z' {
		t.Fatalf("parseKey() = (%v, %q), want (%v, %q)", kind, ch, keyRune, 'z')
	}
}

func TestParseKeyIgnoresUnknownEscape(t *testing.T) {
	kind, _, err := parseKey(bytes.NewReader([]byte("\x1b[Z")))
	if err != nil {
		t.Fatalf("parseKey() error = %v", err)
	}
	if kind != keyIgnore {
		t.Fatalf("parseKey() kind = %v, want %v", kind, keyIgnore)
	}
}

func TestParseKeyCtrlC(t *testing.T) {
	kind, _, err := parseKey(bytes.NewReader([]byte{3}))
	if err != nil {
		t.Fatalf("parseKey() error = %v", err)
	}
	if kind != keyCtrlC {
		t.Fatalf("parseKey() kind = %v, want %v", kind, keyCtrlC)
	}
}

func TestComputeLayout_SingleLine(t *testing.T) {
	// 80-col terminal, prompt "> " (2 chars), buffer 5 chars, cursor at end.
	total, curRow, curCol := computeLayout("> ", []rune("hello"), 5, 80)
	if total != 1 {
		t.Fatalf("totalRows = %d, want 1", total)
	}
	if curRow != 0 || curCol != 7 {
		t.Fatalf("cursor at (%d,%d), want (0,7)", curRow, curCol)
	}
}

func TestComputeLayout_ExactFitNoWrap(t *testing.T) {
	// 80-col terminal, prompt 2 chars, buffer 78 chars = exactly 80 → one row, no wrap.
	buf := []rune(strings.Repeat("x", 78))
	total, curRow, curCol := computeLayout("> ", buf, 78, 80)
	if total != 1 {
		t.Fatalf("totalRows = %d, want 1 (80 chars fit on one 80-col row)", total)
	}
	if curRow != 0 {
		t.Fatalf("cursor row = %d, want 0", curRow)
	}
	// Column 80 is the pending-wrap position — terminal right margin.
	if curCol != 80 {
		t.Fatalf("cursor col = %d, want 80", curCol)
	}
}

func TestComputeLayout_OneOverWraps(t *testing.T) {
	// 80-col terminal, prompt 2 chars, buffer 79 chars = 81 total → wraps to 2 rows.
	buf := []rune(strings.Repeat("x", 79))
	total, curRow, curCol := computeLayout("> ", buf, 79, 80)
	if total != 2 {
		t.Fatalf("totalRows = %d, want 2 (81 chars wrap on 80-col terminal)", total)
	}
	if curRow != 1 {
		t.Fatalf("cursor row = %d, want 1", curRow)
	}
	if curCol != 1 {
		t.Fatalf("cursor col = %d, want 1 (first char on row 2)", curCol)
	}
}

func TestComputeLayout_MidCursorOnFirstRow(t *testing.T) {
	// Cursor in the middle of a single-line buffer.
	total, curRow, curCol := computeLayout("> ", []rune("abcdef"), 3, 80)
	if total != 1 {
		t.Fatalf("totalRows = %d, want 1", total)
	}
	if curRow != 0 || curCol != 5 {
		t.Fatalf("cursor at (%d,%d), want (0,5)", curRow, curCol)
	}
}

func TestComputeLayout_MidCursorOnWrappedRow(t *testing.T) {
	// 10-col terminal, prompt 2 chars, buffer 15 chars = 17 total → wraps.
	// Row 0: "> abcdefgh" (2+8 = 10 chars), Row 1: "ijklmno" (7 chars)
	// Cursor at position 10 (the 'k' character, first char on row 2 after wrap from the 9th buf char).
	buf := []rune("abcdefghijklmno")
	total, curRow, curCol := computeLayout("> ", buf, 10, 10)
	if total != 2 {
		t.Fatalf("totalRows = %d, want 2", total)
	}
	if curRow != 1 || curCol != 2 {
		t.Fatalf("cursor at (%d,%d), want (1,2) — 'k' is col 2 of row 2", curRow, curCol)
	}
}

func TestComputeLayout_NewlineInBuffer(t *testing.T) {
	// Newline in buffer forces a new row regardless of columns.
	buf := []rune("ab\ncd")
	total, curRow, curCol := computeLayout("> ", buf, 5, 80)
	if total != 2 {
		t.Fatalf("totalRows = %d, want 2 (newline in buffer)", total)
	}
	if curRow != 1 || curCol != 2 {
		t.Fatalf("cursor at (%d,%d), want (1,2) — after newline, at 'cd'", curRow, curCol)
	}
}

func TestComputeLayout_EmptyBuffer(t *testing.T) {
	total, curRow, curCol := computeLayout("> ", []rune(""), 0, 80)
	if total != 1 {
		t.Fatalf("totalRows = %d, want 1", total)
	}
	if curRow != 0 || curCol != 2 {
		t.Fatalf("cursor at (%d,%d), want (0,2) — after prompt", curRow, curCol)
	}
}
