package ioutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countTransient returns a transient predicate that reports true for the first
// n calls, so a retry loop can be driven deterministically.
func countTransient(n int, calls *int) func(error) bool {
	return func(error) bool {
		*calls++
		return *calls <= n
	}
}

// TestReadFileRetrySucceedsAfterTransientError pins the loop's reason to exist:
// a read that fails while the file is momentarily unavailable (the target of a
// concurrent temp-file replace) is retried and succeeds once the file appears.
func TestReadFileRetrySucceedsAfterTransientError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	const delay = 50 * time.Millisecond
	// Create the file well before the second attempt (which fires after the
	// first delay), so the retry — not the first attempt — is what succeeds.
	go func() {
		time.Sleep(delay / 2)
		_ = os.WriteFile(path, []byte(`{"ok":true}`), 0o600)
	}()

	calls := 0
	data, err := readFileRetryWith(path, 8, delay, countTransient(8, &calls))
	if err != nil {
		t.Fatalf("readFileRetryWith: %v (calls=%d)", err, calls)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("data = %q", data)
	}
	if calls != 1 {
		t.Fatalf("transient calls = %d, want 1 (first attempt failed, second succeeded)", calls)
	}
}

// TestReadFileRetryGivesUpAndReturnsLastError pins the bound: a file that never
// appears is read at most `attempts` times and the underlying error is returned
// unchanged (so callers keep their os.IsNotExist / errors.Is behavior). The
// predicate is consulted once per *retryable* failure, i.e. attempts-1 times:
// the last attempt returns its error without asking for another retry.
func TestReadFileRetryGivesUpAndReturnsLastError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")

	const delay = time.Millisecond
	calls := 0
	start := time.Now()
	_, err := readFileRetryWith(path, 3, delay, countTransient(100, &calls))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want IsNotExist preserved", err)
	}
	if calls != 2 {
		t.Fatalf("transient calls = %d, want 2 (attempts-1 retryable failures)", calls)
	}
	// Two backoffs were slept (delay, 2*delay); assert a loose lower bound so
	// the check cannot flake on a loaded machine.
	if elapsed := time.Since(start); elapsed < 2*delay {
		t.Fatalf("elapsed = %v, want at least two backoff sleeps (%v)", elapsed, 3*delay)
	}
}

// TestReadFileRetryDoesNotRetryNonTransientError pins that a real error is not
// delayed: the predicate is consulted once and the loop returns immediately.
func TestReadFileRetryDoesNotRetryNonTransientError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")

	calls := 0
	start := time.Now()
	_, err := readFileRetryWith(path, 8, time.Second, countTransient(0, &calls))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if calls != 1 {
		t.Fatalf("transient calls = %d, want 1 (no retry for a non-transient error)", calls)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("non-transient error waited %v, want an immediate return", elapsed)
	}
}

// TestReadFileRetryReadsNormally pins the production entry point: on this
// platform (and for a healthy file) it is a single plain read.
func TestReadFileRetryReadsNormally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFileRetry(path)
	if err != nil {
		t.Fatalf("ReadFileRetry: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", data)
	}
	if _, err := ReadFileRetry(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v, want ErrNotExist", err)
	}
}
