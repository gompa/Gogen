package contextmgr

import (
	"strings"
	"testing"
)

func TestLiveEstimate(t *testing.T) {
	// An op is either a Rebase (rebase=true) or an Add (non-empty add),
	// applied in order.
	type op struct {
		rebase bool
		used   int
		add    string
	}
	tests := []struct {
		name     string
		ops      []op
		wantUsed int
		wantBase int
	}{
		{
			name:     "zero value",
			ops:      nil,
			wantUsed: 0,
			wantBase: 0,
		},
		{
			name:     "rebase only",
			ops:      []op{{rebase: true, used: 1000}},
			wantUsed: 1000,
			wantBase: 1000,
		},
		{
			name:     "accumulate after rebase",
			ops:      []op{{rebase: true, used: 1000}, {add: strings.Repeat("a", 400)}}, // 400 bytes -> 100 tokens
			wantUsed: 1100,
			wantBase: 1000,
		},
		{
			name:     "empty delta ignored",
			ops:      []op{{rebase: true, used: 1000}, {add: ""}},
			wantUsed: 1000,
			wantBase: 1000,
		},
		{
			name: "rebase discards accumulated estimate",
			ops: []op{
				{rebase: true, used: 1000},
				{add: strings.Repeat("a", 400)},
				{rebase: true, used: 1500},
			},
			wantUsed: 1500,
			wantBase: 1500,
		},
		{
			name: "multi-round turn keeps growing from refreshed baseline",
			ops: []op{
				{rebase: true, used: 1000},
				{add: strings.Repeat("a", 400)}, // round 1: +100
				{rebase: true, used: 1180},      // exact post-round-1 measurement
				{add: strings.Repeat("b", 400)}, // round 2: +100
			},
			wantUsed: 1280,
			wantBase: 1180,
		},
		{
			name:     "multibyte text counted by bytes",
			ops:      []op{{rebase: true, used: 0}, {add: "中"}, {add: "é"}}, // 3 bytes -> 1, 2 bytes -> 1
			wantUsed: 2,
			wantBase: 0,
		},
		{
			name: "short deltas round up per chunk",
			ops: []op{
				{rebase: true, used: 0},
				{add: "a"},     // (1+3)/4 = 1
				{add: "ab"},    // (2+3)/4 = 1
				{add: "abc"},   // (3+3)/4 = 1
				{add: "abcd"},  // (4+3)/4 = 1
				{add: "abcde"}, // (5+3)/4 = 2
			},
			wantUsed: 6,
			wantBase: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e LiveEstimate
			for _, o := range tt.ops {
				if o.rebase {
					e.Rebase(o.used)
				}
				e.Add(o.add)
			}
			if e.Used() != tt.wantUsed {
				t.Errorf("Used() = %d, want %d", e.Used(), tt.wantUsed)
			}
			if e.Base() != tt.wantBase {
				t.Errorf("Base() = %d, want %d", e.Base(), tt.wantBase)
			}
		})
	}
}
