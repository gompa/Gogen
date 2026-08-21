package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/ioutil"
	"gogen/internal/projectfile"
)

// webTokenStatePath returns the on-disk location of the auto-generated web
// auth token: .gogen/web_token for project mode, ~/.config/gogen/web_token
// for global mode. It lives next to the config but is a separate file so
// --save-config can never silently drop it (secrets are omitted from saved
// config unless --save-config-secrets is passed, which would otherwise
// rotate the token and log out every paired device on the next boot).
func webTokenStatePath(isGlobalMode bool, workingDir string) string {
	if isGlobalMode {
		return filepath.Join(projectfile.GlobalConfigDir(), "web_token")
	}
	return filepath.Join(workingDir, ".gogen", "web_token")
}

// loadOrCreateWebToken returns the persisted web auth token, generating and
// persisting a fresh one when none exists. Persisting the token is what
// keeps already-paired devices logged in across restarts (the token never
// rotates unless the user sets their own). A read error other than
// "not exist" or a persistence failure returns an error — callers keep
// booting with an ephemeral token in that case.
func loadOrCreateWebToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
		// Empty or whitespace-only file: treat as absent and regenerate.
	case os.IsNotExist(err):
		// Generate below.
	default:
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	tok, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	if err := writeWebToken(path, tok); err != nil {
		return "", fmt.Errorf("persist %s: %w", path, err)
	}
	return tok, nil
}

// writeWebToken persists tok to path atomically with 0600 permissions so
// other users cannot read the credential. It delegates to
// ioutil.WriteFileAtomic (temp file + fsync + rename), so a crash mid-write
// cannot corrupt the token and the shared helper's chmod-unsupported handling
// is reused. The parent dir is created with 0700.
func writeWebToken(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return ioutil.WriteFileAtomic(path, []byte(tok+"\n"), 0o600)
}
