// Package runner wires together storage, the Tesla Owner API client,
// the streaming client, and the vehicle state machine into the daemon
// loop that `teslalog run` executes. It is the only place that decides
// *when* to call vehicle_data vs. the cheap vehicle-list check — the
// rule enforced here, mechanically, is:
//
//	ASLEEP / SUSPENDED -> only ListVehicles (never wakes the car),
//	                      checked every SuspendedCheckInterval.
//	DRIVING            -> vehicle_data every Polling.DrivingInterval.
//	CHARGING           -> vehicle_data every Polling.ChargingInterval.
//	ONLINE / IDLE      -> vehicle_data every Polling.OnlineInterval.
//
// wake_up is never called from this loop.
package runner

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"teslalog/internal/backup"
	"teslalog/internal/config"
	"teslalog/internal/geocode"
	"teslalog/internal/portal"
	"teslalog/internal/storage"
	"teslalog/internal/tesla"
	"teslalog/internal/vehicle"
)

// Run starts the teslalog daemon loop and blocks until ctx is canceled.
func Run(ctx context.Context, cfg config.Config, version string) error {
	var logBuf *portal.LogBuffer // wired up below only if cfg.Portal.Enabled

	store, err := storage.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	client, err := tesla.NewClient(cfg.API, tesla.FileTokenStore{Path: cfg.TokenFile})
	if err != nil {
		return fmt.Errorf("create tesla client: %w", err)
	}

	summary, err := pickVehicle(ctx, client, cfg.VIN)
	if err != nil {
		return fmt.Errorf("select vehicle: %w", err)
	}
	slog.Info("selected vehicle", "vin", summary.VIN, "display_name", summary.DisplayName)

	vehicleDBID, err := store.UpsertVehicle(storage.VehicleMeta{
		VIN: summary.VIN, TeslaID: fmt.Sprint(summary.VehicleID), DisplayName: summary.DisplayName,
	})
	if err != nil {
		return fmt.Errorf("upsert vehicle: %w", err)
	}
	if cfg.Vehicle.EfficiencyWhKm > 0 {
		// Seed, don't overwrite: a figure already derived from real
		// charging history is strictly better than the config guess,
		// and must survive restarts. See SeedVehicleEfficiency.
		if err := store.SeedVehicleEfficiency(vehicleDBID, cfg.Vehicle.EfficiencyWhKm); err != nil {
			slog.Warn("seed vehicle efficiency failed", "error", err)
		}
	}

	if cfg.Backup.Enabled {
		sched := backup.Scheduler{
			DBPath:        cfg.Database,
			BackupDir:     cfg.Backup.Dir,
			RetentionDays: cfg.Backup.RetentionDays,
			Interval:      cfg.Backup.Interval,
		}
		go sched.Start(ctx)
	}

	if cfg.Portal.Enabled {
		// Capture recent log activity into memory so the portal page can
		// show "what's happening" without needing journalctl access or
		// any special permissions. This tees into the buffer IN ADDITION
		// to the normal stderr output (still what systemd/journalctl
		// captures) - never a replacement for it.
		logBuf = portal.NewLogBuffer(200)
		slog.SetDefault(slog.New(teeHandler{slog.NewTextHandler(os.Stderr, nil), logBuf.Handler()}))

		srv := portal.New(store, cfg.Database, logBuf, cfg.Portal.Units, version)
		go func() {
			if err := srv.Run(ctx, cfg.Portal.Addr); err != nil {
				slog.Error("portal server failed", "error", err)
			}
		}()
		slog.Info("portal enabled", "addr", cfg.Portal.Addr)
	}

	// Recover from a crash or a systemd Restart=always cycle that left a
	// drive or charging session marked 'open' in storage: resume writing to
	// that same row instead of leaving it dangling forever while starting a
	// second, parallel one. See vehicle.Resume's doc comment.
	openDriveID, err := store.OpenDriveID(vehicleDBID)
	if err != nil {
		return fmt.Errorf("check for open drive: %w", err)
	}
	openChargeID, err := store.OpenChargingSessionID(vehicleDBID)
	if err != nil {
		return fmt.Errorf("check for open charging session: %w", err)
	}
	if openDriveID != 0 {
		slog.Info("resuming drive left open across restart", "drive_id", openDriveID)
	}
	if openChargeID != 0 {
		slog.Info("resuming charging session left open across restart", "session_id", openChargeID)
	}

	// Resolved below, after the geofence resolver exists: a charging
	// session resumed across a restart must recover which geofence it
	// started in, or it would be priced at the global fallback rate
	// instead of that location's own. TeslaMate keeps this on the
	// charging_processes row so it survives restarts; teslalog stores
	// the coordinates instead and re-resolves from them, which gets to
	// the same place without a schema change.
	var resumedChargeLat, resumedChargeLng float64
	var haveResumedChargeCoords bool
	if openChargeID != 0 {
		var lat, lng sql.NullFloat64
		if err := store.DB().QueryRow(
			`SELECT latitude, longitude FROM charging_sessions WHERE id = ?`, openChargeID,
		).Scan(&lat, &lng); err != nil {
			slog.Warn("could not read resumed charging session's location", "error", err)
		} else if lat.Valid && lng.Valid {
			resumedChargeLat, resumedChargeLng = lat.Float64, lng.Float64
			haveResumedChargeCoords = true
		}
	}

	geofences := make([]geocode.Geofence, 0, len(cfg.Geofences))
	for _, g := range cfg.Geofences {
		geofences = append(geofences, geocode.Geofence{
			Name: g.Name, Lat: g.Lat, Lng: g.Lng, RadiusM: g.RadiusM,
			HasPricing: g.HasPricing, BillingType: g.BillingType,
			CostPerUnit: g.CostPerUnit, SessionFee: g.SessionFee,
		})
	}
	geo := geocode.New(geofences, store, cfg.Geocoding.Enabled, cfg.Geocoding.BaseURL, cfg.Geocoding.UserAgent)

	loop := &loopState{
		cfg: cfg, client: client, store: store,
		// Tesla's vehicle_data/wake_up REST endpoints take the numeric
		// "id" field (VehicleSummary.ID) - NOT "vehicle_id", which is a
		// different identifier used only by the streaming API (see
		// streamID below and VehicleSummary's own doc comment). Using
		// vehicle_id here 404s: "vehicle_data: HTTP 404: not_found".
		apiID:       summary.ID,
		streamID:    summary.VehicleID,
		vin:         summary.VIN,
		vehicleDBID: vehicleDBID,
		machine:     newMachine(cfg, openDriveID != 0, openChargeID != 0),
		geo:         geo,
		driveID:     openDriveID,
		chargeID:    openChargeID,
	}
	if haveResumedChargeCoords {
		loop.chargeGeofence, loop.haveChargeGeofence = geo.FindGeofence(resumedChargeLat, resumedChargeLng)
	}
	return loop.run(ctx)
}

// newMachine builds the state machine with every config-driven
// threshold applied, resuming any drive/charge left open across a
// restart (see vehicle.Resume).
func newMachine(cfg config.Config, driving, charging bool) *vehicle.Machine {
	m := vehicle.Resume(cfg.Polling.IdleTimeout, driving, charging)
	if cfg.Polling.OfflineChargeMinGap > 0 {
		m.SetOfflineChargeThresholds(vehicle.OfflineChargeMinRangeGainKm, cfg.Polling.OfflineChargeMinGap)
	}
	return m
}

// pickVehicle resolves which vehicle to log, by VIN if configured,
// otherwise the account's first vehicle. This call alone (ListVehicles)
// never wakes the car.
func pickVehicle(ctx context.Context, client *tesla.Client, vin string) (tesla.VehicleSummary, error) {
	vehicles, err := client.ListVehicles(ctx)
	if err != nil {
		return tesla.VehicleSummary{}, err
	}
	if len(vehicles) == 0 {
		return tesla.VehicleSummary{}, fmt.Errorf("no vehicles on this account")
	}
	if vin == "" {
		return vehicles[0], nil
	}
	for _, v := range vehicles {
		if v.VIN == vin {
			return v, nil
		}
	}
	return tesla.VehicleSummary{}, fmt.Errorf("no vehicle with VIN %s on this account", vin)
}

type loopState struct {
	cfg    config.Config
	client *tesla.Client
	store  *storage.Store
	// apiID is VehicleSummary.ID: the identifier Tesla's REST API
	// (vehicle_data, wake_up) expects in its URL path.
	apiID int64
	// streamID is VehicleSummary.VehicleID: a separate identifier used
	// only as the streaming websocket's subscription tag - NOT
	// interchangeable with apiID (see VehicleSummary's doc comment).
	streamID int64
	// vin identifies "our" vehicle within a /api/1/products response
	// that may list multiple vehicles (or non-vehicle products) -
	// simpler and more obviously correct than matching on either numeric
	// id, given there are two of those and they mean different things.
	vin         string
	vehicleDBID int64
	machine     *vehicle.Machine
	geo         *geocode.Resolver

	driveID  int64
	chargeID int64
	// chargeGeofence is the geofence the current charging session
	// started inside, if any - captured at start because that's what
	// determines the price (TeslaMate resolves it at
	// start_charging_process the same way). haveChargeGeofence
	// distinguishes "charging somewhere unpriced" from "no geofence
	// looked up yet".
	chargeGeofence     geocode.Geofence
	haveChargeGeofence bool
	// lastBackfillAt throttles the location backfill sweep - see
	// maybeBackfillLocations.
	lastBackfillAt time.Time

	stream *tesla.StreamConn
	// lastStreamAt is the timestamp of the most recent streaming
	// sample actually recorded, used to drop stale/replayed ones after
	// a reconnect - see drainStream. Reset at the start of each drive.
	lastStreamAt time.Time
}

func (l *loopState) run(ctx context.Context) error {
	// Establish the initial state before entering the steady-state
	// loop, using the cheap summary check (never wakes the car).
	if err := l.checkSummary(ctx); err != nil {
		slog.Warn("initial vehicle summary check failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			l.closeStream()
			return ctx.Err()
		default:
		}

		var sleepFor time.Duration
		switch {
		case isAsleepLike(l.machine.State()):
			l.closeStream()
			if err := l.checkSummary(ctx); err != nil {
				slog.Warn("vehicle summary check failed", "error", err)
			}
			if isAsleepLike(l.machine.State()) {
				sleepFor = l.checkInterval(l.machine.State())
			} else {
				// checkSummary just found the car active (e.g. woke up on
				// its own) - start the fast poll/driving/charging cadence
				// on the very next iteration instead of waiting out the
				// full SuspendedCheckInterval (which could be 15+ minutes)
				// before noticing. sleepFor stays its zero value, so the
				// loop goes straight back around with no delay.
			}
		default:
			if err := l.pollVehicleData(ctx); err != nil {
				slog.Warn("vehicle_data poll failed", "error", err)
				// Note the observation gap: a failed poll is one of the
				// two ways we learn the vehicle isn't reachable (the
				// other being the summary check below), and what
				// happened during the gap has to be reconciled once it
				// comes back - see Machine.OnBackOnline.
				l.machine.MarkUnreachable()
				// vehicle_data can't reliably tell "asleep/offline" apart from
				// a network blip (it just errors/times out either way), so
				// fall back to the cheap, non-waking summary check to find
				// out which. Without this, a car that genuinely went
				// offline mid-drive/charge would otherwise keep getting
				// vehicle_data retried at the fast driving/charging cadence
				// forever instead of settling into the sleep-respecting
				// suspended branch above.
				if err := l.checkSummary(ctx); err != nil {
					slog.Warn("fallback vehicle summary check failed", "error", err)
				}
			}
			l.manageStream(ctx)
			l.drainStream()
			// Per-state cadence matching TeslaMate's own defaults: much
			// tighter while driving/charging than while just sitting
			// online-idle (see PollingConfig's doc comment for what's
			// deliberately simplified vs TeslaMate's full adaptive
			// scheduler). l.machine.State() reflects the state *after*
			// the poll just above, so a drive/charge that just ended
			// this iteration already sleeps at the slower online cadence.
			switch l.machine.State() {
			case vehicle.StateDriving:
				sleepFor = l.cfg.Polling.DrivingInterval
			case vehicle.StateCharging:
				sleepFor = l.cfg.Polling.ChargingInterval
			default: // StateOnline, StateIdle, StateUpdating
				// StateUpdating deliberately uses the normal online
				// cadence, matching TeslaMate's own :updating state
				// (schedule_fetch(default_interval())) - frequent
				// enough to notice the install finishing promptly,
				// without hammering a car that's busy installing.
				sleepFor = l.cfg.Polling.OnlineInterval
			}
		}

		l.maybeBackfillLocations(ctx)

		select {
		case <-ctx.Done():
			l.closeStream()
			return ctx.Err()
		case <-time.After(sleepFor):
		}
	}
}

// teeHandler is a slog.Handler that forwards every record to two
// handlers - used to send log output to both stderr (what
// systemd/journalctl captures, unchanged from before) and the portal's
// in-memory LogBuffer, without either one replacing the other.
type teeHandler struct {
	a, b slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.a.Enabled(ctx, level) || t.b.Enabled(ctx, level)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	if t.a.Enabled(ctx, r.Level) {
		if err := t.a.Handle(ctx, r.Clone()); err != nil {
			firstErr = err
		}
	}
	if t.b.Enabled(ctx, r.Level) {
		if err := t.b.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{t.a.WithAttrs(attrs), t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{t.a.WithGroup(name), t.b.WithGroup(name)}
}

// isAsleepLike reports whether s is one of the states that should only
// get the cheap, non-waking summary check (never vehicle_data) at
// SuspendedCheckInterval cadence.
func isAsleepLike(s vehicle.State) bool {
	switch s {
	case vehicle.StateAsleep, vehicle.StateOffline, vehicle.StateSuspended, vehicle.StateUnknown:
		return true
	default:
		return false
	}
}

// checkInterval picks how long to wait before the next cheap,
// non-waking summary check, for whichever isAsleepLike state s is.
// ASLEEP/OFFLINE/UNKNOWN all get AsleepInterval (matching TeslaMate's
// own @asleep_interval, which applies identically to both asleep and
// offline - see PollingConfig.AsleepInterval's doc comment). Only
// SUSPENDED - a deliberate choice made after IdleTimeout, not a raw
// API-reported state - gets the much longer SuspendedCheckInterval.
func (l *loopState) checkInterval(s vehicle.State) time.Duration {
	switch s {
	case vehicle.StateSuspended:
		return l.cfg.Polling.SuspendedCheckInterval
	default: // StateAsleep, StateOffline, StateUnknown
		return l.cfg.Polling.AsleepInterval
	}
}

// checkSummary performs the cheap, non-waking vehicle-list check and
// feeds the result to the state machine.
func (l *loopState) checkSummary(ctx context.Context) error {
	vehicles, err := l.client.ListVehicles(ctx)
	if err != nil {
		return err
	}
	rawState := "offline" // not found in the list at all: treat as offline
	for _, v := range vehicles {
		if v.VIN == l.vin {
			rawState = v.State
			break
		}
	}
	now := time.Now().UTC()
	events := l.machine.OnSummary(now, rawState)
	// If that just left us (or already had us) asleep-like, and a
	// drive/charge was believed in progress, abandon it - see
	// OnUnreachable's doc comment for why this is more than cosmetic.
	if isAsleepLike(l.machine.State()) {
		events = append(events, l.machine.OnUnreachable(now, rawState, l.cfg.Polling.DriveTimeout)...)
	}
	return l.persist(events)
}

// pollVehicleData performs a full vehicle_data poll. Only called while
// the machine believes the vehicle is online/driving/charging/idle.
func (l *loopState) pollVehicleData(ctx context.Context) error {
	snap, meta, err := l.client.VehicleData(ctx, l.apiID)
	if err != nil {
		// A failure here (car went to sleep between our summary check
		// and this poll, network blip, etc.) should NOT be treated as
		// "still online" — let the next loop iteration's summary check
		// sort out the real state rather than spinning on errors.
		return err
	}
	if meta.Model != "" || meta.DisplayName != "" {
		_, err := l.store.UpsertVehicle(storage.VehicleMeta{
			VIN: meta.VIN, TeslaID: fmt.Sprint(l.streamID), DisplayName: meta.DisplayName,
			Model: meta.Model, TrimBadging: meta.TrimBadging, MarketingName: meta.MarketingName,
			ExteriorColor: meta.ExteriorColor, WheelType: meta.WheelType, SpoilerType: meta.SpoilerType,
		})
		if err != nil {
			slog.Warn("update vehicle metadata failed", "error", err)
		}
	}

	// If the vehicle was unreachable at any point since our last real
	// poll, check the before/after pair for a charge that happened
	// while we couldn't see it. Must run BEFORE OnVehicleData, which
	// overwrites the "before" snapshot. The machine tracks the
	// "was unreachable" flag itself rather than this reading it off
	// State(), because by the time a poll succeeds the state has
	// usually already been flipped back to ONLINE by the summary check
	// that noticed the car was reachable again - so State() here says
	// nothing about whether there was a gap. See
	// Machine.OnBackOnline.
	if err := l.persist(l.machine.OnBackOnline(snap)); err != nil {
		return err
	}

	events := l.machine.OnVehicleData(snap)
	if err := l.persist(events); err != nil {
		return err
	}

	if l.machine.State() == vehicle.StateIdle && l.machine.IdleTimedOut(time.Now().UTC()) {
		slog.Info("vehicle idle timeout reached, suspending active polling", "idle_since", l.machine.IdleSince())
		if err := l.persist(l.machine.Suspend(time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}

// persist maps state-machine events onto storage calls.
func (l *loopState) persist(events []vehicle.Event) error {
	for _, ev := range events {
		switch ev.Kind {
		case vehicle.EvStateChanged:
			if _, err := l.store.OpenState(l.vehicleDBID, string(ev.ToState), ev.At); err != nil {
				return fmt.Errorf("record state change: %w", err)
			}
			slog.Info("state changed", "from", ev.FromState, "to", ev.ToState)

		case vehicle.EvDriveStart:
			s := ev.Snapshot
			id, err := l.store.OpenDrive(storage.DriveStart{
				VehicleID: l.vehicleDBID, Time: ev.At, OdometerKm: s.OdometerKm,
				BatteryLevel: s.BatteryLevel, RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm,
				Lat: s.Lat, Lng: s.Lng, StartLocation: l.geo.Resolve(context.Background(), s.Lat, s.Lng),
			})
			if err != nil {
				return fmt.Errorf("open drive: %w", err)
			}
			l.driveID = id
			l.lastStreamAt = time.Time{} // fresh drive, no prior stream samples to compare against
			// TeslaMate's own start_drive records a position for this
			// exact same first sample, in the same transaction as
			// starting the drive (verified directly against its
			// source, lib/teslamate/vehicles/vehicle.ex) - every drive
			// teslalog logged before this fix was missing its first
			// GPS point compared to what TeslaMate would have recorded
			// for the identical drive.
			if err := l.store.AppendPosition(positionFromSnapshot(id, l.vehicleDBID, ev.At, s)); err != nil {
				return fmt.Errorf("append start position: %w", err)
			}
			slog.Info("drive started", "drive_id", id, "odometer_km", s.OdometerKm)

		case vehicle.EvDrivePoint:
			if l.driveID == 0 {
				continue
			}
			if err := l.store.AppendPosition(positionFromSnapshot(l.driveID, l.vehicleDBID, ev.At, ev.Snapshot)); err != nil {
				return fmt.Errorf("append position: %w", err)
			}

		case vehicle.EvDriveEnd:
			if l.driveID == 0 {
				continue
			}
			s := ev.Snapshot
			// TeslaMate's own drive-end handler records a position for
			// this final "now parked" sample too, immediately before
			// calling close_drive (same source, same transaction) -
			// same gap as EvDriveStart's, fixed the same way.
			if err := l.store.AppendPosition(positionFromSnapshot(l.driveID, l.vehicleDBID, ev.At, s)); err != nil {
				return fmt.Errorf("append end position: %w", err)
			}
			if err := l.store.CloseDrive(storage.DriveEnd{
				DriveID: l.driveID, Time: ev.At, OdometerKm: s.OdometerKm,
				BatteryLevel: s.BatteryLevel, RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm,
				Lat: s.Lat, Lng: s.Lng, EndLocation: l.geo.Resolve(context.Background(), s.Lat, s.Lng),
			}); err != nil {
				return fmt.Errorf("close drive: %w", err)
			}
			slog.Info("drive ended", "drive_id", l.driveID)
			l.driveID = 0

		case vehicle.EvChargeStart:
			s := ev.Snapshot
			id, err := l.store.OpenChargingSession(storage.ChargeStart{
				VehicleID: l.vehicleDBID, Time: ev.At, BatteryLevel: s.BatteryLevel,
				RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm, Lat: s.Lat, Lng: s.Lng,
				Location: l.geo.Resolve(context.Background(), s.Lat, s.Lng),
			})
			if err != nil {
				return fmt.Errorf("open charging session: %w", err)
			}
			l.chargeID = id
			// Capture where this charge is happening now, at the
			// start - that's what determines its price, and it's the
			// same point TeslaMate resolves the geofence at.
			l.chargeGeofence, l.haveChargeGeofence = l.geo.FindGeofence(s.Lat, s.Lng)
			// TeslaMate's own start_charging_process records a sample
			// for this exact same first observation, in the same
			// transaction as starting the session (verified directly
			// against its source) - same gap as drives had, fixed the
			// same way.
			if err := l.store.AppendChargingSample(chargingSampleFromSnapshot(id, l.vehicleDBID, ev.At, s)); err != nil {
				return fmt.Errorf("append start charging sample: %w", err)
			}
			slog.Info("charging started", "session_id", id, "battery_level", s.BatteryLevel)

		case vehicle.EvChargePoint:
			if l.chargeID == 0 {
				continue
			}
			if err := l.store.AppendChargingSample(chargingSampleFromSnapshot(l.chargeID, l.vehicleDBID, ev.At, ev.Snapshot)); err != nil {
				return fmt.Errorf("append charging sample: %w", err)
			}

		case vehicle.EvChargeEnd:
			if l.chargeID == 0 {
				continue
			}
			s := ev.Snapshot
			// TeslaMate's own charge-end handler records a sample for
			// this final observation too, immediately before calling
			// complete_charging_process (same source) - same gap,
			// fixed the same way.
			if err := l.store.AppendChargingSample(chargingSampleFromSnapshot(l.chargeID, l.vehicleDBID, ev.At, s)); err != nil {
				return fmt.Errorf("append end charging sample: %w", err)
			}
			if err := l.store.CloseChargingSession(storage.ChargeEnd{
				ChargingSessionID: l.chargeID, Time: ev.At, BatteryLevel: s.BatteryLevel,
				RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm, EnergyAddedKwh: s.ChargeEnergyAddedKwh,
				ChargingEfficiency: l.cfg.Charging.Efficiency, PricePerKwh: l.cfg.Charging.PricePerKwh,
				CostFunc: l.chargeCostFunc(),
			}); err != nil {
				return fmt.Errorf("close charging session: %w", err)
			}
			slog.Info("charging ended", "session_id", l.chargeID, "energy_added_kwh", s.ChargeEnergyAddedKwh)
			l.chargeID = 0
			l.refreshDerivedEfficiency()

		case vehicle.EvDriveAbandoned:
			if l.driveID == 0 {
				continue
			}
			if err := l.store.CloseDriveFromLastPosition(l.driveID, ev.At); err != nil {
				return fmt.Errorf("close abandoned drive: %w", err)
			}
			slog.Warn("drive abandoned: vehicle stopped reporting mid-drive, closed using last known position", "drive_id", l.driveID)
			l.driveID = 0

		case vehicle.EvChargeAbandoned:
			if l.chargeID == 0 {
				continue
			}
			if err := l.store.CloseChargingSessionFromLastSample(l.chargeID, ev.At, l.cfg.Charging.Efficiency, l.cfg.Charging.PricePerKwh); err != nil {
				return fmt.Errorf("close abandoned charging session: %w", err)
			}
			slog.Warn("charging session abandoned: vehicle stopped reporting mid-charge, closed using last known sample", "session_id", l.chargeID)
			l.chargeID = 0

		case vehicle.EvOfflineCharge:
			// The vehicle gained real range while unobservable - it was
			// charging somewhere we couldn't see. Synthesize a
			// complete, already-closed session spanning exactly the
			// last-known-before and first-known-after readings, with a
			// sample at each end, matching TeslaMate's own handling
			// (start_charging_process + insert_charge for both
			// readings + complete_charging_process, all in one
			// transaction - verified against its source).
			before, after := ev.FromSnapshot, ev.Snapshot
			id, err := l.store.OpenChargingSession(storage.ChargeStart{
				VehicleID: l.vehicleDBID, Time: before.Time, BatteryLevel: before.BatteryLevel,
				RangeKm: before.RangeKm, IdealRangeKm: before.IdealRangeKm,
				Lat: before.Lat, Lng: before.Lng,
				Location: l.geo.Resolve(context.Background(), before.Lat, before.Lng),
			})
			if err != nil {
				return fmt.Errorf("open offline charging session: %w", err)
			}
			for _, s := range []vehicle.Snapshot{before, after} {
				if err := l.store.AppendChargingSample(chargingSampleFromSnapshot(id, l.vehicleDBID, s.Time, s)); err != nil {
					return fmt.Errorf("append offline charging sample: %w", err)
				}
			}
			// The API reports charge_energy_added only for a charge it
			// actually observed, so it's unavailable here by
			// definition. Estimate it from the range actually gained
			// and the configured efficiency, the same modeling
			// teslalog already does for charge_energy_used_kwh (see
			// ChargeEnd.ChargingEfficiency) - if no efficiency is
			// configured, the energy stays unknown rather than being
			// guessed, and the session still correctly records the
			// battery/range change.
			// Use the best efficiency available - the figure derived
			// from this car's own charging history if there is one,
			// falling back to the config seed. Using the config value
			// directly would ignore the derived figure entirely, which
			// is both less accurate and inconsistent with everything
			// else that reads efficiency.
			energyAdded := 0.0
			whPerKm, haveEff, effErr := l.store.VehicleEfficiency(l.vehicleDBID)
			if effErr != nil {
				slog.Warn("read vehicle efficiency failed", "error", effErr)
			}
			if !haveEff {
				whPerKm = l.cfg.Vehicle.EfficiencyWhKm
			}
			if whPerKm > 0 {
				energyAdded = (after.IdealRangeKm - before.IdealRangeKm) * whPerKm / 1000
			}
			// Price it by wherever it was parked when it went dark -
			// the same "where did this charge happen" question, just
			// answered from the last reading we have instead of a live
			// one. No charger brand is known for an unobserved charge,
			// so free-supercharging can't apply.
			// No charger type is known for a charge nobody observed, so
			// the free-supercharging rule can't apply here - only the
			// location's own pricing.
			var offlineCost func(float64, float64, float64, string) (float64, bool)
			if g, ok := l.geo.FindGeofence(before.Lat, before.Lng); ok && g.HasPricing {
				offlineCost = func(added, used, durationMin float64, _ string) (float64, bool) {
					return g.Cost(added, used, durationMin)
				}
			}
			if err := l.store.CloseChargingSession(storage.ChargeEnd{
				ChargingSessionID: id, Time: after.Time, BatteryLevel: after.BatteryLevel,
				RangeKm: after.RangeKm, IdealRangeKm: after.IdealRangeKm, EnergyAddedKwh: energyAdded,
				ChargingEfficiency: l.cfg.Charging.Efficiency, PricePerKwh: l.cfg.Charging.PricePerKwh,
				CostFunc: offlineCost,
			}); err != nil {
				return fmt.Errorf("close offline charging session: %w", err)
			}
			slog.Info("charged while unobservable: recorded a synthesized charging session",
				"session_id", id, "battery", fmt.Sprintf("%d%%->%d%%", before.BatteryLevel, after.BatteryLevel),
				"ideal_range_gained_km", after.IdealRangeKm-before.IdealRangeKm,
				"offline_minutes", int(after.Time.Sub(before.Time).Minutes()))
			l.refreshDerivedEfficiency()

		case vehicle.EvBatterySample:
			s := ev.Snapshot
			if err := l.store.InsertBatterySample(l.vehicleDBID, ev.At, s.BatteryLevel, s.RangeKm, s.IdealRangeKm, "poll"); err != nil {
				return fmt.Errorf("insert battery sample: %w", err)
			}
			if err := l.store.UpdateVehicleFirmware(l.vehicleDBID, s.Firmware); err != nil {
				return fmt.Errorf("update vehicle firmware: %w", err)
			}

		case vehicle.EvSoftwareUpdateBeg:
			if err := l.store.UpsertSoftwareUpdateStart(l.vehicleDBID, ev.Snapshot.UpdateVersion, ev.At); err != nil {
				return fmt.Errorf("record software update start: %w", err)
			}
			slog.Info("software update started", "version", ev.Snapshot.UpdateVersion)

		case vehicle.EvSoftwareUpdateEnd:
			if err := l.store.CompleteSoftwareUpdate(l.vehicleDBID, ev.Snapshot.UpdateVersion, ev.At); err != nil {
				return fmt.Errorf("record software update end: %w", err)
			}
			// The car reports its new car_version fairly quickly after an
			// update finishes, but not necessarily in this exact snapshot -
			// fall back to the version we just watched it install to, so
			// the vehicles row doesn't lag behind by a full poll interval.
			firmware := ev.Snapshot.Firmware
			if firmware == "" {
				firmware = ev.Snapshot.UpdateVersion
			}
			if err := l.store.UpdateVehicleFirmware(l.vehicleDBID, firmware); err != nil {
				return fmt.Errorf("update vehicle firmware: %w", err)
			}
			slog.Info("software update finished", "version", ev.Snapshot.UpdateVersion)
		}
	}
	return nil
}

// manageStream opens the streaming websocket while driving (for
// higher-frequency GPS samples than the REST poll interval provides)
// and closes it otherwise. Streaming never drives state decisions —
// see stream.go's package comment.
func (l *loopState) manageStream(ctx context.Context) {
	if !l.cfg.Streaming.Enabled {
		return
	}
	driving := l.machine.State() == vehicle.StateDriving
	if driving && l.stream == nil && l.driveID != 0 {
		token, err := l.client.CurrentAccessToken(ctx)
		if err != nil {
			slog.Warn("cannot start stream: no access token", "error", err)
			return
		}
		conn, err := tesla.Connect(ctx, l.cfg.Streaming.URL, token, l.streamID)
		if err != nil {
			slog.Warn("streaming connect failed, continuing on REST polling only", "error", err)
			return
		}
		l.stream = conn
		slog.Info("streaming connected")
	} else if !driving && l.stream != nil {
		l.closeStream()
	}
}

func (l *loopState) drainStream() {
	if l.stream == nil || l.driveID == 0 {
		return
	}
	for {
		select {
		case s, ok := <-l.stream.Samples():
			if !ok {
				// The stream's read loop has exited (server closed the
				// connection, a read error, etc.); log why before dropping
				// it, rather than silently discarding the one error
				// StreamConn ever reports. manageStream reconnects on the
				// next loop iteration.
				select {
				case err := <-l.stream.Errors():
					slog.Warn("streaming connection closed", "error", err)
				default:
					slog.Info("streaming connection closed")
				}
				l.stream = nil
				return
			}
			// Discard samples older than what we've already recorded.
			// The stream can replay buffered/stale data after a
			// reconnect - and teslalog reconnects often during a long
			// drive - which would otherwise write out-of-order points,
			// corrupting the route's shape and any calculation that
			// depends on sample ordering (elevation gain/loss, energy
			// integration). TeslaMate guards the same way (its stale?
			// check, verified directly against its source).
			if !l.lastStreamAt.IsZero() && !s.Time.After(l.lastStreamAt) {
				continue
			}
			l.lastStreamAt = s.Time

			// The legacy streaming protocol only carries GPS/speed/power/
			// battery/range/shift_state/elevation - richer telemetry
			// (climate, TPMS, usable battery %, ideal/est range) only comes
			// from REST vehicle_data polls, so those fields are left nil
			// (stored as SQL NULL, not a misleading 0) here. ElevationM,
			// unlike those, genuinely IS known from the stream, so it's the
			// one field set here that positionFromSnapshot never sets.
			if err := l.store.AppendPosition(storage.PositionSample{
				DriveID: l.driveID, VehicleID: l.vehicleDBID, Time: s.Time,
				Lat: s.Lat, Lng: s.Lng, SpeedKmh: s.SpeedKmh, Heading: s.Heading,
				ElevationM: ptr(s.ElevationM), PowerKw: s.PowerKw, OdometerKm: s.OdometerKm,
				BatteryLevel: s.BatteryLevel, RangeKm: s.RangeKm, ShiftState: s.ShiftState,
			}); err != nil {
				slog.Warn("append streaming position failed", "error", err)
			}
		default:
			return
		}
	}
}

// ptr returns a pointer to v, for the storage.PositionSample fields that
// distinguish "genuinely unknown" (nil, stored as SQL NULL) from a real
// zero reading.
func ptr(v float64) *float64 { return &v }

// pressurePtr is ptr for a tyre pressure, where the Owner API reports a
// literal 0.0 for a car that does not publish TPMS rather than omitting
// the field. Zero bar is not a pressure any tyre has, so storing it
// would be a made-up reading: NULL is the honest answer, and it matches
// what TeslaMate records for the same vehicle (verified against a
// 400,000-row dump from the same car - not one real TPMS value in it).
func pressurePtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}
func intPtr(v int) *int      { return &v }
func boolPtr(v bool) *bool   { return &v }

// positionFromSnapshot maps a full vehicle_data-derived Snapshot onto a
// storage.PositionSample, carrying every field TeslaMate's positions
// table tracks (see internal/storage/schema.go). ElevationM is left nil:
// the Owner API's vehicle_data response doesn't report elevation at all
// (only the streaming client does, see drainStream below).
// positionFromSnapshot builds a position row from a full REST
// vehicle_data poll - unlike drainStream's streaming-derived samples
// (see its comment), every field here is genuinely known, so every
// nullable PositionSample field gets a real pointer, never nil.
func positionFromSnapshot(driveID, vehicleDBID int64, at time.Time, s vehicle.Snapshot) storage.PositionSample {
	return storage.PositionSample{
		DriveID: driveID, VehicleID: vehicleDBID, Time: at,
		Lat: s.Lat, Lng: s.Lng, SpeedKmh: s.SpeedKmh, Heading: s.Heading,
		PowerKw: s.PowerKw, OdometerKm: s.OdometerKm,

		BatteryLevel: s.BatteryLevel, UsableBatteryLevel: intPtr(s.UsableBatteryLevel),
		RangeKm: s.RangeKm, IdealRangeKm: ptr(s.IdealRangeKm), EstRangeKm: ptr(s.EstRangeKm),
		BatteryHeaterOn: boolPtr(s.BatteryHeaterOn),
		BatteryHeater:   boolPtr(s.BatteryHeater), BatteryHeaterNoPower: boolPtr(s.BatteryHeaterNoPower),

		OutsideTempC: ptr(s.OutsideTempC), InsideTempC: ptr(s.InsideTempC), FanStatus: intPtr(s.FanStatus),
		DriverTempSettingC: ptr(s.DriverTempSettingC), PassengerTempSettingC: ptr(s.PassengerTempSettingC),
		IsClimateOn: boolPtr(s.IsClimateOn), IsRearDefrosterOn: boolPtr(s.IsRearDefrosterOn), IsFrontDefrosterOn: boolPtr(s.IsFrontDefrosterOn),

		TpmsPressureFL: pressurePtr(s.TpmsPressureFL), TpmsPressureFR: pressurePtr(s.TpmsPressureFR),
		TpmsPressureRL: pressurePtr(s.TpmsPressureRL), TpmsPressureRR: pressurePtr(s.TpmsPressureRR),

		ShiftState: s.ShiftState,

		SentryMode: boolPtr(s.SentryMode), IsUserPresent: boolPtr(s.IsUserPresent), ValetMode: boolPtr(s.ValetMode),
		ClimateKeeperMode: s.ClimateKeeperMode,
	}
}

// chargeCostFunc returns the pricing rule for the charging session
// that's currently ending, or nil to fall back to the flat global rate
// (storage.ChargeEnd.PricePerKwh).
//
// Order matches TeslaMate's put_cost exactly (verified against
// lib/teslamate/log.ex): free supercharging short-circuits everything,
// then the geofence's own pricing applies, then nothing.
//
// The free-supercharging test is on fast_charger_TYPE, not brand -
// this distinction is load-bearing, not pedantic. In real recorded
// data the brand reads "Tesla" for ~98,000 samples including every
// home charge (a Tesla wall connector is, after all, a Tesla
// product), while the type reads "Tesla" only for actual
// Superchargers. Testing the brand would mark every home charge free
// and zero out the entire electricity bill.
func (l *loopState) chargeCostFunc() func(float64, float64, float64, string) (float64, bool) {
	freeSupercharging := l.cfg.Charging.FreeSupercharging
	var priced *geocode.Geofence
	if l.haveChargeGeofence && l.chargeGeofence.HasPricing {
		g := l.chargeGeofence
		priced = &g
	}
	if !freeSupercharging && priced == nil {
		return nil // nothing to say; let the flat fallback rate apply
	}

	return func(added, used, durationMin float64, fastChargerType string) (float64, bool) {
		if freeSupercharging && strings.HasPrefix(fastChargerType, "Tesla") {
			return 0, true
		}
		if priced != nil {
			return priced.Cost(added, used, durationMin)
		}
		return 0, false
	}
}

// backfillLocations retries place-name resolution for drive/charge
// endpoints that have coordinates but no name. These fail for
// temporary reasons - geocoder unreachable or rate-limited, or a
// geofence added to the config after the fact - so retrying later
// genuinely fixes them, whereas leaving them alone means a blank
// "from"/"to" forever. Mirrors TeslaMate's own hourly Repair sweep
// (verified directly against lib/teslamate/repair.ex).
//
// Bounded per sweep so a large backlog can't stall the poll loop or
// hammer the geocoding service; the next sweep picks up where this
// one left off.
// backfillMinInterval is the minimum gap between backfill sweeps. The
// geocoder self-throttles to 1 request/second (see
// geocode.reverseGeocode), so a full sweep is already bounded in rate;
// this just stops the loop from re-running one every poll interval.
const backfillMinInterval = 5 * time.Minute

// maybeBackfillLocations runs a bounded backfill sweep unless a drive
// or charge is actively being logged, or one ran too recently.
//
// This used to be gated on the vehicle being asleep/offline, on the
// reasoning that that's when nothing else needs the time or the
// geocoder's rate limit. Found live that this starves it in practice:
// a parked car keeps reporting "online" to the cheap check for hours,
// so the state machine cycles idle -> suspended -> online -> idle
// without ever settling in an asleep-like state at the moment the
// check runs, and the sweep never fires. Gating on "not actively
// logging" instead preserves the original intent while letting it
// actually run during the long idle stretches it was meant for.
func (l *loopState) maybeBackfillLocations(ctx context.Context) {
	if !l.backfillDue() {
		return
	}
	l.lastBackfillAt = time.Now()
	l.backfillLocations(ctx)
}

// backfillDue reports whether a sweep should run now - split out from
// maybeBackfillLocations so the scheduling rule is testable without
// standing up a store and a geocoder.
func (l *loopState) backfillDue() bool {
	switch l.machine.State() {
	case vehicle.StateDriving, vehicle.StateCharging:
		return false
	}
	return l.lastBackfillAt.IsZero() || time.Since(l.lastBackfillAt) >= backfillMinInterval
}

func (l *loopState) backfillLocations(ctx context.Context) {
	const perSweep = 25

	pending, err := l.store.ListUnresolvedLocations(l.vehicleDBID, perSweep)
	if err != nil {
		slog.Warn("list unresolved locations failed", "error", err)
		return
	}

	var fixed int
	for _, u := range pending {
		name := l.geo.Resolve(ctx, u.Lat, u.Lng)
		if name == "" {
			continue // still unresolvable (geocoding off/down) - try again next sweep
		}
		if err := l.store.SetResolvedLocation(u, name); err != nil {
			slog.Warn("backfill location failed", "error", err)
			continue
		}
		fixed++
	}
	if fixed > 0 {
		slog.Info("backfilled previously-unresolved locations", "count", fixed)
	}

	l.backfillPlaceAddresses(ctx)
}

// backfillPlaceAddresses fills in the address components (city, state,
// country...) of cache rows that predate teslalog storing them. Those
// rows already have a correct display name, so nothing re-resolves them
// on its own and the Locations dashboard's city/state/country panels
// would read empty forever on any database logged before this.
//
// Deliberately smaller per sweep than the location backfill: this is a
// cosmetic catch-up on a handful of rows, not missing data, and it
// shares the geocoder's 1 req/sec budget with the sweep above.
func (l *loopState) backfillPlaceAddresses(ctx context.Context) {
	const perSweep = 5

	pending, err := l.store.ListPlacesMissingAddress(perSweep)
	if err != nil {
		slog.Warn("list places missing address failed", "error", err)
		return
	}

	var fixed int
	for _, c := range pending {
		if _, ok := l.geo.RefreshPlace(ctx, c.LatKey, c.LngKey); ok {
			fixed++
		}
	}
	if fixed > 0 {
		slog.Info("backfilled address components for cached places", "count", fixed)
	}
}

// refreshDerivedEfficiency re-derives the vehicle's real Wh/km from
// its own charging history and stores it, called after every charging
// session closes - the same point TeslaMate does it (inside
// complete_charging_process). Non-fatal: a failure here only means
// range projections keep using the previous figure.
func (l *loopState) refreshDerivedEfficiency() {
	whPerKm, ok, err := l.store.RecalculateEfficiency(l.vehicleDBID)
	if err != nil {
		slog.Warn("recalculate efficiency failed", "error", err)
		return
	}
	if ok {
		slog.Info("derived efficiency from charging history", "wh_per_km", fmt.Sprintf("%.1f", whPerKm))
	}
}

// chargingSampleFromSnapshot builds a charging_samples row from a full
// REST vehicle_data poll. Used identically for the first, every
// intermediate, and the final sample of a charging session - matching
// TeslaMate's own behavior (verified directly against its source: it
// calls insert_charge on every one of those, not just the ones in
// between).
func chargingSampleFromSnapshot(chargingSessionID, vehicleDBID int64, at time.Time, s vehicle.Snapshot) storage.ChargingSample {
	return storage.ChargingSample{
		ChargingSessionID: chargingSessionID, VehicleID: vehicleDBID, Time: at,
		BatteryLevel: s.BatteryLevel, UsableBatteryLevel: s.UsableBatteryLevel,
		ChargerPowerKw: s.ChargerPowerKw, ChargerVoltage: s.ChargerVoltage,
		ChargerCurrent: s.ChargerActualCurrent, ChargerPilotCurrent: s.ChargerPilotCurrent,
		ChargerPhases: s.ChargerPhases, ConnChargeCable: s.ConnChargeCable,
		FastChargerPresent: s.FastChargerPresent, FastChargerBrand: s.FastChargerBrand,
		FastChargerType: s.FastChargerType,
		EnergyAddedKwh:  s.ChargeEnergyAddedKwh, RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm,
		BatteryHeaterOn: s.BatteryHeaterOn, NotEnoughPowerToHeat: s.NotEnoughPowerToHeat,
		OutsideTempC: s.OutsideTempC, ChargeLimitSoc: s.ChargeLimitSoc,
	}
}

func (l *loopState) closeStream() {
	if l.stream == nil {
		return
	}
	_ = l.stream.Close()
	l.stream = nil
}
