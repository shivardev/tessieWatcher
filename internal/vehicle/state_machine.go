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
	StateUnknown   State = "unknown"
	StateAsleep    State = "asleep"
	StateOffline   State = "offline"
	StateOnline    State = "online"
	StateDriving   State = "driving"
	StateCharging  State = "charging"
	StateIdle      State = "idle"
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

func (s Snapshot) isDriving() bool {
	return s.ShiftState == "D" || s.ShiftState == "R"
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

	updateInProgress bool
	updateVersion    string
}

// New creates a Machine starting in StateUnknown. Call OnSummary or
// OnVehicleData with the first real observation to establish the
// initial state.
func New(idleTimeout time.Duration) *Machine {
	return &Machine{
		state:       StateUnknown,
		idleTimeout: idleTimeout,
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
			return m.transition(now, StateOnline)
		}
		return nil
	}

	if m.state != target {
		return m.transition(now, target)
	}
	return nil
}

// Suspend forces a transition to StateSuspended. The runner calls this
// once IdleTimedOut(now) returns true, after which it must stop calling
// OnVehicleData and switch to periodic OnSummary checks only.
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

	switch {
	case snap.isDriving():
		events = append(events, m.handleDriving(snap)...)
	case snap.isCharging():
		events = append(events, m.handleCharging(snap)...)
	default:
		events = append(events, m.handleIdle(snap)...)
	}

	events = append(events, Event{Kind: EvBatterySample, At: now, Snapshot: snap})

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
