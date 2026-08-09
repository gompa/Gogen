package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCallGraphImpactDirection pins the change-impact analysis folded into the
// call_graph tool: direction=impact must report dependents (files referencing
// the symbol) and a risk score, not just call sites. The text fallback runs
// without tree-sitter, so this test works in any environment.
func TestCallGraphImpactDirection(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.go": "package main\n\nfunc helper() {}\n",
		"use.go":  "package main\n\nfunc use() { helper() }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	exec := NewExecutor(dir)
	out, err := exec.CallGraph(context.Background(), "helper", "", "", "impact")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Direct dependents") {
		t.Fatalf("impact output missing dependents: %q", out)
	}
	if !strings.Contains(out, "use.go") {
		t.Fatalf("impact output missing dependent file: %q", out)
	}
	if !strings.Contains(out, "Impact score") {
		t.Fatalf("impact output missing score: %q", out)
	}
	if !strings.Contains(out, "impact change") {
		t.Fatalf("impact output missing recommendation: %q", out)
	}
}

// TestCallGraphDirectionsDoNotLeakImpact ensures the plain call-graph
// directions are unaffected by the impact branch.
func TestCallGraphDirectionsDoNotLeakImpact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(dir)
	for _, direction := range []string{"callers", "callees", "both"} {
		out, err := exec.CallGraph(context.Background(), "f", "", "", direction)
		if err != nil {
			t.Fatalf("direction %q: unexpected error: %v", direction, err)
		}
		if strings.Contains(out, "Impact score") {
			t.Fatalf("direction %q leaked impact output: %q", direction, out)
		}
	}
}
