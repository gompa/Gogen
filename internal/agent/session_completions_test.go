package agent

import (
	"testing"

	"gogen/internal/llm"
)

func TestResumeArgCompletions(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"abc123": {Messages: []llm.Message{{Role: "user", Content: "one"}}},
		"abc999": {Messages: []llm.Message{{Role: "user", Content: "two"}}},
	}}
	store.order = []string{"abc123", "abc999"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store}

	got := a.ResumeArgCompletions("")
	if len(got) < 3 {
		t.Fatalf("expected keywords and ids, got %v", got)
	}

	got = a.ResumeArgCompletions("abc")
	if len(got) != 2 {
		t.Fatalf("expected 2 id matches, got %v", got)
	}

	got = a.ResumeArgCompletions("del abc")
	if len(got) != 2 || got[0] != "del abc123" || got[1] != "del abc999" {
		t.Fatalf("expected del-prefixed id matches, got %v", got)
	}

	got = a.ResumeArgCompletions("del")
	if len(got) != 2 || got[0] != "del abc123" {
		t.Fatalf("expected del-prefixed ids when arg is del, got %v", got)
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	if got := LongestCommonPrefix([]string{"abc123", "abc999"}); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
