package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteFileRequiresApproval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	ctx := context.Background()
	_, err := exec.DeleteFile(ctx, "remove.txt")
	if err != ErrDeleteApprovalRequired {
		t.Fatalf("expected ErrDeleteApprovalRequired, got %v", err)
	}
}

func TestDeleteFileDenied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	ctx := ContextWithDeleteApprover(context.Background(), func(context.Context, DeleteRequest) (bool, error) {
		return false, nil
	})
	_, err := exec.DeleteFile(ctx, "remove.txt")
	if err != ErrDeleteDenied {
		t.Fatalf("expected ErrDeleteDenied, got %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("file should still exist")
	}
}

func TestDeleteFileApproved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	ctx := ContextWithDeleteApprover(context.Background(), func(context.Context, DeleteRequest) (bool, error) {
		return true, nil
	})
	out, err := exec.DeleteFile(ctx, "remove.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected success message")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("file should be deleted")
	}
}

func TestDeleteApprovalOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	exec.SetDeleteApproval(false)
	_, err := exec.DeleteFile(context.Background(), "remove.txt")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEmptyDirApproved(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	ctx := ContextWithDeleteApprover(context.Background(), func(context.Context, DeleteRequest) (bool, error) {
		return true, nil
	})
	out, err := exec.DeleteFile(ctx, "emptydir")
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected success message")
	}
	if _, statErr := os.Stat(sub); !os.IsNotExist(statErr) {
		t.Fatal("empty directory should be deleted")
	}
}

func TestDeleteEmptyDirRequiresApproval(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	_, err := exec.DeleteFile(context.Background(), "emptydir")
	if err != ErrDeleteApprovalRequired {
		t.Fatalf("expected ErrDeleteApprovalRequired, got %v", err)
	}
	if _, statErr := os.Stat(sub); statErr != nil {
		t.Fatal("directory should still exist")
	}
}

func TestDeleteNonEmptyDirRefused(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "notempty")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	ctx := ContextWithDeleteApprover(context.Background(), func(context.Context, DeleteRequest) (bool, error) {
		return true, nil
	})
	_, err := exec.DeleteFile(ctx, "notempty")
	if err == nil {
		t.Fatal("expected refusal for non-empty directory")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected not-empty error, got %v", err)
	}
	if _, statErr := os.Stat(sub); statErr != nil {
		t.Fatal("directory should still exist")
	}
	if _, statErr := os.Stat(filepath.Join(sub, "keep.txt")); statErr != nil {
		t.Fatal("contents should be untouched")
	}
}

func TestPatchFileDeleteRequiresApproval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "--- a/remove.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"
	exec := NewExecutor(dir)
	_, err := exec.PatchFile(context.Background(), diff, false, false)
	if err != ErrDeleteApprovalRequired {
		t.Fatalf("expected ErrDeleteApprovalRequired, got %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("file should still exist")
	}
}

func TestPatchFileDeleteApproved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remove.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := "--- a/remove.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"
	exec := NewExecutor(dir)
	ctx := ContextWithDeleteApprover(context.Background(), func(context.Context, DeleteRequest) (bool, error) {
		return true, nil
	})
	if _, err := exec.PatchFile(ctx, diff, false, false); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("file should be deleted")
	}
}
