package tesla

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"teslalog/internal/config"
	"teslalog/internal/vehicle"
)

// TokenStore is the minimal persistence interface the Client needs so
// it can save refreshed tokens. *storeFile below is the disk-backed
// implementation used by the CLI; tests can supply an in-memory one.
type TokenStore interface {
	Load() (TokenSet, error)
	Save(TokenSet) error
}

// FileTokenStore persists tokens to a JSON file on disk.
type FileTokenStore struct{ Path string }

func (f FileTokenStore) Load() (TokenSet, error) { return LoadTokenFile(f.Path) }
func (f FileTokenStore) Save(ts TokenSet) error  { return SaveTokenFile(f.Path, ts) }

// Client is an authenticated Owner API client with automatic token
// refresh. It is safe for concurrent use.
type Client struct {
	http  *http.Client
	api   config.APIConfig
	store TokenStore

	mu     sync.Mutex
	tokens TokenSet
}

// NewClient constructs a Client, loading the initial token set from
// store.
func NewClient(api config.APIConfig, store TokenStore) (*Client, error) {
	ts, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load tokens (run `teslalog auth` first): %w", err)
	}
	return &Client{
		http:   &http.Client{Timeout: 30 * time.Second},
		api:    api,
		store:  store,
		tokens: ts,
	}, nil
}

// ensureFreshToken refreshes the access token if it's expired or about
// to expire, persisting the new tokens via the store.
func (c *Client) ensureFreshToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.tokens.Expired(2 * time.Minute) {
		return nil
	}
	if c.tokens.RefreshToken == "" {
		return fmt.Errorf("access token expired and no refresh token available; run `teslalog auth`")
	}

	ts, err := RefreshAccessToken(c.http, c.api, c.tokens.RefreshToken)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	c.tokens = ts
	if err := c.store.Save(ts); err != nil {
		return fmt.Errorf("persist refreshed token: %w", err)
	}
	return nil
}

// CurrentAccessToken returns a valid (refreshed if necessary) access
// token, for use with the streaming client which authenticates
// out-of-band from this Client's own request path.
func (c *Client) CurrentAccessToken(ctx context.Context) (string, error) {
	if err := c.ensureFreshToken(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens.AccessToken, nil
}

// do performs an authenticated request against the Owner API with a
// small retry/backoff loop for 429/5xx, and a single retry after a
// forced token refresh on 401.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, int, error) {
	if err := c.ensureFreshToken(ctx); err != nil {
		return nil, 0, err
	}

	url := strings.TrimSuffix(c.api.OwnerAPIBaseURL, "/") + path
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, 0, err
		}
		c.mu.Lock()
		token := c.tokens.AccessToken
		c.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", c.api.UserAgent)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, 0, fmt.Errorf("request to %s: %w", path, ctx.Err())
			}
			backoff(attempt)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, 0, fmt.Errorf("request to %s: %w", path, ctx.Err())
			}
			backoff(attempt)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			// Force a refresh and retry once.
			c.mu.Lock()
			c.tokens.ExpiresAt = time.Time{} // force expired
			c.mu.Unlock()
			if err := c.ensureFreshToken(ctx); err != nil {
				return nil, resp.StatusCode, err
			}
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
			backoff(attempt)
			continue
		default:
			return data, resp.StatusCode, nil
		}
	}
	return nil, 0, fmt.Errorf("request to %s failed after retries: %w", path, lastErr)
}

func backoff(attempt int) {
	base := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	jitter := time.Duration(rand.Intn(250)) * time.Millisecond
	time.Sleep(base + jitter)
}

// VehicleSummary is one entry from GET /api/1/vehicles — a cheap call
// that reports whether the car is online WITHOUT waking it up. This is
// the only call teslalog makes while the vehicle is ASLEEP/SUSPENDED.
type VehicleSummary struct {
	ID          int64  `json:"id"`
	VehicleID   int64  `json:"vehicle_id"`
	VIN         string `json:"vin"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"` // "online", "asleep", "offline"
	InService   bool   `json:"in_service"`
}

// Awake reports whether the vehicle summary indicates the car is
// reachable right now (as opposed to asleep/offline).
func (v VehicleSummary) Awake() bool { return v.State == "online" }

type listVehiclesResponse struct {
	Response []VehicleSummary `json:"response"`
	Count    int              `json:"count"`
}

// ListVehicles fetches the account's vehicles and their summary state.
// Safe to call at any time, including while the vehicle is asleep — it
// never wakes the car.
func (c *Client) ListVehicles(ctx context.Context) ([]VehicleSummary, error) {
	data, status, err := c.do(ctx, http.MethodGet, "/api/1/vehicles", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list vehicles: HTTP %d: %s", status, string(data))
	}
	var lr listVehiclesResponse
	if err := json.Unmarshal(data, &lr); err != nil {
		return nil, fmt.Errorf("decode vehicles list: %w", err)
	}
	return lr.Response, nil
}

// WakeUp explicitly issues a wake command. Deliberately NOT called
// anywhere in the daemon's automatic polling loop — see cmd/teslalog's
// `wake` subcommand for the only (manual, user-invoked) call site.
func (c *Client) WakeUp(ctx context.Context, vehicleID int64) error {
	path := fmt.Sprintf("/api/1/vehicles/%d/wake_up", vehicleID)
	data, status, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("wake_up: HTTP %d: %s", status, string(data))
	}
	return nil
}

// rawVehicleData mirrors the subset of Tesla's vehicle_data response
// teslalog cares about. Field names/shapes follow the long-stable
// public Owner API schema (see tesla-api.timdorr.com), cross-checked
// against TeslaMate's own field usage (lib/teslamate/log/*.ex) for data
// parity: every field TeslaMate persists per-position/per-charge-sample
// is captured here too.
type rawVehicleData struct {
	Response struct {
		ID            int64  `json:"id"`
		VehicleID     int64  `json:"vehicle_id"`
		VIN           string `json:"vin"`
		DisplayName   string `json:"display_name"`
		State         string `json:"state"`
		VehicleConfig struct {
			CarType       string `json:"car_type"`
			TrimBadging   string `json:"trim_badging"`
			ExteriorColor string `json:"exterior_color"`
			WheelType     string `json:"wheel_type"`
			SpoilerType   string `json:"spoiler_type"`
		} `json:"vehicle_config"`
		DriveState struct {
			ShiftState string  `json:"shift_state"`
			Latitude   float64 `json:"latitude"`
			Longitude  float64 `json:"longitude"`
			Speed      float64 `json:"speed"`
			Heading    float64 `json:"heading"`
			Power      float64 `json:"power"`
		} `json:"drive_state"`
		ChargeState struct {
			ChargingState        string  `json:"charging_state"`
			BatteryLevel         int     `json:"battery_level"`
			UsableBatteryLevel   int     `json:"usable_battery_level"`
			BatteryRange         float64 `json:"battery_range"` // "rated" range
			IdealBatteryRange    float64 `json:"ideal_battery_range"`
			EstBatteryRange      float64 `json:"est_battery_range"`
			BatteryHeaterOn      bool    `json:"battery_heater_on"`
			ChargeEnergyAdded    float64 `json:"charge_energy_added"`
			ChargerPower         float64 `json:"charger_power"`
			ChargerVoltage       float64 `json:"charger_voltage"`
			ChargerActualCurrent float64 `json:"charger_actual_current"`
			ChargerPilotCurrent  float64 `json:"charger_pilot_current"`
			ChargerPhases        int     `json:"charger_phases"`
			ConnChargeCable      string  `json:"conn_charge_cable"`
			FastChargerPresent   bool    `json:"fast_charger_present"`
			FastChargerBrand     string  `json:"fast_charger_brand"`
			FastChargerType      string  `json:"fast_charger_type"`
			NotEnoughPowerToHeat bool    `json:"not_enough_power_to_heat"`
		} `json:"charge_state"`
		ClimateState struct {
			InsideTemp           float64 `json:"inside_temp"`
			OutsideTemp          float64 `json:"outside_temp"`
			DriverTempSetting    float64 `json:"driver_temp_setting"`
			PassengerTempSetting float64 `json:"passenger_temp_setting"`
			IsClimateOn          bool    `json:"is_climate_on"`
			FanStatus            int     `json:"fan_status"`
			IsRearDefrosterOn    bool    `json:"is_rear_defroster_on"`
			IsFrontDefrosterOn   bool    `json:"is_front_defroster_on"`
		} `json:"climate_state"`
		VehicleState struct {
			Odometer       float64 `json:"odometer"`
			CarVersion     string  `json:"car_version"`
			TpmsPressureFL float64 `json:"tpms_pressure_fl"`
			TpmsPressureFR float64 `json:"tpms_pressure_fr"`
			TpmsPressureRL float64 `json:"tpms_pressure_rl"`
			TpmsPressureRR float64 `json:"tpms_pressure_rr"`
			SoftwareUpdate struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			} `json:"software_update"`
		} `json:"vehicle_state"`
	} `json:"response"`
}

// VehicleMeta is static-ish vehicle identity info parsed alongside a
// vehicle_data snapshot.
type VehicleMeta struct {
	VIN           string
	DisplayName   string
	Model         string
	TrimBadging   string
	ExteriorColor string
	WheelType     string
	SpoilerType   string
}

// VehicleData fetches a full vehicle_data snapshot. The caller MUST
// only invoke this while the vehicle is known to be online (e.g. after
// ListVehicles reported State=="online") — vehicle_data returns errors
// (and, more importantly, is pointless load) when the car is asleep,
// and teslalog never calls wake_up to work around that.
func (c *Client) VehicleData(ctx context.Context, vehicleID int64) (vehicle.Snapshot, VehicleMeta, error) {
	path := fmt.Sprintf("/api/1/vehicles/%d/vehicle_data", vehicleID)
	data, status, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return vehicle.Snapshot{}, VehicleMeta{}, err
	}
	if status != http.StatusOK {
		return vehicle.Snapshot{}, VehicleMeta{}, fmt.Errorf("vehicle_data: HTTP %d: %s", status, string(data))
	}

	var rv rawVehicleData
	if err := json.Unmarshal(data, &rv); err != nil {
		return vehicle.Snapshot{}, VehicleMeta{}, fmt.Errorf("decode vehicle_data: %w", err)
	}

	cs := rv.Response.ChargeState
	cl := rv.Response.ClimateState
	vs := rv.Response.VehicleState

	snap := vehicle.Snapshot{
		Time:          time.Now().UTC(),
		ShiftState:    rv.Response.DriveState.ShiftState,
		ChargingState: cs.ChargingState,
		OdometerKm:    milesToKm(vs.Odometer),
		Lat:           rv.Response.DriveState.Latitude,
		Lng:           rv.Response.DriveState.Longitude,
		SpeedKmh:      milesToKm(rv.Response.DriveState.Speed),
		Heading:       rv.Response.DriveState.Heading,
		PowerKw:       rv.Response.DriveState.Power,

		BatteryLevel:         cs.BatteryLevel,
		UsableBatteryLevel:   cs.UsableBatteryLevel,
		RangeKm:              milesToKm(cs.BatteryRange),
		IdealRangeKm:         milesToKm(cs.IdealBatteryRange),
		EstRangeKm:           milesToKm(cs.EstBatteryRange),
		BatteryHeaterOn:      cs.BatteryHeaterOn,
		NotEnoughPowerToHeat: cs.NotEnoughPowerToHeat,

		ChargeEnergyAddedKwh: cs.ChargeEnergyAdded,
		ChargerPowerKw:       cs.ChargerPower,
		ChargerVoltage:       cs.ChargerVoltage,
		ChargerActualCurrent: cs.ChargerActualCurrent,
		ChargerPilotCurrent:  int(cs.ChargerPilotCurrent),
		ChargerPhases:        cs.ChargerPhases,
		ConnChargeCable:      cs.ConnChargeCable,
		FastChargerPresent:   cs.FastChargerPresent,
		FastChargerBrand:     cs.FastChargerBrand,
		FastChargerType:      cs.FastChargerType,

		OutsideTempC:          cl.OutsideTemp,
		InsideTempC:           cl.InsideTemp,
		FanStatus:             cl.FanStatus,
		DriverTempSettingC:    cl.DriverTempSetting,
		PassengerTempSettingC: cl.PassengerTempSetting,
		IsClimateOn:           cl.IsClimateOn,
		IsRearDefrosterOn:     cl.IsRearDefrosterOn,
		IsFrontDefrosterOn:    cl.IsFrontDefrosterOn,

		TpmsPressureFL: vs.TpmsPressureFL,
		TpmsPressureFR: vs.TpmsPressureFR,
		TpmsPressureRL: vs.TpmsPressureRL,
		TpmsPressureRR: vs.TpmsPressureRR,

		UpdateStatus:  vs.SoftwareUpdate.Status,
		UpdateVersion: vs.SoftwareUpdate.Version,
	}

	meta := VehicleMeta{
		VIN:           rv.Response.VIN,
		DisplayName:   rv.Response.DisplayName,
		Model:         rv.Response.VehicleConfig.CarType,
		TrimBadging:   rv.Response.VehicleConfig.TrimBadging,
		ExteriorColor: rv.Response.VehicleConfig.ExteriorColor,
		WheelType:     rv.Response.VehicleConfig.WheelType,
		SpoilerType:   rv.Response.VehicleConfig.SpoilerType,
	}

	return snap, meta, nil
}

// milesToKm converts Tesla's imperial-unit API fields to metric, which
// is what the rest of teslalog (and the design doc) uses throughout.
func milesToKm(miles float64) float64 {
	return miles * 1.609344
}
