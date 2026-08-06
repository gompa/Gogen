package agent

// Regression test for the dirty-flag lost-update bug in doPersist. Two
// flushes overlap — the turn's flush and the shutdown flush (persistMu was
// added to serialize them, but the Load-at-start + Store(false)-at-end
// protocol still lost data): the earlier writer's trailing Store(false)
// cleared the dirty flag the turn had set, so the later (potentially the
// LAST flush before process exit) saw dirty==false and returned without
// writing. doPersist now consumes the flag up front with Swap, so the last
// writer always persists the full in-memory conversation.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

type blockingPersistStore struct {
	mu          sync.Mutex
	snaps       map[string][]llm.Message
	deltas      map[string][]llm.Message
	saveStarted chan struct{}
	release     chan struct{}
	blocking    bool
}

func newBlockingPersistStore() *blockingPersistStore {
	return &blockingPersistStore{
		snaps:       map[string][]llm.Message{},
		deltas:      map[string][]llm.Message{},
		saveStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (b *blockingPersistStore) Save(id string, snap SessionSnapshot) error {
	if b.blocking {
		select {
		case <-b.saveStarted:
		default:
			close(b.saveStarted)
		}
		<-b.release // simulate a slow disk write to widen the race window
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snaps[id] = append([]llm.Message(nil), snap.Messages...)
	return nil
}

func (b *blockingPersistStore) AppendMessages(id string, snap SessionSnapshot, totalMsgCount int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deltas[id] = append([]llm.Message(nil), snap.Messages...)
	return nil
}
func (b *blockingPersistStore) LoadInWorkingDir(_ string, id string) (SessionSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	snap, ok := b.snaps[id]
	if !ok {
		return SessionSnapshot{}, fmt.Errorf("session not found: %s", id)
	}
	out := append([]llm.Message(nil), snap...)
	out = append(out, b.deltas[id]...)
	return SessionSnapshot{Messages: out}, nil
}
func (b *blockingPersistStore) List(string) ([]SessionInfo, error) { return nil, nil }
func (b *blockingPersistStore) LatestID(string) (string, error)    { return "", nil }
func (b *blockingPersistStore) Delete(string, string) error        { return nil }
func (b *blockingPersistStore) TouchSession(string, string) error  { return nil }

// TestDirtyFlagLostUpdateOnFinalFlush reproduces: flush A (from a turn) is
// mid-write when the turn appends m2 (marking the session dirty) and flush B
// (the shutdown sweep's non-forcing FlushPending) blocks on persistMu. A
// finishes and clears the flag; B sees dirty==false and returns WITHOUT
// writing — the messages appended after A's snapshot are lost even though B
// was the final flush before process exit. The fix (Swap the flag up front,
// plus the sweep using FlushPending) must keep B writing m2.
func TestDirtyFlagLostUpdateOnFinalFlush(t *testing.T) {
	store := newBlockingPersistStore()
	prov := llm.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 1000})
	a := NewAgent(prov, exec, ctxMgr)
	a.SessionStore = store
	a.SessionID = "sess-final"
	a.appendMessage(llm.Message{Role: "user", Content: "seed"})
	a.FlushSession()
	store.blocking = true // subsequent saves block until release
	// Force A's doPersist onto the full-snapshot path (which calls Save, the
	// blocking store method) instead of the incremental delta path.
	a.resetSaveTracking()

	var wg sync.WaitGroup
	// Turn flush: appends m1 and starts persisting (blocks inside Save).
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.appendMessage(llm.Message{Role: "user", Content: "m1"})
		a.FlushSession() // doPersist A — blocks in store.Save
	}()

	<-store.saveStarted // A is mid-write, holding persistMu

	// The turn appends m2 and marks the session dirty (persistSession sets
	// the flag before any debounce check). The shutdown sweep uses the
	// non-forcing FlushPending, which must still write m2: doPersist
	// consumes the flag at its start, so the last flush always persists the
	// full in-memory conversation. The flush blocks on persistMu behind A.
	a.appendMessage(llm.Message{Role: "user", Content: "m2"})
	a.sessionDirty.Store(true)
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.FlushPending() // doPersist B — must write m2
	}()

	// Give B time to reach the persistMu wait, then let A finish.
	time.Sleep(100 * time.Millisecond)
	close(store.release) // let A finish
	wg.Wait()

	snap, err := store.LoadInWorkingDir("", a.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"seed", "m1", "m2"}
	if len(snap.Messages) != len(want) {
		t.Fatalf("persisted %d messages %v, want %d (%v): m2 was lost",
			len(snap.Messages), msgsContents(snap.Messages), len(want), want)
	}
	for i, w := range want {
		if snap.Messages[i].Content != w {
			t.Fatalf("message %d = %q, want %q", i, snap.Messages[i].Content, w)
		}
	}
}

func msgsContents(msgs []llm.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}
