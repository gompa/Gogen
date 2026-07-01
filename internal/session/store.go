package session

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

const version = 1

type file struct {
	Version    int           `json:"version"`
	ID         string        `json:"id"`
	Created    time.Time     `json:"created"`
	Updated    time.Time     `json:"updated"`
	WorkingDir string        `json:"workingDir"`
	Model      string        `json:"model"`
	Mode       string        `json:"mode"`
	Messages   []llm.Message `json:"messages"`
}

// Store persists sessions under .gogen/sessions/.
type Store struct {
	enabled bool
}

// NewStore creates a session store.
func NewStore(enabled bool) *Store {
	return &Store{enabled: enabled}
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
	dir := s.dir(snap.WorkingDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := s.path(snap.WorkingDir, id)
	existing := file{Created: time.Now().UTC()}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	out := file{
		Version:    version,
		ID:         id,
		Created:    existing.Created,
		Updated:    time.Now().UTC(),
		WorkingDir: snap.WorkingDir,
		Model:      snap.Model,
		Mode:       snap.Mode,
		Messages:   snap.Messages,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadInWorkingDir loads a session from a working directory.
func (s *Store) LoadInWorkingDir(workingDir, id string) (agent.SessionSnapshot, error) {
	if s == nil || !s.enabled {
		return agent.SessionSnapshot{}, fmt.Errorf("session persistence disabled")
	}
	data, err := os.ReadFile(s.path(workingDir, id))
	if err != nil {
		return agent.SessionSnapshot{}, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return agent.SessionSnapshot{}, err
	}
	return agent.SessionSnapshot{
		WorkingDir: f.WorkingDir,
		Model:      f.Model,
		Mode:       f.Mode,
		Messages:   f.Messages,
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
			Label:        llm.SessionLabel(f.Messages, llm.DefaultSessionLabelMaxLen),
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// LatestID returns the most recently updated session id.
func (s *Store) LatestID(workingDir string) (string, error) {
	list, err := s.List(workingDir)
	if err != nil || len(list) == 0 {
		return "", err
	}
	return list[0].ID, nil
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
	return nil
}

// NewID generates a new session id.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
