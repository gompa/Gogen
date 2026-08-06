package agent

import "testing"

// TestAllowlistConsistency guards the two hand-maintained tool allowlists
// (planModeAllowedTools in mode.go, parallelSafeTools in tools.go) against
// silent drift from builtinToolDefs:
//
//   - every referenced tool name must actually exist (typos or tools that
//     were renamed/removed fail loudly), and
//   - every parallel-safe tool must also be allowed in plan mode: parallel
//     safety is the stricter property (read-only, no shell execution, no
//     workspace or session-state mutation), so a tool that is safe to run
//     concurrently is by definition safe to allow in read-only plan mode.
//
// The reverse direction (plan-mode tools being parallel-safe) is NOT
// asserted: plan mode deliberately permits a few session-local mutations
// (todo_add, session_rename, context_pin_last) that must stay sequential.
func TestAllowlistConsistency(t *testing.T) {
	defined := make(map[string]bool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		defined[d.Definition.Name] = true
	}

	for name := range planModeAllowedTools {
		if !defined[name] {
			t.Errorf("planModeAllowedTools references undefined tool %q", name)
		}
	}
	for name := range parallelSafeTools {
		if !defined[name] {
			t.Errorf("parallelSafeTools references undefined tool %q", name)
		}
		if _, ok := planModeAllowedTools[name]; !ok {
			t.Errorf("parallelSafeTools tool %q is missing from planModeAllowedTools: parallel-safe tools are read-only by definition and must be allowed in plan mode", name)
		}
	}
}
