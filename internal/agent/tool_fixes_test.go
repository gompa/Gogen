package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMultiEditModifiesExistingFiles verifies that multi_edit actually
// replaces content in existing files (the main bug that was fixed).
func TestMultiEditModifiesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	file2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(file1, []byte("hello world\nhello universe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("goodbye world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)

	// Use a glob that matches both files
	result, err := exec.MultiEdit(context.Background(), "*.txt", "world", "earth", false)
	if err != nil {
		t.Fatalf("MultiEdit failed: %v", err)
	}

	// Verify result mentions both files
	if !strings.Contains(result, "a.txt") || !strings.Contains(result, "b.txt") {
		t.Fatalf("result missing file names: %s", result)
	}
	if !strings.Contains(result, "2 occurrence(s)") {
		t.Fatalf("expected 2 total occurrences, got: %s", result)
	}

	// Verify file content was actually changed
	content1, err := os.ReadFile(file1)
	if err != nil {
		t.Fatal(err)
	}
	if string(content1) != "hello earth\nhello universe\n" {
		t.Fatalf("file1 content wrong: %q", string(content1))
	}

	content2, err := os.ReadFile(file2)
	if err != nil {
		t.Fatal(err)
	}
	if string(content2) != "goodbye earth\n" {
		t.Fatalf("file2 content wrong: %q", string(content2))
	}
}

// TestMultiEditDryRunDoesNotModifyFiles verifies dry-run leaves files untouched.
func TestMultiEditDryRunDoesNotModifyFiles(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file1, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	result, err := exec.MultiEdit(context.Background(), "*.txt", "world", "earth", true)
	if err != nil {
		t.Fatalf("MultiEdit dry-run failed: %v", err)
	}
	if !strings.Contains(result, "Would replace") {
		t.Fatalf("expected dry-run phrasing, got: %s", result)
	}

	// Verify file was NOT modified
	content, err := os.ReadFile(file1)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello world\n" {
		t.Fatalf("file should be unchanged in dry-run: %q", string(content))
	}
}

// TestMultiEditNoPatternMatch verifies graceful handling when no files match.
func TestMultiEditNoPatternMatch(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	result, err := exec.MultiEdit(context.Background(), "*.nonexistent", "foo", "bar", false)
	if err != nil {
		t.Fatalf("MultiEdit on empty glob failed: %v", err)
	}
	if !strings.Contains(result, "No files matched") {
		t.Fatalf("expected 'no files matched', got: %s", result)
	}
}

// TestMultiEditReadsRawContent verifies no headers/truncation in file read.
func TestMultiEditReadsRawContent(t *testing.T) {
	dir := t.TempDir()
	// Create a file with enough content that ReadFileRange would normally
	// add a warning header > 100KB.
	size := 101 * 1024
	data := make([]byte, size)
	for i := 0; i < size; i++ {
		data[i] = 'a'
	}
	// Put the search word at the beginning
	copy(data, "xxxxxxxAAA")
	// And once more at the end (so total count = 2)
	copy(data[size-20:], "xxxxxxxAAA")

	// Same-length replacement so file size stays the same
	const search = "xxxxxxxAAA"
	const replace = "yyyyyyyBBB"

	file1 := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(file1, data, 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	result, err := exec.MultiEdit(context.Background(), "large.txt", search, replace, false)
	if err != nil {
		t.Fatalf("MultiEdit on large file failed: %v", err)
	}
	if !strings.Contains(result, "2 occurrence(s)") {
		t.Fatalf("expected 2 occurrences, got: %s", result)
	}

	// Verify the file still has the right size (no truncation, no headers added)
	content, err := os.ReadFile(file1)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != size {
		t.Fatalf("file size changed from %d to %d (header/truncation bug)", size, len(content))
	}
}

// TestHandleExtractFunctionIntArg verifies handleExtractFunction requires start_line/end_line.
func TestHandleExtractFunctionIntArg(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := &Agent{Mode: ModeAct, Executor: exec, WorkingDir: dir}

	// Test that missing start_line returns an error mentioning it's required
	_, err := handleExtractFunction(context.Background(), a, map[string]interface{}{
		"file":      "/tmp/test.go",
		"end_line":  10,
		"func_name": "foo",
		// start_line intentionally omitted
	})
	if err == nil {
		t.Fatal("expected error for missing start_line")
	}
	if !strings.Contains(err.Error(), "missing required argument") ||
		!strings.Contains(err.Error(), "start_line") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test that start_line=0 is properly rejected (not silently mapped to "missing")
	_, err = handleExtractFunction(context.Background(), a, map[string]interface{}{
		"file":       "/tmp/test.go",
		"start_line": 0,
		"end_line":   10,
		"func_name":  "foo",
	})
	if err == nil {
		t.Fatal("expected error for start_line=0")
	}
	if strings.Contains(err.Error(), "missing required argument") {
		t.Fatalf("should say 'must be a positive integer' not 'missing', got: %v", err)
	}
}

// TestHandleGenerateTestStyleValidation verifies style parameter is validated.
func TestHandleGenerateTestStyleValidation(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := &Agent{Mode: ModeAct, Executor: exec, WorkingDir: dir}

	// Invalid style should be rejected
	_, err := handleGenerateTest(context.Background(), a, map[string]interface{}{
		"func_name": "Foo",
		"style":     "bogus-style",
	})
	if err == nil {
		t.Fatal("expected error for invalid style")
	}
	if !strings.Contains(err.Error(), "invalid style") {
		t.Fatalf("expected 'invalid style' error, got: %v", err)
	}

	// Empty style should be accepted (defaults to subtests)
	_, err = handleGenerateTest(context.Background(), a, map[string]interface{}{
		"func_name": "Foo",
	})
	// Might fail because function not found in source, but NOT on style validation
	if err != nil && strings.Contains(err.Error(), "invalid style") {
		t.Fatalf("empty style should be accepted, got: %v", err)
	}
}
