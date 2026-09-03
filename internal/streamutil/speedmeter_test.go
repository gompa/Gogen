package streamutil

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// statCollector gathers SpeedMeter emissions from the ticker goroutine.
type statCollector struct {
	mu   sync.Mutex
	vals []float64
}

func (c *statCollector) add(v float64) {
	c.mu.Lock()
	c.vals = append(c.vals, v)
	c.mu.Unlock()
}

func (c *statCollector) last() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.vals) == 0 {
		return 0
	}
	return c.vals[len(c.vals)-1]
}

func (c *statCollector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.vals)
}

// TestSpeedMeterReportsSteadyRate feeds at a constant ~1000 tok/s (25
// heuristic tokens every 25 ms) and expects the smoothed rate to land in
// the neighborhood of the feed rate.
func TestSpeedMeterReportsSteadyRate(t *testing.T) {
	m := NewSpeedMeter(10 * time.Millisecond)
	var c statCollector
	m.Start(c.add)
	defer m.Stop()

	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(25 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				m.Feed(strings.Repeat("a", 100)) // 25 tokens
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	var got float64
	for time.Now().Before(deadline) {
		got = c.last()
		if got >= 500 && got <= 1050 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	if got < 500 || got > 1050 {
		t.Fatalf("rate = %.0f tok/s, want within [500, 1050] of the 1000 tok/s feed", got)
	}
}

// TestSpeedMeterSilentBeforeFeed pins that the meter emits nothing until
// the first token lands: a round's thinking phase must not display a
// "0 tok/s" rate.
func TestSpeedMeterSilentBeforeFeed(t *testing.T) {
	m := NewSpeedMeter(10 * time.Millisecond)
	var c statCollector
	m.Start(c.add)
	time.Sleep(100 * time.Millisecond)
	m.Stop()
	if n := c.len(); n != 0 {
		t.Fatalf("emissions before any Feed = %d, want 0", n)
	}
}

// TestSpeedMeterDecaysToSilenceOnStall feeds a burst, then goes quiet:
// the EMA must decay past the emission floor and the meter must stop
// emitting (a stalled stream should not spam "0 tok/s" frames).
func TestSpeedMeterDecaysToSilenceOnStall(t *testing.T) {
	m := NewSpeedMeter(10 * time.Millisecond)
	var c statCollector
	m.Start(c.add)
	defer m.Stop()

	for i := 0; i < 20; i++ {
		m.Feed(strings.Repeat("a", 400)) // 100 tokens
		time.Sleep(10 * time.Millisecond)
	}
	if c.len() == 0 {
		t.Fatal("no emissions during the burst")
	}

	// Let the EMA decay past the 0.5 floor, then require the emission
	// count to be stable (quiet).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1 := c.len()
		time.Sleep(200 * time.Millisecond)
		if c.len() == n1 {
			return // stable: the meter is quiet
		}
	}
	t.Fatalf("meter still emitting long after the stall (count = %d)", c.len())
}

// TestSpeedMeterStopHaltsEmissions pins the Stop contract: after Stop
// returns no further emission may run, and Stop is idempotent.
func TestSpeedMeterStopHaltsEmissions(t *testing.T) {
	m := NewSpeedMeter(5 * time.Millisecond)
	var c statCollector
	m.Start(c.add)
	for i := 0; i < 10; i++ {
		m.Feed(strings.Repeat("a", 400))
		time.Sleep(5 * time.Millisecond)
	}
	m.Stop()
	n := c.len()
	time.Sleep(50 * time.Millisecond)
	if got := c.len(); got != n {
		t.Fatalf("emissions continued after Stop: %d -> %d", n, got)
	}
	m.Stop() // idempotent
}

// TestSpeedMeterRestartResets pins that a re-armed meter (round
// boundary) does not inherit the previous round's rate: after Stop +
// Start, emissions are silent until the new round is fed.
func TestSpeedMeterRestartResets(t *testing.T) {
	m := NewSpeedMeter(10 * time.Millisecond)
	var c statCollector
	m.Start(c.add)
	for i := 0; i < 10; i++ {
		m.Feed(strings.Repeat("a", 400))
		time.Sleep(10 * time.Millisecond)
	}
	if c.len() == 0 {
		t.Fatal("no emissions during the first round's feed")
	}
	m.Stop()
	m.Start(c.add)
	n := c.len()
	time.Sleep(100 * time.Millisecond)
	if got := c.len(); got != n {
		t.Fatalf("re-armed meter emitted before any new Feed: %d -> %d", n, got)
	}
	m.Stop()
}

// TestBuildStreamHandlersSpeedLifecycle pins the builder's meter wiring:
// all three delta kinds (content, thinking, tool args) feed the rate,
// and the round-end Stop leaves the round-end event last — no stats
// event may overtake OnStreamEnd.
func TestBuildStreamHandlersSpeedLifecycle(t *testing.T) {
	rec := &recordingSink{}
	h := BuildStreamHandlers(rec, HandlersConfig{
		Tokens: NewTokenBatcher(func(bool, string) {}, time.Hour),
		Args:   NewArgsBatcher(func(int, string, string, string) {}, time.Hour),
		Speed:  NewSpeedMeter(5 * time.Millisecond),
	})

	h.OnStart()
	for i := 0; i < 100; i++ {
		h.OnToken(strings.Repeat("a", 100))
		h.OnThinkingToken(strings.Repeat("b", 100))
		h.OnToolCallArgsDelta(0, "id", "patch_file", strings.Repeat("c", 100))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !rec.saw("OnStreamStats") {
		time.Sleep(5 * time.Millisecond)
	}
	if !rec.saw("OnStreamStats") {
		t.Fatal("no OnStreamStats dispatched despite a high-rate feed")
	}

	h.OnStreamEnd()
	// Give a (buggy) in-flight emission time to surface.
	time.Sleep(20 * time.Millisecond)
	if !rec.lastIs("OnStreamEnd") {
		t.Fatal("last dispatch is not OnStreamEnd (stats must precede round end)")
	}
}
