package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNoOpWithoutEnv(t *testing.T) {
	Configure("", "")
	t.Cleanup(func() {
		Configure("", "")
		CloseLog()
	})
	Write("test.go:1", "msg", "H", map[string]interface{}{"k": "v"})
}

func TestWriteCreatesLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.log")
	// Configure (not ambient env): tests must not inherit a developer's
	// GOGEN_DEBUG_LOG, and Write ignores env while testing.Testing().
	Configure(path, "test-session")
	t.Cleanup(func() {
		CloseLog()
		Configure("", "")
	})

	Write("test.go:1", "hello", "H", map[string]interface{}{"k": "v"})
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

func TestWriteIgnoresAmbientEnvDuringTests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "should-not-create.log")
	Configure("", "") // clear any prior Configure from other tests
	t.Setenv("GOGEN_DEBUG_LOG", path)
	t.Cleanup(func() {
		CloseLog()
		Configure("", "")
	})

	Write("test.go:1", "should-not-land", "H", nil)
	CloseLog()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ambient GOGEN_DEBUG_LOG should be ignored during tests, got err=%v", err)
	}
}
