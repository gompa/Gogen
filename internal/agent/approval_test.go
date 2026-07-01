package agent

import (
	"context"
	"os"
	"path/filepath"
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
	exec.RequireDeleteApproval = false
	_, err := exec.DeleteFile(context.Background(), "remove.txt")
	if err != nil {
		t.Fatal(err)
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
