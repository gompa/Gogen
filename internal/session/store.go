package session

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/ioutil"
	"gogen/internal/llm"
	"gogen/internal/randhex"
)

const version = 1

// legacyLabelMaxLen is the label length older versions truncated to. Stored
// labels of exactly this length are the only ones that may need migration to
// the full untruncated label, so List can skip re-reading session files for
// every other entry.
const legacyLabelMaxLen = 50

type file struct {
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	WorkingDir    string    `json:"workingDir"`
	Model         string    `json:"model"`
	Mode          string    `json:"mode"`
	ThinkingLevel string    `json:"thinkingLevel,omitempty"`
	Label         string    `json:"label,omitempty"`
	// LabelRenamed records that Label was set deliberately (RenameSession /
	// session_rename tool) rather than derived from the first user message.
	// The store must never regenerate a renamed label — not even one that
	// looks like a legacy 50-char truncation (see sessionLabel).
	LabelRenamed   bool            `json:"labelRenamed,omitempty"`
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
	// BaseCount is the number of messages in the full snapshot this delta
	// was written against (the message index where Messages starts). Load
	// uses it to tell "snapshot is the delta's base" (merge) from "snapshot
	// already contains the delta" (stale) and "snapshot was truncated after
	// the delta was written" (drop). Zero means a legacy delta predating
	// this field; legacy deltas are merged unconditionally.
	BaseCount int `json:"baseCount,omitempty"`
}

// sessionIndexEntry is a lightweight entry in the session index file.
type sessionIndexEntry struct {
	ID           string    `json:"id"`
	Created      time.Time `json:"created,omitempty"`
	Updated      time.Time `json:"updated"`
	Oneshot      bool      `json:"oneshot,omitempty"`
	MessageCount int       `json:"messageCount"`
	Label        string    `json:"label,omitempty"`
	// LabelRenamed mirrors the session file's marker (see file.LabelRenamed)
	// so List can keep a deliberate rename authoritative without re-reading
	// the session file.
	LabelRenamed bool `json:"labelRenamed,omitempty"`
}

// sessionIndex is the on-disk index of session metadata for fast listing.
type sessionIndex struct {
	Entries []sessionIndexEntry `json:"entries"`
}

// Store persists sessions under .gogen/sessions/.
//
// All public operations (Save, AppendMessages, LoadInWorkingDir, Delete,
// TouchSession, LatestID, List, Prune) are serialized internally by mu so
// multiple goroutines (multi-session web server, TUI persist goroutine) can
// write concurrently without corrupting index.json or createdCache. List may
// be called concurrently and is safe under mu; the in-memory list cache keeps
// its own lock so hot reads do not serialize on the store mutex.
type Store struct {
	mu           sync.Mutex
	enabled      bool
	maxCount     int
	maxAgeDays   int
	createdCache map[string]time.Time // sessionID → Created timestamp (avoids re-read)
	saveCount    int                  // counter for periodic pruning
	autoPrune    bool                 // internal prune on Save; disabled when a registry owns pruning
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
	MaxCount   int // keep at most this many sessions (0 = config.DefaultSessionMaxCount)
	MaxAgeDays int // drop sessions older than this many days (0 = config.DefaultSessionMaxAgeDays)
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
	s.mu.Lock()
	s.globalDir = dir
	s.mu.Unlock()
}

// SetAutoPrune toggles the internal prune that Save performs every few
// writes. The TUI keeps it enabled (single active session — pruning around
// the current session is correct). The multi-session web server disables it
// and becomes the sole pruner via Prune, so a Save from one active session
// can never delete another live session's file. Default is enabled.
func (s *Store) SetAutoPrune(enabled bool) {
	s.mu.Lock()
	s.autoPrune = enabled
	s.mu.Unlock()
}

// NewStoreWithOptions creates a session store with custom retention.
func NewStoreWithOptions(enabled bool, opts StoreOptions) *Store {
	maxCount := config.Effective(opts.MaxCount, config.DefaultSessionMaxCount)
	maxAge := config.Effective(opts.MaxAgeDays, config.DefaultSessionMaxAgeDays)
	return &Store{enabled: enabled, maxCount: maxCount, maxAgeDays: maxAge, createdCache: make(map[string]time.Time), autoPrune: true}
}

// setCreatedCache adds an entry to the created-timestamp cache, evicting the
// entry with the oldest Created timestamp if the cache exceeds
// maxCreatedCacheEntries to prevent unbounded memory growth on long-running
// processes. (Go map iteration is unordered, so the oldest must be found by
// comparing timestamps rather than "first entry".)
//
// Callers must hold s.mu (all callers are Store methods that already guard
// against a nil receiver).
func (s *Store) setCreatedCache(id string, created time.Time) {
	if len(s.createdCache) >= maxCreatedCacheEntries {
		var oldestID string
		var oldest time.Time
		for k, v := range s.createdCache {
			if oldestID == "" || v.Before(oldest) {
				oldestID, oldest = k, v
			}
		}
		if oldestID != "" {
			delete(s.createdCache, oldestID)
		}
	}
	s.createdCache[id] = created
}

func (s *Store) dir(workingDir string) string {
	if s.globalDir != "" {
		return s.globalDir
	}
	return filepath.Join(workingDir, ".gogen", "sessions")
}

func (s *Store) path(workingDir, id string) string {
	if s.globalDir != "" {
		// In global mode, sessions are stored flat in the global dir.
		return filepath.Join(s.globalDir, id+".json")
	}
	return filepath.Join(s.dir(workingDir), id+".json")
}

// deltaPath returns the path to the delta file for a session.
func (s *Store) deltaPath(workingDir, id string) string {
	if s.globalDir != "" {
		return filepath.Join(s.globalDir, id+".delta")
	}
	return filepath.Join(s.dir(workingDir), id+".delta")
}

// Save writes a session snapshot.
func (s *Store) Save(id string, snap agent.SessionSnapshot) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// Skip persisting a session that has never had content and has no
	// on-disk state yet: /new panes that are never used are flushed on
	// server quit (ShutdownSessions flushes every registered runtime), and
	// writing a 0-message file + index entry for each one would bloat the
	// saved-session list with empty sessions after every restart (the
	// sidebar renders a row per index entry; a 0-message row shows no
	// count — messageCount is omitempty, so zero is absent from the
	// payload). A session that
	// WAS saved before and was rolled back to empty must still update its
	// file — the old content was deliberately dropped — so the skip applies
	// only when neither the snapshot nor a pending delta exists on disk. A
	// label is the one exception: it can only be set deliberately
	// (RenameSession / the session_rename tool), so an empty session that
	// was explicitly named must be persisted — otherwise the rename is
	// silently dropped. (Oneshot single-prompt sessions are NOT an
	// exception: their pre-prompt flush must stay skipped or every `-p`
	// run would leave an empty session entry behind.)
	if len(snap.Messages) == 0 && snap.Label == "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if _, err := os.Stat(s.deltaPath(snap.WorkingDir, id)); os.IsNotExist(err) {
				return nil
			}
		}
	}
	created := time.Now().UTC()
	// preloadedIdx is the metadata index read during Created recovery
	// (cache-miss path only); updateIndex reuses it so a full save reads
	// index.json at most once.
	var preloadedIdx *sessionIndex
	if cached, ok := s.createdCache[id]; ok {
		created = cached
	} else {
		// Cache miss (e.g. first save after process restart before Load):
		// preserve Created instead of resetting it. Prefer the metadata
		// index — Created is immutable per session id and is written there
		// on every save, so the common restart case avoids re-reading (and
		// re-unmarshalling) the potentially large session file. Fall back to
		// a minimal field-only decode of the file for legacy indexes that
		// predate the created field (index-loss recovery); the minimal
		// target skips allocating the previous session's messages just to
		// recover one timestamp.
		found := false
		preloadedIdx = s.readIndex(snap.WorkingDir)
		if preloadedIdx != nil {
			for _, e := range preloadedIdx.Entries {
				if e.ID == id && !e.Created.IsZero() {
					created = e.Created
					found = true
					break
				}
			}
		}
		if !found {
			if data, err := os.ReadFile(path); err == nil {
				var prevMeta struct {
					Created time.Time `json:"created"`
				}
				if err := json.Unmarshal(data, &prevMeta); err == nil && !prevMeta.Created.IsZero() {
					created = prevMeta.Created
				}
			}
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
		ThinkingLevel:  snap.ThinkingLevel,
		Label:          snap.Label,
		LabelRenamed:   snap.LabelRenamed,
		ProjectProfile: snap.ProjectProfile,
		Todos:          snap.Todos,
		Messages:       snap.Messages,
		Oneshot:        snap.Oneshot,
		TokenCounts:    snap.TokenCounts,
		ContextLimit:   snap.ContextLimit,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFileAtomicNoSync(path, data, 0o600); err != nil {
		return err
	}
	// The full snapshot now supersedes the delta. Remove it only after the
	// snapshot write succeeded: clearing it first meant a crash or a write
	// failure between the two operations permanently lost the messages that
	// existed only in the delta. A stale delta that survives into a later
	// load is detected and dropped by LoadInWorkingDir's baseCount check.
	if err := s.clearDeltaFile(snap.WorkingDir, id); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: failed to remove delta for session %s: %v", id, err)
	}
	s.setCreatedCache(id, created)
	s.saveCount++
	// Update index and invalidate in-memory cache. Runs before prune (order
	// swap): prune always keeps the current session id, so its later index
	// rewrite cannot clobber this entry, and the preloaded index — read
	// during Created recovery and untouched since, under s.mu — is still
	// fresh here. This lets a cache-miss full save read index.json once
	// instead of twice.
	label := sessionLabel(snap.Messages, snap.Label, snap.LabelRenamed)
	s.updateIndex(snap.WorkingDir, id, out.Created, out.Updated, len(snap.Messages), label, snap.Oneshot, snap.LabelRenamed, preloadedIdx)
	// Prune every 3 saves to avoid repeated directory scans on every write.
	if s.autoPrune && s.saveCount%3 == 0 {
		s.prune(snap.WorkingDir, id)
	}
	return nil
}

// TouchSession updates only the session's timestamp metadata without
// rewriting the full message payload. Uses file mtime plus the index file.
func (s *Store) TouchSession(workingDir, id string) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// UpdatedAt returns the session's persisted Updated timestamp for workingDir:
// the metadata index entry when present, falling back to the session file's
// updated field (index-loss recovery). Returns the zero time when the session
// has no persisted state in that directory (a never-saved /new pane).
func (s *Store) UpdatedAt(workingDir, id string) time.Time {
	if !s.enabled || id == "" || workingDir == "" {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return time.Time{}
	}
	if idx := s.readIndex(workingDir); idx != nil {
		for _, e := range idx.Entries {
			if e.ID == id {
				return e.Updated
			}
		}
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return time.Time{}
	}
	if data, err := os.ReadFile(path); err == nil {
		var f file
		if json.Unmarshal(data, &f) == nil {
			// Delta-aware fallback (see sessionUpdatedAt): a delta written
			// after the last full save means the file field is stale.
			return s.sessionUpdatedAt(workingDir, id, f.Updated)
		}
	}
	return time.Time{}
}

// SetUpdatedAt rewrites the session's Updated timestamp in the metadata index
// (index file only — the session file's updated field is left as-is, the same
// accepted divergence AppendMessages/TouchSession have). Used to preserve a
// session's recency ranking across a working-directory change, where the
// relocation flush would otherwise stamp Updated=now on a session the user
// did not touch. No-op when the session has no index entry in workingDir.
func (s *Store) SetUpdatedAt(workingDir, id string, updated time.Time) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	return s.touchIndex(workingDir, id, updated)
}

// LoadInWorkingDir loads a session from a working directory.
func (s *Store) LoadInWorkingDir(workingDir, id string) (agent.SessionSnapshot, error) {
	if !s.enabled {
		return agent.SessionSnapshot{}, fmt.Errorf("session persistence disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if !f.Created.IsZero() {
		s.setCreatedCache(id, f.Created)
	}

	// Merge any delta file (messages appended since the last full snapshot).
	// The delta is deliberately NOT removed after a successful merge: it
	// stays on disk until the next full save replaces it, so a crash or an
	// early exit after restore cannot lose messages that only exist in the
	// delta. The baseCount check below also detects stale deltas (the
	// snapshot already contains them) and truncated sessions (the delta's
	// messages were deliberately dropped).
	snap, deltaErr := s.loadDelta(workingDir, id)
	if deltaErr == nil && snap.Messages != nil {
		// mergeFrom is the index into snap.Messages where the merge starts:
		// 0 merges the whole delta, k>0 merges only the delta tail (the
		// snapshot already absorbed the delta's first k messages), and -1
		// drops the delta entirely.
		mergeFrom := -1
		switch {
		case snap.BaseCount == 0:
			// Legacy delta written before baseCount was recorded. Merging
			// unconditionally (the historic behavior) double-appends when a
			// later full save already included the delta's messages — the
			// crash window Save has between the snapshot write and the delta
			// unlink, which baseCount deltas survive but legacy deltas do
			// not. The snapshot's Updated timestamp versus the delta file's
			// mtime disambiguates: a snapshot saved AFTER the delta was
			// written already contains its messages. When the timestamps
			// are ambiguous (coarse filesystems), prefer merging — the
			// historic behavior — rather than risk dropping real messages.
			if fi, err := os.Stat(s.deltaPath(workingDir, id)); err == nil && f.Updated.After(fi.ModTime()) {
				_ = s.clearDeltaFile(workingDir, id)
			} else {
				mergeFrom = 0
			}
		case len(f.Messages) == snap.BaseCount:
			// Snapshot is exactly the base the delta was written against:
			// the delta extends it.
			mergeFrom = 0
		case len(f.Messages) >= snap.BaseCount+len(snap.Messages):
			// Snapshot already contains the delta's messages (a full save
			// wrote the snapshot but crashed before removing the delta).
			_ = s.clearDeltaFile(workingDir, id)
		case len(f.Messages) > snap.BaseCount:
			// Snapshot is LONGER than the delta's base but shorter than
			// base+delta. This is the two-writer race shape: a concurrent
			// full save (another process, or an attach whose restore flush
			// landed while the registered agent still appends deltas against
			// its older base) absorbed a PREFIX of the delta into the
			// snapshot. When the snapshot's extra messages are exactly the
			// delta's leading messages, the delta's TAIL is real history
			// that exists nowhere else — merge just the tail instead of
			// dropping the delta (which previously lost those messages). A
			// content mismatch means the snapshot diverged (another writer
			// truncated/rolled back) — keep the snapshot and drop the delta,
			// the same conservative choice as the truncation case below.
			overlap := len(f.Messages) - snap.BaseCount
			if deltaPrefixMatches(f.Messages[snap.BaseCount:], snap.Messages[:overlap]) {
				mergeFrom = overlap
			} else {
				_ = s.clearDeltaFile(workingDir, id)
			}
		default:
			// Snapshot is shorter than the delta's base: the conversation
			// was truncated (compaction/rollback) and re-saved, so the
			// delta's messages were deliberately dropped.
			_ = s.clearDeltaFile(workingDir, id)
		}
		if mergeFrom >= 0 {
			if mergeFrom > 0 {
				// The snapshot already contains the delta's first
				// mergeFrom messages; append only the tail.
				f.Messages = append(f.Messages, snap.Messages[mergeFrom:]...)
				if len(snap.TokenCounts) > 0 {
					f.TokenCounts = append(f.TokenCounts, snap.TokenCounts[mergeFrom:]...)
				}
			} else {
				f.Messages = append(f.Messages, snap.Messages...)
				if len(snap.TokenCounts) > 0 {
					f.TokenCounts = append(f.TokenCounts, snap.TokenCounts...)
				}
			}
		}
	}

	return agent.SessionSnapshot{
		WorkingDir:     f.WorkingDir,
		Model:          f.Model,
		Mode:           f.Mode,
		ThinkingLevel:  f.ThinkingLevel,
		Oneshot:        f.Oneshot,
		Label:          f.Label,
		LabelRenamed:   f.LabelRenamed,
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
// totalMsgCount is the agent's total message count at save time; because the
// delta is cumulative, it is the correct index count between full snapshots.
// BaseCount (totalMsgCount minus the delta length) records the message count
// of the snapshot the delta extends, letting loads detect stale deltas.
func (s *Store) AppendMessages(id string, snap agent.SessionSnapshot, totalMsgCount int) error {
	if !s.enabled || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	df.BaseCount = totalMsgCount - len(snap.Messages)
	if df.BaseCount < 0 {
		df.BaseCount = 0
	}
	data, err := json.Marshal(df)
	if err != nil {
		return err
	}
	if err := ioutil.WriteFileAtomicNoSync(path, data, 0o600); err != nil {
		return err
	}

	// Update the main file's mtime so the session appears recent in listings.
	mainPath := s.path(snap.WorkingDir, id)
	_ = os.Chtimes(mainPath, time.Now(), time.Now())

	// Update the index so the session appears recent in listings. The delta
	// file always holds every message since the last full snapshot, so the
	// agent's total count is authoritative. Label and oneshot flags are
	// preserved — appends do not change them.
	s.updateIndexCount(snap.WorkingDir, id, time.Now().UTC(), totalMsgCount)
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

// deltaPrefixMatches reports whether the snapshot's trailing messages (the
// portion beyond the delta's base) are the same messages as the delta's
// leading messages — i.e., the snapshot already absorbed a prefix of the
// delta (a concurrent full save raced the delta; see LoadInWorkingDir's
// mergeFrom branch). Only the persisted content is compared: transient
// fields that are not round-tripped identically (the stream Index and
// ArgsStabilized) are ignored.
func deltaPrefixMatches(snapshotTail, deltaPrefix []llm.Message) bool {
	if len(snapshotTail) != len(deltaPrefix) {
		return false
	}
	for i := range snapshotTail {
		if !sameMessageContent(snapshotTail[i], deltaPrefix[i]) {
			return false
		}
	}
	return true
}

// sameMessageContent compares two messages by their persisted fields. Used
// by deltaPrefixMatches to decide whether a snapshot absorbed a delta's
// prefix; tool-call Args maps are compared structurally because their JSON
// round-trip order can differ.
func sameMessageContent(a, b llm.Message) bool {
	if a.Role != b.Role || a.Content != b.Content ||
		a.Reasoning != b.Reasoning || a.Refusal != b.Refusal ||
		a.ToolCallID != b.ToolCallID || a.Model != b.Model {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	// Images (vision input) are persisted with json:"images,omitempty"; the
	// element-wise compare keeps the persisted-content contract complete.
	if len(a.Images) != len(b.Images) {
		return false
	}
	for i := range a.Images {
		if a.Images[i] != b.Images[i] {
			return false
		}
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		ta, tb := a.ToolCalls[i], b.ToolCalls[i]
		if ta.ID != tb.ID || ta.Name != tb.Name || ta.ArgsStr != tb.ArgsStr {
			return false
		}
		if !reflect.DeepEqual(ta.Args, tb.Args) {
			return false
		}
	}
	return true
}

// sessionUpdatedAt returns the effective persisted update time for a session
// file: the file's updated field, or the delta file's mtime when that is
// newer. Incremental saves (AppendMessages) and TouchSession update the
// metadata index and the file mtimes but deliberately do NOT rewrite the
// session file's updated field, so the no-index fallback scans must consult
// the delta's mtime to keep delta-only updates in their recency order.
// Correct in both directions: after a full save that superseded a delta, the
// file's updated is newer than the (surviving, crash-left) delta's mtime, so
// the max picks the file field. Callers must hold s.mu.
func (s *Store) sessionUpdatedAt(workingDir, id string, fileUpdated time.Time) time.Time {
	if fi, err := os.Stat(s.deltaPath(workingDir, id)); err == nil {
		if mt := fi.ModTime().UTC(); mt.After(fileUpdated) {
			return mt
		}
	}
	return fileUpdated
}

// clearDeltaFile removes the delta file for a session.
func (s *Store) clearDeltaFile(workingDir, id string) error {
	return os.Remove(s.deltaPath(workingDir, id))
}

// sessionFiles returns the names of the .json session files in the store
// directory for workingDir (excluding index.json). A missing directory
// surfaces as an *os.PathError from os.ReadDir (os.IsNotExist).
func (s *Store) sessionFiles(workingDir string) ([]string, error) {
	entries, err := os.ReadDir(s.dir(workingDir))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Skip the index file if somehow named .json
		if e.Name() == "index.json" {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// legacySession is one entry from a legacy (no-index) session directory scan.
type legacySession struct {
	id      string
	updated time.Time
}

// legacySessionUpdated scans the session files in workingDir's store
// directory (legacy layout without an index) and returns each session id
// with its delta-aware updated timestamp. Sessions whose updated timestamp
// cannot be decoded are omitted. A missing directory surfaces as an
// *os.PathError (os.IsNotExist). Shared by LatestID and prune, whose legacy
// fallbacks need exactly this data.
func (s *Store) legacySessionUpdated(workingDir string) ([]legacySession, error) {
	names, err := s.sessionFiles(workingDir)
	if err != nil {
		return nil, err
	}
	out := make([]legacySession, 0, len(names))
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
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
		// Delta-aware timestamp (see sessionUpdatedAt): delta-only updates
		// would otherwise under-rank next to full-save timestamps.
		out = append(out, legacySession{id: id, updated: s.sessionUpdatedAt(workingDir, id, meta.Updated)})
	}
	return out, nil
}

// List returns session info for a working directory, ordered by most recently
// updated first. Uses the metadata index when available, falling back to a
// full-file scan for legacy directories. Results are cached briefly in memory
// to avoid repeated disk I/O on reconnects.
func (s *Store) List(workingDir string) ([]agent.SessionInfo, error) {
	if !s.enabled {
		return nil, nil
	}
	// The cache is keyed by the EFFECTIVE store directory, not the working
	// dir: in global mode (SetGlobalDir) every working dir shares one session
	// dir, so keying by workingDir would both duplicate entries and — worse —
	// let a Save/Delete for one working dir leave another working dir's cached
	// listing of the same sessions stale for the TTL window.
	cacheKey := s.dir(workingDir)

	// Check in-memory cache first (1-second TTL).
	listCacheMu.RLock()
	if ce, ok := listCache[cacheKey]; ok && time.Since(ce.time) < time.Second {
		out := make([]agent.SessionInfo, len(ce.info))
		copy(out, ce.info)
		listCacheMu.RUnlock()
		return out, nil
	}
	listCacheMu.RUnlock()

	// The index read/scan/write below must be serialized with Save/Delete/
	// Prune (read-modify-write of index.json). The list cache itself was
	// checked above without the store mutex — concurrent List calls stay
	// parallel on the hot cache path.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Try the metadata index file.
	idx := s.readIndex(workingDir)
	if idx != nil && len(idx.Entries) > 0 {
		// Migration: older sessions may still have a truncated Label (50-char
		// limit from before CSS handled dynamic truncation). Load the session
		// file and refresh with the full untruncated label. Only entries whose
		// stored label is exactly the legacy truncation length are ambiguous,
		// so this is O(1) for the common case instead of re-reading every
		// session file on every list.
		needsRewrite := false
		for i, e := range idx.Entries {
			// Deliberate renames (marker persisted by Save) are always
			// authoritative: never regenerate them from the conversation,
			// even when they are exactly the legacy truncation length.
			if e.LabelRenamed {
				continue
			}
			if len(e.Label) == legacyLabelMaxLen {
				if raw := s.loadRawLabel(workingDir, e.ID); raw != "" && raw != e.Label {
					idx.Entries[i].Label = raw
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
			}
		}
		listCacheMu.Lock()
		listCache[cacheKey] = listCacheEntry{info: out, time: time.Now()}
		listCacheMu.Unlock()
		return out, nil
	}

	// Fallback: legacy full-file scan (no index file).
	idx = &sessionIndex{} // collect entries to build the index
	names, err := s.sessionFiles(workingDir)
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
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		data, err := os.ReadFile(s.path(workingDir, id))
		if err != nil {
			continue
		}
		var f file
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		// Use the delta-aware effective timestamp (AppendMessages bumps only
		// the index and mtimes, not the file's updated field).
		updated := s.sessionUpdatedAt(workingDir, id, f.Updated)
		lbl := sessionLabel(f.Messages, f.Label, f.LabelRenamed)
		items = append(items, item{
			info: agent.SessionInfo{
				ID:           id,
				Oneshot:      f.Oneshot,
				UpdatedAt:    updated.UTC().Format(time.RFC3339Nano),
				MessageCount: len(f.Messages),
				Label:        lbl,
			},
			updated: updated,
		})
		idx.Entries = append(idx.Entries, sessionIndexEntry{
			ID: id, Updated: updated, Oneshot: f.Oneshot, MessageCount: len(f.Messages), Label: lbl,
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
	listCache[cacheKey] = listCacheEntry{info: out, time: time.Now()}
	listCacheMu.Unlock()

	return out, nil
}

// LatestID returns the most recently updated session id.
// Uses the Updated field in each session JSON (not file mtime), so copied or
// restored files cannot displace the true latest. Only the updated timestamp
// is decoded — messages and other fields are skipped for a cheap scan.
func (s *Store) LatestID(workingDir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	legacy, err := s.legacySessionUpdated(workingDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Already tried index above; nothing more to do.
			return "", nil
		}
		return "", err
	}
	var latestID string
	var latestUpdated time.Time
	for _, l := range legacy {
		if l.updated.After(latestUpdated) {
			latestUpdated = l.updated
			latestID = l.id
		}
	}
	return latestID, nil
}

// Delete removes a saved session file.
func (s *Store) Delete(workingDir, id string) error {
	if !s.enabled {
		return fmt.Errorf("session persistence disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateSessionID(id); err != nil {
		return err
	}
	path := s.path(workingDir, id)
	if err := ensureUnderSessionsDir(workingDir, path, s.globalDir); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			// The session may exist only in memory: a /new pane that was
			// never used is not persisted on quit (see Save's empty-session
			// skip), yet the user can still delete it (sidebar ✕ / resume
			// del). Deleting it is a success — there is no persisted state
			// to remove — so clean up any leftover delta/index entry and
			// return nil instead of "session not found".
			if err := s.clearDeltaFile(workingDir, id); err != nil && !os.IsNotExist(err) {
				log.Printf("warning: failed to remove delta for session %s: %v", id, err)
			}
			// Mirror the found-path cleanup: a stale Created cache entry would
			// otherwise be re-used by a later Save of a new session with the
			// same id (in-memory-only sessions are created and deleted without
			// ever touching disk).
			delete(s.createdCache, id)
			s.removeFromIndex(workingDir, id)
			s.invalidateListCache(workingDir)
			return nil
		}
		return err
	}
	// Remove any pending delta for the session as well.
	if err := s.clearDeltaFile(workingDir, id); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: failed to remove delta for session %s: %v", id, err)
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
	return llm.SessionLabel(f.Messages)
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
	return ioutil.WriteFileAtomicNoSync(indexPath, data, 0o600)
}

// mutateIndex reads the session metadata index, applies mutate, and writes it
// back when mutate reports a change. When createIfMissing is false and no
// index file exists, the mutation is a no-op; when true, a fresh empty index
// is passed in so callers can seed the first entry. Write errors are returned
// so callers that care (touchIndex) can propagate them; the other index
// mutators swallow them, matching their historical behavior.
func (s *Store) mutateIndex(workingDir string, createIfMissing bool, mutate func(idx *sessionIndex) bool) error {
	return s.mutateIndexWith(workingDir, createIfMissing, nil, mutate)
}

// mutateIndexWith is mutateIndex with a caller-supplied preloaded index: when
// preloaded is non-nil it is used instead of re-reading the index file (Save
// reuses the index it already loaded for Created recovery). The preloaded
// snapshot is only valid because the caller holds s.mu and no other index
// mutator runs between the read and this mutation.
func (s *Store) mutateIndexWith(workingDir string, createIfMissing bool, preloaded *sessionIndex, mutate func(idx *sessionIndex) bool) error {
	if !s.enabled {
		return nil
	}
	idx := preloaded
	if idx == nil {
		idx = s.readIndex(workingDir)
	}
	if idx == nil {
		if !createIfMissing {
			return nil
		}
		idx = &sessionIndex{}
	}
	if !mutate(idx) {
		return nil
	}
	if err := s.writeIndex(workingDir, idx); err != nil {
		return err
	}
	s.invalidateListCache(workingDir)
	return nil
}

// updateIndex adds or updates an entry in the session metadata index.
// created is written alongside updated so a later cache-miss Save (e.g. after
// a process restart) can recover the immutable Created timestamp from the
// index instead of re-reading the session file. Existing entries keep their
// Created when created is zero (defensive: Save always passes non-zero).
// preloaded, when non-nil, is a caller-supplied index (see mutateIndexWith);
// Save passes the index it already loaded for Created recovery so a full save
// reads index.json at most once. labelRenamed mirrors the session file's
// rename marker so List can keep deliberate renames authoritative.
func (s *Store) updateIndex(workingDir, id string, created, updated time.Time, msgCount int, label string, oneshot, labelRenamed bool, preloaded *sessionIndex) {
	s.mutateIndexWith(workingDir, true, preloaded, func(idx *sessionIndex) bool {
		found := false
		for i, e := range idx.Entries {
			if e.ID == id {
				if !created.IsZero() {
					idx.Entries[i].Created = created
				}
				idx.Entries[i].Updated = updated
				idx.Entries[i].Oneshot = oneshot
				idx.Entries[i].MessageCount = msgCount
				idx.Entries[i].Label = label
				idx.Entries[i].LabelRenamed = labelRenamed
				found = true
				break
			}
		}
		if !found {
			idx.Entries = append(idx.Entries, sessionIndexEntry{
				ID: id, Created: created, Updated: updated, Oneshot: oneshot, MessageCount: msgCount, Label: label, LabelRenamed: labelRenamed,
			})
		}
		return true
	})
}

// touchIndex updates only the timestamp for a session in the index.
func (s *Store) touchIndex(workingDir, id string, updated time.Time) error {
	return s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		for i, e := range idx.Entries {
			if e.ID == id {
				idx.Entries[i].Updated = updated
				return true
			}
		}
		// Entry not in index; skip. The list fallback will re-scan on next miss.
		return false
	})
}

// updateIndexCount updates the timestamp and message count for a session in
// the metadata index, preserving the label and oneshot flags. Used by
// AppendMessages so listings stay accurate between full snapshots. Entries
// missing from the index are skipped — the list fallback re-scans the
// directory on the next miss.
func (s *Store) updateIndexCount(workingDir, id string, updated time.Time, msgCount int) {
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		for i, e := range idx.Entries {
			if e.ID == id {
				idx.Entries[i].Updated = updated
				idx.Entries[i].MessageCount = msgCount
				return true
			}
		}
		return false
	})
}

// removeFromIndex deletes an entry from the session metadata index.
func (s *Store) removeFromIndex(workingDir, id string) {
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
		filtered := idx.Entries[:0]
		for _, e := range idx.Entries {
			if e.ID != id {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == len(idx.Entries) {
			return false // nothing removed
		}
		idx.Entries = filtered
		return true
	})
}

// removeFromIndexBatch removes multiple entries from the session metadata index
// in a single pass, rewriting the index file only once.
func (s *Store) removeFromIndexBatch(workingDir string, ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mutateIndex(workingDir, false, func(idx *sessionIndex) bool {
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
			return false
		}
		idx.Entries = filtered
		return true
	})
}

// invalidateListCache clears the in-memory list cache for a working directory.
func (s *Store) invalidateListCache(workingDir string) {
	// Keyed by the effective store dir (see List) so global-mode invalidations
	// hit every working dir that shares the store.
	listCacheMu.Lock()
	delete(listCache, s.dir(workingDir))
	listCacheMu.Unlock()
}

func sessionLabel(messages []llm.Message, stored string, renamed bool) string {
	// A deliberate rename is always authoritative: the user (or the
	// session_rename tool) chose it, so it must never be regenerated from
	// the conversation. This closes the legacy-migration hole where a
	// rename of exactly legacyLabelMaxLen characters that happens to be a
	// prefix of the derived first-user-message label would be silently
	// replaced by the longer derived label on the next full save.
	if renamed {
		return stored
	}
	derived := llm.SessionLabel(messages)
	// The stored label is authoritative UNLESS it is a legacy 50-char
	// truncation of the derived label. An older version truncated labels
	// when persisting, so a stored label that is exactly the legacy length
	// and a prefix of the full message is stale and must be migrated to the
	// untruncated text. Any other stored label is either already full or a
	// deliberate rename (RenameSession / the session_rename tool) and must
	// NOT be regenerated: the web sidebar shows the live (renamed) label
	// while the session is open, and would otherwise fall back to the first
	// user message the moment the session is closed (the saved-session row
	// reads this index entry).
	if stored != "" && (len(stored) != legacyLabelMaxLen || !strings.HasPrefix(derived, stored)) {
		return stored
	}
	return derived
}

// NewID generates a new session id.
func NewID() string {
	return randhex.ID(16, "")
}

// Prune deletes expired and excess sessions while retaining every keepID
// (all active in-memory sessions). Callers that manage multiple live sessions
// (the multi-session web registry) must pass the full active ID set; the
// internal auto-prune in Save is disabled for them via SetAutoPrune(false).
// Uses the Updated field from the session index or session JSON, not file
// mtime, to be consistent with LatestID. Deletions are batched so the index
// is rewritten only once. Serialized by the store mutex.
func (s *Store) Prune(workingDir string, keepIDs ...string) {
	if !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(workingDir, keepIDs...)
}

// prune is the lock-free implementation of Prune. Callers must hold s.mu.
func (s *Store) prune(workingDir string, keepIDs ...string) {
	if !s.enabled {
		return
	}
	keep := make(map[string]struct{}, len(keepIDs))
	for _, id := range keepIDs {
		if id != "" {
			keep[id] = struct{}{}
		}
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
		legacy, err := s.legacySessionUpdated(workingDir)
		if err != nil {
			return
		}
		for _, l := range legacy {
			items = append(items, item{id: l.id, updated: l.updated.UTC()})
		}
	}
	if len(items) == 0 {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].updated.After(items[j].updated) })

	cutoff := time.Now().UTC().AddDate(0, 0, -s.maxAgeDays)
	otherBudget := s.maxCount - len(keep)
	if otherBudget < 0 {
		otherBudget = 0
	}
	var toDelete []string
	others := 0
	for _, it := range items {
		if _, ok := keep[it.id]; ok {
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
