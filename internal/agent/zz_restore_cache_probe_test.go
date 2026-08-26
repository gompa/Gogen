package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"gogen/internal/llm"
)

// jsonRoundTripStore mimics the session store's JSON persistence of a
// SessionSnapshot (messages serialize identically in both).
type jsonRoundTripStore struct {
	snap SessionSnapshot
}

func (s *jsonRoundTripStore) Save(id string, snap SessionSnapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	var back SessionSnapshot
	if err := json.Unmarshal(data, &back); err != nil {
		return err
	}
	s.snap = back
	return nil
}

func (s *jsonRoundTripStore) AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error {
	return nil
}

func (s *jsonRoundTripStore) LoadInWorkingDir(workingDir, id string) (SessionSnapshot, error) {
	return s.snap, nil
}

func (s *jsonRoundTripStore) List(workingDir string) ([]SessionInfo, error) {
	return nil, nil
}

func (s *jsonRoundTripStore) LatestID(workingDir string) (string, error) {
	return "", nil
}

func (s *jsonRoundTripStore) Delete(workingDir, id string) error { return nil }

func (s *jsonRoundTripStore) TouchSession(workingDir, id string) error {
	return nil
}

// TestRestoreRoundTripWireBytes probes whether a save/restore round trip
// preserves the LLM view (system prompt + history) exactly. The wire bytes
// are a pure function of llm.Message (ArgsStr + validity), so a deep
// comparison of the view is equivalent to a byte comparison of the request.
func TestRestoreRoundTripWireBytes(t *testing.T) {
	dir := t.TempDir()

	a := NewAgent(llm.NewMockProvider(), NewExecutor(dir), nil)
	defer a.Close()
	a.SetProjectContext("", "guidelines", "", "")

	a.appendMessage(llm.Message{Role: "user", Content: "q1"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "let me check",
		ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file", ArgsStr: `{"path":"a.go"}`}}})
	a.appendMessage(llm.Message{Role: "tool", Content: "file contents", ToolCallID: "call_1"})
	a.appendMessage(llm.Message{Role: "assistant", Content: "done", Reasoning: "thoughts"})

	a.stabilizeToolArgs()
	view1 := a.rebuildView()

	store := &jsonRoundTripStore{}
	a.SessionStore = store
	a.SessionID = "probe-session"
	a.FlushSession()

	snap, err := store.LoadInWorkingDir(dir, "probe-session")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	b := NewAgent(llm.NewMockProvider(), NewExecutor(dir), nil)
	defer b.Close()
	b.SetProjectContext("", "guidelines", "", "")
	b.RestoreSession(snap, "probe-session")

	view2 := b.rebuildView()

	if len(view1) != len(view2) {
		t.Fatalf("view length differs: %d vs %d", len(view1), len(view2))
	}
	for i := range view1 {
		if !reflect.DeepEqual(view1[i], view2[i]) {
			j1, _ := json.Marshal(view1[i])
			j2, _ := json.Marshal(view2[i])
			t.Fatalf("message %d differs after restore:\nbefore: %s\nafter:  %s", i, j1, j2)
		}
	}
	t.Logf("round trip preserved %d view messages", len(view1))
}
