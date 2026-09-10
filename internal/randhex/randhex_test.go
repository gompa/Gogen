package randhex

import (
	"strings"
	"testing"
)

func TestID(t *testing.T) {
	tests := []struct {
		name    string
		n       int
		prefix  string
		wantHex int
	}{
		{name: "min bytes", n: 1, prefix: "", wantHex: 2},
		{name: "job id", n: 8, prefix: "job-", wantHex: 16},
		{name: "session id", n: 16, prefix: "", wantHex: 32},
		{name: "max bytes", n: MaxBytes, prefix: "p-", wantHex: MaxBytes * 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ID(tt.n, tt.prefix)
			if !strings.HasPrefix(got, tt.prefix) {
				t.Fatalf("ID(%d, %q) = %q, want prefix %q", tt.n, tt.prefix, got, tt.prefix)
			}
			if body := strings.TrimPrefix(got, tt.prefix); len(body) != tt.wantHex {
				t.Fatalf("ID(%d, %q) body %q has %d chars, want %d", tt.n, tt.prefix, body, len(body), tt.wantHex)
			}
		})
	}
}

func TestIDPanicsOutsideRange(t *testing.T) {
	tests := []struct {
		name string
		n    int
	}{
		{name: "zero bytes", n: 0},
		{name: "negative bytes", n: -1},
		{name: "above max", n: MaxBytes + 1},
		{name: "forty bytes", n: 40},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("ID(%d, \"\") did not panic", tt.n)
				}
			}()
			ID(tt.n, "")
		})
	}
}

func TestIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := ID(16, "")
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
