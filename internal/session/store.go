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
	"sync"
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
	Oneshot        bool            `json:"oneshot,omitempty"`
	TokenCounts    []int           `json:"tokenCounts,omitempty"`
	ContextLimit   int             `json:"contextLimit,omitempty"`
}

// deltaFile holds messages appended since the last full snapshot save.
type deltaFile struct {
	Messages    []llm.Message `json:"messages"`
	TokenCounts []int         `json:"tokenCounts,omitempty"`
}

// sessionIndexEntry is a lightweight entry in the session index file.
type sessionIndexEntry struct {
	ID           string    `json:"id"`
	Updated      time.Time `json:"updated"`
	Oneshot      bool      `json:"oneshot,omitempty"`
	MessageCount int       `json:"messageCount"`
	Label        string    `json:"label,omitempty"`
	RawLabel     string    `json:"rawLabel,omitempty"`
}

// sessionIndex is the on-disk index of session metadata for fast listing.
type sessionIndex struct {
	Entries []sessionIndexEntry `json:"entries"`
}

// Store persists sessions under .gogen/sessions/.
//
// Save/Load/Delete operations are externally serialized by the caller (turnMu
// in the web server). List may be called concurrently from the WS read loop,
// so the in-memory list cache is protected by a mutex. The metadata index
// file is protected by the same mutex to avoid concurrent reads/writes.
type Store struct {
	enabled      bool
	maxCount     int
	maxAgeDays   int
	createdCache map[string]time.Time // sessionID → Created timestamp (avoids re-read)
	saveCount    int                  // counter for periodic pruning
	globalDir    string               // non-empty: override per-project .gogen/sessions/ dir
}

// listCacheEntry holds a cached session list for one working directory.
type listCacheEntry struct {
	info []agent.SessionInfo
	time time.Time
}

var (
	listCache   = make(map[string]listCacheEntry) // workingDir → cache
	listCacheMu sync.RWMutex
)

// StoreOptions configures retention for persisted sessions.
type StoreOptions struct {
	MaxCount   int // keep at most this many sessions (0 = default 50)
	MaxAgeDays int // drop sessions older than this many days (0 = default 30)
}

// maxCreatedCacheEntries limits the in-memory created-timestamp cache so it
// cannot grow unboundedly on long-running processes with many sessions.
const maxCreatedCacheEntries = 200

// NewStore creates a session store with default retention.
func NewStore(enabled bool) *Store {
	return NewStoreWithOptions(enabled, StoreOptions{})
}

// SetGlobalDir configures the store to use a fixed directory for session
// storage instead of the per-project .gogen/sessions/. Used in global mode.
func (s *Store) SetGlobalDir(dir string) {
	if s != nil {
		s.globalDir = dir
	}
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
	return &Store{enabled: enabled, maxCount: maxCount, maxAgeDays: maxAge, createdCache: make(map[string]time.Time)}
}

// setCreatedCache adds an entry to the created-timestamp cache, evicting the
// oldest entry if the cache exceeds maxCreatedCacheEntries to prevent
// unbounded memory growth on long-running processes.
func (s *Store) setCreatedCache(id string, created time.Time) {
	if s == nil {
		return
	}
	if len(s.createdCache) >= maxCreatedCacheEntries {
		// Evict the first (oldest) entry.
		for k := range s.createdCache {
			delete(s.createdCache, k)
			break
		}
	}
	s.createdCache[id] = created
}

func (s *Store) dir(workingDir string) string {
	if s != nil && s.globalDir != "" {
		return s.globalDir
	}
	return filepath.Join(workingDir, ".gogen", "sessions")
}

func (s *Store) path(workingDir, id string) string {
	if s != nil && s.globalDir != "" {
		// In global mode, sessions are stored flat in the global dir.
		return filepath.Join(s.globalDir, id+".json")
	}
	return filepath.Join(s.dir(workingDir), id+".json")
}

// deltaPath returns the path to the delta file for a session.
func (s *Store) deltaPath(workingDir, id string) string {
	if s != nil && s.globalDir != "" {
		return filepath.Join(s.globalDir, id+".delta")
	}
	return filepath.Join(s.dir(workingDir), id+".delta")
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
	if err := ensureUnderSessionsDir(snap.WorkingDir, path, s.globalDir); err != nil {
		return err
	}
	created := time.Now().UTC()
	if cached, ok := s.createdCache[id]; ok {
		created = cached
	} else if data, err := os.ReadFile(path); err == nil {
		// Cache miss (e.g. first save after process restart before Load):
		// preserve Created from the existing file instead of resetting it.
		var prev file
		if err := json.Unmarshal(data, &prev); err == nil && !prev.Created.IsZero() {
			created = prev.Created
		}
	}
	out := file{
		Version:        version,
		ID:             id,
		Created:        created,
		Updated:        time.Now().UTC(),
		WorkingDir:     snap.WorkingDir,
		Model:          snap.Model,
		Mode:           snap.Mode,
		Label:          snap.Label,
		ProjectProfile: snap.ProjectProfile,
		Todos:          snap.Todos,
		Messages:       snap.Messages,
		Oneshot:        snap.Oneshot,
		TokenCounts:    snap.TokenCounts,
		ContextLimit:   snap.ContextLimit,
	}
	// Remove stale delta — the full snapshot supersedes it.
	_ = s.clearDeltaFile(snap.WorkingDir, id)

	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	s.setCreatedCache(id, created)
	s.saveCount++
	// Prune every 3 saves to avoid repeated directory scans on every write.
	if s.saveCount%3 == 0 {
		s.prune(snap.WorkingDir, id)
	}
	// Update index and invalidate in-memory cache.
	label := sessionLabel(snap.Messages, snap.Label)
	rawLabel := llm.FirstUserMessage(snap.Messages)
	s.updateIndex(snap.WorkingDir, id, out.Updated, len(snap.Messages), label, snap.Oneshot, rawLabel)
	return nil
}

// TouchSession updates only the session's timestamp metadata without
// rewriting the full message payload. Uses file mtime plus the index file.
func (s *Store) TouchSession(workingDir, id string) error {
	if s == nil || !s.enabled || id == "" {
		return nil
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	return s.touchIndex(workingDir, id, now)
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
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
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
	if s != nil && !f.Created.IsZero() {
		s.setCreatedCache(id, f.Created)
	}

	// Merge any delta file (messages appended since last full snapshot).
	snap, deltaErr := s.loadDelta(workingDir, id)
	if deltaErr == nil && snap.Messages != nil {
		f.Messages = append(f.Messages, snap.Messages...)
		if len(snap.TokenCounts) > 0 {
			f.TokenCounts = append(f.TokenCounts, snap.TokenCounts...)
		}
		// Delta consumed — remove it so we don't double-merge on next load.
		_ = s.clearDeltaFile(workingDir, id)
	}

	return agent.SessionSnapshot{
		WorkingDir:     f.WorkingDir,
		Model:          f.Model,
		Mode:           f.Mode,
		Oneshot:        f.Oneshot,
		Label:          f.Label,
		ProjectProfile: f.ProjectProfile,
		Todos:          f.Todos,
		Messages:       f.Messages,
		TokenCounts:    f.TokenCounts,
		ContextLimit:   f.ContextLimit,
	}, nil
}

// AppendMessages writes new messages to the delta file for incremental saves.
// The delta file is a lightweight JSON file that holds messages appended since
// the last full snapshot. On restore, the delta is merged into the full snapshot.
// This avoids rewriting the entire (potentially large) message list on every save.
func (s *Store) AppendMessages(id string, snap agent.SessionSnapshot) error {
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
	path := s.deltaPath(snap.WorkingDir, id)
	if err := ensureUnderSessionsDir(snap.WorkingDir, path, s.globalDir); err != nil {
		return err
	}

	df := deltaFile{
		Messages:    snap.Messages,
		TokenCounts: snap.TokenCounts,
	}
	data, err := json.Marshal(df)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return err
	}

	// Update the main file's mtime so the session appears recent in listings.
	mainPath := s.path(snap.WorkingDir, id)
	_ = os.Chtimes(mainPath, time.Now(), time.Now())

	// Update index timestamp so the session appears recent in listings.
	// The message count from the last full snapshot remains correct until
	// the next full save overwrites it.
	s.touchIndex(snap.WorkingDir, id, time.Now().UTC())
	return nil
}

// loadDelta reads and returns the delta file for a session, if it exists.
func (s *Store) loadDelta(workingDir, id string) (deltaFile, error) {
	path := s.deltaPath(workingDir, id)
	data, err := os.ReadFile(path)
	if err != nil {
		return deltaFile{}, err
	}
	var df deltaFile
	if err := json.Unmarshal(data, &df); err != nil {
		return deltaFile{}, err
	}
	return df, nil
}

// clearDeltaFile removes the delta file for a session.
func (s *Store) clearDeltaFile(workingDir, id string) error {
	return os.Remove(s.deltaPath(workingDir, id))
}

// List returns session info for a working directory, ordered by most recently
// updated first. Uses the metadata index when available, falling back to a
// full-file scan for legacy directories. Results are cached briefly in memory
// to avoid repeated disk I/O on reconnects.
func (s *Store) List(workingDir string) ([]agent.SessionInfo, error) {
	if s == nil || !s.enabled {
		return nil, nil
	}

	// Check in-memory cache first (1-second TTL).
	listCacheMu.RLock()
	if ce, ok := listCache[workingDir]; ok && time.Since(ce.time) < time.Second {
		out := make([]agent.SessionInfo, len(ce.info))
		copy(out, ce.info)
		listCacheMu.RUnlock()
		return out, nil
	}
	listCacheMu.RUnlock()

	// Try the metadata index file.
	idx := s.readIndex(workingDir)
	if idx != nil && len(idx.Entries) > 0 {
		// Migration: sessions saved before the RawLabel field existed won't have
		// it in the index. For those, load the session file to compute it.
		needsRewrite := false
		for i, e := range idx.Entries {
			if e.RawLabel == "" && e.Label != "" {
				if raw := s.loadRawLabel(workingDir, e.ID); raw != "" {
					idx.Entries[i].RawLabel = raw
					needsRewrite = true
				}
			}
		}
		if needsRewrite {
			_ = s.writeIndex(workingDir, idx)
		}
		sort.Slice(idx.Entries, func(i, j int) bool {
			return idx.Entries[i].Updated.After(idx.Entries[j].Updated)
		})
		out := make([]agent.SessionInfo, len(idx.Entries))
		for i, e := range idx.Entries {
			out[i] = agent.SessionInfo{
				ID:           e.ID,
				Oneshot:      e.Oneshot,
				UpdatedAt:    e.Updated.UTC().Format(time.RFC3339Nano),
				MessageCount: e.MessageCount,
				Label:        e.Label,
				RawLabel:     e.RawLabel,
			}
		}
		listCacheMu.Lock()
		listCache[workingDir] = listCacheEntry{info: out, time: time.Now()}
		listCacheMu.Unlock()
		return out, nil
	}

	// Fallback: legacy full-file scan (no index file).
	idx = &sessionIndex{} // collect entries to build the index
	entries, err := os.ReadDir(s.dir(workingDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		info    agent.SessionInfo
		updated time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Skip the index file if somehow named .json
		if e.Name() == "index.json" {
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
		lbl := sessionLabel(f.Messages, f.Label)
		rawLabel := llm.FirstUserMessage(f.Messages)
		items = append(items, item{
			info: agent.SessionInfo{
				ID:           id,
				Oneshot:      f.Oneshot,
				UpdatedAt:    f.Updated.UTC().Format(time.RFC3339Nano),
				MessageCount: len(f.Messages),
				Label:        lbl,
				RawLabel:     rawLabel,
			},
			updated: f.Updated,
		})
		idx.Entries = append(idx.Entries, sessionIndexEntry{
			ID: id, Updated: f.Updated, Oneshot: f.Oneshot, MessageCount: len(f.Messages), Label: lbl, RawLabel: rawLabel,
		})
	}
	// Persist the index for next time (best-effort).
	if len(idx.Entries) > 0 {
		sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Updated.After(idx.Entries[j].Updated) })
		_ = s.writeIndex(workingDir, idx)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].updated.After(items[j].updated) })
	out := make([]agent.SessionInfo, len(items))
	for i, it := range items {
		out[i] = it.info
	}

	// Populate in-memory cache.
	listCacheMu.Lock()
	listCache[workingDir] = listCacheEntry{info: out, time: time.Now()}
	listCacheMu.Unlock()

	return out, nil
}

// LatestID returns the most recently updated session id.
// Uses the Updated field in each session JSON (not file mtime), so copied or
// restored files cannot displace the true latest. Only the updated timestamp
// is decoded — messages and other fields are skipped for a cheap scan.
func (s *Store) LatestID(workingDir string) (string, error) {
	// Fast path: use the metadata index file (avoids reading every session file).
	idx := s.readIndex(workingDir)
	if idx != nil && len(idx.Entries) > 0 {
		var latestID string
		var latestUpdated time.Time
		for _, e := range idx.Entries {
			if e.Updated.After(latestUpdated) {
				latestUpdated = e.Updated
				latestID = e.ID
			}
		}
		if latestID != "" {
			return latestID, nil
		}
	}

	// Fallback: scan all session files (legacy directories without index).
	dir := s.dir(workingDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Already tried index above; nothing more to do.
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
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session not found: %s", id)
		}
		return err
	}
	delete(s.createdCache, id)
	// Remove from index and invalidate in-memory cache.
	s.removeFromIndex(workingDir, id)
	s.invalidateListCache(workingDir)
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

func ensureUnderSessionsDir(workingDir, path string, globalDirs ...string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Check against global dir first if provided.
	for _, gd := range globalDirs {
		if gd == "" {
			continue
		}
		gdAbs, err := filepath.Abs(gd)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(gdAbs, absPath)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	// Fall back to project-local sessions dir.
	sessionsDir, err := filepath.Abs(filepath.Join(workingDir, ".gogen", "sessions"))
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

// writeFileAtomic is a convenience wrapper around ioutil.WriteFileAtomicNoSync.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return ioutil.WriteFileAtomicNoSync(path, data, perm)
}

// indexFile returns the path to the metadata index for a working directory.
func (s *Store) indexFile(workingDir string) string {
	return filepath.Join(s.dir(workingDir), "index.json")
}

// readIndex loads the session metadata index from disk. Returns nil if the
// file does not exist or is corrupt.
func (s *Store) readIndex(workingDir string) *sessionIndex {
	data, err := os.ReadFile(s.indexFile(workingDir))
	if err != nil {
		return nil
	}
	var idx sessionIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		log.Printf("warning: corrupt session index, rebuilding: %v", err)
		return nil
	}
	return &idx
}

// loadRawLabel loads a session file and returns the full first user message.
func (s *Store) loadRawLabel(workingDir, id string) string {
	data, err := os.ReadFile(s.path(workingDir, id))
	if err != nil {
		return ""
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	if len(f.Messages) == 0 {
		return ""
	}
	return llm.FirstUserMessage(f.Messages)
}

// writeIndex writes the session metadata index atomically.
func (s *Store) writeIndex(workingDir string, idx *sessionIndex) error {
	if idx == nil {
		return nil
	}
	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(workingDir), 0o700); err != nil {
		return err
	}
	indexPath := s.indexFile(workingDir)
	if err := ensureUnderSessionsDir(workingDir, indexPath, s.globalDir); err != nil {
		return err
	}
	return writeFileAtomic(indexPath, data, 0o600)
}

// updateIndex adds or updates an entry in the session metadata index.
func (s *Store) updateIndex(workingDir, id string, updated time.Time, msgCount int, label string, oneshot bool, rawLabel string) {
	if s == nil || !s.enabled {
		return
	}
	idx := s.readIndex(workingDir)
	if idx == nil {
		idx = &sessionIndex{}
	}
	found := false
	for i, e := range idx.Entries {
		if e.ID == id {
			idx.Entries[i].Updated = updated
			idx.Entries[i].Oneshot = oneshot
			idx.Entries[i].MessageCount = msgCount
			idx.Entries[i].Label = label
			idx.Entries[i].RawLabel = rawLabel
			found = true
			break
		}
	}
	if !found {
		idx.Entries = append(idx.Entries, sessionIndexEntry{
			ID: id, Updated: updated, Oneshot: oneshot, MessageCount: msgCount, Label: label, RawLabel: rawLabel,
		})
	}
	_ = s.writeIndex(workingDir, idx)
	s.invalidateListCache(workingDir)
}

// touchIndex updates only the timestamp for a session in the index.
func (s *Store) touchIndex(workingDir, id string, updated time.Time) error {
	if s == nil || !s.enabled {
		return nil
	}
	idx := s.readIndex(workingDir)
	if idx == nil {
		// No index yet; nothing to touch.
		return nil
	}
	found := false
	for i, e := range idx.Entries {
		if e.ID == id {
			idx.Entries[i].Updated = updated
			found = true
			break
		}
	}
	if !found {
		// Entry not in index; skip. The list fallback will re-scan on next miss.
		return nil
	}
	if err := s.writeIndex(workingDir, idx); err != nil {
		return err
	}
	s.invalidateListCache(workingDir)
	return nil
}

// removeFromIndex deletes an entry from the session metadata index.
func (s *Store) removeFromIndex(workingDir, id string) {
	if s == nil || !s.enabled {
		return
	}
	idx := s.readIndex(workingDir)
	if idx == nil {
		return
	}
	filtered := idx.Entries[:0]
	for _, e := range idx.Entries {
		if e.ID != id {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == len(idx.Entries) {
		return // nothing removed
	}
	idx.Entries = filtered
	_ = s.writeIndex(workingDir, idx)
}

// removeFromIndexBatch removes multiple entries from the session metadata index
// in a single pass, rewriting the index file only once.
func (s *Store) removeFromIndexBatch(workingDir string, ids []string) {
	if s == nil || !s.enabled || len(ids) == 0 {
		return
	}
	idx := s.readIndex(workingDir)
	if idx == nil {
		return
	}
	del := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		del[id] = struct{}{}
	}
	filtered := make([]sessionIndexEntry, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		if _, ok := del[e.ID]; !ok {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == len(idx.Entries) {
		return
	}
	idx.Entries = filtered
	_ = s.writeIndex(workingDir, idx)
}

// invalidateListCache clears the in-memory list cache for a working directory.
func (s *Store) invalidateListCache(workingDir string) {
	listCacheMu.Lock()
	delete(listCache, workingDir)
	listCacheMu.Unlock()
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

// prune deletes expired and excess sessions, always retaining keepID
// (the current session). Uses the Updated field from the session index or
// session JSON, not file mtime, to be consistent with LatestID.
// Deletions are batched so the index is rewritten only once.
func (s *Store) prune(workingDir, keepID string) {
	if s == nil || !s.enabled {
		return
	}

	type item struct {
		id      string
		updated time.Time
	}
	var items []item

	// Prefer the metadata index (fast, no per-file I/O).
	idx := s.readIndex(workingDir)
	if idx != nil && len(idx.Entries) > 0 {
		for _, e := range idx.Entries {
			items = append(items, item{id: e.ID, updated: e.Updated})
		}
	} else {
		// Fallback: read updated from each session JSON file.
		entries, err := os.ReadDir(s.dir(workingDir))
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			data, err := os.ReadFile(s.path(workingDir, id))
			if err != nil {
				continue
			}
			var meta struct {
				Updated time.Time `json:"updated"`
			}
			if err := json.Unmarshal(data, &meta); err != nil || meta.Updated.IsZero() {
				continue
			}
			items = append(items, item{id: id, updated: meta.Updated.UTC()})
		}
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
	var toDelete []string
	others := 0
	for _, it := range items {
		if it.id == keepID {
			continue
		}
		expired := it.updated.Before(cutoff)
		if expired || others >= otherBudget {
			toDelete = append(toDelete, it.id)
			continue
		}
		others++
	}

	// Batch-delete without rewriting the index per file.
	for _, id := range toDelete {
		path := s.path(workingDir, id)
		_ = os.Remove(path)
		delete(s.createdCache, id)
		_ = s.clearDeltaFile(workingDir, id)
	}
	if len(toDelete) > 0 {
		s.removeFromIndexBatch(workingDir, toDelete)
		s.invalidateListCache(workingDir)
	}
}
