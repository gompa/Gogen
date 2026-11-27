package server

import (
	"context"
	"testing"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
)

// defaultProbeCeiling is the ceiling the probe uses when
// mcp_probe_timeout_secs is unset (0 = the built-in default).
var defaultProbeCeiling = config.DefaultMCPProbeTimeoutSecs * time.Second

// TestMCPProbeTimeoutIsBounded pins the test_mcp deadline. The floor is the
// cold-npx allowance (mcpTestTimeout) and a raised value is honored — but the
// raise comes from the client-supplied init_timeout_secs, so it is clamped to
// the CONFIGURED ceiling (mcp_probe_timeout_secs, default 10 minutes). Without
// the clamp a typed "1000000" kept a throwaway child process (and the probe
// goroutine holding it) alive for days.
func TestMCPProbeTimeoutIsBounded(t *testing.T) {
	tests := []struct {
		name     string
		initSecs int
		ceiling  time.Duration
		want     time.Duration
	}{
		{name: "unset uses the cold-npx floor", initSecs: 0, ceiling: defaultProbeCeiling, want: mcpTestTimeout},
		{name: "negative falls back to the floor", initSecs: -5, ceiling: defaultProbeCeiling, want: mcpTestTimeout},
		{name: "a value below the floor stays the floor", initSecs: 10, ceiling: defaultProbeCeiling, want: mcpTestTimeout},
		{name: "a raised value is honored", initSecs: 300, ceiling: defaultProbeCeiling, want: 300 * time.Second},
		{
			name:     "the ceiling itself is allowed",
			initSecs: int(defaultProbeCeiling / time.Second),
			ceiling:  defaultProbeCeiling,
			want:     defaultProbeCeiling,
		},
		{name: "above the ceiling is clamped", initSecs: 1000000, ceiling: defaultProbeCeiling, want: defaultProbeCeiling},
		{name: "an overflowing value is clamped", initSecs: 1 << 40, ceiling: defaultProbeCeiling, want: defaultProbeCeiling},
		// The configured ceiling is honored in both directions: a lowered one
		// wins over init_timeout_secs, and a raised one lets a slow launcher
		// through. A ceiling below the floor is honored too (fail fast).
		{name: "a lowered ceiling caps a raised init", initSecs: 300, ceiling: 120 * time.Second, want: 120 * time.Second},
		{name: "a raised ceiling admits a slow launcher", initSecs: 1200, ceiling: 1200 * time.Second, want: 1200 * time.Second},
		{name: "a ceiling below the floor is honored", initSecs: 0, ceiling: 10 * time.Second, want: 10 * time.Second},
		{name: "a sub-second ceiling still yields a usable deadline", initSecs: 300, ceiling: time.Millisecond, want: time.Millisecond},
		{name: "an unset ceiling falls back to the built-in default", initSecs: 300, ceiling: 0, want: 300 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpProbeTimeout(config.MCPServerConfig{InitTimeoutSecs: tc.initSecs}, tc.ceiling)
			if got != tc.want {
				t.Fatalf("mcpProbeTimeout(init_timeout_secs=%d, ceiling=%v) = %v, want %v",
					tc.initSecs, tc.ceiling, got, tc.want)
			}
		})
	}
}

// TestMCPProbeConcurrencyIsBounded pins the probe-slot cap: each test_mcp
// spawns a throwaway child process in a bare goroutine, so N messages used to
// mean N children. The (cap+1)-th request must be refused without spawning
// anything, and a finished probe must release its slot.
func TestMCPProbeConcurrencyIsBounded(t *testing.T) {
	limit := config.DefaultMCPProbeMaxConcurrent
	s := &Server{}
	started := make(chan struct{}, limit+1)
	release := make(chan struct{})
	s.mcpTestFn = func(_ context.Context, _ config.MCPServerConfig) ([]llm.Tool, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}
	send := func() {
		wsHandleTestMCP(&wsRequest{server: s, msg: WSMessage{
			Type:    "test_mcp",
			MCPTest: &MCPTestRequest{Command: "true"},
		}})
	}

	for i := 0; i < limit; i++ {
		send()
	}
	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("probe %d never started", i+1)
		}
	}

	// The cap is reached: one more request is refused instead of spawning
	// another child.
	send()
	select {
	case <-started:
		t.Fatalf("a %d-th probe started although the cap is %d", limit+1, limit)
	case <-time.After(150 * time.Millisecond):
	}
	if n := runningProbes(s); n != limit {
		t.Fatalf("probe count = %d while %d probes run, want %d", n, limit, limit)
	}

	// Finishing them releases the slots (a leaked slot would wedge the modal).
	close(release)
	waitForCond(t, 5*time.Second, func() bool { return runningProbes(s) == 0 })
	if !s.beginMCPProbe(0) {
		t.Fatal("a probe slot was not released after the running probes finished")
	}
	s.endMCPProbe()
	if n := runningProbes(s); n != 0 {
		t.Fatalf("probe count = %d after endMCPProbe, want 0", n)
	}
}

// TestMCPProbeBoundsAreLive pins the LIVE half of the two new settings
// (mcp_probe_timeout_secs / mcp_probe_max_concurrent): the handler reads both
// from the runtime overlay at call time, so a settings-modal save takes effect
// on the next test with no restart. This is the property that would break if
// the bounds were snapshotted into Server fields at construction.
func TestMCPProbeBoundsAreLive(t *testing.T) {
	s := &Server{}

	// The runtime overlay is lowered to a 120s ceiling and one concurrent
	// probe. mcpTestFn reports the deadline it was handed, so the assertion is
	// on the value the probe actually ran with.
	overlay := config.Defaults()
	overlay.MCPProbeTimeoutSecs = 120
	overlay.MCPProbeMaxConcurrent = 1
	s.ws = &Workspace{runtime: overlay}

	deadline := make(chan time.Time, 4)
	s.mcpTestFn = func(ctx context.Context, _ config.MCPServerConfig) ([]llm.Tool, error) {
		if dl, ok := ctx.Deadline(); ok {
			deadline <- dl
		}
		return nil, nil
	}
	start := time.Now()
	wsHandleTestMCP(&wsRequest{server: s, msg: WSMessage{
		Type:    "test_mcp",
		MCPTest: &MCPTestRequest{Command: "true"},
	}})
	select {
	case dl := <-deadline:
		if d := dl.Sub(start); d > 130*time.Second {
			t.Fatalf("probe deadline is %v away, want the configured 120s ceiling (the built-in default is 10m)", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe never ran")
	}
	waitForCond(t, 5*time.Second, func() bool { return runningProbes(s) == 0 })

	// The lowered concurrency cap is live too: with one slot allowed, a second
	// request is refused instead of spawning another child.
	hold := make(chan struct{})
	defer close(hold)
	s.mcpTestFn = func(_ context.Context, _ config.MCPServerConfig) ([]llm.Tool, error) {
		<-hold
		return nil, nil
	}
	send := func() {
		wsHandleTestMCP(&wsRequest{server: s, msg: WSMessage{
			Type:    "test_mcp",
			MCPTest: &MCPTestRequest{Command: "true"},
		}})
	}
	send()
	waitForCond(t, 5*time.Second, func() bool { return runningProbes(s) == 1 })
	send()
	time.Sleep(100 * time.Millisecond)
	if n := runningProbes(s); n != 1 {
		t.Fatalf("probe count = %d, want 1 (the live mcp_probe_max_concurrent is 1)", n)
	}
}

// runningProbes reads the live probe count under its lock.
func runningProbes(s *Server) int {
	s.mcpProbeMu.Lock()
	defer s.mcpProbeMu.Unlock()
	return s.mcpProbes
}
