package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type stubSessionStore struct {
	sessions map[string]SessionSnapshot
	order    []string
	saveErr  error
}

func (s *stubSessionStore) Save(id string, snap SessionSnapshot) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.sessions == nil {
		s.sessions = make(map[string]SessionSnapshot)
	}
	if _, ok := s.sessions[id]; !ok {
		s.order = append(s.order, id)
	}
	s.sessions[id] = snap
	return nil
}

func (s *stubSessionStore) LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error) {
	snap, ok := s.sessions[id]
	if !ok {
		return SessionSnapshot{}, errNotFound
	}
	return snap, nil
}

func (s *stubSessionStore) List(workingDir string) ([]SessionInfo, error) {
	var out []SessionInfo
	for _, id := range s.order {
		snap := s.sessions[id]
		out = append(out, SessionInfo{
			ID:           id,
			MessageCount: len(snap.Messages),
			Label:        llm.SessionLabel(snap.Messages, llm.DefaultSessionLabelMaxLen),
		})
	}
	return out, nil
}

func (s *stubSessionStore) LatestID(workingDir string) (string, error) {
	if len(s.order) == 0 {
		return "", errNotFound
	}
	return s.order[len(s.order)-1], nil
}

func (s *stubSessionStore) Delete(workingDir, id string) error {
	if _, ok := s.sessions[id]; !ok {
		return errNotFound
	}
	delete(s.sessions, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

var errNotFound = errString("not found")

func TestHandleSessionCommandNew(t *testing.T) {
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "old-session",
		Messages:     []llm.Message{{Role: "user", Content: "hello"}},
	}
	result, handled, err := a.HandleSessionCommand(context.Background(), "/new", "new-session")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionClearChat {
		t.Fatalf("action=%q", result.Action)
	}
	if a.SessionID != "new-session" || len(a.Messages) != 0 {
		t.Fatalf("session=%s messages=%d", a.SessionID, len(a.Messages))
	}
	if _, ok := store.sessions["old-session"]; !ok {
		t.Fatal("old session not saved")
	}
}

func TestHandleSessionCommandResumeList(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"s1": {Messages: []llm.Message{{Role: "user", Content: "first task here"}}},
	}}
	store.order = []string{"s1"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "s1"}

	result, handled, err := a.HandleSessionCommand(context.Background(), "/resume", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(result.Output, "Saved sessions:") {
		t.Fatalf("output=%q", result.Output)
	}
	if !strings.Contains(result.Output, "← current") {
		t.Fatalf("expected current marker: %q", result.Output)
	}
	if !strings.Contains(result.Output, "first task here") {
		t.Fatalf("expected label: %q", result.Output)
	}
}

func TestHandleSessionCommandResumeLatest(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"current": {Messages: []llm.Message{{Role: "user", Content: "current"}}},
		"older":   {Messages: []llm.Message{{Role: "user", Content: "older task"}}},
	}}
	store.order = []string{"older", "current"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "current", Messages: []llm.Message{{Role: "user", Content: "current"}}}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume latest", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if a.SessionID != "older" {
		t.Fatalf("got session %s", a.SessionID)
	}
	if !strings.Contains(result.Output, "older task") {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestHandleSessionCommandResumeByID(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"abc": {Messages: []llm.Message{{Role: "user", Content: "restore me"}}, Mode: "plan"},
	}}
	store.order = []string{"abc"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "other"}

	_, handled, err := a.HandleSessionCommand(context.Background(), "resume abc", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if a.SessionID != "abc" || a.Mode != ModePlan {
		t.Fatalf("session=%s mode=%s", a.SessionID, a.Mode)
	}
}

func TestResumeSessionShowsContextUsage(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"abc": {Messages: []llm.Message{
			{Role: "user", Content: strings.Repeat("word ", 500)},
			{Role: "assistant", Content: strings.Repeat("reply ", 200)},
		}},
	}}
	store.order = []string{"abc"}
	ctxMgr := contextmgr.NewManager(&statsStubProvider{limit: 8000}, contextmgr.Settings{ContextLimit: 8000})
	a := &Agent{
		Provider:     &statsStubProvider{limit: 8000},
		Context:      ctxMgr,
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "other",
	}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume abc", "")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(result.Output, "context:") {
		t.Fatalf("expected context line in output, got %q", result.Output)
	}
}

func TestHandleSessionCommandDelete(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"keep": {Messages: []llm.Message{{Role: "user", Content: "stay"}}},
		"gone": {Messages: []llm.Message{{Role: "user", Content: "bye"}}},
	}}
	store.order = []string{"keep", "gone"}
	a := &Agent{WorkingDir: "/tmp", SessionStore: store, SessionID: "other"}

	result, handled, err := a.HandleSessionCommand(context.Background(), "resume del gone", "new-one")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionNone {
		t.Fatalf("action=%q", result.Action)
	}
	if _, ok := store.sessions["gone"]; ok {
		t.Fatal("session file should be deleted")
	}
	if !strings.Contains(result.Output, "Deleted session gone") {
		t.Fatalf("output=%q", result.Output)
	}
}

func TestHandleSessionCommandDeleteCurrent(t *testing.T) {
	store := &stubSessionStore{sessions: map[string]SessionSnapshot{
		"current": {Messages: []llm.Message{{Role: "user", Content: "active"}}},
	}}
	store.order = []string{"current"}
	a := &Agent{Provider: &statsStubProvider{}, WorkingDir: "/tmp", SessionStore: store, SessionID: "current", Messages: []llm.Message{{Role: "user", Content: "active"}}}

	result, handled, err := a.HandleSessionCommand(context.Background(), "/resume del current", "fresh-id")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if result.Action != SessionActionClearChat {
		t.Fatalf("action=%q", result.Action)
	}
	if a.SessionID != "fresh-id" || len(a.Messages) != 0 {
		t.Fatalf("session=%s messages=%d", a.SessionID, len(a.Messages))
	}
}

func TestPersistSessionRecordsError(t *testing.T) {
	store := &stubSessionStore{saveErr: errString("disk full")}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   "/tmp",
		SessionStore: store,
		SessionID:    "s1",
		Messages:     []llm.Message{{Role: "user", Content: "hi"}},
	}
	a.persistSession()
	err := a.ConsumePersistError()
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected persist error, got %v", err)
	}
	if a.ConsumePersistError() != nil {
		t.Fatal("expected consume to clear error")
	}

	store.saveErr = nil
	a.persistSession()
	if a.ConsumePersistError() != nil {
		t.Fatal("expected successful save to clear persist error")
	}
}
