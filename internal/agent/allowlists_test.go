package agent

import "testing"

// TestAllowlistConsistency guards the derived tool allowlists
// (planModeAllowedTools in mode.go, parallelSafeTools and the MutatesFS set
// in tools.go) against silent drift from builtinToolDefs:
//
//   - every referenced tool name must actually exist (typos or tools that
//     were renamed/removed fail loudly), and
//   - every parallel-safe tool must also be allowed in plan mode: parallel
//     safety is the stricter property (read-only, no shell execution, no
//     workspace or session-state mutation), so a tool that is safe to run
//     concurrently is by definition safe to allow in read-only plan mode, and
//   - the ReadOnly / PlanAllowed / MutatesFS flags stay mutually consistent:
//     a tool can't be both read-only and FS-mutating, and an FS-mutating tool
//     must not be plan-allowed.
//
// The reverse direction (plan-mode tools being parallel-safe) is NOT
// asserted: plan mode deliberately permits a few session-local mutations
// (todo, session_rename, context_pin_last) that must stay sequential.
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

	// Flag invariants and derivation sync: these catch a hand-maintained
	// literal map sneaking back in, or a flag set that contradicts another.
	for _, d := range builtinToolDefs {
		name := d.Definition.Name
		if d.ReadOnly && d.MutatesFS {
			t.Errorf("tool %q is both ReadOnly and MutatesFS: read-only and workspace-mutating are mutually exclusive", name)
		}
		if d.MutatesFS && d.PlanAllowed {
			t.Errorf("FS-mutating tool %q must not be PlanAllowed: plan mode is read-only", name)
		}
		if _, ok := parallelSafeTools[name]; ok != d.ReadOnly {
			t.Errorf("parallelSafeTools out of sync with ReadOnly flag for %q", name)
		}
		if _, ok := planModeAllowedTools[name]; ok != d.PlanAllowed {
			t.Errorf("planModeAllowedTools out of sync with PlanAllowed flag for %q", name)
		}
	}

	// The MutatesFS set exported for the server's fsMu wrapper must match the
	// flag exactly (server/workspace.go derives its lock set from it).
	fsNames := FSMutatingToolNames()
	inFS := make(map[string]bool, len(fsNames))
	for _, n := range fsNames {
		inFS[n] = true
	}
	for _, d := range builtinToolDefs {
		if _, ok := inFS[d.Definition.Name]; ok != d.MutatesFS {
			t.Errorf("FSMutatingToolNames out of sync with MutatesFS flag for %q", d.Definition.Name)
		}
	}
}
