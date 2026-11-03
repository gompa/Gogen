package llm

import (
	"os"
	"testing"

	"gogen/internal/modelinfo"
)

// TestMain keeps the package's tests off the public internet: a provider built
// against an httptest endpoint still constructs a models.dev resolver rooted at
// a temp dir, and a cold resolver starts a ~6 MB catalog download that these
// tests neither need nor control (see modelinfo.Offline).
func TestMain(m *testing.M) {
	modelinfo.Offline()
	os.Exit(m.Run())
}
