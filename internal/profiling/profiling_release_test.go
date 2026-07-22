//go:build !debug

package profiling

import (
	"testing"
)

// TestStartStopNoOp verifies that in non-debug builds, Start and Stop are no-ops.
// They should not panic, block, or produce side effects.
func TestStartStopNoOp(t *testing.T) {
	// Call multiple times to verify idempotency.
	for i := 0; i < 3; i++ {
		Start()
		Stop()
	}
}
