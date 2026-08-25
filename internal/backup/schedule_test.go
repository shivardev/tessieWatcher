package backup

import (
	"testing"
	"time"
)

// TestFileNameStampsLocalTime pins the naming the user reads. A UTC date
// rolls over at 8pm in New York, so a UTC-named file disagrees with the
// day anyone would call it; the offset is included so the name stays
// unambiguous across a DST change, when the same local hour happens
// twice.
func TestFileNameStampsLocalTime(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}

	// 00:30 UTC on the 26th is still the evening of the 25th in New York.
	at := time.Date(2026, 8, 26, 0, 30, 0, 0, time.UTC).In(newYork)
	if got, want := FileName(at), "teslalog-2026-08-25_203000-0400.db.gz"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	// Winter, so the offset differs. Both must round-trip.
	winter := time.Date(2026, 1, 15, 8, 5, 0, 0, newYork)
	if got, want := FileName(winter), "teslalog-2026-01-15_080500-0500.db.gz"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestParseBackupTimeRoundTrips(t *testing.T) {
	at := time.Date(2026, 8, 25, 3, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	parsed, err := parseBackupTime(FileName(at))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Equal(at) {
		t.Errorf("round trip lost the instant: got %s, want %s", parsed, at)
	}
}

// Upgrading must not orphan the backups already on disk: prune skips
// names it cannot parse, so an unrecognised older file would never be
// cleaned up again.
func TestParseBackupTimeAcceptsTheOlderDateOnlyName(t *testing.T) {
	parsed, err := parseBackupTime("teslalog-2026-08-21.db.gz")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Year() != 2026 || parsed.Month() != time.August || parsed.Day() != 21 {
		t.Errorf("unexpected date: %s", parsed)
	}
}

func TestParseBackupTimeRejectsForeignFiles(t *testing.T) {
	for _, name := range []string{"notes.db.gz", "teslalog-latest.db.gz", "teslalog-.db.gz"} {
		if _, err := parseBackupTime(name); err == nil {
			t.Errorf("%s: expected a parse failure so prune leaves it alone", name)
		}
	}
}

// TestNextDelayTargetsTheNextLocalOccurrence is the property that makes
// "every day at 03:00" true rather than approximately true. Computed from
// the wall clock each time rather than by adding 24h, so a DST change
// does not shift the backup an hour twice a year.
func TestNextDelayTargetsTheNextLocalOccurrence(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	s := Scheduler{DailyAt: "03:00", Interval: 24 * time.Hour}

	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"before today's run", time.Date(2026, 8, 25, 1, 0, 0, 0, newYork), 2 * time.Hour},
		{"after today's run", time.Date(2026, 8, 25, 4, 0, 0, 0, newYork), 23 * time.Hour},
		{"exactly at the target waits a full day", time.Date(2026, 8, 25, 3, 0, 0, 0, newYork), 24 * time.Hour},
	}
	for _, tc := range cases {
		if got := s.nextDelay(tc.now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Spring forward: 02:00 to 03:00 does not exist on 2026-03-08 in New
// York. Adding 24h to the previous run would land the backup at 04:00 and
// keep it there. Recomputing from the wall clock keeps it at 03:00.
func TestNextDelaySurvivesSpringForward(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	s := Scheduler{DailyAt: "03:00", Interval: 24 * time.Hour}

	before := time.Date(2026, 3, 7, 3, 0, 0, 0, newYork)
	next := before.Add(s.nextDelay(before))
	if hour := next.In(newYork).Hour(); hour != 3 {
		t.Errorf("expected the next run at 03:00 local, got %02d:00 (%s)", hour, next.In(newYork))
	}
}

func TestNextDelayFallsBackToIntervalWithNoDailyTime(t *testing.T) {
	s := Scheduler{Interval: 6 * time.Hour}
	if got := s.nextDelay(time.Now()); got != 6*time.Hour {
		t.Errorf("got %v, want the configured interval", got)
	}
}

// An unparseable time must not silently stop backups altogether.
func TestNextDelayFallsBackOnAnInvalidDailyTime(t *testing.T) {
	s := Scheduler{DailyAt: "tea time", Interval: 12 * time.Hour}
	if got := s.nextDelay(time.Now()); got != 12*time.Hour {
		t.Errorf("got %v, want the configured interval", got)
	}
}
