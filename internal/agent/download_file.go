package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gogen/internal/ioutil"
)

const (
	// downloadDefaultMaxBytes is the default cap for download_file when
	// max_bytes is not provided. Larger than web_fetch's default because
	// downloads target source trees and binaries, not rendered pages.
	downloadDefaultMaxBytes = 50 * 1024 * 1024
	// downloadHardMaxBytes is the absolute ceiling for a single download.
	downloadHardMaxBytes = 200 * 1024 * 1024
	// downloadTimeout bounds a single download (headers + body). Generous
	// enough for multi-MB files over slow links; the caller's context
	// deadline still applies on top.
	downloadTimeout = 5 * time.Minute
)

// DownloadFile fetches rawURL and writes the response body to targetPath
// under the working directory (path boundary enforced via SecurePath).
//
// Unlike WebFetch it does not strip HTML or extract text: the raw bytes are
// written verbatim, so source files and binaries arrive intact and can then
// be explored with read_file offset/limit, search_code, list_definitions,
// patch_file, etc. This is the intended path for files larger than the
// web_fetch text caps (~256 KB default body / 2 MB max).
//
// The same network policy as web_fetch applies: https-only in the default
// mode, private/internal hosts and redirects blocked, domain allowlist
// honored. The response must fit within maxBytes (default
// downloadDefaultMaxBytes, capped at downloadHardMaxBytes); a larger body
// fails with an error instead of writing a truncated file. Existing files
// are not overwritten unless overwrite is true.
func (e *Executor) DownloadFile(ctx context.Context, rawURL, targetPath string, maxBytes int, overwrite bool) (string, error) {
	if !webCfg.isFetchOn() {
		return "", fmt.Errorf("download_file is disabled (set GOGEN_WEB_FETCH=on to re-enable)")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if maxBytes <= 0 {
		maxBytes = downloadDefaultMaxBytes
	}
	if maxBytes > downloadHardMaxBytes {
		maxBytes = downloadHardMaxBytes
	}

	parsed, err := fetchURLValidator(rawURL)
	if err != nil {
		return "", err
	}

	secure, err := e.SecurePath(targetPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(secure)
	switch {
	case err == nil && info.IsDir():
		return "", fmt.Errorf("path is a directory: %s", targetPath)
	case err == nil && !overwrite:
		return "", fmt.Errorf("file already exists: %s (pass overwrite=true to replace it)", targetPath)
	case err == nil:
		// overwrite requested — fall through
	case !os.IsNotExist(err):
		return "", err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	body, _, finalURL, truncated, err := doFetch(ctx, fetchRequest{
		URL:      parsed.String(),
		MaxBytes: maxBytes,
	})
	if err != nil {
		return "", err
	}
	if truncated {
		return "", fmt.Errorf("download exceeds max_bytes (%d); body is larger than %d bytes — raise max_bytes (up to %d) or use execute_command with curl for larger files",
			maxBytes, maxBytes, downloadHardMaxBytes)
	}

	dir := filepath.Dir(secure)
	if err := os.MkdirAll(dir, defaultDirPerm); err != nil {
		return "", fmt.Errorf("create parent dir: %w", err)
	}
	if err := ioutil.WriteFileAtomic(secure, body, defaultFilePerm); err != nil {
		return "", err
	}

	from := parsed.String()
	if finalURL != from {
		from = fmt.Sprintf("%s (final URL %s)", from, finalURL)
	}
	return fmt.Sprintf("Downloaded %d bytes to %s (from %s)", len(body), targetPath, from), nil
}
