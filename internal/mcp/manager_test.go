package mcp

import "testing"

func TestBytesTrimSpace(t *testing.T) {
	got := bytesTrimSpace([]byte("  hello  \n"))
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestExternalToolName(t *testing.T) {
	got := ExternalToolName("Fetch Server", "get-url")
	if got != "mcp_fetch_server_get_url" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitize(t *testing.T) {
	if sanitize("") != "x" {
		t.Fatal("empty should become x")
	}
}
