package agent

// Regression test for the quit-races-running-turn bug: ShutdownSessions
// flushes every session while a still-running turn may be flushing too (the
// 2s stream drain can time out), so two doPersist executions ran
// concurrently on one agent. They interleaved their message snapshots,
// lastSavedMsgCount reads, and store writes, leaving a torn (snapshot,
// delta) pair on disk that LoadInWorkingDir then dropped or double-merged —
// the session's history appeared bloated or lost after quit. doPersist is
// now serialized by persistMu, so concurrent flushes must always leave the
// persisted state exactly equal to the in-memory conversation.

import (
	"fmt"
	"sync"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type fakeDeltaState struct {
	base int
	msgs []llm.Message
}

type fakePersistStore struct {
	mu     sync.Mutex
	snaps  map[string][]llm.Message
	deltas map[string]fakeDeltaState
	counts map[string]int // index message count (AppendMessages' totalMsgCount)
}

func newFakePersistStore() *fakePersistStore {
	return &fakePersistStore{
		snaps:  map[string][]llm.Message{},
		deltas: map[string]fakeDeltaState{},
		counts: map[string]int{},
	}
}

func (f *fakePersistStore) Save(id string, snap SessionSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[id] = append([]llm.Message(nil), snap.Messages...)
	delete(f.deltas, id)
	f.counts[id] = len(snap.Messages)
	return nil
}

func (f *fakePersistStore) AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := totalMsgCount - len(snap.Messages)
	if base < 0 {
		base = 0
	}
	f.deltas[id] = fakeDeltaState{base: base, msgs: append([]llm.Message(nil), snap.Messages...)}
	f.counts[id] = totalMsgCount
	return nil
}

// LoadInWorkingDir mirrors session.Store.LoadInWorkingDir's merge rules.
func (f *fakePersistStore) LoadInWorkingDir(_ string, id string) (SessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snaps[id]
	if !ok {
		return SessionSnapshot{}, fmt.Errorf("session not found: %s", id)
	}
	delta, ok := f.deltas[id]
	if !ok {
		return SessionSnapshot{Messages: append([]llm.Message(nil), snap...)}, nil
	}
	merge := false
	switch {
	case len(snap) == delta.base:
		merge = true
	case len(snap) >= delta.base+len(delta.msgs):
		// snapshot already contains the delta's messages: stale delta.
		delete(f.deltas, id)
	default:
		// snapshot shorter than the delta's base: truncated, delta dropped.
		delete(f.deltas, id)
	}
	out := append([]llm.Message(nil), snap...)
	if merge {
		out = append(out, delta.msgs...)
	}
	return SessionSnapshot{Messages: out}, nil
}

func (f *fakePersistStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (f *fakePersistStore) LatestID(string) (string, error)    { return "", nil }
func (f *fakePersistStore) Delete(string, string) error        { return nil }
func (f *fakePersistStore) TouchSession(string, string) error  { return nil }

// TestConcurrentFlushKeepsStoreConsistent hammers one agent with concurrent
// append+flush pairs (the shutdown-flush vs turn-flush pattern) and asserts
// the store loads back exactly the in-memory conversation: no lost messages,
// no duplicates. Run with -race to also prove the counters are synchronized.
func TestConcurrentFlushKeepsStoreConsistent(t *testing.T) {
	store := newFakePersistStore()
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(prov, exec, ctxMgr)
	a.SessionStore = store
	a.SessionID = "sess-race"
	// Seed a full snapshot so the incremental-delta window is active.
	a.appendMessage(llm.Message{Role: "user", Content: "seed"})
	a.FlushSession()

	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				a.appendMessage(llm.Message{Role: "user", Content: fmt.Sprintf("w%d-%d", i, j)})
				a.FlushSession()
			}
		}(i)
	}
	wg.Wait()

	want := a.MessageCount()
	snap, err := store.LoadInWorkingDir("", a.SessionID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snap.Messages) != want {
		t.Fatalf("persisted %d messages, in-memory %d (torn snapshot/delta lost %d)",
			len(snap.Messages), want, want-len(snap.Messages))
	}
	seen := map[string]bool{}
	for _, m := range snap.Messages {
		if seen[m.Content] {
			t.Fatalf("duplicate message %q in persisted history", m.Content)
		}
		seen[m.Content] = true
	}
	if len(seen) != want {
		t.Fatalf("unique persisted messages = %d, want %d", len(seen), want)
	}
}
