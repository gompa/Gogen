package automation

import (
	"testing"
	"time"
)

// ny is the DST-rich zone the schedule tests use, resolved with
// time.LoadLocation (same source as production math). mustZone returns UTC
// when the zone database is unavailable; the DST cases additionally skip on
// zoneAvailable so a platform without tzdata never fails them.
var ny = mustZone("America/New_York")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func zoneAvailable(name string) bool {
	_, err := time.LoadLocation(name)
	return err == nil
}

// TestNextRun runs the next-fire-time table. Every case pins the expected
// instant with time.Date in the target zone (the same primitive NextRun
// uses), so the tests document the DST behavior of the tz library rather
// than hand-computed seconds.
func TestNextRun(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	berlin, _ := time.LoadLocation("Europe/Berlin")

	cases := []struct {
		name     string
		schedule Schedule
		after    time.Time
		want     time.Time
		wantZero bool // no next fire
		wantErr  bool
	}{
		{
			name:     "daily/every day same wall clock",
			schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "daily/strictly after, same-day later tick",
			schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 7, 30, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "daily/empty time defaults to 09:00",
			schedule: Schedule{Kind: KindDaily, Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "daily/skips to next weekday (Mon–Fri) from Friday evening",
			schedule: Schedule{Kind: KindWeekdays, Time: "10:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC), // Friday
			want:     time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC), // Monday
		},
		{
			name:     "weekdays/fire same weekday before the time",
			schedule: Schedule{Kind: KindWeekdays, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 5, 7, 0, 0, 0, time.UTC), // Monday
			want:     time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "weekly/Mon+Wed+Fri from Saturday goes to Monday",
			schedule: Schedule{Kind: KindWeekly, Weekdays: []int{1, 3, 5}, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC), // Saturday
			want:     time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC),  // Monday
		},
		{
			name:     "weekly/earliest selected weekday wins",
			schedule: Schedule{Kind: KindWeekly, Weekdays: []int{3, 5}, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 5, 9, 1, 0, 0, time.UTC), // Monday (not selected)
			want:     time.Date(2026, 1, 7, 9, 0, 0, 0, time.UTC), // Wednesday
		},
		{
			name:     "hourly/next :15 tick",
			schedule: Schedule{Kind: KindHourly, Minute: 15, Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 10, 20, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC),
		},
		{
			name:     "hourly/exactly on the tick advances one hour",
			schedule: Schedule{Kind: KindHourly, Minute: 15, Timezone: "UTC"},
			after:    time.Date(2026, 1, 2, 10, 15, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC),
		},
		{
			name:     "once/future instant fires as-is",
			schedule: Schedule{Kind: KindOnce, At: "2026-08-10T09:00:00Z"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			want:     time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
		},
		{
			name:     "once/past instant yields no next fire",
			schedule: Schedule{Kind: KindOnce, At: "2025-08-10T09:00:00Z"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantZero: true,
		},
		{
			name:     "once/plain local spelling is interpreted in the schedule zone",
			schedule: Schedule{Kind: KindOnce, At: "2026-08-10 09:00", Timezone: "America/New_York"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			want:     time.Date(2026, 8, 10, 9, 0, 0, 0, ny),
		},
		{
			name:     "weekly with no weekdays yields no next fire (not an error)",
			schedule: Schedule{Kind: KindWeekly, Weekdays: nil, Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantZero: true,
		},
		{
			name:     "unknown kind is an error",
			schedule: Schedule{Kind: " fortnightly", Time: "09:00", Timezone: "UTC"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		{
			name:     "invalid timezone is an error",
			schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "Mars/Olympus"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		{
			name:     "malformed once-time is an error",
			schedule: Schedule{Kind: KindOnce, At: "not-a-time"},
			after:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantErr:  true,
		},
		// --- DST edges ----------------------------------------------------
		{
			// 2025-03-09: US spring forward, 02:00→03:00. A daily 09:00 job
			// stays 09:00 WALL CLOCK; the real-time gap is 23 h, not 24 h.
			name:     "dst/daily 09:00 America/New_York across spring forward",
			schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "America/New_York"},
			after:    time.Date(2025, 3, 8, 9, 0, 0, 0, ny),
			want:     time.Date(2025, 3, 9, 9, 0, 0, 0, ny),
		},
		{
			// 2025-03-09 02:30 does not exist: the job must NOT fire an hour
			// early (Go's raw normalization would say 01:30 EST) — it fires
			// just after the gap, at 03:30 EDT (the cron convention).
			name:     "dst/daily 02:30 in the spring-forward gap fires after the gap",
			schedule: Schedule{Kind: KindDaily, Time: "02:30", Timezone: "America/New_York"},
			after:    time.Date(2025, 3, 9, 0, 0, 0, 0, ny),
			want:     time.Date(2025, 3, 9, 3, 30, 0, 0, ny), // 07:30Z = 03:30 EDT
		},
		{
			// 2025-11-02: US fall back, 02:00 EDT→01:00 EST. 01:30 occurs
			// twice; Go's wall-pinned construction picks the FIRST (EDT)
			// occurrence — the cron convention for "take the earlier fire".
			name:     "dst/daily 01:30 on the fall-back day takes the earlier occurrence",
			schedule: Schedule{Kind: KindDaily, Time: "01:30", Timezone: "America/New_York"},
			after:    time.Date(2025, 11, 1, 13, 0, 0, 0, ny),
			want:     time.Date(2025, 11, 2, 1, 30, 0, 0, ny), // 05:30Z = 01:30 EDT
		},
		{
			// 02:30 does not exist on the gap day: the next hourly tick past
			// the 01:30 candidate is 03:30 EDT, never 01:30/02:30 twice.
			name:     "dst/hourly :30 through the spring-forward gap",
			schedule: Schedule{Kind: KindHourly, Minute: 30, Timezone: "America/New_York"},
			after:    time.Date(2025, 3, 9, 1, 30, 0, 0, ny), // 06:30Z (the wall-pinned 01:30 EST)
			want:     time.Date(2025, 3, 9, 3, 30, 0, 0, ny), // 07:30Z
		},
		{
			// The Europe/Berlin gap (2025-03-30, 02:00→03:00 CET→CEST) hits
			// at a different hour of day than the US one: same rule, other
			// zone, proving the fix is zone-generic.
			name:     "dst/daily 02:30 in the Berlin spring-forward gap",
			schedule: Schedule{Kind: KindDaily, Time: "02:30", Timezone: "Europe/Berlin"},
			after:    time.Date(2025, 3, 29, 23, 0, 0, 0, berlin),
			want:     time.Date(2025, 3, 30, 3, 30, 0, 0, berlin),
		},
		{
			// Wall-clock pinning means the real-time day is 23 h on the
			// spring-forward day and 25 h on the fall-back day.
			name:     "dst/daily 09:00 on the fall-back day stays 09:00 (25 h gap)",
			schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "America/New_York"},
			after:    time.Date(2025, 11, 1, 9, 0, 0, 0, ny),
			want:     time.Date(2025, 11, 2, 9, 0, 0, 0, ny),
		},
		{
			name:     "dst/weekdays over a weekend that contains the zone change",
			schedule: Schedule{Kind: KindWeekdays, Time: "09:00", Timezone: "America/New_York"},
			after:    time.Date(2025, 3, 7, 17, 0, 0, 0, ny), // Friday
			want:     time.Date(2025, 3, 10, 9, 0, 0, 0, ny), // Monday (EDT)
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.schedule.Timezone != "" && !zoneAvailable(tc.schedule.Timezone) {
				t.Skipf("tzdata for %q unavailable on this platform", tc.schedule.Timezone)
			}
			got, err := NextRun(tc.schedule, tc.after)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NextRun(%v, %v) = %v, want error", tc.schedule, tc.after, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NextRun(%v, %v) error: %v", tc.schedule, tc.after, err)
			}
			if tc.wantZero {
				if !got.IsZero() {
					t.Fatalf("NextRun(%v, %v) = %v, want zero (no next fire)", tc.schedule, tc.after, got)
				}
				return
			}
			if got.IsZero() {
				t.Fatalf("NextRun(%v, %v) = zero, want %v", tc.schedule, tc.after, tc.want)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("NextRun(%v, %v) = %v (utc %s, %s), want %v (utc %s, %s)",
					tc.schedule, tc.after,
					got.Format(time.RFC3339), got.UTC().Format("15:04Z"), got.Format("15:04 MST"),
					tc.want.Format(time.RFC3339), tc.want.UTC().Format("15:04Z"), tc.want.Format("15:04 MST"))
			}
			if !got.After(tc.after) {
				t.Fatalf("NextRun = %v not after %v", got, tc.after)
			}
		})
	}
}

// TestNextRunDaylightSavingGapHourLong pins the real-time day length the
// wall-clock pinning produces: 23 h on spring-forward, 25 h on fall-back.
func TestNextRunDaylightSavingDayLength(t *testing.T) {
	if !zoneAvailable("America/New_York") {
		t.Skip("tzdata for America/New_York unavailable on this platform")
	}
	cases := []struct {
		name    string
		after   time.Time
		wantDur time.Duration
	}{
		{"spring forward day is 23h", time.Date(2025, 3, 8, 9, 0, 0, 0, ny), 23 * time.Hour},
		{"normal day is 24h", time.Date(2025, 6, 1, 9, 0, 0, 0, ny), 24 * time.Hour},
		{"fall back day is 25h", time.Date(2025, 11, 1, 9, 0, 0, 0, ny), 25 * time.Hour},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			next, err := NextRun(Schedule{Kind: KindDaily, Time: "09:00", Timezone: "America/New_York"}, tc.after)
			if err != nil {
				t.Fatalf("NextRun: %v", err)
			}
			if got := next.Sub(tc.after); got != tc.wantDur {
				t.Fatalf("day length = %v, want %v", got, tc.wantDur)
			}
		})
	}
}

// TestParseClockTable covers the HH:MM parser (and the 09:00 default).
func TestParseClockTable(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantH   int
		wantM   int
		wantErr bool
	}{
		{"empty defaults to 09:00", "", 9, 0, false},
		{"plain", "09:30", 9, 30, false},
		{"no leading zeros", "9:05", 9, 5, false},
		{"midnight", "00:00", 0, 0, false},
		{"end of day", "23:59", 23, 59, false},
		{"hour overflow", "24:00", 0, 0, true},
		{"minute overflow", "09:60", 0, 0, true},
		{"garbage", "nine", 0, 0, true},
		{"missing minutes", "09", 0, 0, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h, m, err := ParseClock(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseClock(%q) = %d:%d, want error", tc.in, h, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClock(%q) error: %v", tc.in, err)
			}
			if h != tc.wantH || m != tc.wantM {
				t.Fatalf("ParseClock(%q) = %d:%d, want %d:%d", tc.in, h, m, tc.wantH, tc.wantM)
			}
		})
	}
}

// TestParseWeekdaysTable covers the --weekly list parser.
func TestParseWeekdaysTable(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []int
		wantErr bool
	}{
		{"single", "3", []int{3}, false},
		{"multiple sorted and deduped", "5,1,3,1", []int{1, 3, 5}, false},
		{"spaced", " 1, 5 ", []int{1, 5}, false},
		{"sunday is 0", "0", []int{0}, false},
		{"saturday is 6", "6", []int{6}, false},
		{"out of range", "7", nil, true},
		{"negative", "-1", nil, true},
		{"empty", "", nil, true},
		{"garbage", "mon", nil, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseWeekdays(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseWeekdays(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWeekdays(%q) error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseWeekdays(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseWeekdays(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestValidateTable pins the automation validator: a bad schedule must be
// rejected at create time so the sweep never sees one.
func TestValidateTable(t *testing.T) {
	base := func(mutate func(*Automation)) Automation {
		a := Automation{
			Title:      "Nightly triage",
			Prompt:     "triage",
			WorkingDir: "/tmp/x",
			Schedule:   Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"},
		}
		mutate(&a)
		return a
	}
	cases := []struct {
		name    string
		auto    Automation
		wantErr string
	}{
		{"valid daily", base(func(*Automation) {}), ""},
		{"missing title", base(func(a *Automation) { a.Title = " " }), "title"},
		{"missing prompt", base(func(a *Automation) { a.Prompt = "" }), "prompt"},
		{"missing working dir", base(func(a *Automation) { a.WorkingDir = "" }), "working dir"},
		{"unknown kind", base(func(a *Automation) { a.Schedule.Kind = "monthly" }), "unknown schedule kind"},
		{"hourly minute out of range", base(func(a *Automation) { a.Schedule = Schedule{Kind: KindHourly, Minute: 61} }), "minute"},
		{"bad time", base(func(a *Automation) { a.Schedule.Time = "9am" }), "time"},
		{"weekly without weekdays", base(func(a *Automation) { a.Schedule = Schedule{Kind: KindWeekly, Time: "09:00"} }), "weekday"},
		{"weekly weekday out of range", base(func(a *Automation) { a.Schedule = Schedule{Kind: KindWeekly, Weekdays: []int{7}, Time: "09:00"} }), "weekday"},
		{"bad timezone", base(func(a *Automation) { a.Schedule.Timezone = "Nope/Nope" }), "timezone"},
		{"once without at", base(func(a *Automation) { a.Schedule = Schedule{Kind: KindOnce} }), "at"},
		{"valid once", base(func(a *Automation) { a.Schedule = Schedule{Kind: KindOnce, At: "2026-08-10T09:00:00Z"} }), ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.auto.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestScheduleDescribe covers the human-readable schedule rendering used by
// the CLI table.
func TestScheduleDescribe(t *testing.T) {
	cases := []struct {
		name     string
		schedule Schedule
		want     string
	}{
		{"once", Schedule{Kind: KindOnce, At: "2026-08-10T09:00:00Z"}, "once 2026-08-10T09:00:00Z"},
		{"hourly local", Schedule{Kind: KindHourly, Minute: 15}, "hourly :15 local"},
		{"daily", Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"}, "daily 09:00 UTC"},
		{"weekdays", Schedule{Kind: KindWeekdays, Time: "22:00", Timezone: "Europe/Berlin"}, "weekdays 22:00 Europe/Berlin"},
		{"weekly", Schedule{Kind: KindWeekly, Weekdays: []int{1, 3, 5}, Time: "09:00"}, "weekly Mon,Wed,Fri 09:00 local"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.schedule.Describe(); got != tc.want {
				t.Fatalf("Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}
