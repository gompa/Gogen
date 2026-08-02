package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enableWebFetchForTest turns the web-fetch/download toggle on for the
// duration of a test. Shared package state — do not run these in parallel.
func enableWebFetchForTest(t *testing.T) {
	t.Helper()
	ConfigureWebFetch(true, "https", "")
	t.Cleanup(func() { ConfigureWebFetch(false, "https", "") })
}

// bypassFetchURLValidation lets tests exercise the fetch/download paths
// against a local httptest server; the real validator blocks private hosts.
func bypassFetchURLValidation(t *testing.T) {
	t.Helper()
	old := fetchURLValidator
	fetchURLValidator = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	t.Cleanup(func() { fetchURLValidator = old })
}

// useLocalFetchClient swaps the shared (SSRF-guarded) HTTP client for the
// httptest server's client so doFetch can reach 127.0.0.1 in tests.
func useLocalFetchClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := sharedFetchClient
	sharedFetchClient = srv.Client()
	t.Cleanup(func() { sharedFetchClient = old })
}

func TestDownloadFileDisabledByDefault(t *testing.T) {
	// webCfg is unconfigured in tests, so GOGEN_WEB_FETCH env is off.
	exec := NewExecutor(t.TempDir())
	_, err := exec.DownloadFile(context.Background(), "https://example.com/f.c", "f.c", 0, false)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error, got %v", err)
	}
}

func TestDownloadFileValidation(t *testing.T) {
	enableWebFetchForTest(t)
	exec := NewExecutor(t.TempDir())

	cases := []struct {
		name, fetchURL, path string
		wantErr              string
	}{
		{"empty url", "", "f.c", "url is required"},
		{"empty path", "https://example.com/f.c", "", "path is required"},
		{"private host blocked", "http://127.0.0.1/f.c", "f.c", "private/internal hosts are blocked"},
		{"localhost blocked", "https://localhost/f.c", "f.c", "private/internal hosts are blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec.DownloadFile(context.Background(), tc.fetchURL, tc.path, 0, false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DownloadFile(%q, %q) error = %v, want containing %q", tc.fetchURL, tc.path, err, tc.wantErr)
			}
		})
	}
}

func TestDownloadFilePathBoundary(t *testing.T) {
	enableWebFetchForTest(t)
	bypassFetchURLValidation(t) // avoid DNS on example.com
	wd := t.TempDir()
	exec := NewExecutor(wd)

	_, err := exec.DownloadFile(context.Background(), "https://example.com/f.c", "../escape.c", 0, false)
	if err == nil || !strings.Contains(err.Error(), "outside of allowed boundary") {
		t.Fatalf("expected boundary error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(wd), "escape.c")); !os.IsNotExist(statErr) {
		t.Fatalf("file escaped the working directory (stat err = %v)", statErr)
	}
}

func TestDownloadFileRefusesOverwrite(t *testing.T) {
	enableWebFetchForTest(t)
	bypassFetchURLValidation(t)
	wd := t.TempDir()
	exec := NewExecutor(wd)
	target := filepath.Join(wd, "existing.c")
	if err := os.WriteFile(target, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := exec.DownloadFile(context.Background(), "https://example.com/f.c", "existing.c", 0, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Fatalf("existing file was modified: %q", data)
	}
}

func TestDownloadFileSuccess(t *testing.T) {
	enableWebFetchForTest(t)
	payload := strings.Repeat("kernel-source-line\n", 200) // ~4 KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	wd := t.TempDir()
	exec := NewExecutor(wd)
	msg, err := exec.DownloadFile(context.Background(), srv.URL, "src/file.c", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Downloaded") {
		t.Fatalf("unexpected message: %q", msg)
	}
	data, err := os.ReadFile(filepath.Join(wd, "src", "file.c"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Fatalf("downloaded content mismatch: got %d bytes, want %d", len(data), len(payload))
	}
}

func TestDownloadFileSizeCapRejected(t *testing.T) {
	enableWebFetchForTest(t)
	payload := strings.Repeat("x", 10*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	wd := t.TempDir()
	exec := NewExecutor(wd)
	_, err := exec.DownloadFile(context.Background(), srv.URL, "big.bin", 4096, false)
	if err == nil || !strings.Contains(err.Error(), "exceeds max_bytes") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
	// A partial file must not be left behind.
	if _, statErr := os.Stat(filepath.Join(wd, "big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("truncated download was written to disk (stat err = %v)", statErr)
	}
}

func TestDoFetchReportsTruncation(t *testing.T) {
	payload := strings.Repeat("y", 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	useLocalFetchClient(t, srv)

	// Under the cap: truncated = false, full body returned.
	body, _, _, truncated, err := doFetch(context.Background(), fetchRequest{URL: srv.URL, MaxBytes: len(payload) + 1})
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatalf("expected no truncation, got truncated=true (%d bytes)", len(body))
	}
	if len(body) != len(payload) {
		t.Fatalf("body = %d bytes, want %d", len(body), len(payload))
	}

	// Over the cap: truncated = true, body trimmed exactly to MaxBytes.
	body, _, _, truncated, err = doFetch(context.Background(), fetchRequest{URL: srv.URL, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatalf("expected truncation, got truncated=false")
	}
	if len(body) != 4096 {
		t.Fatalf("body = %d bytes, want 4096", len(body))
	}
}

func TestWebFetchTruncationNotice(t *testing.T) {
	enableWebFetchForTest(t)
	// Make the body clearly larger than the default 256 KB cap so the
	// truncated flag path is exercised with default max_bytes.
	payload := strings.Repeat("static int line_of_source_0123456789 = 1;\n", 8000) // ~288 KB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	bypassFetchURLValidation(t)
	useLocalFetchClient(t, srv)

	exec := NewExecutor(t.TempDir())
	out, err := exec.WebFetch(context.Background(), srv.URL, WebFetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The result must explicitly say the body was cut (no silent mid-file end).
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "more than") {
		t.Fatalf("expected explicit truncation notice, got %d-byte result: %.120q…", len(out), out)
	}
	if !strings.Contains(out, "\n… truncated (") {
		t.Fatalf("result must carry the truncation marker so MaxToolResultBytes keeps it intact: %.120q…", out)
	}
}
