package vehicle

import (
	"testing"
	"time"
)

func t0() time.Time { return time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC) }

func kindsOf(events []Event) []EventKind {
	var out []EventKind
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func containsKind(events []Event, k EventKind) bool {
	for _, e := range events {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func TestInitialSummaryAwake(t *testing.T) {
	m := New(3 * time.Minute)
	events := m.OnSummary(t0(), "online")
	if m.State() != StateOnline {
		t.Fatalf("expected StateOnline, got %s", m.State())
	}
	if len(events) != 1 || events[0].Kind != EvStateChanged || events[0].ToState != StateOnline {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestInitialSummaryAsleep(t *testing.T) {
	m := New(3 * time.Minute)
	events := m.OnSummary(t0(), "asleep")
	if m.State() != StateAsleep {
		t.Fatalf("expected StateAsleep, got %s", m.State())
	}
	if !containsKind(events, EvStateChanged) {
		t.Fatalf("expected a state change event, got %+v", events)
	}
}

func TestSummaryNoOpWhenAlreadyAsleep(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "asleep")
	events := m.OnSummary(t0().Add(time.Minute), "asleep")
	if events != nil {
		t.Fatalf("expected no events for repeated asleep check, got %+v", events)
	}
}

func TestDriveLifecycle(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online") // -> online

	start := Snapshot{Time: t0().Add(time.Second), ShiftState: "D", OdometerKm: 1000, BatteryLevel: 76, RangeKm: 300, Lat: 40.0, Lng: -74.0}
	events := m.OnVehicleData(start)
	if m.State() != StateDriving {
		t.Fatalf("expected StateDriving, got %s", m.State())
	}
	if !containsKind(events, EvDriveStart) {
		t.Fatalf("expected EvDriveStart, got %+v", kindsOf(events))
	}
	if !containsKind(events, EvBatterySample) {
		t.Fatalf("expected EvBatterySample on every poll, got %+v", kindsOf(events))
	}

	mid := Snapshot{Time: t0().Add(5 * time.Minute), ShiftState: "D", OdometerKm: 1010, BatteryLevel: 72, RangeKm: 288}
	events = m.OnVehicleData(mid)
	if containsKind(events, EvDriveStart) {
		t.Fatalf("should not re-emit EvDriveStart mid-drive: %+v", kindsOf(events))
	}
	if !containsKind(events, EvDrivePoint) {
		t.Fatalf("expected EvDrivePoint, got %+v", kindsOf(events))
	}

	end := Snapshot{Time: t0().Add(21 * time.Minute), ShiftState: "P", OdometerKm: 1013, BatteryLevel: 70, RangeKm: 282}
	events = m.OnVehicleData(end)
	if !containsKind(events, EvDriveEnd) {
		t.Fatalf("expected EvDriveEnd, got %+v", kindsOf(events))
	}
	if m.State() != StateIdle {
		t.Fatalf("expected StateIdle after drive ends, got %s", m.State())
	}
}

func TestChargeLifecycle(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")

	start := Snapshot{Time: t0(), ChargingState: "Charging", BatteryLevel: 20, RangeKm: 80}
	events := m.OnVehicleData(start)
	if m.State() != StateCharging {
		t.Fatalf("expected StateCharging, got %s", m.State())
	}
	if !containsKind(events, EvChargeStart) {
		t.Fatalf("expected EvChargeStart, got %+v", kindsOf(events))
	}

	mid := Snapshot{Time: t0().Add(30 * time.Minute), ChargingState: "Charging", BatteryLevel: 50, RangeKm: 200, ChargeEnergyAddedKwh: 15}
	events = m.OnVehicleData(mid)
	if !containsKind(events, EvChargePoint) {
		t.Fatalf("expected EvChargePoint, got %+v", kindsOf(events))
	}

	done := Snapshot{Time: t0().Add(60 * time.Minute), ChargingState: "Complete", BatteryLevel: 80, RangeKm: 320, ChargeEnergyAddedKwh: 34.8}
	events = m.OnVehicleData(done)
	if !containsKind(events, EvChargeEnd) {
		t.Fatalf("expected EvChargeEnd, got %+v", kindsOf(events))
	}
	if m.State() != StateIdle {
		t.Fatalf("expected StateIdle after charge ends, got %s", m.State())
	}
}

func TestIdleTimeoutAndSuspend(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")

	idleSnap := Snapshot{Time: t0(), ShiftState: "", ChargingState: "Disconnected", BatteryLevel: 80, RangeKm: 320}
	m.OnVehicleData(idleSnap)
	if m.State() != StateIdle {
		t.Fatalf("expected StateIdle, got %s", m.State())
	}

	if m.IdleTimedOut(t0().Add(2 * time.Minute)) {
		t.Fatalf("should not be timed out at 2m with a 3m timeout")
	}
	if !m.IdleTimedOut(t0().Add(4 * time.Minute)) {
		t.Fatalf("should be timed out at 4m with a 3m timeout")
	}

	events := m.Suspend(t0().Add(4 * time.Minute))
	if m.State() != StateSuspended {
		t.Fatalf("expected StateSuspended, got %s", m.State())
	}
	if len(events) != 1 || events[0].Kind != EvStateChanged || events[0].ToState != StateSuspended {
		t.Fatalf("unexpected suspend events: %+v", events)
	}

	// Idle timer resets after a fresh idle period begins (post-drive).
	m2 := New(3 * time.Minute)
	m2.OnSummary(t0(), "online")
	m2.OnVehicleData(Snapshot{Time: t0(), ShiftState: "D", OdometerKm: 0})
	m2.OnVehicleData(Snapshot{Time: t0().Add(10 * time.Minute), ShiftState: "", OdometerKm: 5})
	if m2.IdleTimedOut(t0().Add(11 * time.Minute)) {
		t.Fatalf("idle timer should have just started, not timed out yet")
	}
}

func TestNoSuspendWhileDrivingOrCharging(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")
	m.OnVehicleData(Snapshot{Time: t0(), ShiftState: "D"})
	if events := m.Suspend(t0().Add(time.Hour)); events != nil {
		t.Fatalf("Suspend should be a no-op while driving, got %+v", events)
	}
	if m.State() != StateDriving {
		t.Fatalf("state should remain Driving, got %s", m.State())
	}
}

func TestResumeFromSuspendedOnSummary(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")
	m.OnVehicleData(Snapshot{Time: t0(), ShiftState: ""})
	m.Suspend(t0().Add(5 * time.Minute))
	if m.State() != StateSuspended {
		t.Fatalf("expected StateSuspended, got %s", m.State())
	}

	events := m.OnSummary(t0().Add(time.Hour), "online")
	if m.State() != StateOnline {
		t.Fatalf("expected StateOnline after resuming, got %s", m.State())
	}
	if !containsKind(events, EvStateChanged) {
		t.Fatalf("expected state change event, got %+v", events)
	}
}

func TestSoftwareUpdateLifecycle(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")

	events := m.OnVehicleData(Snapshot{Time: t0(), UpdateStatus: "installing", UpdateVersion: "2026.20.1"})
	if !containsKind(events, EvSoftwareUpdateBeg) {
		t.Fatalf("expected EvSoftwareUpdateBeg, got %+v", kindsOf(events))
	}

	events = m.OnVehicleData(Snapshot{Time: t0().Add(10 * time.Minute), UpdateStatus: ""})
	if !containsKind(events, EvSoftwareUpdateEnd) {
		t.Fatalf("expected EvSoftwareUpdateEnd, got %+v", kindsOf(events))
	}
	for _, e := range events {
		if e.Kind == EvSoftwareUpdateEnd && e.Snapshot.UpdateVersion != "2026.20.1" {
			t.Fatalf("expected end event to carry the version that was installing, got %q", e.Snapshot.UpdateVersion)
		}
	}
}

// TestInstallingUpdateEntersItsOwnStateAndNeverIdleSuspends pins a real
// gap found by reading TeslaMate's actual source: it treats an
// installing update as its own top-level {:updating, _} state with no
// idle/suspend path out of it, precisely because a >20-minute install
// is normal - i.e. longer than the idle timeout. teslalog previously
// tracked "installing" only as a side-flag observed during whatever
// state a poll landed in (usually IDLE), so a long install would
// idle-timeout into SUSPENDED, whose cheap summary check never calls
// vehicle_data - meaning the install finishing could go unobserved
// and the software_updates row would never be completed.
func TestInstallingUpdateEntersItsOwnStateAndNeverIdleSuspends(t *testing.T) {
	const idleTimeout = 3 * time.Minute
	m := New(idleTimeout)
	m.OnSummary(t0(), "online")

	// Update starts installing while parked.
	events := m.OnVehicleData(Snapshot{Time: t0(), UpdateStatus: "installing", UpdateVersion: "2026.20.1"})
	if !containsKind(events, EvSoftwareUpdateBeg) {
		t.Fatalf("expected EvSoftwareUpdateBeg, got %+v", kindsOf(events))
	}
	if m.State() != StateUpdating {
		t.Fatalf("expected StateUpdating, got %s", m.State())
	}

	// Well past the idle timeout, still installing: must NOT be
	// considered idle-timed-out, and Suspend must refuse to fire.
	later := t0().Add(30 * time.Minute)
	m.OnVehicleData(Snapshot{Time: later, UpdateStatus: "installing", UpdateVersion: "2026.20.1"})
	if m.State() != StateUpdating {
		t.Fatalf("expected to still be StateUpdating 30min into an install, got %s", m.State())
	}
	if m.IdleTimedOut(later) {
		t.Fatalf("an installing update must never report as idle-timed-out")
	}
	if evs := m.Suspend(later); len(evs) != 0 {
		t.Fatalf("Suspend must be a no-op while updating, got %+v", kindsOf(evs))
	}
	if m.State() != StateUpdating {
		t.Fatalf("Suspend must not move the machine out of StateUpdating, got %s", m.State())
	}

	// Install finishes - now it can behave normally again.
	events = m.OnVehicleData(Snapshot{Time: later.Add(time.Minute), UpdateStatus: "", Firmware: "2026.20.1"})
	if !containsKind(events, EvSoftwareUpdateEnd) {
		t.Fatalf("expected EvSoftwareUpdateEnd, got %+v", kindsOf(events))
	}
	if m.State() != StateIdle {
		t.Fatalf("expected StateIdle once the install finished, got %s", m.State())
	}
}

// TestGoingOfflineMidUpdateReturnsToUpdatingNotOnline pins TeslaMate's
// own "went offline while updating" behavior: the update stays open and
// it just keeps re-checking. Without this, coming back online would
// land in plain StateOnline, which CAN idle-suspend - reintroducing
// exactly the gap StateUpdating exists to prevent.
func TestGoingOfflineMidUpdateReturnsToUpdatingNotOnline(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")
	m.OnVehicleData(Snapshot{Time: t0(), UpdateStatus: "installing", UpdateVersion: "2026.20.1"})
	if m.State() != StateUpdating {
		t.Fatalf("expected StateUpdating, got %s", m.State())
	}

	// Car drops offline mid-install (normal: it reboots during one).
	m.OnSummary(t0().Add(2*time.Minute), "offline")
	if m.State() != StateOffline {
		t.Fatalf("expected StateOffline while unreachable, got %s", m.State())
	}

	// Comes back - must resume StateUpdating, not plain StateOnline.
	m.OnSummary(t0().Add(8*time.Minute), "online")
	if m.State() != StateUpdating {
		t.Fatalf("expected to resume StateUpdating after coming back online mid-install, got %s", m.State())
	}
}

func TestDrivingWhileChargingClosesChargeFirst(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")
	m.OnVehicleData(Snapshot{Time: t0(), ChargingState: "Charging", BatteryLevel: 40})

	events := m.OnVehicleData(Snapshot{Time: t0().Add(time.Minute), ShiftState: "D", ChargingState: "Disconnected"})
	if !containsKind(events, EvChargeEnd) {
		t.Fatalf("expected defensive EvChargeEnd, got %+v", kindsOf(events))
	}
	if !containsKind(events, EvDriveStart) {
		t.Fatalf("expected EvDriveStart, got %+v", kindsOf(events))
	}
	if m.State() != StateDriving {
		t.Fatalf("expected StateDriving, got %s", m.State())
	}
}

// TestResumeContinuesOpenDriveInsteadOfRestarting simulates a daemon
// restart (crash, or systemd Restart=always) mid-drive: the runner would
// find a still-'open' drive row in storage and construct the machine with
// Resume(..., driving=true, ...) instead of New(...). The next
// OnVehicleData while still driving must continue that drive (EvDrivePoint)
// rather than opening a second, parallel one (EvDriveStart) that would
// leave the original dangling open forever.
func TestResumeContinuesOpenDriveInsteadOfRestarting(t *testing.T) {
	m := Resume(3*time.Minute, true, false)
	m.OnSummary(t0(), "online")

	events := m.OnVehicleData(Snapshot{Time: t0().Add(time.Second), ShiftState: "D", OdometerKm: 1010, BatteryLevel: 72})
	if containsKind(events, EvDriveStart) {
		t.Fatalf("resumed machine should not re-open the drive with EvDriveStart, got %+v", kindsOf(events))
	}
	if !containsKind(events, EvDrivePoint) {
		t.Fatalf("expected EvDrivePoint continuing the resumed drive, got %+v", kindsOf(events))
	}
	if m.State() != StateDriving {
		t.Fatalf("expected StateDriving, got %s", m.State())
	}
}

// TestResumeClosesOpenDriveIfNoLongerDriving covers the other half: the
// car finished the drive (and maybe started/finished charging too) while
// the daemon was down. On reconnect it must close out the resumed
// drive/charge with the first fresh snapshot rather than leaving them open.
func TestResumeClosesOpenDriveIfNoLongerDriving(t *testing.T) {
	m := Resume(3*time.Minute, true, true)
	m.OnSummary(t0(), "online")

	events := m.OnVehicleData(Snapshot{Time: t0().Add(time.Second), ShiftState: "", ChargingState: "Disconnected", OdometerKm: 1020, BatteryLevel: 68})
	if !containsKind(events, EvDriveEnd) {
		t.Fatalf("expected EvDriveEnd closing the resumed drive, got %+v", kindsOf(events))
	}
	if !containsKind(events, EvChargeEnd) {
		t.Fatalf("expected EvChargeEnd closing the resumed charging session, got %+v", kindsOf(events))
	}
	if m.State() != StateIdle {
		t.Fatalf("expected StateIdle, got %s", m.State())
	}
}

// TestOnUnreachablePreventsMergingAFutureDriveIntoAnAbandonedOne is a
// regression test for a real, live-found bug more severe than just
// "the drive stays incomplete": if the vehicle went unreachable
// mid-drive (see OnUnreachable's doc comment - confirmed to happen for
// real, not just theoretically) and the internal "we're mid-drive"
// belief were never reset, a LATER drive starting directly from that
// same unreachable period would be silently treated as a *continuation*
// of the abandoned one (EvDrivePoint instead of EvDriveStart) - merging
// two unrelated real-world trips, with a multi-hour gap in the middle,
// into a single corrupted row. OnUnreachable must reset that belief so
// the next real drive starts clean.
func TestOnUnreachablePreventsMergingAFutureDriveIntoAnAbandonedOne(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")

	// Drive starts normally.
	events := m.OnVehicleData(Snapshot{Time: t0().Add(time.Second), ShiftState: "D", OdometerKm: 1000, BatteryLevel: 76, RangeKm: 300})
	if !containsKind(events, EvDriveStart) {
		t.Fatalf("expected EvDriveStart, got %+v", kindsOf(events))
	}

	// The vehicle reports itself fully asleep mid-drive (not just
	// offline - see OnUnreachable's doc comment for why "asleep"
	// abandons immediately with no grace period, unlike "offline").
	events = m.OnSummary(t0().Add(2*time.Minute), "asleep")
	if !containsKind(events, EvStateChanged) {
		t.Fatalf("expected EvStateChanged to asleep, got %+v", kindsOf(events))
	}
	events = m.OnUnreachable(t0().Add(2*time.Minute), "asleep", 15*time.Minute)
	if !containsKind(events, EvDriveAbandoned) {
		t.Fatalf("expected EvDriveAbandoned, got %+v", kindsOf(events))
	}

	// Hours later, it comes back online and immediately starts a
	// brand new, unrelated drive - exactly the sequence seen live
	// (offline -> online -> driving in the same poll cycle).
	m.OnSummary(t0().Add(3*time.Hour), "online")
	events = m.OnVehicleData(Snapshot{
		Time: t0().Add(3*time.Hour + time.Second), ShiftState: "D",
		OdometerKm: 1050, BatteryLevel: 90, RangeKm: 310, // unrelated odometer/battery - a different trip
	})
	if !containsKind(events, EvDriveStart) {
		t.Fatalf("expected a fresh EvDriveStart for the new drive, got %+v (bug: would silently merge into the abandoned drive instead)", kindsOf(events))
	}
	if containsKind(events, EvDrivePoint) {
		t.Fatalf("must not emit EvDrivePoint here - that would mean this got merged into the abandoned drive: %+v", kindsOf(events))
	}
}

// TestOnBackOnlineDetectsAChargeThatHappenedWhileUnobservable pins
// TeslaMate's own inference (verified directly against its source): if
// the vehicle comes back after being unreachable having gained more
// than 5 km of ideal range over at least 5 minutes, it must have been
// charging somewhere we couldn't see. Without this, that energy is
// silently lost from charging history - and shows up in vampire-drain
// analysis as an impossible battery *gain* while parked.
func TestOnBackOnlineDetectsAChargeThatHappenedWhileUnobservable(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")

	// Last thing we saw before losing contact: parked, 40% / 200 km ideal.
	m.OnVehicleData(Snapshot{
		Time: t0(), ShiftState: "P", BatteryLevel: 40, RangeKm: 195, IdealRangeKm: 200,
	})
	m.OnSummary(t0().Add(10*time.Minute), "offline")

	// Comes back 2 hours later with substantially more range.
	after := Snapshot{
		Time: t0().Add(2 * time.Hour), ShiftState: "P", BatteryLevel: 75, RangeKm: 360, IdealRangeKm: 370,
	}
	events := m.OnBackOnline(after)
	if !containsKind(events, EvOfflineCharge) {
		t.Fatalf("expected EvOfflineCharge, got %+v", kindsOf(events))
	}
	for _, e := range events {
		if e.Kind != EvOfflineCharge {
			continue
		}
		if e.FromSnapshot.BatteryLevel != 40 || e.Snapshot.BatteryLevel != 75 {
			t.Fatalf("expected the event to carry both the before (40%%) and after (75%%) readings, got %d%% -> %d%%",
				e.FromSnapshot.BatteryLevel, e.Snapshot.BatteryLevel)
		}
		if !e.FromSnapshot.Time.Equal(t0()) {
			t.Fatalf("expected FromSnapshot to be the last pre-offline reading, got %s", e.FromSnapshot.Time)
		}
	}
}

func TestOnBackOnlineIgnoresSmallOrBriefChanges(t *testing.T) {
	cases := []struct {
		name       string
		afterIdeal float64
		afterAt    time.Duration
	}{
		// Range barely moved - normal estimation drift, not a charge.
		{"range gain under the threshold", 203, 2 * time.Hour},
		// Real range gain but over a too-short window to be a charge -
		// matches TeslaMate's own >= 5 minute floor.
		{"gap too brief", 370, 2 * time.Minute},
		// Range went DOWN (normal vampire drain) - definitely not a charge.
		{"range decreased", 194, 2 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(3 * time.Minute)
			m.OnSummary(t0(), "online")
			m.OnVehicleData(Snapshot{Time: t0(), ShiftState: "P", BatteryLevel: 40, RangeKm: 195, IdealRangeKm: 200})
			m.OnSummary(t0().Add(time.Minute), "offline")

			events := m.OnBackOnline(Snapshot{
				Time: t0().Add(c.afterAt), ShiftState: "P", BatteryLevel: 41, RangeKm: c.afterIdeal - 5, IdealRangeKm: c.afterIdeal,
			})
			if len(events) != 0 {
				t.Fatalf("expected no offline-charge inference, got %+v", kindsOf(events))
			}
		})
	}
}

func TestOnBackOnlineWithNoPriorSnapshotIsANoOp(t *testing.T) {
	// Fresh start (e.g. daemon just launched): nothing to compare
	// against, so nothing can be inferred.
	m := New(3 * time.Minute)
	if events := m.OnBackOnline(Snapshot{Time: t0(), BatteryLevel: 80, IdealRangeKm: 300}); len(events) != 0 {
		t.Fatalf("expected no events with no prior snapshot, got %+v", kindsOf(events))
	}
}

func TestOnUnreachableIsANoOpWhenNothingWasInProgress(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online") // idle, not driving/charging

	if events := m.OnUnreachable(t0().Add(time.Minute), "offline", 15*time.Minute); len(events) != 0 {
		t.Fatalf("expected no events when nothing was in progress, got %+v", kindsOf(events))
	}
}

// TestOnUnreachableOfflineWhileDrivingWaitsOutTheGracePeriod pins
// TeslaMate's real behavior (verified directly against its source):
// OFFLINE while driving is NOT abandoned immediately - only once
// unreachable for at least driveTimeout, measured from the last time
// the vehicle actually reported in, not from "now". A brief
// reconnect-within-the-grace-period (e.g. a tunnel) must not emit
// anything at all.
func TestOnUnreachableOfflineWhileDrivingWaitsOutTheGracePeriod(t *testing.T) {
	m := New(3 * time.Minute)
	m.OnSummary(t0(), "online")
	m.OnVehicleData(Snapshot{Time: t0(), ShiftState: "D", OdometerKm: 1000, BatteryLevel: 76, RangeKm: 300})

	// Only 2 minutes since the last real observation - well within a
	// 15-minute grace period.
	m.OnSummary(t0().Add(2*time.Minute), "offline")
	if events := m.OnUnreachable(t0().Add(2*time.Minute), "offline", 15*time.Minute); len(events) != 0 {
		t.Fatalf("expected no abandonment within the grace period, got %+v", kindsOf(events))
	}

	// 16 minutes since the last real observation - past the grace period.
	if events := m.OnUnreachable(t0().Add(16*time.Minute), "offline", 15*time.Minute); !containsKind(events, EvDriveAbandoned) {
		t.Fatalf("expected EvDriveAbandoned once past the grace period, got %+v", kindsOf(events))
	}
}

// TestOnUnreachableOfflineWhileChargingNeverAbandons pins TeslaMate's
// real behavior (verified directly against its source): unlike
// driving, a charging session is NEVER auto-abandoned just for going
// OFFLINE, no matter how long - only ASLEEP closes it. TeslaMate
// itself just keeps re-checking indefinitely in this case.
func TestOnUnreachableOfflineWhileChargingNeverAbandons(t *testing.T) {
	m := Resume(3*time.Minute, false, true)
	if events := m.OnUnreachable(t0().Add(365*24*time.Hour), "offline", 15*time.Minute); len(events) != 0 {
		t.Fatalf("expected charging to never auto-abandon on OFFLINE regardless of elapsed time, got %+v", kindsOf(events))
	}
}

func TestOnUnreachableAbandonsBothDriveAndCharge(t *testing.T) {
	// Mirrors handleDriving's own defensive "can't drive while
	// charging" case: shouldn't normally happen, but if it did,
	// OnUnreachable should still close out whichever ones were open
	// once the vehicle reports itself fully asleep.
	m := Resume(3*time.Minute, true, true)
	events := m.OnUnreachable(t0(), "asleep", 15*time.Minute)
	if !containsKind(events, EvDriveAbandoned) || !containsKind(events, EvChargeAbandoned) {
		t.Fatalf("expected both EvDriveAbandoned and EvChargeAbandoned, got %+v", kindsOf(events))
	}
}
