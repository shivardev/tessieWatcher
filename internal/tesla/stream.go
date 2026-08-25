// stream.go implements a client for Tesla's legacy streaming websocket
// (wss://streaming.vn.teslamotors.com/streaming/). This is purely a
// supplemental, high-frequency GPS/telemetry source layered on top of
// REST polling — it is NOT used to make drive/charge/sleep decisions.
// Those decisions always come from vehicle_data polls in ownerapi.go
// and the vehicle.Machine state machine, which is a more conservative
// and better-documented source of truth than an unofficial websocket
// protocol. If the stream drops, teslalog keeps working fine on REST
// polling alone; stream.go only adds extra Position rows while a drive
// is open.
package tesla

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// streamFields is the field list we request, in order. The server
// echoes values in this exact order after a leading timestamp field,
// per Tesla's documented streaming protocol.
var streamFields = []string{
	"speed", "odometer", "soc", "elevation", "est_heading",
	"est_lat", "est_lng", "power", "shift_state", "range", "est_range", "heading",
}

// StreamSample is one parsed "data:update" message.
type StreamSample struct {
	Time         time.Time
	SpeedKmh     float64
	OdometerKm   float64
	BatteryLevel int
	ElevationM   float64
	Heading      float64
	Lat, Lng     float64
	PowerKw      float64
	ShiftState   string
	RangeKm      float64
}

// StreamConn is an open streaming connection for one vehicle.
type StreamConn struct {
	ws      *websocket.Conn
	samples chan StreamSample
	errs    chan error
	done    chan struct{}
}

// Samples returns the channel of parsed telemetry samples.
func (c *StreamConn) Samples() <-chan StreamSample { return c.samples }

// Errors returns the channel that receives a single terminal error
// (server error message, read failure, or normal close) before
// closing.
func (c *StreamConn) Errors() <-chan error { return c.errs }

// Close closes the underlying websocket.
func (c *StreamConn) Close() error {
	close(c.done)
	return c.ws.Close()
}

// Connect opens the streaming websocket for vehicleID and subscribes
// using an OAuth access token (no separate streaming password needed
// with the modern data:subscribe_oauth message type).
func Connect(ctx context.Context, streamURL, accessToken string, vehicleID int64) (*StreamConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, streamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial streaming websocket: %w", err)
	}

	sub := map[string]any{
		"msg_type": "data:subscribe_oauth",
		"token":    accessToken,
		"value":    strings.Join(streamFields, ","),
		"tag":      strconv.FormatInt(vehicleID, 10),
	}
	if err := ws.WriteJSON(sub); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send subscribe message: %w", err)
	}

	c := &StreamConn{
		ws:      ws,
		samples: make(chan StreamSample, 32),
		errs:    make(chan error, 1),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

type wireMessage struct {
	MsgType   string `json:"msg_type"`
	Value     string `json:"value"`
	ErrorType string `json:"error_type"`
	Tag       string `json:"tag"`
}

func (c *StreamConn) readLoop() {
	defer close(c.samples)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		var msg wireMessage
		if err := c.ws.ReadJSON(&msg); err != nil {
			select {
			case c.errs <- fmt.Errorf("streaming read: %w", err):
			default:
			}
			return
		}

		switch msg.MsgType {
		case "control:hello":
			// Connection established; nothing to do.
		case "data:update":
			sample, err := parseStreamValue(msg.Value)
			if err != nil {
				continue // skip malformed frame, keep the connection alive
			}
			select {
			case c.samples <- sample:
			case <-c.done:
				return
			}
		case "data:error", "control:error":
			select {
			case c.errs <- fmt.Errorf("streaming server error (%s): %s", msg.ErrorType, msg.Value):
			default:
			}
			return
		}
	}
}

// parseStreamValue parses the CSV "timestamp,field1,field2,..." payload
// of a data:update message into a StreamSample.
func parseStreamValue(value string) (StreamSample, error) {
	parts := strings.Split(value, ",")
	if len(parts) != len(streamFields)+1 {
		return StreamSample{}, fmt.Errorf("expected %d fields, got %d", len(streamFields)+1, len(parts))
	}

	tsMillis, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return StreamSample{}, fmt.Errorf("parse timestamp: %w", err)
	}

	f := func(i int) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		return v
	}

	s := StreamSample{
		Time:         time.UnixMilli(tsMillis).UTC(),
		SpeedKmh:     milesToKm(f(1)),
		OdometerKm:   milesToKm(f(2)),
		BatteryLevel: int(f(3)),
		// Elevation arrives in METRES, unlike speed/odometer/range on
		// either side of it, which are miles. Verified three ways
		// against the same car's TeslaMate instance rather than assumed:
		// TeslaMate stores this field unconverted and holds 147 for a
		// Tennessee location (147 ft would be 45 m, impossible there);
		// its cumulative-ascent figure for one drive was 266 ft against
		// our 82, a ratio of 3.24 - the foot-to-metre factor exactly;
		// and the raw 228 read on that drive is 228 m, matching
		// Chattanooga's real 205-250 m, where 69 m is not a place.
		//
		// Treating it as feet made every elevation 3.28x too small,
		// which also skewed drives.ascent_m/descent_m and the
		// slope-adjusted efficiency computed from them.
		ElevationM:   f(4),
		Heading:      f(5), // est_heading
		Lat:          f(6),
		Lng:          f(7),
		PowerKw:      f(8),
		ShiftState:   strings.TrimSpace(parts[9]),
		RangeKm:      milesToKm(f(10)),
	}
	return s, nil
}
