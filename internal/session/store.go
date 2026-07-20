package session

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/ioutil"
	"gogen/internal/llm"
)

const version = 1

type file struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	Created        time.Time       `json:"created"`
	Updated        time.Time       `json:"updated"`
	WorkingDir     string          `json:"workingDir"`
	Model          string          `json:"model"`
	Mode           string          `json:"mode"`
	Label          string          `json:"label,omitempty"`
	ProjectProfile string          `json:"projectProfile,omitempty"`
	Todos          *agent.TodoList `json:"todos,omitempty"`
	Messages       []llm.Message   `json:"messages"`
}

// Store persists sessions under .gogen/sessions/.
type Store struct {
	enabled    bool
	maxCount   int
	maxAgeDays int
}

// StoreOptions configures retention for persisted sessions.
type StoreOptions struct {
	MaxCount   int // keep at most this many sessions (0 = default 50)
	MaxAgeDays int // drop sessions older than this many days (0 = default 30)
}

// NewStore creates a session store with default retention.
func NewStore(enabled bool) *Store {
	return NewStoreWithOptions(enabled, StoreOptions{})
}

// NewStoreWithOptions creates a session store with custom retention.
func NewStoreWithOptions(enabled bool, opts StoreOptions) *Store {
	maxCount := opts.MaxCount
	if maxCount <= 0 {
		maxCount = 50
	}
	maxAge := opts.MaxAgeDays
	if maxAge <= 0 {
		maxAge = 30
	}
	return &Store{enabled: enabled, maxCount: maxCount, maxAgeDays: maxAge}
}

func (s *Store) dir(workingDir string) string {
	return filepath.Join(workingDir, ".gogen", "sessions")
}

func (s *Store) path(workingDir, id string) string {
	return filepath.Join(s.dir(workingDir), id+".json")
}

// Save writes a session snapshot.
func (s *Store) Save(id string, snap agent.SessionSnapshot) error {
	if s == nil || !s.enabled || id == "" {
		return nil
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	dir := s.dir(snap.WorkingDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := s.path(snap.WorkingDir, id)
	if err := ensureUnderSessionsDir(snap.WorkingDir, path); err != nil {
		return err
	}
	existing := file{Created: time.Now().UTC()}
	if data, err := os.ReadFile(path); err == nil {
		var prev file
		if err := json.Unmarshal(data, &prev); err != nil {
			log.Printf("session save: ignoring corrupt session file %s: %v", path, err)
		} else if !prev.Created.IsZero() {
			existing.Created = prev.Created
		}
	}
	out := file{
		Version:        version,
		ID:             id,
		Created:        existing.Created,
		Updated:        time.Now().UTC(),
		WorkingDir:     snap.WorkingDir,
		Model:          snap.Model,
		Mode:           snap.Mode,
		Label:          snap.Label,
		ProjectProfile: snap.ProjectProfile,
		Todos:          snap.Todos,
		Messages:       snap.Messages,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	s.prune(snap.WorkingDir, id)
	return nil
}

// LoadInWorkingDir loads a session from a working directory.
func (s *Store) LoadInWorkingDir(workingDir, id string) (agent.SessionSnapshot, error) {
	if s == nil || !s.enabled {
		return agent.SessionSnapshot{}, fmt.Errorf("session persistence disabled")
	}
	if err := validateSessionID(id); err != nil {
		return agent.SessionSnapshot{}, err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path); err != nil {
		return agent.SessionSnapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.SessionSnapshot{}, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return agent.SessionSnapshot{}, err
	}
	return agent.SessionSnapshot{
		WorkingDir:     f.WorkingDir,
		Model:          f.Model,
		Mode:           f.Mode,
		Label:          f.Label,
		ProjectProfile: f.ProjectProfile,
		Todos:          f.Todos,
		Messages:       f.Messages,
	}, nil
}

// List returns session ids for a working directory.
func (s *Store) List(workingDir string) ([]agent.SessionInfo, error) {
	if s == nil || !s.enabled {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir(workingDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []agent.SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		data, err := os.ReadFile(s.path(workingDir, id))
		if err != nil {
			continue
		}
		var f file
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		entry := agent.SessionInfo{
			ID:           id,
			UpdatedAt:    f.Updated.UTC().Format(time.RFC3339),
			MessageCount: len(f.Messages),
			Label:        sessionLabel(f.Messages, f.Label),
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// LatestID returns the most recently updated session id.
// Uses the Updated field in each session JSON (not file mtime), so copied or
// restored files cannot displace the true latest. Only the updated timestamp
// is decoded — messages and other fields are skipped for a cheap scan.
func (s *Store) LatestID(workingDir string) (string, error) {
	dir := s.dir(workingDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var latestID string
	var latestUpdated time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			Updated time.Time `json:"updated"`
		}
		if err := json.Unmarshal(data, &meta); err != nil || meta.Updated.IsZero() {
			continue
		}
		if meta.Updated.After(latestUpdated) {
			latestUpdated = meta.Updated
			latestID = strings.TrimSuffix(e.Name(), ".json")
		}
	}
	return latestID, nil
}

// Delete removes a saved session file.
func (s *Store) Delete(workingDir, id string) error {
	if s == nil || !s.enabled {
		return fmt.Errorf("session persistence disabled")
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session not found: %s", id)
		}
		return err
	}
	return nil
}

func validateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("session id is required")
	}
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id")
	}
	if id != filepath.Base(id) {
		return fmt.Errorf("invalid session id")
	}
	return nil
}

func ensureUnderSessionsDir(workingDir, path string) error {
	sessionsDir, err := filepath.Abs(filepath.Join(workingDir, ".gogen", "sessions"))
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(sessionsDir, absPath)
	if err != nil {
		return fmt.Errorf("invalid session path")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid session path")
	}
	return nil
}

// writeFileAtomic is a convenience wrapper around ioutil.WriteFileAtomic.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return ioutil.WriteFileAtomic(path, data, perm)
}

func sessionLabel(messages []llm.Message, label string) string {
	if label != "" {
		return label
	}
	return llm.SessionLabel(messages, llm.DefaultSessionLabelMaxLen)
}

// NewID generates a new session id.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

// prune deletes expired and excess sessions, always retaining keepID.
// Uses file mtimes so it does not need to open or parse any session files.
func (s *Store) prune(workingDir, keepID string) {
	if s == nil || !s.enabled {
		return
	}
	entries, err := os.ReadDir(s.dir(workingDir))
	if err != nil {
		return
	}
	type item struct {
		id      string
		updated time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{
			id:      strings.TrimSuffix(e.Name(), ".json"),
			updated: info.ModTime().UTC(),
		})
	}
	if len(items) == 0 {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].updated.After(items[j].updated) })

	cutoff := time.Now().UTC().AddDate(0, 0, -s.maxAgeDays)
	otherBudget := s.maxCount
	if keepID != "" {
		otherBudget--
		if otherBudget < 0 {
			otherBudget = 0
		}
	}
	others := 0
	for _, it := range items {
		if it.id == keepID {
			continue
		}
		expired := it.updated.Before(cutoff)
		if expired || others >= otherBudget {
			_ = s.Delete(workingDir, it.id)
			continue
		}
		others++
	}
}
