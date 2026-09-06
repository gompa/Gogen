package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a store in a fresh temp dir with a controllable clock.
func newTestStore(t *testing.T) (*Store, *timeCursor) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cursor := &timeCursor{t: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)}
	st.now = cursor.Now
	return st, cursor
}

// timeCursor is a controllable clock for deterministic claim tests.
type timeCursor struct {
	t time.Time
}

func (c *timeCursor) Now() time.Time { return c.t }

func (c *timeCursor) Advance(d time.Duration) { c.t = c.t.Add(d) }

func ptrTime(t time.Time) *time.Time { return &t }

// daily automation due exactly now.
func dueDaily(st *Store, now time.Time) Automation {
	a := Automation{
		Title:      "Nightly triage",
		Prompt:     "triage the board",
		WorkingDir: "/tmp/proj",
		Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		panic(err)
	}
	// Force the stored next run to exactly `now` so the claim is due.
	st.autos[a.ID].NextRunAt = ptrTime(now)
	return a
}

func TestCreateAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	a := Automation{
		Title:      "Nightly triage",
		Prompt:     "triage the board",
		WorkingDir: "relative/dir",
		Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.ID == "" || a.CreatedAt.IsZero() {
		t.Fatalf("Create did not assign id/timestamps: %+v", a)
	}
	if !filepath.IsAbs(a.WorkingDir) {
		t.Fatalf("Create did not absolutize the working dir: %q", a.WorkingDir)
	}
	if a.NextRunAt == nil {
		t.Fatalf("Create did not compute next_run_at")
	}
	wantNext, _ := NextRun(a.Schedule, time.Now())
	if !a.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at = %v, want %v", a.NextRunAt, wantNext)
	}

	// A fresh store instance over the same dir reads the same record.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := st2.Get(a.ID)
	if !ok {
		t.Fatalf("reopened store lost automation %s", a.ID)
	}
	if got.Title != a.Title || got.Schedule.Kind != KindDaily || !got.NextRunAt.Equal(*a.NextRunAt) {
		t.Fatalf("reopened record drifted: %+v vs %+v", got, a)
	}
}

func TestCreateRejectsInvalid(t *testing.T) {
	st, _ := newTestStore(t)
	cases := []struct {
		name string
		a    Automation
	}{
		{"no title", Automation{Prompt: "p", WorkingDir: "/x", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"no prompt", Automation{Title: "t", WorkingDir: "/x", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"no dir", Automation{Title: "t", Prompt: "p", Schedule: Schedule{Kind: KindDaily, Time: "09:00"}}},
		{"bad kind", Automation{Title: "t", Prompt: "p", WorkingDir: "/x", Schedule: Schedule{Kind: "monthly"}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := tc.a
			if err := st.Create(&a); err == nil {
				t.Fatalf("Create accepted an invalid automation: %+v", a)
			}
		})
	}
}

// TestOpenCorruptedStore verifies a corrupted store file surfaces as an
// error (never a panic), and that the store does NOT overwrite the file it
// could not read.
func TestOpenCorruptedStore(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		file string
		body string
	}{
		{"truncated automations file", AutomationsFile, `{"version":1,"automations":[{"id":"auto-x"`},
		{"garbage automations file", AutomationsFile, "\x00\x01 not json at all"},
		{"garbage runs file", RunsFile, "{oops"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := OpenStore(dir)
			if err == nil {
				t.Fatalf("OpenStore accepted corrupted %s", tc.file)
			}
			if st != nil {
				t.Fatalf("OpenStore returned a store alongside an error")
			}
			if !strings.Contains(err.Error(), "corrupted") {
				t.Fatalf("error %v should mention corruption", err)
			}
			// The corrupted bytes are still on disk: the store never
			// destroys data it could not read.
			data, rerr := os.ReadFile(path)
			if rerr != nil || string(data) != tc.body {
				t.Fatalf("corrupted file was modified: %q (err %v)", data, rerr)
			}
		})
	}
}

// TestOpenMissingStoreDir covers the first-run path: no files exist yet.
func TestOpenMissingStoreDir(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	if err != nil {
		t.Fatalf("OpenStore on a missing dir: %v", err)
	}
	if got := st.List(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %v", got)
	}
}

// TestClaimDueFiresOnce is the dedup guarantee: one sweep claims a due row
// and advances it, so an immediate second sweep finds nothing.
func TestClaimDueFiresOnce(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("first claim: got %d claims, want 1", len(claims))
	}
	if claims[0].Automation.ID != a.ID {
		t.Fatalf("claim id = %s, want %s", claims[0].Automation.ID, a.ID)
	}
	if claims[0].PlannedAt != cursor.t {
		t.Fatalf("planned = %v, want %v", claims[0].PlannedAt, cursor.t)
	}
	// The row advanced to tomorrow's 09:00.
	got, _ := st.Get(a.ID)
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at after claim = %v, want %v", got.NextRunAt, want)
	}
	if !got.Enabled {
		t.Fatalf("recurring row disabled by a normal claim")
	}

	// Second sweep at the same instant: nothing due (the dedup guarantee).
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("second claim fired %d rows — a due row must be claimed exactly once", len(claims))
	}
}

// TestClaimDueCatchupMissed pins the catch-up policy: a due row far older
// than the window is recorded as missed and skipped, not fired.
func TestClaimDueCatchupMissed(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)

	// The row went stale: it was due 2 days ago (GoGen was closed).
	stale := cursor.t.Add(-48 * time.Hour)
	st.autos[a.ID].NextRunAt = ptrTime(stale)

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 0 {
		t.Fatalf("a 48h-stale row must not fire, got %d claims", len(claims))
	}
	runs := st.Runs(a.ID)
	if len(runs) != 1 || runs[0].Status != RunMissed {
		t.Fatalf("missed run not recorded: %+v", runs)
	}
	if runs[0].PlannedAt != stale {
		t.Fatalf("missed planned_at = %v, want %v", runs[0].PlannedAt, stale)
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunMissed {
		t.Fatalf("last_run_status = %q, want missed", got.LastRunStatus)
	}
	// Advanced from NOW (not from the stale time) so no extra misses queue.
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at after miss = %v, want %v", got.NextRunAt, want)
	}
}

// TestClaimDueWithinCatchupWindow pins the other half of the policy: a run
// missed by 40 minutes still fires (run-once-on-startup semantics).
func TestClaimDueWithinCatchupWindow(t *testing.T) {
	st, cursor := newTestStore(t)
	a := dueDaily(st, cursor.t)
	st.autos[a.ID].NextRunAt = ptrTime(cursor.t.Add(-40 * time.Minute))

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("a 40-minute-late due row must fire, got %d claims", len(claims))
	}
	if claims[0].PlannedAt != cursor.t.Add(-40*time.Minute) {
		t.Fatalf("planned = %v, want the stale scheduled time", claims[0].PlannedAt)
	}
}

// TestClaimOnceDisarms verifies a due once-automation claims and disables
// itself (fire once, then off).
func TestClaimOnceDisarms(t *testing.T) {
	st, cursor := newTestStore(t)
	a := Automation{
		Title:      "One-shot deploy",
		Prompt:     "deploy",
		WorkingDir: "/tmp/proj",
		Schedule:   Schedule{Kind: KindOnce, At: "2026-01-05T09:00:00Z"},
		Enabled:    true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("once row not claimed: %d", len(claims))
	}
	got, _ := st.Get(a.ID)
	if got.Enabled || got.NextRunAt != nil {
		t.Fatalf("once row must disarm after the claim: enabled=%v next=%v", got.Enabled, got.NextRunAt)
	}
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disarmed row claimed again")
	}
}

// TestClaimUnfixableDisables pins the "disable instead of spin" rule: a
// hand-edited record whose recurrence cannot produce a future time is
// disabled by the sweep instead of being re-claimed forever.
func TestClaimUnfixableDisables(t *testing.T) {
	st, cursor := newTestStore(t)
	// Bypass validation by writing the record like a hand-edited store file
	// would: an unknown schedule kind.
	a := Automation{ID: NewID(), Title: "broken", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: "fortnightly", Time: "09:00"}, Enabled: true, NextRunAt: ptrTime(cursor.t)}
	st.autos[a.ID] = &a

	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("due broken row should still be claimed once (it is due): %d", len(claims))
	}
	got, _ := st.Get(a.ID)
	if got.Enabled || got.NextRunAt != nil {
		t.Fatalf("unfixable row must disable itself: %+v", got)
	}
	if claims := st.ClaimDue(cursor.t, DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disabled row claimed again — the row would spin")
	}
}

// TestSetEnabledPersistsAndRecomputes covers enable/disable across a
// restart (persisted, and re-enable recomputes next_run_at from now).
func TestSetEnabledPersistsAndRecomputes(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{
		Title: "Nightly triage", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"}, Enabled: true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabled(a.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// Reopen: the disable survived the restart, and disabled rows are never
	// claimed even when their next_run_at has passed.
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.Get(a.ID)
	if !ok || got.Enabled {
		t.Fatalf("disable did not persist: %+v", got)
	}
	st2.autos[a.ID].NextRunAt = ptrTime(time.Now().Add(-time.Hour))
	if claims := st2.ClaimDue(time.Now(), DefaultCatchupWindow); len(claims) != 0 {
		t.Fatalf("disabled row was claimed")
	}
	// Re-enable recomputes next_run_at to the future.
	if err := st2.SetEnabled(a.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ = st2.Get(a.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.After(time.Now().Add(-time.Second)) {
		t.Fatalf("re-enable did not recompute next_run_at: %+v", got)
	}
}

func TestSetEnabledUnknownID(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.SetEnabled("auto-nope", true); err == nil {
		t.Fatal("SetEnabled on an unknown id should fail")
	}
}

// TestRunsHistoryAndCaps covers the run-history plumbing: append, finish,
// session-id link, newest-first ordering, and the per-automation cap.
func TestRunsHistoryAndCaps(t *testing.T) {
	st, _ := newTestStore(t)
	a := dueDaily(st, st.now())

	runID, err := st.RecordFire(a.ID, *st.autos[a.ID].NextRunAt)
	if err != nil {
		t.Fatalf("RecordFire: %v", err)
	}
	st.RecordLink(runID, "sess-123")
	if err := st.RecordFinish(runID, RunCompleted, "", "sess-123"); err != nil {
		t.Fatalf("RecordFinish: %v", err)
	}
	runs := st.Runs(a.ID)
	if len(runs) != 1 {
		t.Fatalf("history has %d rows, want 1", len(runs))
	}
	r := runs[0]
	if r.Status != RunCompleted || r.SessionID != "sess-123" || r.FiredAt == nil || r.EndedAt == nil {
		t.Fatalf("finished run incomplete: %+v", r)
	}
	got, _ := st.Get(a.ID)
	if got.LastRunStatus != RunCompleted || got.LastRunAt == nil {
		t.Fatalf("parent last-run mirror not updated: %+v", got)
	}

	// Overflow the cap: only the newest maxRunsPerAutomation survive.
	for i := 0; i < maxRunsPerAutomation+5; i++ {
		id, err := st.RecordFire(a.ID, time.Now())
		if err != nil {
			t.Fatalf("RecordFire %d: %v", i, err)
		}
		if err := st.RecordFinish(id, RunFailed, "boom", ""); err != nil {
			t.Fatalf("RecordFinish %d: %v", i, err)
		}
	}
	if got := len(st.Runs(a.ID)); got != maxRunsPerAutomation {
		t.Fatalf("history size = %d, want the cap %d", got, maxRunsPerAutomation)
	}
	if err := st.RecordFinish("run-nope", RunCompleted, "", ""); err == nil {
		t.Fatal("RecordFinish on an unknown run id should fail")
	}
}

// TestRunsCapIndependentPerAutomation pins the per-automation cap when TWO
// automations both overflow (a regression guard: the compaction used to
// clobber the backing array while later automations' slices still read it).
func TestRunsCapIndependentPerAutomation(t *testing.T) {
	st, _ := newTestStore(t)
	a1 := dueDaily(st, st.now())
	a2 := dueDaily(st, st.now())
	for i := 0; i < maxRunsPerAutomation+7; i++ {
		for _, a := range []Automation{a1, a2} {
			id, err := st.RecordFire(a.ID, time.Now())
			if err != nil {
				t.Fatalf("RecordFire %s: %v", a.ID, err)
			}
			if err := st.RecordFinish(id, RunCompleted, "", ""); err != nil {
				t.Fatalf("RecordFinish %s: %v", a.ID, err)
			}
		}
	}
	for _, a := range []Automation{a1, a2} {
		if got := len(st.Runs(a.ID)); got != maxRunsPerAutomation {
			t.Fatalf("automation %s history size = %d, want the cap %d", a.ID, got, maxRunsPerAutomation)
		}
	}
	// And the total stays bounded when the automations differ.
	if got := len(st.runs); got > maxRunsPerAutomation*2 {
		t.Fatalf("total runs = %d, want <= %d", got, maxRunsPerAutomation*2)
	}
}

// TestRunsPersistAcrossReopen covers the run-history file.
func TestRunsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{Title: "t", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindOnce, At: "2030-01-01T00:00:00Z"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	runID, err := st.RecordFire(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.RecordFinish(runID, RunCompleted, "", "sess-9")

	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs := st2.Runs(a.ID)
	if len(runs) != 1 || runs[0].SessionID != "sess-9" {
		t.Fatalf("run history did not persist: %+v", runs)
	}
}

// TestMarkInterruptedRuns verifies startup cleanup of runs left "running"
// by a previous host process.
func TestMarkInterruptedRuns(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := Automation{Title: "t", Prompt: "p", WorkingDir: "/x",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	id1, _ := st.RecordFire(a.ID, time.Now())
	_, _ = st.RecordFire(a.ID, time.Now())
	_ = st.RecordFinish(id1, RunFailed, "boom", "")

	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := st2.MarkInterruptedRuns(); n != 1 {
		t.Fatalf("MarkInterruptedRuns = %d, want 1", n)
	}
	for _, r := range st2.Runs(a.ID) {
		if r.Status == RunRunning {
			t.Fatalf("run row still running after startup cleanup: %+v", r)
		}
	}
	if n := st2.MarkInterruptedRuns(); n != 0 {
		t.Fatalf("second cleanup marked %d more rows", n)
	}
}

// TestDeleteRemovesAutomationAndHistory covers the CLI delete path.
func TestDeleteRemovesAutomationAndHistory(t *testing.T) {
	st, _ := newTestStore(t)
	a := dueDaily(st, st.now())
	runID, err := st.RecordFire(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = runID
	if err := st.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.Get(a.ID); ok {
		t.Fatal("automation survived delete")
	}
	if runs := st.Runs(a.ID); len(runs) != 0 {
		t.Fatalf("history survived delete: %+v", runs)
	}
	if err := st.Delete("auto-nope"); err == nil {
		t.Fatal("delete of unknown id should fail")
	}
}

// TestStoreFileShapes pins the on-disk formats (machine-readable, versioned
// JSON under the global config dir).
func TestStoreFileShapes(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := dueDaily(st, st.now())
	if _, err := st.RecordFire(a.ID, st.now()); err != nil {
		t.Fatal(err)
	}

	var autos struct {
		Version     int          `json:"version"`
		Automations []Automation `json:"automations"`
	}
	data, err := os.ReadFile(filepath.Join(dir, AutomationsFile))
	if err != nil {
		t.Fatalf("automations file missing: %v", err)
	}
	if err := json.Unmarshal(data, &autos); err != nil {
		t.Fatalf("automations file is not the documented shape: %v", err)
	}
	if autos.Version != 1 || len(autos.Automations) != 1 {
		t.Fatalf("unexpected automations file: %s", data)
	}

	var runs struct {
		Version int   `json:"version"`
		Runs    []Run `json:"runs"`
	}
	data, err = os.ReadFile(filepath.Join(dir, RunsFile))
	if err != nil {
		t.Fatalf("runs file missing: %v", err)
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		t.Fatalf("runs file is not the documented shape: %v", err)
	}
	if runs.Version != 1 || len(runs.Runs) != 1 {
		t.Fatalf("unexpected runs file: %s", data)
	}
}
