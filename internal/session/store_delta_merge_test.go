package session

// Verifies the load-time delta merge against the crash window: Save writes
// the full snapshot and THEN clears the delta. A crash (or failed unlink)
// between the two leaves a snapshot that already contains the delta's
// messages plus the delta file. LoadInWorkingDir must detect that and drop
// the delta — never merge it again. The baseCount checks handle modern
// deltas; a legacy delta (BaseCount == 0, written before the field existed)
// must apply the same staleness check (snapshot Updated vs delta file mtime)
// instead of merging unconditionally, which would duplicate the messages on
// every load until the next full save.

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"gogen/internal/ioutil"
	"gogen/internal/llm"
)

func TestLoadDropsStaleDeltaAfterCrash(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(true)
	store.SetAutoPrune(false)

	id := "sess1"
	// Base full save with 2 messages.
	if err := store.Save(id, SessionSnapshot{
		WorkingDir: dir,
		Messages: []llm.Message{
			{Role: "user", Content: "A"},
			{Role: "assistant", Content: "B"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	writeDelta := func(baseCount int) {
		df := deltaFile{
			Messages: []llm.Message{{Role: "user", Content: "C"}},
		}
		if baseCount > 0 {
			df.BaseCount = baseCount
		}
		data, err := jsonMarshal(df)
		if err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFileAtomicNoSync(store.deltaPath(dir, id), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Crash simulation, once per delta variant:
	//   1. append C via a delta (file mtime set to `past`),
	//   2. a full save includes C (snapshot Updated = now) and clears the
	//      delta — the write that crashes BEFORE the unlink,
	//   3. the delta file is left behind (re-created, mtime restored to
	//      `past`), exactly as a crash between snapshot-write and unlink
	//      would leave it.
	past := time.Now().Add(-2 * time.Minute)

	for _, base := range []int{0, 2} {
		writeDelta(base)
		_ = os.Chtimes(store.deltaPath(dir, id), past, past)
		if err := store.Save(id, SessionSnapshot{
			WorkingDir: dir,
			Messages: []llm.Message{
				{Role: "user", Content: "A"},
				{Role: "assistant", Content: "B"},
				{Role: "user", Content: "C"},
			},
		}); err != nil {
			t.Fatal(err)
		}
		writeDelta(base)
		_ = os.Chtimes(store.deltaPath(dir, id), past, past)
		snap, err := store.LoadInWorkingDir(dir, id)
		if err != nil {
			t.Fatalf("load (base=%d): %v", base, err)
		}
		var contents []string
		for _, m := range snap.Messages {
			contents = append(contents, m.Content)
		}
		t.Logf("base=%d loaded: %v", base, contents)
		want := []string{"A", "B", "C"}
		if len(contents) != len(want) {
			t.Errorf("base=%d: got %d messages %v, want %d %v (stale delta merged = duplicated history)",
				base, len(contents), contents, len(want), want)
			continue
		}
		for i := range want {
			if contents[i] != want[i] {
				t.Errorf("base=%d: message %d = %q, want %q", base, i, contents[i], want[i])
			}
		}
		// The stale delta must have been removed so a later load cannot
		// double-merge.
		if _, err := os.Stat(store.deltaPath(dir, id)); !os.IsNotExist(err) {
			t.Errorf("base=%d: stale delta was not removed on load", base)
		}
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
