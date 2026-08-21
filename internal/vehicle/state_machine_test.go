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
