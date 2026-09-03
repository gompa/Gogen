package streamutil

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
)

// StatsInterval is the shared emission cadence of SpeedMeter: both hosts
// (TUI, WebSocket) create their meter with it, so the displayed rate
// updates at the same rate on every frontend.
const StatsInterval = 250 * time.Millisecond

// speedSmoothTau is the exponential-moving-average time constant of the
// reported rate. At the 250 ms emission cadence each sample moves ~63%
// toward the instantaneous rate: responsive enough that a real speed
// change shows up within a couple of emissions, smooth enough to absorb
// the 32 ms token batcher's burstiness. A stalled stream (zero tokens
// per window) decays to the emission floor in about 1–2.5 s and the
// meter goes quiet.
const speedSmoothTau = 250 * time.Millisecond

// SpeedMeter measures the token rate of one in-flight stream so every
// host can display the same "N tok/s" figure from identical input.
//
// The builder (BuildStreamHandlers) feeds it every content, thinking,
// and tool-args delta — the rate covers everything the model emits, not
// just visible content — and drives its ticker across the round
// lifecycle: Start (re)arms the meter at turn/round start, Stop halts it
// at round end, so the last stats callback always precedes the round's
// OnStreamEnd on the consumer's FIFO queue.
//
// Feed runs on the stream goroutine; the ticker callback runs on its own
// goroutine. All internal state is mutex-guarded, and the Stop contract
// holds by construction: a tick registers its in-flight emission on
// emitWg inside the SAME mu section that checks stopped, so Stop (which
// sets stopped under mu and then waits on emitWg) can never return while
// an emission is still about to run — no stats event can overtake the
// round-end frames that follow it.
type SpeedMeter struct {
	interval time.Duration

	mu       sync.Mutex // guards timer, pending, ema, armed, lastTick, onStats, emitWg registration
	stopped  atomic.Bool
	emitWg   sync.WaitGroup // in-flight onStats calls (Stop waits them out)
	onStats  func(float64)
	timer    *time.Timer
	pending  int     // heuristic tokens fed since the last tick
	ema      float64 // smoothed tokens/sec
	armed    bool    // true once any token has been fed
	lastTick time.Time
}

// NewSpeedMeter creates a meter that emits at most once per interval.
// The meter is inert until Start.
func NewSpeedMeter(interval time.Duration) *SpeedMeter {
	if interval <= 0 {
		interval = StatsInterval
	}
	return &SpeedMeter{interval: interval}
}

// Start resets the meter's counters and starts (or re-arms) its ticker,
// which calls onStats with the smoothed tokens/sec at most once per
// interval. Emissions begin only after the first Feed. Calling Start
// while a ticker is already running keeps the ticker and re-arms the
// counters (idempotent re-arm at a round boundary).
func (m *SpeedMeter) Start(onStats func(float64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = 0
	m.ema = 0
	m.armed = false
	m.lastTick = time.Now()
	m.onStats = onStats
	m.stopped.Store(false)
	if m.timer == nil {
		m.timer = time.AfterFunc(m.interval, m.tick)
	}
}

// Stop halts the ticker and waits out any in-flight emission. It
// returns only once no onStats call can still run, so a stats event can
// never be delivered after Stop (the builder calls it before the round's
// OnStreamEnd). Idempotent.
func (m *SpeedMeter) Stop() {
	m.mu.Lock()
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.stopped.Store(true)
	m.mu.Unlock()
	// No emission can be registered after stopped was set: the check
	// and the registration happen in one mu section (see tick), so
	// everything still about to run is already on the wait group.
	m.emitWg.Wait()
}

// Feed records one streamed delta (content, thinking, or tool-args text)
// toward the rate. Safe to call from any goroutine; a no-op once Stop
// has run for the round.
func (m *SpeedMeter) Feed(text string) {
	if text == "" {
		return
	}
	m.mu.Lock()
	if !m.stopped.Load() {
		// contextmgr.HeuristicTokenCount is the shared bytes/4
		// approximation — tokenizer-free, so it stays cheap mid-stream,
		// and identical to the estimator the context indicators use.
		m.pending += contextmgr.HeuristicTokenCount(text)
		m.armed = true
	}
	m.mu.Unlock()
}

// tick is the ticker callback: it consumes the window's token count into
// the EMA and re-schedules itself, all under mu so Stop (which stops the
// timer under the same lock) always observes the latest re-schedule.
func (m *SpeedMeter) tick() {
	m.mu.Lock()
	if m.stopped.Load() {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	dt := now.Sub(m.lastTick)
	m.lastTick = now
	var rate float64
	var cb func(float64)
	if m.armed {
		toks := m.pending
		m.pending = 0
		if dt > 0 {
			inst := float64(toks) / dt.Seconds()
			if m.ema <= 0 {
				// First sample after arming: adopt it directly instead
				// of blending toward zero.
				m.ema = inst
			} else {
				alpha := 1 - math.Exp(-dt.Seconds()/speedSmoothTau.Seconds())
				m.ema = m.ema*(1-alpha) + inst*alpha
			}
			// Emit only while the rate is displayable (rounds to >= 1
			// tok/s): a stalled stream decays to zero and goes quiet
			// instead of spamming "0 tok/s" for the rest of the round.
			if m.ema >= 0.5 {
				rate = m.ema
				cb = m.onStats
				// Register the in-flight emission BEFORE releasing mu:
				// Stop sets stopped under mu and then waits on emitWg,
				// so it can never return while this call is pending.
				m.emitWg.Add(1)
			}
		}
	}
	m.timer = time.AfterFunc(m.interval, m.tick)
	m.mu.Unlock()
	if cb != nil {
		cb(rate)
		m.emitWg.Done()
	}
}
