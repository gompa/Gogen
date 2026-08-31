package streamutil

import (
	"sync"
	"testing"
	"time"
)

// argsCall captures one ArgsBatcher send callback invocation.
type argsCall struct {
	index int
	id    string
	name  string
	delta string
}

// argsCollector accumulates ArgsBatcher sends for testing.
type argsCollector struct {
	mu    sync.Mutex
	calls []argsCall
}

func (c *argsCollector) add(index int, id, name, delta string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, argsCall{index: index, id: id, name: name, delta: delta})
}

func (c *argsCollector) all() []argsCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]argsCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestArgsBatcherCoalescesPerIndex pins the core guarantee the WebSocket
// path depends on: many small deltas for one tool call collapse into a
// single send whose text is their exact concatenation (a client that
// appends it sees byte-identical args).
func TestArgsBatcherCoalescesPerIndex(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	// A patch_file diff arriving in fragments.
	for _, frag := range []string{`{"diff":"@@`, ` -old`, ` +new`, `"}`} {
		b.Add(0, "call_1", "patch_file", frag)
	}
	b.Flush()

	calls := col.all()
	if len(calls) != 1 {
		t.Fatalf("got %d sends, want 1 coalesced: %+v", len(calls), calls)
	}
	want := `{"diff":"@@ -old +new"}`
	if calls[0].delta != want {
		t.Fatalf("delta = %q, want %q", calls[0].delta, want)
	}
	if calls[0].index != 0 || calls[0].id != "call_1" || calls[0].name != "patch_file" {
		t.Fatalf("call = %+v, want index 0 / call_1 / patch_file", calls[0])
	}
}

// TestArgsBatcherFlushesInIndexOrder: parallel tool calls (multiple indices
// pending at once) flush one send per index in ascending index order, so the
// client never sees index 1's args before index 0's.
func TestArgsBatcherFlushesInIndexOrder(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	// Interleaved, out of order: the flush must still be index-ordered.
	b.Add(2, "call_c", "read_file", "c")
	b.Add(0, "call_a", "read_file", "a")
	b.Add(1, "call_b", "read_file", "b")
	b.Add(0, "call_a", "read_file", "a2")
	b.Flush()

	calls := col.all()
	if len(calls) != 3 {
		t.Fatalf("got %d sends, want 3 (one per index): %+v", len(calls), calls)
	}
	want := []argsCall{
		{index: 0, id: "call_a", name: "read_file", delta: "aa2"},
		{index: 1, id: "call_b", name: "read_file", delta: "b"},
		{index: 2, id: "call_c", name: "read_file", delta: "c"},
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], w)
		}
	}
}

// TestArgsBatcherEmptyDeltaIgnored: empty deltas must not create a pending
// entry (an empty send would carry no args and could open a bogus card).
func TestArgsBatcherEmptyDeltaIgnored(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.Add(0, "call_1", "read_file", "")
	b.Flush()
	if calls := col.all(); len(calls) != 0 {
		t.Fatalf("empty delta produced sends: %+v", calls)
	}
}

// TestArgsBatcherCloseDropsPending: after Close, pending deltas are dropped
// and later Add calls are no-ops (the round boundary discards stale args).
func TestArgsBatcherCloseDropsPending(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)

	b.Add(0, "call_1", "read_file", "stale")
	b.Close()
	b.Flush()
	if calls := col.all(); len(calls) != 0 {
		t.Fatalf("Close should drop pending, got %+v", calls)
	}
	b.Add(0, "call_1", "read_file", "after-close")
	b.Flush()
	if calls := col.all(); len(calls) != 0 {
		t.Fatalf("Add after Close must be a no-op, got %+v", calls)
	}
}

// TestArgsBatcherResetReusesBatcher: Reset re-arms a closed batcher for the
// next stream round (the server calls Close+Reset on OnStart/OnRoundStart).
func TestArgsBatcherResetReusesBatcher(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.Add(0, "call_1", "read_file", "round-1")
	b.Close()
	b.Reset()
	b.Add(0, "call_2", "write_file", "round-2")
	b.Flush()

	calls := col.all()
	if len(calls) != 1 || calls[0].delta != "round-2" {
		t.Fatalf("got %+v, want exactly [round-2]", calls)
	}
	if calls[0].id != "call_2" {
		t.Fatalf("id = %q, want call_2 (Reset must not carry stale id)", calls[0].id)
	}
}

// TestArgsBatcherTimerFlush: the interval timer flushes without an explicit
// Flush call — the mechanism that bounds frame rate on the web path.
func TestArgsBatcherTimerFlush(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 10*time.Millisecond)
	defer b.Close()

	b.Add(0, "call_1", "read_file", "auto")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if calls := col.all(); len(calls) > 0 {
			if calls[0].delta != "auto" {
				t.Fatalf("timer flush delta = %q, want %q", calls[0].delta, "auto")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timer never flushed pending args")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestArgsBatcherConcurrentAddFlush exercises the race the -race detector
// guards: producers on several goroutines plus concurrent explicit flushes.
func TestArgsBatcherConcurrentAddFlush(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 5*time.Millisecond)
	defer b.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Add(i%3, "call", "read_file", "x")
		}(i)
		if i%10 == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.Flush()
			}()
		}
	}
	wg.Wait()
	b.Flush()

	// Every delta must be delivered exactly once, concatenated across
	// however many flushes happened.
	total := 0
	for _, c := range col.all() {
		total += len(c.delta)
	}
	if total != 50 {
		t.Fatalf("delivered %d bytes across %d sends, want 50", total, len(col.all()))
	}
}

// TestArgsBatcherLastNonEmptyIDAndNameWins: providers repeat id/name on
// every fragment; the flush reports the last non-empty values.
func TestArgsBatcherLastNonEmptyIDAndNameWins(t *testing.T) {
	var col argsCollector
	b := NewArgsBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.Add(0, "call_1", "patch_file", "a")
	b.Add(0, "", "", "b")
	b.Flush()

	calls := col.all()
	if len(calls) != 1 {
		t.Fatalf("got %+v, want 1 send", calls)
	}
	if calls[0].id != "call_1" || calls[0].name != "patch_file" {
		t.Fatalf("call = %+v, want id/name preserved from the first fragment", calls[0])
	}
	if calls[0].delta != "ab" {
		t.Fatalf("delta = %q, want %q", calls[0].delta, "ab")
	}
}
