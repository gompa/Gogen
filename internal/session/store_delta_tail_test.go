package session

// Probe for the delta-merge default case: when the snapshot on disk is
// LONGER than the delta's base but SHORTER than base+delta (a stale snapshot
// written by a concurrent writer / second process raced a newer delta), the
// load currently treats it as "truncated" and DROPS the delta — losing the
// messages that only exist in it. The correct handling depends on the shape:
//
//   - S == B            → the snapshot is exactly the base; merge the delta.
//   - S >= B + D        → the snapshot already contains the delta; drop it.
//   - B < S < B + D     → the snapshot contains the base plus a PREFIX of the
//     delta (S-B messages); merge only the remaining D-(S-B) messages.
//   - S < B             → the snapshot was truncated after the delta was
//     written; drop the delta (deliberate compaction/rollback).
//
// The current code folds the B < S < B+D case into the truncation branch,
// silently dropping the delta tail.

import (
	"encoding/json"
	"os"
	"testing"

	"gogen/internal/ioutil"
	"gogen/internal/llm"
)

func writeDeltaFor(t *testing.T, store *Store, dir, id string, base int, msgs ...llm.Message) {
	t.Helper()
	df := deltaFile{Messages: msgs, BaseCount: base}
	data, err := json.Marshal(df)
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFileAtomicNoSync(store.deltaPath(dir, id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadKeepsDeltaTailWhenSnapshotExtendsBase creates the B < S < B+D
// state: base snapshot of 3 messages, delta of 3 messages written against
// that base (baseCount=3), then a stale writer overwrites the snapshot with
// 4 messages (the base + ONE of the delta's messages — the exact state a
// second writer produces when it loaded base+1 delta messages and did a full
// save before the first writer's remaining delta messages landed). Loading
// must keep the two messages that exist only in the delta.
func TestLoadKeepsDeltaTailWhenSnapshotExtendsBase(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false)
	id := "sess1"

	msg := func(role, content string) llm.Message {
		return llm.Message{Role: role, Content: content}
	}

	// Base full save: 3 messages.
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C")},
	}); err != nil {
		t.Fatal(err)
	}

	// Delta with 3 more messages written against the 3-message base.
	writeDeltaFor(t, store, dir, id, 3,
		msg("assistant", "D"), msg("user", "E"), msg("assistant", "F"))

	// A stale writer overwrites the snapshot with 4 messages: the base plus
	// the FIRST delta message (D). This is the two-writer race: the snapshot
	// now sits BETWEEN the delta's base and base+delta.
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C"), msg("assistant", "D")},
	}); err != nil {
		t.Fatal(err)
	}
	// The stale writer's Save cleared the delta (Save unlinks it after a
	// successful write); re-create it exactly as a crash/race would leave it.
	writeDeltaFor(t, store, dir, id, 3,
		msg("assistant", "D"), msg("user", "E"), msg("assistant", "F"))

	snap, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var contents []string
	for _, m := range snap.Messages {
		contents = append(contents, m.Content)
	}
	t.Logf("loaded: %v", contents)
	want := []string{"A", "B", "C", "D", "E", "F"}
	if len(contents) != len(want) {
		t.Fatalf("got %d messages %v, want %d %v (delta tail lost)", len(contents), contents, len(want), want)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("message %d = %q, want %q", i, contents[i], want[i])
		}
	}
}

// TestLoadDropsDivergedDeltaWhenSnapshotTailDiffers covers the other side of
// the two-writer race: the snapshot's extra messages do NOT match the
// delta's prefix (the other writer truncated/rolled back, or its content
// diverged). The snapshot is authoritative — the delta is dropped, never
// merged, so no foreign/duplicated messages appear.
func TestLoadDropsDivergedDeltaWhenSnapshotTailDiffers(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false)
	id := "sess1"

	msg := func(role, content string) llm.Message {
		return llm.Message{Role: role, Content: content}
	}
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C")},
	}); err != nil {
		t.Fatal(err)
	}
	// Delta written against the 3-message base.
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"))
	// A divergent writer saved a snapshot with 4 messages whose 4th message
	// is NOT the delta's D (rollback + rewrite).
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C"), msg("user", "X")},
	}); err != nil {
		t.Fatal(err)
	}
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"))

	snap, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var contents []string
	for _, m := range snap.Messages {
		contents = append(contents, m.Content)
	}
	t.Logf("loaded: %v", contents)
	want := []string{"A", "B", "C", "X"}
	if len(contents) != len(want) {
		t.Fatalf("got %d messages %v, want %d %v (diverged delta must not be merged)", len(contents), contents, len(want), want)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("message %d = %q, want %q", i, contents[i], want[i])
		}
	}
}

// TestTailMergeIsIdempotent verifies that a tail-merged load keeps the delta
// on disk (the crash-safety design) and that a second load produces the same
// merged result without duplicating the absorbed prefix.
func TestTailMergeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false)
	id := "sess1"

	msg := func(role, content string) llm.Message {
		return llm.Message{Role: role, Content: content}
	}
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C")},
	}); err != nil {
		t.Fatal(err)
	}
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"), msg("assistant", "F"))
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C"), msg("assistant", "D")},
	}); err != nil {
		t.Fatal(err)
	}
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"), msg("assistant", "F"))

	for i := 0; i < 2; i++ {
		snap, err := store.LoadInWorkingDir(dir, id)
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		var contents []string
		for _, m := range snap.Messages {
			contents = append(contents, m.Content)
		}
		t.Logf("load %d: %v", i, contents)
		want := []string{"A", "B", "C", "D", "E", "F"}
		if len(contents) != len(want) {
			t.Fatalf("load %d: got %d messages %v, want %d %v", i, len(contents), contents, len(want), want)
		}
		for j := range want {
			if contents[j] != want[j] {
				t.Fatalf("load %d: message %d = %q, want %q", i, j, contents[j], want[j])
			}
		}
	}
}

// TestLoadDropsDeltaWhenSnapshotShorterThanBase keeps the truncation contract: a
// snapshot SHORTER than the delta's base means the conversation was
// deliberately rolled back/compacted after the delta was written — the delta
// must be dropped, never merged.
func TestLoadDropsDeltaWhenSnapshotShorterThanBase(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false)
	id := "sess1"

	msg := func(role, content string) llm.Message {
		return llm.Message{Role: role, Content: content}
	}
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B"), msg("user", "C")},
	}); err != nil {
		t.Fatal(err)
	}
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"))

	// Compacted to 2 messages (fewer than the delta base).
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages:   []llm.Message{msg("user", "A"), msg("assistant", "B")},
	}); err != nil {
		t.Fatal(err)
	}
	writeDeltaFor(t, store, dir, id, 3, msg("assistant", "D"), msg("user", "E"))

	snap, err := store.LoadInWorkingDir(dir, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var contents []string
	for _, m := range snap.Messages {
		contents = append(contents, m.Content)
	}
	t.Logf("loaded: %v", contents)
	if len(contents) != 2 || contents[0] != "A" || contents[1] != "B" {
		t.Fatalf("got %v, want [A B] (truncated session must NOT re-merge the delta)", contents)
	}
	if _, err := os.Stat(store.deltaPath(dir, id)); !os.IsNotExist(err) {
		t.Fatalf("delta was not cleared after truncated load")
	}
}
