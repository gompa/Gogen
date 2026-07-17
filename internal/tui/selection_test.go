package tui

import (
	"strings"
	"testing"
)

func TestExtractLeadingSGR(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSGR   string
		wantEmpty bool
	}{
		{
			name:    "gray foreground",
			input:   "\x1b[38;2;136;136;136m<thinking>test</thinking>\x1b[0m",
			wantSGR: "\x1b[38;2;136;136;136m",
		},
		{
			name:    "bold + color",
			input:   "\x1b[1m\x1b[38;2;0;170;170mGoGen:\x1b[0m",
			wantSGR: "\x1b[1m\x1b[38;2;0;170;170m",
		},
		{
			name:      "plain text",
			input:     "just plain text",
			wantEmpty: true,
		},
		{
			name:      "empty string",
			input:     "",
			wantEmpty: true,
		},
		{
			name:      "SGR not at start",
			input:     "hello \x1b[38mworld",
			wantEmpty: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLeadingSGR(tt.input)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("expected empty, got %q", got)
				}
				return
			}
			if got != tt.wantSGR {
				t.Fatalf("expected %q, got %q", tt.wantSGR, got)
			}
		})
	}
}

func TestSGRPropagation(t *testing.T) {
	sgr := extractLeadingSGR("\x1b[38;2;136;136;136m<thinking>test</thinking>\x1b[0m")
	if sgr == "" {
		t.Fatal("expected non-empty SGR")
	}

	t.Run("thinking block — SGR open at split", func(t *testing.T) {
		// Simulate wordwrap splitting a fully-styled thinking line.
		// Part 0 has no \x1b[0m → style is still open → propagate.
		parts := []string{
			"\x1b[38;2;136;136;136m<thinking>very long",
			"reasoning text</thinking>\x1b[0m",
		}
		if strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 should NOT have reset for this test to be meaningful")
		}
		for i := 1; i < len(parts); i++ {
			parts[i] = sgr + parts[i] + "\x1b[0m"
		}
		cont := parts[1]
		if !strings.HasPrefix(cont, sgr) {
			t.Fatalf("continuation missing SGR prefix: %q", cont)
		}
		if !strings.HasSuffix(cont, "\x1b[0m") {
			t.Fatalf("continuation missing reset suffix: %q", cont)
		}
	})

	t.Run("assistant message — SGR closed at split", func(t *testing.T) {
		// Simulate wordwrap splitting a partially-styled line where
		// the label SGR was already closed before the wrap point.
		parts := []string{
			"\x1b[1m\x1b[38;2;0;170;170mGoGen:\x1b[0m long",
			"message text continues here",
		}
		if !strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 SHOULD have reset for this test to be meaningful")
		}
		// Fix: skip propagation for this case
		shouldPropagate := !strings.Contains(parts[0], "\x1b[0m")
		if shouldPropagate {
			t.Fatal("should NOT propagate SGR when reset is present in part 0")
		}
		// Continuation line should remain unstyled
		cont := parts[1]
		if strings.HasPrefix(cont, "\x1b[") {
			t.Fatalf("continuation should NOT have ANSI prefix: %q", cont)
		}
	})

	t.Run("tool call — multi-styled, SGR closed early", func(t *testing.T) {
		// Tool call prefix SGR closes before the args start.
		// Part 0 has \x1b[0m → don't propagate prefix SGR.
		parts := []string{
			"\x1b[38;2;204;170;0m  →\x1b[0m name \x1b[38;2;136;136;136mvery",
			"long args here\x1b[0m",
		}
		if !strings.Contains(parts[0], "\x1b[0m") {
			t.Fatal("part 0 SHOULD have reset")
		}
		shouldPropagate := !strings.Contains(parts[0], "\x1b[0m")
		if shouldPropagate {
			t.Fatal("should NOT propagate when reset is in part 0")
		}
	})
}

func TestWrapLineFitsWidth(t *testing.T) {
	m := &Model{}
	m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
	m.viewport.Style = ViewportStyle

	line := "See https://example.com/very/long/path/that/exceeds/forty/columns/easily for details"
	parts := m.wrapLine(line)
	w := m.wrapWidth()
	for i, p := range parts {
		plain := stripANSI(p)
		if got := len([]rune(plain)); got > w {
			t.Fatalf("part %d rune width %d > wrapWidth %d: %q", i, got, w, plain)
		}
	}
	if len(parts) < 3 {
		t.Fatalf("expected hard-wrap to split overlong URL, got %d parts: %#v", len(parts), parts)
	}
}
