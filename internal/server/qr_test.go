package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderBitmap(t *testing.T) {
	cases := []struct {
		name   string
		bitmap [][]bool
		want   string
	}{
		{"both dark", [][]bool{{true, true}, {true, true}}, "██\n"},
		{"top dark", [][]bool{{true, false}, {false, false}}, "▀ \n"},
		{"bottom dark", [][]bool{{false, false}, {true, false}}, "▄ \n"},
		{"empty pair", [][]bool{{false, false}, {false, false}}, "  \n"},
		{"odd row count", [][]bool{{true, true}}, "▀▀\n"},
		{"mixed", [][]bool{{true, false, true}, {false, true, true}}, "▀▄█\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderBitmap(tc.bitmap); got != tc.want {
				t.Fatalf("renderBitmap() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderQR(t *testing.T) {
	const content = "http://192.168.1.5:8081/pair/9f3a2b1c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a"
	out, err := RenderQR(content)
	if err != nil {
		t.Fatalf("RenderQR: %v", err)
	}
	if out == "" {
		t.Fatal("RenderQR returned empty output")
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	// Blocks are 3-byte runes and spaces are 1-byte, so compare rune counts
	// (each module column is exactly one rune).
	width := utf8.RuneCountInString(lines[0])
	if width < 21 {
		t.Fatalf("QR width = %d, want >= 21 (smallest version + quiet zone)", width)
	}
	hasDark := false
	hasLight := false
	for _, line := range lines {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("ragged QR line: %d runes, want %d", utf8.RuneCountInString(line), width)
		}
		for _, r := range line {
			switch r {
			case ' ', '▀', '▄', '█':
			default:
				t.Fatalf("unexpected character %q in QR output", r)
			}
			if r == ' ' {
				hasLight = true
			} else {
				hasDark = true
			}
		}
	}
	if !hasDark || !hasLight {
		t.Fatal("QR output must contain both dark modules and quiet-zone spaces")
	}
	// Deterministic output: same content renders identically.
	out2, err := RenderQR(content)
	if err != nil {
		t.Fatalf("RenderQR second call: %v", err)
	}
	if out2 != out {
		t.Fatal("RenderQR is not deterministic for identical content")
	}
}
