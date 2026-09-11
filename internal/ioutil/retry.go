package ioutil

import (
	"os"
	"time"
)

// Transient-read retry parameters: eight attempts with an exponential backoff
// (2ms, 4ms, … 256ms; ~0.5s total). The window they cover is the instant a
// writer's temp-file rename replaces the target — sub-millisecond in practice,
// so the first retry almost always wins and the full budget is only paid on a
// path that is genuinely failing.
const (
	readFileRetryAttempts = 8
	readFileRetryDelay    = 2 * time.Millisecond
)

// ReadFileRetry reads path like os.ReadFile, retrying briefly when the open
// fails with a transient sharing violation (see isTransientSharingErr).
//
// Store files are published by writing a temp file and renaming it over the
// target. On Windows, Go opens files with share mode FILE_SHARE_READ|
// FILE_SHARE_WRITE (syscall.Open) — no FILE_SHARE_DELETE — so a reader that
// opens the target while the replace (or a delete) is in flight is refused with
// ERROR_SHARING_VIOLATION "being used by another process" (or ERROR_ACCESS_DENIED
// while the file is delete-pending). A plain os.ReadFile turns that momentary
// race into a hard failure: a session-delta read silently skips the merge (lost
// messages on restore), an index read rebuilds the index, a store reopen (the
// automation CLI against a running host, the polling regression test) fails.
//
// Off Windows, and for every non-transient error, this is exactly os.ReadFile:
// one attempt, no delay.
func ReadFileRetry(path string) ([]byte, error) {
	return readFileRetryWith(path, readFileRetryAttempts, readFileRetryDelay, isTransientSharingErr)
}

// readFileRetryWith is ReadFileRetry with an injectable transient predicate and
// retry budget, so the loop can be tested on any platform.
func readFileRetryWith(path string, attempts int, delay time.Duration, transient func(error) bool) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		data, err := os.ReadFile(path)
		if err == nil || attempt >= attempts || !transient(err) {
			return data, err
		}
		time.Sleep(delay)
		delay *= 2
	}
}
