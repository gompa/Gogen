package server

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	qrcodegen "github.com/skip2/go-qrcode"
)

// decodeImage decodes a QR code from an image using an independent decoder
// (gozxing — not the encoder's own code), proving the rendered output is
// scannable by real-world decoders.
func decodeImage(t *testing.T, img image.Image) string {
	t.Helper()
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatalf("binary bitmap: %v", err)
	}
	reader := qrcode.NewQRCodeReader()
	result, err := reader.Decode(bmp, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result.GetText()
}

// bitmapToImage renders a module matrix as a crisp image with scale pixels
// per module (dark = black), simulating what a phone camera sees from a
// correctly displayed QR.
func bitmapToImage(bm [][]bool, scale int) image.Image {
	img := image.NewGray(image.Rect(0, 0, len(bm[0])*scale, len(bm)*scale))
	for y, row := range bm {
		for x, dark := range row {
			c := color.Gray{Y: 255}
			if dark {
				c.Y = 0
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray(x*scale+dx, y*scale+dy, c)
				}
			}
		}
	}
	return img
}

// textToBitmap inverts renderBitmap: each character maps back to two module
// rows (' '=00, '▀'=10, '▄'=01, '█'=11). This is the exact inverse of the
// terminal rendering, so decoding the reconstructed bitmap proves the
// encode → render chain is lossless (a phone scanning a correctly displayed
// QR sees exactly these modules).
func textToBitmap(t *testing.T, text string) [][]bool {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("empty QR text")
	}
	width := len([]rune(lines[0]))
	for _, line := range lines {
		if len([]rune(line)) != width {
			t.Fatalf("ragged QR line: %d runes, want %d", len([]rune(line)), width)
		}
	}
	bm := make([][]bool, 0, len(lines)*2)
	for _, line := range lines {
		var top, bottom []bool
		for _, r := range line {
			switch r {
			case ' ':
				top = append(top, false)
				bottom = append(bottom, false)
			case '▀':
				top = append(top, true)
				bottom = append(bottom, false)
			case '▄':
				top = append(top, false)
				bottom = append(bottom, true)
			case '█':
				top = append(top, true)
				bottom = append(bottom, true)
			default:
				t.Fatalf("unexpected character %q in QR output", r)
			}
		}
		bm = append(bm, top, bottom)
	}
	return bm
}

// TestQRDecodesFromBitmap verifies the encoder output (what the renderer
// displays) decodes to the exact original content with an independent
// decoder.
func TestQRDecodesFromBitmap(t *testing.T) {
	contents := []string{
		"http://192.168.1.5:8081/pair/abc123",
		"http://192.168.1.183:8098/pair/7b59bc6f1e86615c4ce9b512dd3d55c010824204174666e5fb13623a5dd9cc07",
	}
	for _, content := range contents {
		q, err := qrcodegen.New(content, qrcodegen.Medium)
		if err != nil {
			t.Fatalf("encode %q: %v", content, err)
		}
		if got := decodeImage(t, bitmapToImage(q.Bitmap(), 8)); got != content {
			t.Fatalf("decode = %q, want %q", got, content)
		}
	}
}

// TestQRTextRoundTrip decodes the rendered TEXT (inverted back to modules)
// — the strongest proof that a phone scanning the terminal output gets the
// exact URL, including the 64-hex pairing code.
func TestQRTextRoundTrip(t *testing.T) {
	content := "http://192.168.1.183:8098/pair/7b59bc6f1e86615c4ce9b512dd3d55c010824204174666e5fb13623a5dd9cc07"
	out, err := RenderQR(content)
	if err != nil {
		t.Fatalf("RenderQR: %v", err)
	}
	bm := textToBitmap(t, out)
	if got := decodeImage(t, bitmapToImage(bm, 8)); got != content {
		t.Fatalf("decode of rendered QR = %q, want %q", got, content)
	}
}
