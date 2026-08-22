// Package vehicle implements the sleep-aware vehicle state machine.
//
// This is the part of TeslaMate worth reproducing carefully: a logger
// that blindly polls vehicle_data on a fixed interval forever will keep
// the car awake and burn phantom-drain energy. This package encodes the
// rule "no repeated vehicle_data calls and no wake commands while the
// car is ASLEEP/SUSPENDED" as a small, pure, unit-testable state
// machine that knows nothing about HTTP, SQLite, or the Owner API.
//
// The machine only reacts to two kinds of input the caller feeds it:
//
//   - OnSummary: the result of a *cheap* "is the vehicle awake" check
//     (Owner API GET /vehicles, which never wakes the car) — used while
//     ASLEEP/SUSPENDED to notice the car has become active on its own.
//   - OnVehicleData: the result of a full vehicle_data poll — only ever
//     called by the runner while the machine is NOT asleep/suspended.
//
// It emits a slice of Events describing what happened (state changes,
// drive/charge open/point/close) which the caller persists to storage.
// The machine itself never touches a database or the network.
package vehicle

import "time"

type State string

const (
	StateUnknown  State = "unknown"
	StateAsleep   State = "asleep"
	StateOffline  State = "offline"
	StateOnline   State = "online"
	StateDriving  State = "driving"
	StateCharging State = "charging"
	StateIdle     State = "idle"
	// StateUpdating means a software update is actively installing.
	// A real top-level state, matching TeslaMate's own {:updating, _}
	// state (verified directly against its source, lib/teslamate/
	// vehicles/vehicle.ex) rather than a flag observed incidentally
	// during whatever other state a poll happens to land in - the
	// distinction matters because an installing update must keep being
	// polled at the normal online cadence until it finishes, and must
	// NOT be allowed to idle-timeout into StateSuspended (whose cheap
	// summary check never calls vehicle_data at all, so the update
	// finishing would never be observed). A ~20+ minute install is
	// entirely normal, i.e. longer than IdleTimeout.
	StateUpdating  State = "updating"
	StateSuspended State = "suspended"
)

// Snapshot is one vehicle_data poll (or streaming-derived equivalent).
// Field set mirrors TeslaMate's positions/charges tables (see
// TeslaMate's lib/teslamate/log/{position,charge}.ex) plus a few extras
// (Heading, ShiftState) TeslaMate derives differently.
type Snapshot struct {
	Time time.Time

	// ShiftState mirrors Tesla's drive_state.shift_state: "", "P", "D",
	// "R", "N". Only "D"/"R" count as driving, matching TeslaMate.
	ShiftState string

	// ChargingState mirrors charge_state.charging_state: "Charging",
	// "Starting", "Complete", "Stopped", "Disconnected", "NoPower", ...
	ChargingState string

	OdometerKm float64
	Lat, Lng   float64
	SpeedKmh   float64
	Heading    float64
	ElevationM float64
	PowerKw    float64

	// Battery/range: TeslaMate keeps ideal, estimated, and rated range
	// as three separate figures (they diverge, especially on packs
	// where "ideal" range is deprecated/frozen) plus raw vs. usable
	// battery percentage.
	BatteryLevel       int
	UsableBatteryLevel int
	RangeKm            float64 // rated_battery_range_km
	IdealRangeKm       float64
	EstRangeKm         float64
	// BatteryHeaterOn is the only real API field here - Tesla's actual
	// charge_state has no separate bare "battery_heater" (confirmed
	// against the community API reference); "no power to heat" is
	// NotEnoughPowerToHeat below, a genuinely distinct real field.
	BatteryHeaterOn bool

	// Charging-specific.
	ChargeEnergyAddedKwh float64
	ChargerPowerKw       float64
	ChargerVoltage       float64
	ChargerActualCurrent float64
	ChargerPilotCurrent  int
	ChargerPhases        int
	ConnChargeCable      string
	FastChargerPresent   bool
	FastChargerBrand     string
	FastChargerType      string
	NotEnoughPowerToHeat bool
	// ChargeLimitSoc distinguishes "charging stopped because it hit the
	// limit" from "unplugged" - not tracked by TeslaMate at all.
	ChargeLimitSoc int

	// Climate.
	OutsideTempC          float64
	InsideTempC           float64
	FanStatus             int
	DriverTempSettingC    float64
	PassengerTempSettingC float64
	IsClimateOn           bool
	IsRearDefrosterOn     bool
	IsFrontDefrosterOn    bool
	// ClimateKeeperMode: "off"/"dog"/"camp"/"on" - not tracked by TeslaMate.
	ClimateKeeperMode string

	// Tire pressures (bar), from vehicle_state.tpms_pressure_*.
	TpmsPressureFL, TpmsPressureFR, TpmsPressureRL, TpmsPressureRR float64

	// SentryMode/IsUserPresent/ValetMode: TeslaMate reads these live only
	// to decide sleep-safety, never persists them. Stored here so the
	// data itself can explain "why won't my car sleep" independent of
	// any real-time decision - see rawVehicleData's doc comments.
	SentryMode    bool
	IsUserPresent bool
	ValetMode     bool

	// Optional software-update tracking.
	UpdateStatus  string // "", "available", "downloading", "installing"
	UpdateVersion string

	// Firmware is the car's currently *installed* software version
	// (vehicle_state.car_version), e.g. "2026.20.1" - distinct from
	// UpdateVersion, which is the version an in-progress update is
	// installing *to*. TeslaMate doesn't track this on the vehicle
	// itself either, only via software_updates rows; teslalog also
	// keeps it on vehicles for a cheap "what's currently running" read.
	Firmware string
}

// isDriving reports whether the vehicle is in a moving gear. Matches
// TeslaMate exactly: it treats D, N and R alike as driving, and only
// nil/"P" as parked (verified directly against its source - all five
// of its driving checks use `shift_state in ~w(D N R)`, and its
// drive-end check is `shift_state in [nil, "P"]`). Neutral matters:
// coasting, rolling through a car wash, or being towed/pushed all
// report N, and treating that as parked would cut a drive short
// mid-trip and start a spurious second one when it shifts back.
func (s Snapshot) isDriving() bool {
	switch s.ShiftState {
	case "D", "N", "R":
		return true
	default:
		return false
	}
}

func (s Snapshot) isCharging() bool {
	switch s.ChargingState {
	case "Charging", "Starting":
		return true
	default:
		return false
	}
}

func (s Snapshot) isInstalling() bool {
	return s.UpdateStatus == "installing"
}

type EventKind string

const (
	EvStateChanged      EventKind = "state_changed"
	EvDriveStart        EventKind = "drive_start"
	EvDrivePoint        EventKind = "drive_point"
	EvDriveEnd          EventKind = "drive_end"
	EvChargeStart       EventKind = "charge_start"
	EvChargePoint       EventKind = "charge_point"
	EvChargeEnd         EventKind = "charge_end"
	EvBatterySample     EventKind = "battery_sample"
	EvSoftwareUpdateBeg EventKind = "software_update_start"
	EvSoftwareUpdateEnd EventKind = "software_update_end"
	// EvDriveAbandoned/EvChargeAbandoned mean the vehicle stopped
	// reporting entirely (went unreachable, per OnUnreachable) while a
	// drive/charge was believed in progress - never a normal shift_state/
	// charging_state observation confirming it actually stopped. There is
	// no Snapshot to close the row with (a cheap summary check carries no
	// telemetry) - the runner closes it from the last position/sample
	// already recorded instead. See OnUnreachable's doc comment for why
	// this matters beyond just "the row stays incomplete": without it,
	// a later drive starting directly from the same unreachable period
	// (which real-world testing confirmed does happen) would otherwise
	// be silently treated as a continuation of the abandoned one.
	EvDriveAbandoned  EventKind = "drive_abandoned"
	EvChargeAbandoned EventKind = "charge_abandoned"
	// EvOfflineCharge means the vehicle gained a meaningful amount of
	// range while we couldn't see it at all, so it must have been
	// charging somewhere unobserved. Carries FromSnapshot (the last
	// reading before it went dark) and Snapshot (the first reading
	// after it came back); the runner synthesizes a complete, closed
	// charging session spanning exactly those two points. See
	// OnBackOnline.
	EvOfflineCharge EventKind = "offline_charge"
)

// Event is a single thing the runner should persist. Not every field is
// populated for every Kind; see the runner for which fields it reads
// per Kind.
type Event struct {
	Kind EventKind
	At   time.Time

	FromState State
	ToState   State

	Snapshot Snapshot
	// FromSnapshot is only set for EvOfflineCharge: the last reading
	// before the vehicle went unreachable, paired with Snapshot's
	// first reading after it came back.
	FromSnapshot Snapshot
}

// Machine is the sleep-aware vehicle state machine. Zero value is not
// usable; construct with New.
type Machine struct {
	state State

	idleTimeout time.Duration

	// idleSince is when we most recently entered StateIdle (online,
	// parked, not charging). Zero if not currently idle.
	idleSince time.Time

	driving  bool
	charging bool

	// lastDrivingAt/lastChargingAt are the timestamp of the most
	// recent successful OnVehicleData observation while driving/
	// charging - i.e. the last time we actually heard from the
	// vehicle with real telemetry, as opposed to "now" (when we
	// happen to notice it's unreachable). OnUnreachable measures its
	// grace period against this, matching TeslaMate's own real
	// behavior (verified against its source, lib/teslamate/vehicles/
	// vehicle.ex): it tracks offline_since as the last known-good
	// drive_state.timestamp, not the moment the timeout check runs.
	lastDrivingAt  time.Time
	lastChargingAt time.Time

	// lastSnapshot is the most recent successful OnVehicleData
	// observation of any kind, and haveLastSnapshot says whether
	// there's been one at all yet. Retained so that when the vehicle
	// comes back after being unreachable, the *before* and *after*
	// readings can be compared - see OnBackOnline, which uses exactly
	// that pair to detect a charge that happened while we couldn't
	// see it.
	lastSnapshot     Snapshot
	haveLastSnapshot bool
	// wasUnreachable records that the vehicle was observed
	// asleep/offline at some point since the last successful
	// OnVehicleData poll - i.e. that there's a real observation gap
	// worth examining. Tracked here rather than inferred from State()
	// by the caller, because by the time a poll succeeds the state has
	// usually already flipped back to ONLINE (the cheap summary check
	// notices reachability before the full poll runs), so State()
	// alone can't tell "there was a gap" from "never left".
	wasUnreachable bool

	// Thresholds for OnBackOnline's offline-charge inference. Default
	// to the TeslaMate-matching package constants; overridable via
	// SetOfflineChargeThresholds.
	offlineChargeMinRangeGainKm float64
	offlineChargeMinGap         time.Duration

	updateInProgress bool
	updateVersion    string
}

// New creates a Machine starting in StateUnknown. Call OnSummary or
// OnVehicleData with the first real observation to establish the
// initial state.
func New(idleTimeout time.Duration) *Machine {
	return &Machine{
		state:                       StateUnknown,
		idleTimeout:                 idleTimeout,
		offlineChargeMinRangeGainKm: OfflineChargeMinRangeGainKm,
		offlineChargeMinGap:         OfflineChargeMinGap,
	}
}

// Resume creates a Machine like New, but pre-flagged as already mid-drive
// and/or mid-charge. Use this when the runner finds a drive/charging_session
// row still marked 'open' in storage at startup (e.g. after a crash or a
// systemd Restart=always cycle) — rather than starting a brand-new
// drive/charge on top of one that's still genuinely in progress, the next
// OnVehicleData call correctly emits EvDrivePoint/EvChargePoint (continuing
// the existing open row), or EvDriveEnd/EvChargeEnd if the vehicle is no
// longer driving/charging by the time we reconnect.
func Resume(idleTimeout time.Duration, driving, charging bool) *Machine {
	m := New(idleTimeout)
	m.driving = driving
	m.charging = charging
	return m
}

// State returns the machine's current state.
func (m *Machine) State() State {
	return m.state
}

// IdleTimeout returns the configured idle timeout.
func (m *Machine) IdleTimeout() time.Duration {
	return m.idleTimeout
}

// IdleSince returns when the vehicle most recently became idle, or the
// zero time if it is not currently idle.
func (m *Machine) IdleSince() time.Time {
	return m.idleSince
}

// IdleTimedOut reports whether the vehicle has been continuously idle
// for at least the configured idle timeout as of now. The runner should
// call Suspend(now) when this returns true, which stops active
// vehicle_data polling and switches to the lightweight summary check.
func (m *Machine) IdleTimedOut(now time.Time) bool {
	if m.state != StateIdle || m.idleSince.IsZero() {
		return false
	}
	return now.Sub(m.idleSince) >= m.idleTimeout
}

func (m *Machine) transition(now time.Time, to State) []Event {
	if m.state == to {
		return nil
	}
	from := m.state
	m.state = to
	if to != StateIdle {
		m.idleSince = time.Time{}
	} else if from != StateIdle {
		m.idleSince = now
	}
	return []Event{{Kind: EvStateChanged, At: now, FromState: from, ToState: to}}
}

// OnSummary processes a cheap "is the vehicle awake" check (the Owner
// API vehicle list endpoint, which does NOT wake the car). Call this
// while ASLEEP/OFFLINE/SUSPENDED (or before the first observation) at
// the configured SuspendedCheckInterval. It never calls vehicle_data
// itself and never issues a wake command.
//
// rawState is the Owner API's own vehicle summary state string
// ("online", "asleep", or "offline" — matching TeslaMate's `states`
// enum exactly; "offline" means the car hasn't phoned home at all,
// distinct from a normal sleep).
func (m *Machine) OnSummary(now time.Time, rawState string) []Event {
	var target State
	switch rawState {
	case "online":
		target = StateOnline
	case "offline":
		target = StateOffline
	default:
		target = StateAsleep // "asleep", or anything unrecognized
	}

	if target == StateOnline {
		if m.state == StateAsleep || m.state == StateOffline || m.state == StateSuspended || m.state == StateUnknown {
			// If an update was installing when the vehicle dropped
			// offline, go back to StateUpdating rather than plain
			// StateOnline: the install is still in progress as far as
			// anything here knows (updateInProgress is still set,
			// since only observing a *finished* install clears it),
			// and StateUpdating is what keeps it from idle-suspending
			// before that observation ever happens. Matches
			// TeslaMate's own "went offline while updating" handling,
			// which keeps its {:updating, _} state and just keeps
			// re-checking (verified against its source).
			if m.updateInProgress {
				return m.transition(now, StateUpdating)
			}
			return m.transition(now, StateOnline)
		}
		return nil
	}

	// target is StateOffline or StateAsleep here - either way the
	// vehicle is unobservable, so note that there's now a gap in what
	// we've actually seen (see wasUnreachable's doc comment).
	m.wasUnreachable = true

	if m.state != target {
		return m.transition(now, target)
	}
	return nil
}

// MarkUnreachable records an observation gap without going through
// OnSummary - used when a full vehicle_data poll fails outright (the
// car returning "vehicle unavailable", a network error), which is the
// other way teslalog learns the vehicle isn't reachable. See
// wasUnreachable's doc comment.
func (m *Machine) MarkUnreachable() {
	m.wasUnreachable = true
}

// OnUnreachable is called (in addition to OnSummary) whenever a cheap
// summary check finds the vehicle ASLEEP/OFFLINE while a drive or
// charge was believed still in progress - i.e. the vehicle stopped
// reporting entirely instead of ever confirming a normal stop via
// OnVehicleData's shift_state/charging_state observation. rawState is
// OnSummary's own rawState parameter ("asleep" or "offline" - this is
// only ever called from the asleep-like branch, so no other value is
// expected); driveTimeout matches TeslaMate's own @drive_timeout_min
// (default 15 minutes).
//
// The exact rules below are copied from TeslaMate's real behavior
// (verified directly against lib/teslamate/vehicles/vehicle.ex's
// source, not reconstructed from memory/docs - see this project's
// commit history for why that distinction matters), not invented:
//
//   - DRIVING + asleep: abandon immediately. The vehicle actively
//     reporting itself as fully asleep while we thought it was
//     driving is a strong, immediate signal something changed -
//     TeslaMate doesn't wait here either.
//   - DRIVING + offline: only abandon once unreachable for at least
//     driveTimeout, measured from the last time we actually heard
//     from it (lastDrivingAt) - NOT from now. A car that reconnects
//     within the grace period (e.g. a brief tunnel/parking-garage
//     dead zone) is left alone entirely; no event, no premature split
//     of one real drive into two.
//   - CHARGING + asleep: abandon immediately, matching TeslaMate's
//     own "asleep while charging (?)" handling.
//   - CHARGING + offline: never auto-abandoned. TeslaMate itself just
//     keeps re-checking forever in this case (logging a warning, no
//     timeout at all) - a charging session left plugged in is in no
//     hurry to be closed on a guess, unlike a drive.
func (m *Machine) OnUnreachable(now time.Time, rawState string, driveTimeout time.Duration) []Event {
	var events []Event

	if m.driving {
		switch rawState {
		case "asleep":
			events = append(events, Event{Kind: EvDriveAbandoned, At: now})
			m.driving = false
		case "offline":
			if !m.lastDrivingAt.IsZero() && now.Sub(m.lastDrivingAt) >= driveTimeout {
				events = append(events, Event{Kind: EvDriveAbandoned, At: now})
				m.driving = false
			}
			// else: still within the grace period - keep waiting.
		}
	}

	if m.charging && rawState == "asleep" {
		events = append(events, Event{Kind: EvChargeAbandoned, At: now})
		m.charging = false
	}
	// charging + "offline": deliberately no timeout at all - see doc comment.

	return events
}

// OfflineChargeMinRangeGainKm/OfflineChargeMinGap match TeslaMate's
// own thresholds for this inference exactly (verified directly against
// its source: `ideal_battery_range` gain > 5 and offline_min >= 5).
// Package-level defaults; a Machine can override them via
// SetOfflineChargeThresholds (only end-to-end tests need to, so a
// simulated gap doesn't have to take five real minutes).
const (
	OfflineChargeMinRangeGainKm = 5.0
	OfflineChargeMinGap         = 5 * time.Minute
)

// SetOfflineChargeThresholds overrides this machine's offline-charge
// detection thresholds. Intended for tests that need to exercise the
// full detection path without waiting out the real defaults; leave
// them alone in production so the behavior matches TeslaMate's.
func (m *Machine) SetOfflineChargeThresholds(minRangeGainKm float64, minGap time.Duration) {
	m.offlineChargeMinRangeGainKm = minRangeGainKm
	m.offlineChargeMinGap = minGap
}

// OnBackOnline is called with the first successful OnVehicleData
// snapshot after the vehicle had been unreachable, BEFORE that
// snapshot is passed to OnVehicleData itself. It detects the one thing
// that can only be inferred from the before/after pair: the vehicle
// gained a meaningful amount of range while we couldn't see it, which
// means it was charging somewhere unobserved.
//
// TeslaMate does exactly this (verified directly against its source,
// lib/teslamate/vehicles/vehicle.ex): on coming back online after
// being offline, if ideal range grew by more than 5 km over at least 5
// minutes, it synthesizes a complete charging session from the two
// readings so the energy shows up in charging history instead of
// silently vanishing. Without it, a charge that happened during any
// connectivity gap is simply lost - and (worse) shows up in
// vampire-drain analysis as an impossible battery *gain* while parked.
//
// Returns an EvOfflineCharge event carrying both snapshots, or nothing
// if the thresholds aren't met.
func (m *Machine) OnBackOnline(snap Snapshot) []Event {
	// Only meaningful if there was actually a gap - a normal
	// back-to-back poll sequence has nothing unobserved to infer from.
	if !m.wasUnreachable {
		return nil
	}
	m.wasUnreachable = false

	if !m.haveLastSnapshot {
		return nil
	}
	last := m.lastSnapshot

	// Use ideal range specifically, matching TeslaMate - it's the
	// figure least affected by driving-style/temperature estimation
	// than the rated/estimated ones, so a gain in it is the strongest
	// available signal that real energy went in.
	if last.IdealRangeKm <= 0 || snap.IdealRangeKm <= 0 {
		return nil
	}
	if snap.IdealRangeKm-last.IdealRangeKm <= m.offlineChargeMinRangeGainKm {
		return nil
	}
	if snap.Time.Sub(last.Time) < m.offlineChargeMinGap {
		return nil
	}

	return []Event{{
		Kind: EvOfflineCharge, At: snap.Time,
		FromSnapshot: last, Snapshot: snap,
	}}
}

// Suspend forces a transition to StateSuspended. The runner calls this
// once IdleTimedOut(now) returns true, after which it must stop calling
// OnVehicleData and switch to periodic OnSummary checks only.
//
// Deliberately a no-op from any state other than IDLE/ONLINE - notably
// including StateUpdating, which must keep being polled until the
// install finishes (see its doc comment). IdleTimedOut can't return
// true while updating anyway (transition() clears idleSince on the way
// into any non-IDLE state), but this guard makes the invariant local
// and explicit rather than depending on that.
func (m *Machine) Suspend(now time.Time) []Event {
	if m.state != StateIdle && m.state != StateOnline {
		return nil
	}
	return m.transition(now, StateSuspended)
}

// OnVehicleData processes a full vehicle_data poll. The runner must
// only call this while State() is Online, Driving, Charging, or Idle —
// never while Asleep/Suspended (doing so would defeat the point of
// this whole package).
func (m *Machine) OnVehicleData(snap Snapshot) []Event {
	now := snap.Time
	var events []Event

	// A successful vehicle_data response means the car is awake, no
	// matter what state we thought it was in.
	if m.state == StateAsleep || m.state == StateOffline || m.state == StateSuspended || m.state == StateUnknown {
		events = append(events, m.transition(now, StateOnline)...)
	}

	events = append(events, m.handleSoftwareUpdate(snap)...)

	// An installing update takes priority over driving/charging/idle
	// classification and holds the machine in StateUpdating until it
	// finishes, matching TeslaMate's own {:updating, _} state (its
	// dispatch checks software_update.status == "installing" before
	// anything else too, and its :updating state has no idle/suspend
	// path out at all). Critically this keeps IdleTimedOut from ever
	// firing mid-install - see StateUpdating's doc comment.
	if snap.isInstalling() {
		events = append(events, m.transition(now, StateUpdating)...)
		events = append(events, Event{Kind: EvBatterySample, At: now, Snapshot: snap})
		m.lastSnapshot, m.haveLastSnapshot = snap, true
		return events
	}

	switch {
	case snap.isDriving():
		events = append(events, m.handleDriving(snap)...)
	case snap.isCharging():
		events = append(events, m.handleCharging(snap)...)
	default:
		events = append(events, m.handleIdle(snap)...)
	}

	events = append(events, Event{Kind: EvBatterySample, At: now, Snapshot: snap})

	m.lastSnapshot, m.haveLastSnapshot = snap, true
	return events
}

func (m *Machine) handleDriving(snap Snapshot) []Event {
	var events []Event

	if m.charging {
		// Shouldn't normally happen (can't drive while plugged in and
		// actively charging), but close out defensively.
		events = append(events, Event{Kind: EvChargeEnd, At: snap.Time, Snapshot: snap})
		m.charging = false
	}

	events = append(events, m.transition(snap.Time, StateDriving)...)

	if !m.driving {
		m.driving = true
		events = append(events, Event{Kind: EvDriveStart, At: snap.Time, Snapshot: snap})
	} else {
		events = append(events, Event{Kind: EvDrivePoint, At: snap.Time, Snapshot: snap})
	}
	m.lastDrivingAt = snap.Time
	return events
}

func (m *Machine) handleCharging(snap Snapshot) []Event {
	var events []Event

	if m.driving {
		events = append(events, Event{Kind: EvDriveEnd, At: snap.Time, Snapshot: snap})
		m.driving = false
	}

	events = append(events, m.transition(snap.Time, StateCharging)...)

	if !m.charging {
		m.charging = true
		events = append(events, Event{Kind: EvChargeStart, At: snap.Time, Snapshot: snap})
	} else {
		events = append(events, Event{Kind: EvChargePoint, At: snap.Time, Snapshot: snap})
	}
	m.lastChargingAt = snap.Time
	return events
}

func (m *Machine) handleIdle(snap Snapshot) []Event {
	var events []Event

	if m.driving {
		events = append(events, Event{Kind: EvDriveEnd, At: snap.Time, Snapshot: snap})
		m.driving = false
	}
	if m.charging {
		events = append(events, Event{Kind: EvChargeEnd, At: snap.Time, Snapshot: snap})
		m.charging = false
	}

	events = append(events, m.transition(snap.Time, StateIdle)...)
	return events
}

func (m *Machine) handleSoftwareUpdate(snap Snapshot) []Event {
	var events []Event
	switch {
	case snap.isInstalling() && !m.updateInProgress:
		m.updateInProgress = true
		m.updateVersion = snap.UpdateVersion
		events = append(events, Event{Kind: EvSoftwareUpdateBeg, At: snap.Time, Snapshot: snap})
	case !snap.isInstalling() && m.updateInProgress:
		// The car may already report a new car_version/no version by
		// the time we observe installation finished, so stamp the
		// event with the version we were tracking, not snap's
		// (possibly now-empty) UpdateVersion.
		finished := snap
		finished.UpdateVersion = m.updateVersion
		m.updateInProgress = false
		m.updateVersion = ""
		events = append(events, Event{Kind: EvSoftwareUpdateEnd, At: snap.Time, Snapshot: finished})
	}
	return events
}
