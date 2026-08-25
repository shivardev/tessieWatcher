package tesla

import (
	"math"
	"testing"
)

// A representative data:update payload: a timestamp followed by one
// value per entry in streamFields, in that order.
const sampleFrame = "1787668890000,35,4719,60,228,180,35.022209,-85.162476,12,D,187,190,181"

func TestParseStreamValueUnits(t *testing.T) {
	s, err := parseStreamValue(sampleFrame)
	if err != nil {
		t.Fatalf("parseStreamValue: %v", err)
	}

	// Elevation is the one field in this frame that is NOT imperial.
	// Treating it as feet and scaling by 0.3048 made every elevation
	// 3.28x too small, which also skewed drives.ascent_m/descent_m and
	// the slope-adjusted efficiency derived from them. Caught by
	// comparing one drive against the same car's TeslaMate instance:
	// TeslaMate reported 266 ft of cumulative ascent where teslalog
	// reported 82, a ratio of exactly the foot-to-metre factor, and 228
	// read as metres matches Chattanooga's real elevation while 69 m
	// does not.
	if s.ElevationM != 228 {
		t.Errorf("elevation must be taken as metres verbatim: got %v, want 228", s.ElevationM)
	}

	// Everything else in the frame really is imperial.
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"speed", s.SpeedKmh, 35 * 1.609344},
		{"odometer", s.OdometerKm, 4719 * 1.609344},
		{"range", s.RangeKm, 187 * 1.609344},
	} {
		if math.Abs(tc.got-tc.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	if s.BatteryLevel != 60 {
		t.Errorf("battery level: got %d, want 60", s.BatteryLevel)
	}
	if s.ShiftState != "D" {
		t.Errorf("shift state: got %q, want D", s.ShiftState)
	}
	// est_lat/est_lng, not the trailing heading - an off-by-one here
	// would put every position somewhere else entirely.
	if s.Lat != 35.022209 || s.Lng != -85.162476 {
		t.Errorf("coordinates: got %v, %v", s.Lat, s.Lng)
	}
	if s.Heading != 180 {
		t.Errorf("heading should come from est_heading: got %v, want 180", s.Heading)
	}
	if s.PowerKw != 12 {
		t.Errorf("power: got %v, want 12", s.PowerKw)
	}
	if got := s.Time.UnixMilli(); got != 1787668890000 {
		t.Errorf("timestamp: got %d", got)
	}
}

// A frame with the wrong field count is skipped rather than parsed into
// silently shifted values.
func TestParseStreamValueRejectsAWrongFieldCount(t *testing.T) {
	for _, frame := range []string{
		"1787668890000,35,4719",
		sampleFrame + ",extra",
		"",
	} {
		if _, err := parseStreamValue(frame); err == nil {
			t.Errorf("expected a malformed frame to be rejected: %q", frame)
		}
	}
}

func TestParseStreamValueRejectsABadTimestamp(t *testing.T) {
	if _, err := parseStreamValue("not-a-time,35,4719,60,228,180,35.0,-85.0,12,D,187,190,181"); err == nil {
		t.Error("expected an unparseable timestamp to be rejected")
	}
}

// Tesla leaves fields blank rather than sending zero when a value is
// unavailable; those parse to 0 without failing the whole frame, which
// keeps a partial sample rather than dropping the position entirely.
func TestParseStreamValueToleratesBlankFields(t *testing.T) {
	s, err := parseStreamValue("1787668890000,,4719,60,,180,35.0,-85.0,,D,187,190,181")
	if err != nil {
		t.Fatalf("parseStreamValue: %v", err)
	}
	if s.SpeedKmh != 0 || s.ElevationM != 0 || s.PowerKw != 0 {
		t.Errorf("blank fields should read as zero, got %+v", s)
	}
	if s.OdometerKm == 0 {
		t.Error("the fields that were present should still be parsed")
	}
}
