package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNoOpWithoutEnv(t *testing.T) {
	t.Setenv("GOGEN_DEBUG_LOG", "")
	Write("test.go:1", "msg", "H", map[string]interface{}{"k": "v"})
}

func TestWriteCreatesLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	t.Setenv("GOGEN_DEBUG_LOG", path)
	t.Setenv("GOGEN_DEBUG_SESSION", "test-session")

	Write("test.go:1", "hello", "H", map[string]interface{}{"k": "v"})

	// Close so Windows can delete the temp dir.
	CloseLog()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"hello"`, `"test.go:1"`, `"test-session"`, `"k"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in log content: %q", want, text)
		}
	}
}
