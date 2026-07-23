package mcp

import (
	"bytes"
	"testing"

	"gogen/internal/config"
)

func TestBytesTrimSpace(t *testing.T) {
	got := string(bytes.TrimSpace([]byte("  hello  \n")))
	if got != "hello" {
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

func TestValidServersDropsIncomplete(t *testing.T) {
	in := []config.MCPServerConfig{
		{Name: "", Command: "npx"},
		{Name: "ok", Command: ""},
		{Name: "fetch", Command: "npx"},
	}
	got := ValidServers(in)
	if len(got) != 1 || got[0].Name != "fetch" {
		t.Fatalf("got %#v", got)
	}
	if ValidServers(nil) != nil {
		t.Fatal("nil in → nil out")
	}
}
