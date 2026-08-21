package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebTokenStatePath(t *testing.T) {
	// Project mode: under .gogen/ in the working dir.
	got := webTokenStatePath(false, "/work/dir")
	want := filepath.Join("/work", "dir", ".gogen", "web_token")
	if got != want {
		t.Fatalf("project mode path = %q, want %q", got, want)
	}
	// Global mode: under the global config dir.
	got = webTokenStatePath(true, "/work/dir")
	if filepath.Base(got) != "web_token" {
		t.Fatalf("global mode path = %q, want a web_token file", got)
	}
	if !strings.Contains(got, string(filepath.Separator)+"gogen") {
		t.Fatalf("global mode path = %q, want a gogen config dir", got)
	}
}

func TestLoadOrCreateWebTokenCreatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web_token")

	tok, err := loadOrCreateWebToken(path)
	if err != nil {
		t.Fatalf("loadOrCreateWebToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}

	// Persisted with 0600 so other users cannot read the credential.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != tok+"\n" {
		t.Fatalf("state file content = %q, want %q", data, tok+"\n")
	}

	// A second load returns the same token: restart does not rotate it.
	tok2, err := loadOrCreateWebToken(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if tok2 != tok {
		t.Fatal("token rotated across loads, want the persisted value")
	}
}

func TestLoadOrCreateWebTokenReadsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web_token")
	if err := os.WriteFile(path, []byte("existing-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadOrCreateWebToken(path)
	if err != nil {
		t.Fatalf("loadOrCreateWebToken: %v", err)
	}
	if tok != "existing-token" {
		t.Fatalf("token = %q, want the persisted value", tok)
	}
	// The existing file is not rewritten.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing-token\n" {
		t.Fatalf("state file was rewritten: %q", data)
	}
}

func TestLoadOrCreateWebTokenEmptyFileRegenerates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web_token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadOrCreateWebToken(path)
	if err != nil {
		t.Fatalf("loadOrCreateWebToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want a fresh 64-hex-char token", len(tok))
	}
}

func TestLoadOrCreateWebTokenUnreadableState(t *testing.T) {
	// A directory in place of the file is a non-NotExist read error: the
	// caller must learn about it (and fall back to an ephemeral token).
	path := filepath.Join(t.TempDir(), "web_token")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateWebToken(path); err == nil {
		t.Fatal("expected an error for an unreadable state path")
	}
}

func TestWriteWebTokenAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "web_token")
	if err := writeWebToken(path, "tok-1"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeWebToken(path, "tok-2"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "tok-2\n" {
		t.Fatalf("content = %q, want the latest token", data)
	}
	// No temp files left behind.
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".web_token.tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
