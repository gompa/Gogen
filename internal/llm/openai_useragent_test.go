package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gogen/internal/buildinfo"
)

// TestBuildProfileClientsSendBuildUserAgent verifies that the clients built
// by buildProfileClients (the only production construction path) send the
// GoGen build User-Agent instead of the SDK default ("OpenAI/Go <version>")
// on both request kinds: the catalog client (ListModels → GET /models) and
// the stream client (GenerateResponseStream → POST /chat/completions).
func TestBuildProfileClientsSendBuildUserAgent(t *testing.T) {
	const sse = `data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}

`
	var mu sync.Mutex
	uas := make(map[string]string) // request path -> User-Agent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uas[r.URL.Path] = r.Header.Get("User-Agent")
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "test-model", "object": "model", "context_length": 8192},
				},
			})
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse))
		}
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithProfiles(
		[]ProviderProfile{{Name: "default", BaseURL: srv.URL + "/v1", APIKey: "test", Model: "test-model"}},
		"test-model", t.TempDir(), nil)

	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if _, err := p.GenerateResponseStream(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, nil, nil,
	); err != nil {
		t.Fatalf("GenerateResponseStream: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string]string{
		"/v1/models":           buildinfo.UserAgent(),
		"/v1/chat/completions": buildinfo.UserAgent(),
	} {
		got, ok := uas[path]
		if !ok {
			t.Fatalf("no request recorded for %s (requests: %v)", path, uas)
		}
		if got != want {
			t.Errorf("%s User-Agent = %q, want %q", path, got, want)
		}
	}
}
