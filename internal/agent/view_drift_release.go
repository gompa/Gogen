//go:build !debug

package agent

import "gogen/internal/llm"

// ViewDriftCompiledIn reports whether this binary includes the view-drift
// detector. Production builds always return false so GOGEN_DEBUG_COMPARE_MESSAGES
// cannot enable code that was compiled out.
func ViewDriftCompiledIn() bool { return false }

// recordViewForDrift is a no-op in production builds.
func (a *Agent) recordViewForDrift([]llm.Message) {}

// compareViewOnRestore is a no-op in production builds.
func (a *Agent) compareViewOnRestore(_, _ string) {}

// clearViewDriftSnapshot is a no-op in production builds.
func (a *Agent) clearViewDriftSnapshot() {}
