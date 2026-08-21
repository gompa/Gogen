package server

import (
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// RenderQR encodes content as a QR code and renders it as terminal-safe
// block art: one character per module column, two module rows per text line
// (Unicode half-blocks). The output uses only ' ', '▀', '▄', '█' and
// newlines, so it is readable on both light and dark terminal backgrounds
// without any ANSI color codes. The bitmap from the encoder includes the
// required quiet zone, so the render is directly scannable.
func RenderQR(content string) (string, error) {
	// High error correction: a terminal-rendered QR photographed at an
	// angle or with slight aspect distortion must still decode to the exact
	// URL (the pairing code is 64 hex chars — a single misdecoded module
	// would produce a wrong code and a 401). The cost is a slightly larger
	// QR, which is irrelevant for a printed onboarding code.
	q, err := qrcode.New(content, qrcode.High)
	if err != nil {
		return "", err
	}
	return renderBitmap(q.Bitmap()), nil
}

// renderBitmap renders a QR module matrix (bitmap[y][x], true = dark module)
// as half-block text. Each output line merges two module rows: '█' for a
// dark upper and lower module, '▀' for dark upper only, '▄' for dark lower
// only, and a space for an empty pair.
func renderBitmap(bitmap [][]bool) string {
	if len(bitmap) == 0 {
		return ""
	}
	var b strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		for x := 0; x < len(bitmap[y]); x++ {
			top := bitmap[y][x]
			bottom := y+1 < len(bitmap) && bitmap[y+1][x]
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
