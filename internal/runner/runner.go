// Package runner wires together storage, the Tesla Owner API client,
// the streaming client, and the vehicle state machine into the daemon
// loop that `teslalog run` executes. It is the only place that decides
// *when* to call vehicle_data vs. the cheap vehicle-list check — the
// rule enforced here, mechanically, is:
//
//	ASLEEP / SUSPENDED -> only ListVehicles (never wakes the car),
//	                      checked every SuspendedCheckInterval.
//	otherwise          -> vehicle_data, checked every ActiveInterval.
//
// wake_up is never called from this loop.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"teslalog/internal/backup"
	"teslalog/internal/config"
	"teslalog/internal/storage"
	"teslalog/internal/tesla"
	"teslalog/internal/vehicle"
)

// Run starts the teslalog daemon loop and blocks until ctx is canceled.
func Run(ctx context.Context, cfg config.Config) error {
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
		if err := store.SetVehicleEfficiency(vehicleDBID, cfg.Vehicle.EfficiencyWhKm); err != nil {
			slog.Warn("set vehicle efficiency failed", "error", err)
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

	loop := &loopState{
		cfg:         cfg,
		client:      client,
		store:       store,
		vehicleID:   summary.VehicleID,
		vehicleDBID: vehicleDBID,
		machine:     vehicle.New(cfg.Polling.IdleTimeout),
	}
	return loop.run(ctx)
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
	cfg         config.Config
	client      *tesla.Client
	store       *storage.Store
	vehicleID   int64
	vehicleDBID int64
	machine     *vehicle.Machine

	driveID  int64
	chargeID int64

	stream *tesla.StreamConn
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
		switch l.machine.State() {
		case vehicle.StateAsleep, vehicle.StateOffline, vehicle.StateSuspended, vehicle.StateUnknown:
			l.closeStream()
			if err := l.checkSummary(ctx); err != nil {
				slog.Warn("vehicle summary check failed", "error", err)
			}
			sleepFor = l.cfg.Polling.SuspendedCheckInterval
		default:
			if err := l.pollVehicleData(ctx); err != nil {
				slog.Warn("vehicle_data poll failed", "error", err)
			}
			l.manageStream(ctx)
			l.drainStream()
			sleepFor = l.cfg.Polling.ActiveInterval
		}

		select {
		case <-ctx.Done():
			l.closeStream()
			return ctx.Err()
		case <-time.After(sleepFor):
		}
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
		if v.VehicleID == l.vehicleID {
			rawState = v.State
			break
		}
	}
	events := l.machine.OnSummary(time.Now().UTC(), rawState)
	return l.persist(events)
}

// pollVehicleData performs a full vehicle_data poll. Only called while
// the machine believes the vehicle is online/driving/charging/idle.
func (l *loopState) pollVehicleData(ctx context.Context) error {
	snap, meta, err := l.client.VehicleData(ctx, l.vehicleID)
	if err != nil {
		// A failure here (car went to sleep between our summary check
		// and this poll, network blip, etc.) should NOT be treated as
		// "still online" — let the next loop iteration's summary check
		// sort out the real state rather than spinning on errors.
		return err
	}
	if meta.Model != "" || meta.DisplayName != "" {
		_, err := l.store.UpsertVehicle(storage.VehicleMeta{
			VIN: meta.VIN, TeslaID: fmt.Sprint(l.vehicleID), DisplayName: meta.DisplayName,
			Model: meta.Model, TrimBadging: meta.TrimBadging,
			ExteriorColor: meta.ExteriorColor, WheelType: meta.WheelType, SpoilerType: meta.SpoilerType,
		})
		if err != nil {
			slog.Warn("update vehicle metadata failed", "error", err)
		}
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
				Lat: s.Lat, Lng: s.Lng,
			})
			if err != nil {
				return fmt.Errorf("open drive: %w", err)
			}
			l.driveID = id
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
			if err := l.store.CloseDrive(storage.DriveEnd{
				DriveID: l.driveID, Time: ev.At, OdometerKm: s.OdometerKm,
				BatteryLevel: s.BatteryLevel, RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm,
				Lat: s.Lat, Lng: s.Lng,
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
			})
			if err != nil {
				return fmt.Errorf("open charging session: %w", err)
			}
			l.chargeID = id
			slog.Info("charging started", "session_id", id, "battery_level", s.BatteryLevel)

		case vehicle.EvChargePoint:
			if l.chargeID == 0 {
				continue
			}
			s := ev.Snapshot
			if err := l.store.AppendChargingSample(storage.ChargingSample{
				ChargingSessionID: l.chargeID, VehicleID: l.vehicleDBID, Time: ev.At,
				BatteryLevel: s.BatteryLevel, UsableBatteryLevel: s.UsableBatteryLevel,
				ChargerPowerKw: s.ChargerPowerKw, ChargerVoltage: s.ChargerVoltage,
				ChargerCurrent: s.ChargerActualCurrent, ChargerPilotCurrent: s.ChargerPilotCurrent,
				ChargerPhases: s.ChargerPhases, ConnChargeCable: s.ConnChargeCable,
				FastChargerPresent: s.FastChargerPresent, FastChargerBrand: s.FastChargerBrand,
				FastChargerType: s.FastChargerType,
				EnergyAddedKwh:  s.ChargeEnergyAddedKwh, RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm,
				BatteryHeaterOn: s.BatteryHeaterOn, NotEnoughPowerToHeat: s.NotEnoughPowerToHeat,
				OutsideTempC: s.OutsideTempC,
			}); err != nil {
				return fmt.Errorf("append charging sample: %w", err)
			}

		case vehicle.EvChargeEnd:
			if l.chargeID == 0 {
				continue
			}
			s := ev.Snapshot
			if err := l.store.CloseChargingSession(storage.ChargeEnd{
				ChargingSessionID: l.chargeID, Time: ev.At, BatteryLevel: s.BatteryLevel,
				RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm, EnergyAddedKwh: s.ChargeEnergyAddedKwh,
				ChargingEfficiency: l.cfg.Charging.Efficiency, PricePerKwh: l.cfg.Charging.PricePerKwh,
			}); err != nil {
				return fmt.Errorf("close charging session: %w", err)
			}
			slog.Info("charging ended", "session_id", l.chargeID, "energy_added_kwh", s.ChargeEnergyAddedKwh)
			l.chargeID = 0

		case vehicle.EvBatterySample:
			s := ev.Snapshot
			if err := l.store.InsertBatterySample(l.vehicleDBID, ev.At, s.BatteryLevel, s.RangeKm, s.IdealRangeKm, "poll"); err != nil {
				return fmt.Errorf("insert battery sample: %w", err)
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
		conn, err := tesla.Connect(ctx, l.cfg.Streaming.URL, token, l.vehicleID)
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
				l.stream = nil
				return
			}
			// The legacy streaming protocol only carries GPS/speed/power/
			// battery/range/shift_state - richer telemetry (climate, TPMS,
			// usable battery %, ideal/est range) only comes from REST
			// vehicle_data polls, so those fields are left zero here.
			if err := l.store.AppendPosition(storage.PositionSample{
				DriveID: l.driveID, VehicleID: l.vehicleDBID, Time: s.Time,
				Lat: s.Lat, Lng: s.Lng, SpeedKmh: s.SpeedKmh, Heading: s.Heading,
				ElevationM: s.ElevationM, PowerKw: s.PowerKw, OdometerKm: s.OdometerKm,
				BatteryLevel: s.BatteryLevel, RangeKm: s.RangeKm, ShiftState: s.ShiftState,
			}); err != nil {
				slog.Warn("append streaming position failed", "error", err)
			}
		default:
			return
		}
	}
}

// positionFromSnapshot maps a full vehicle_data-derived Snapshot onto a
// storage.PositionSample, carrying every field TeslaMate's positions
// table tracks (see internal/storage/schema.go).
func positionFromSnapshot(driveID, vehicleDBID int64, at time.Time, s vehicle.Snapshot) storage.PositionSample {
	return storage.PositionSample{
		DriveID: driveID, VehicleID: vehicleDBID, Time: at,
		Lat: s.Lat, Lng: s.Lng, SpeedKmh: s.SpeedKmh, Heading: s.Heading,
		ElevationM: s.ElevationM, PowerKw: s.PowerKw, OdometerKm: s.OdometerKm,

		BatteryLevel: s.BatteryLevel, UsableBatteryLevel: s.UsableBatteryLevel,
		RangeKm: s.RangeKm, IdealRangeKm: s.IdealRangeKm, EstRangeKm: s.EstRangeKm,
		BatteryHeaterOn: s.BatteryHeaterOn,

		OutsideTempC: s.OutsideTempC, InsideTempC: s.InsideTempC, FanStatus: s.FanStatus,
		DriverTempSettingC: s.DriverTempSettingC, PassengerTempSettingC: s.PassengerTempSettingC,
		IsClimateOn: s.IsClimateOn, IsRearDefrosterOn: s.IsRearDefrosterOn, IsFrontDefrosterOn: s.IsFrontDefrosterOn,

		TpmsPressureFL: s.TpmsPressureFL, TpmsPressureFR: s.TpmsPressureFR,
		TpmsPressureRL: s.TpmsPressureRL, TpmsPressureRR: s.TpmsPressureRR,

		ShiftState: s.ShiftState,
	}
}

func (l *loopState) closeStream() {
	if l.stream == nil {
		return
	}
	_ = l.stream.Close()
	l.stream = nil
}
