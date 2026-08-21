package session

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gogen/internal/agent"
)

// archivePath returns the path to the archive sidecar for a session: the
// session file with its ".json" suffix replaced by ".archive.jsonl" (the
// Phase 5 compaction archive — an append-only JSONL of content shadowed out
// of the live history).
func (s *Store) archivePath(workingDir, id string) string {
	base := strings.TrimSuffix(filepath.Base(s.path(workingDir, id)), ".json")
	return filepath.Join(filepath.Dir(s.path(workingDir, id)), base+".archive.jsonl")
}

// AppendArchive appends one entry to the session's archive sidecar (Phase 5
// compaction archive). The sidecar is append-only JSONL — one entry per
// line — so a torn final line from a crash cannot corrupt earlier entries
// (each line is self-contained JSON). The sidecar is created on first use
// and removed with the session (Delete / prune / nested cascade). It is
// deliberately NOT part of the session snapshot: restoring a session
// replays the LIVE history, and the archive exists only so shadowed content
// is recoverable after the fact.
func (s *Store) AppendArchive(workingDir, id string, entry agent.ArchiveEntry) error {
	if !s.enabled || id == "" {
		return fmt.Errorf("session persistence disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	dir := s.dir(workingDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := s.archivePath(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// removeArchiveFile removes a session's archive sidecar. Callers must hold
// s.mu. A missing sidecar is not an error (most sessions never shadow
// anything).
func (s *Store) removeArchiveFile(workingDir, id string) {
	if err := os.Remove(s.archivePath(workingDir, id)); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: failed to remove archive sidecar for session %s: %v", id, err)
	}
}
