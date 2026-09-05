//go:build !debug

package server

import "time"

// The GOGEN_WS_STALL_*/GOGEN_WS_SENDQ_SIZE live-harness knobs are compiled
// out of production builds (see ws_conn_debug.go); these stubs keep the
// write path free of any debug instrumentation.

// wsDebugSendQueueSize is 0 in production builds, so the default
// wsSendQueueSize is always used.
func wsDebugSendQueueSize() int { return 0 }

// wsStallState is empty in production builds.
type wsStallState struct{}

// newWSStallState returns a no-op stall state in production builds.
func newWSStallState() wsStallState { return wsStallState{} }

// delay is always 0 in production builds.
func (wsStallState) delay(time.Time) time.Duration { return 0 }
