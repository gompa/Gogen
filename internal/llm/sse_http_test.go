package llm

import (
	"context"
	"testing"
	"time"
)

func TestNewSSEHTTPClientDisablesCompression(t *testing.T) {
	t.Parallel()
	c := newSSEHTTPClient()
	tr, ok := baseHTTPTransport(c)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport under the SSE filter transport", c.Transport)
	}
	if !tr.DisableCompression {
		t.Fatal("DisableCompression = false, want true")
	}
}

// TestStreamRetryBackoff pins the recovery-ladder delay schedule: the
// default base is 1s doubling per stage, the env knob overrides it, and
// 0/off disables the wait entirely.
func TestStreamRetryBackoff(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		stage int
		want  time.Duration
	}{
		{name: "default stage 0", want: time.Second},
		{name: "default stage 1", stage: 1, want: 2 * time.Second},
		{name: "custom base", env: "500ms", want: 500 * time.Millisecond},
		{name: "custom base stage 1", env: "500ms", stage: 1, want: time.Second},
		{name: "zero disables", env: "0", want: 0},
		{name: "off disables", env: "off", want: 0},
		{name: "invalid falls back to default", env: "banana", want: time.Second},
		{name: "negative falls back to default", env: "-1s", want: time.Second},
	}
	// t.Setenv precludes t.Parallel.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", tt.env)
			if got := streamRetryBackoff(tt.stage); got != tt.want {
				t.Fatalf("streamRetryBackoff(%d) = %v, want %v", tt.stage, got, tt.want)
			}
		})
	}
}

// TestWaitStreamBackoffCancelled pins that a turn cancelled mid-backoff
// returns the context error immediately instead of sleeping out the delay.
func TestWaitStreamBackoffCancelled(t *testing.T) {
	t.Parallel()
	if err := waitStreamBackoff(context.Background(), 0); err != nil {
		t.Fatalf("zero delay must not error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitStreamBackoff(ctx, time.Minute); err == nil {
		t.Fatal("cancelled context must return an error")
	} else if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled wait took %v, want immediate", elapsed)
	}
}
