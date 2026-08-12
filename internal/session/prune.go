package session

import (
	"os"
	"sort"
	"time"
)

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

	// A negative maxAgeDays disables age-based retention ("keep forever");
	// the count-based budget still applies.
	agePrune := s.maxAgeDays >= 0
	var cutoff time.Time
	if agePrune {
		cutoff = time.Now().UTC().AddDate(0, 0, -s.maxAgeDays)
	}
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
		expired := agePrune && it.updated.Before(cutoff)
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
