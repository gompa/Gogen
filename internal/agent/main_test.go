package agent

import (
	"os"
	"testing"

	"gogen/internal/modelinfo"
)

// TestMain keeps the package's tests off the public internet.
//
// Many tests build a real llm provider (context-limit resolution, restore
// validation, model auto-selection); every such provider constructs its own
// modelinfo resolver, and a cold resolver — each test's t.TempDir() has no
// .gogen/models.json — starts a ~6 MB models.dev catalog download. The catalog
// is irrelevant to these tests (they resolve limits from their own httptest
// endpoint), and parsing it under the race detector was enough background CPU
// to push this package past CI's per-package test timeout.
func TestMain(m *testing.M) {
	modelinfo.Offline()
	os.Exit(m.Run())
}
