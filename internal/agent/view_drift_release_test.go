//go:build !debug

package agent

import (
	"testing"

	"gogen/internal/llm"
)

func TestViewDriftHooksNoOpInRelease(t *testing.T) {
	a := &Agent{DebugCompareMessages: true, WorkingDir: "/tmp"}
	// Even with the runtime flag set, release builds must not retain snapshots
	// or panic when session/turn hooks run.
	a.recordViewForDrift([]llm.Message{{Role: "user", Content: "x"}})
	a.compareViewOnRestore("old", "new")
	a.clearViewDriftSnapshot()
	if ViewDriftCompiledIn() {
		t.Fatal("ViewDriftCompiledIn must be false in !debug builds")
	}
	if a.lastViewMessages != nil {
		t.Fatalf("release build must not populate lastViewMessages: %#v", a.lastViewMessages)
	}
}
