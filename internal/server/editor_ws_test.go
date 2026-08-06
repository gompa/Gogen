package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// dialEditor opens a websocket to the /ws/editor endpoint of a running test
// server (the real mux path, so routing/upgrade/auth are exercised).
func dialEditor(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/editor", nil)
	if err != nil {
		t.Fatalf("dial /ws/editor: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestWSEditorEndpointFSRoundTrip exercises the editor transport split: the
// editor socket serves fs_list/fs_write with requestId correlation, ignores
// chat-shaped messages, and keeps working afterwards.
func TestWSEditorEndpointFSRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llm.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := NewServer(a, &config.Config{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws/editor":
			s.HandleWSEditor(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	editor := dialEditor(t, srv)

	// fs_list over the editor socket.
	if err := editor.WriteJSON(WSMessage{Type: "fs_list", RequestID: "ed-1"}); err != nil {
		t.Fatalf("send fs_list: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var listResp WSMessage
	if err := editor.ReadJSON(&listResp); err != nil {
		t.Fatalf("read fs_list_result: %v", err)
	}
	if listResp.Type != "fs_list_result" || listResp.RequestID != "ed-1" {
		t.Fatalf("fs_list response = %+v, want fs_list_result/ed-1", listResp)
	}
	if !listResp.Success {
		t.Fatalf("fs_list failed: %s", listResp.Error)
	}

	// fs_write over the editor socket.
	if err := editor.WriteJSON(WSMessage{Type: "fs_write", RequestID: "ed-2", Path: "hello.txt", Content: "hello editor"}); err != nil {
		t.Fatalf("send fs_write: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var writeResp WSMessage
	if err := editor.ReadJSON(&writeResp); err != nil {
		t.Fatalf("read fs_write_result: %v", err)
	}
	if writeResp.Type != "fs_write_result" || writeResp.RequestID != "ed-2" || !writeResp.Success {
		t.Fatalf("fs_write response = %+v, want successful fs_write_result/ed-2", writeResp)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(got) != "hello editor" {
		t.Fatalf("file content = %q, %v; want %q", got, err, "hello editor")
	}

	// A non-editor message (chat) sent over the editor socket must be ignored:
	// the server should not respond with anything chat-shaped. We assert the
	// socket stays usable by issuing a real editor request afterwards.
	if err := editor.WriteJSON(WSMessage{Type: "list_sessions"}); err != nil {
		t.Fatalf("send list_sessions: %v", err)
	}
	if err := editor.WriteJSON(WSMessage{Type: "fs_list", RequestID: "ed-3"}); err != nil {
		t.Fatalf("send fs_list #2: %v", err)
	}
	_ = editor.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp3 WSMessage
	if err := editor.ReadJSON(&resp3); err != nil {
		t.Fatalf("read fs_list_result #2: %v", err)
	}
	if resp3.Type != "fs_list_result" || resp3.RequestID != "ed-3" {
		t.Fatalf("fs_list #2 response = %+v, want fs_list_result/ed-3", resp3)
	}
}
