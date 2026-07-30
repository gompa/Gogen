package streamutil

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordedCall captures one send callback invocation.
type recordedCall struct {
	think bool
	text  string
}

// collector accumulates send callbacks for testing.
type collector struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (c *collector) add(think bool, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, recordedCall{think: think, text: text})
}

func (c *collector) all() []recordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordedCall, len(c.calls))
	copy(out, c.calls)
	return out
}

func TestStreamAndFlush(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.StreamToken("hello")
	b.StreamToken(" ")
	b.StreamToken("world")
	b.Flush()

	calls := col.all()
	if len(calls) != 1 {
		t.Fatalf("expected 1 coalesced call, got %d: %+v", len(calls), calls)
	}
	if calls[0].think {
		t.Errorf("expected think=false, got true")
	}
	if calls[0].text != "hello world" {
		t.Errorf("expected text=%q, got %q", "hello world", calls[0].text)
	}
}

func TestThinkTokenCoalescing(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.ThinkToken("let me")
	b.ThinkToken(" think")
	b.Flush()

	calls := col.all()
	if len(calls) != 1 {
		t.Fatalf("expected 1 coalesced call, got %d", len(calls))
	}
	if !calls[0].think {
		t.Errorf("expected think=true, got false")
	}
	if calls[0].text != "let me think" {
		t.Errorf("expected text=%q, got %q", "let me think", calls[0].text)
	}
}

func TestAlternatingTokensCreateSeparateSegments(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.StreamToken("Hello")
	b.ThinkToken(" hmm ")
	b.StreamToken(" world")
	b.Flush()

	calls := col.all()
	if len(calls) != 3 {
		t.Fatalf("expected 3 separate segments, got %d: %+v", len(calls), calls)
	}
	// First: content
	if calls[0].think || calls[0].text != "Hello" {
		t.Errorf("segment 0: expected think=false text=%q, got think=%v text=%q", "Hello", calls[0].think, calls[0].text)
	}
	// Second: thinking
	if !calls[1].think || calls[1].text != " hmm " {
		t.Errorf("segment 1: expected think=true text=%q, got think=%v text=%q", " hmm ", calls[1].think, calls[1].text)
	}
	// Third: content again
	if calls[2].think || calls[2].text != " world" {
		t.Errorf("segment 2: expected think=false text=%q, got think=%v text=%q", " world", calls[2].think, calls[2].text)
	}
}

func TestEmptyTokensIgnored(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	b.StreamToken("")
	b.ThinkToken("")
	b.Flush()

	calls := col.all()
	if len(calls) != 0 {
		t.Errorf("expected 0 calls for empty tokens, got %d", len(calls))
	}
}

func TestCloseDiscards(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)

	b.StreamToken("keep")
	b.Close()
	b.Flush() // should be no-op after Close

	calls := col.all()
	if len(calls) != 0 {
		t.Errorf("expected 0 calls after Close, got %d", len(calls))
	}
}

func TestFlushAfterCloseNoop(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)

	b.StreamToken("before")
	b.Flush()
	b.Close()
	b.StreamToken("after")
	b.Flush()

	calls := col.all()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call (before close), got %d", len(calls))
	}
	if calls[0].text != "before" {
		t.Errorf("expected text=%q, got %q", "before", calls[0].text)
	}
}

func TestResetReuses(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)

	// First round
	b.StreamToken("round1")
	b.Flush()
	b.Close()

	// Reset and reuse
	b.Reset()
	b.StreamToken("round2")
	b.Flush()
	b.Close()

	calls := col.all()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls across rounds, got %d", len(calls))
	}
	if calls[0].text != "round1" {
		t.Errorf("call 0: expected %q, got %q", "round1", calls[0].text)
	}
	if calls[1].text != "round2" {
		t.Errorf("call 1: expected %q, got %q", "round2", calls[1].text)
	}
}

func TestConcurrentAccess(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 50*time.Millisecond)
	defer b.Close()

	var wg sync.WaitGroup
	n := 100

	// Concurrent producers
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				b.StreamToken("a")
			} else {
				b.ThinkToken("b")
			}
		}(i)
	}

	wg.Wait()
	b.Flush()

	calls := col.all()
	if len(calls) == 0 {
		t.Fatal("expected some calls after concurrent access, got 0")
	}
}

func TestTimerAutoFlush(t *testing.T) {
	var (
		col    collector
		mu     sync.Mutex
		called bool
	)
	send := func(think bool, text string) {
		col.add(think, text)
		mu.Lock()
		called = true
		mu.Unlock()
	}

	b := NewTokenBatcher(send, 10*time.Millisecond)
	defer b.Close()

	b.StreamToken("auto")

	// Wait for timer to fire
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	was := called
	mu.Unlock()

	if !was {
		t.Fatal("expected timer to auto-flush, but Flush was never called")
	}

	calls := col.all()
	if len(calls) != 1 || calls[0].text != "auto" {
		t.Fatalf("expected 1 auto-flushed call with text=%q, got %+v", "auto", calls)
	}
}

func TestFlushDuringStreamRaceSafe(t *testing.T) {
	var col collector
	b := NewTokenBatcher(col.add, 100*time.Millisecond)
	defer b.Close()

	var wg sync.WaitGroup
	var saw atomic.Int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.StreamToken("x")
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			b.Flush()
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
	b.Flush()

	calls := col.all()
	for _, c := range calls {
		saw.Add(int64(len(c.text)))
	}
	total := saw.Load()
	if total == 0 {
		t.Fatal("expected some tokens delivered under concurrent flush")
	}
	// We expect at most 50 'x' chars (some may be lost if Close races with Flush,
	// but flush counts the total from all segments)
	if total > 50 {
		t.Fatalf("expected at most 50 chars, got %d", total)
	}
}
