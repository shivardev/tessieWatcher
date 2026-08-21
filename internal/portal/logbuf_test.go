package portal

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLogBufferCapsAtMax(t *testing.T) {
	buf := NewLogBuffer(3)
	logger := slog.New(buf.Handler())
	for i := 0; i < 5; i++ {
		logger.Info("line", "i", i)
	}

	lines := buf.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (capped), got %d: %v", len(lines), lines)
	}
	// Oldest two (i=0, i=1) must have been dropped - only the most
	// recent 3 survive, oldest-first.
	if !strings.Contains(lines[0], "i=2") || !strings.Contains(lines[2], "i=4") {
		t.Fatalf("expected the 3 most recent lines in order, got: %v", lines)
	}
}

func TestLogBufferCapturesMessageAndAttrs(t *testing.T) {
	buf := NewLogBuffer(10)
	logger := slog.New(buf.Handler())
	logger.Warn("vehicle_data poll failed", "error", "timeout")

	lines := buf.Lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	line := lines[0]
	if !strings.Contains(line, "WARN") || !strings.Contains(line, "vehicle_data poll failed") || !strings.Contains(line, "error=timeout") {
		t.Fatalf("expected level, message, and attrs in the line, got: %q", line)
	}
}

func TestLogBufferHandlerWithAttrsPersistsAcrossCalls(t *testing.T) {
	buf := NewLogBuffer(10)
	logger := slog.New(buf.Handler()).With("component", "test")
	logger.Info("hello")

	lines := buf.Lines()
	if len(lines) != 1 || !strings.Contains(lines[0], "component=test") {
		t.Fatalf("expected the .With attr to persist into the logged line, got: %v", lines)
	}
}

func TestLogBufferHandlerEnabledAlwaysTrue(t *testing.T) {
	buf := NewLogBuffer(1)
	h := buf.Handler()
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatalf("expected the buffer handler to accept all levels")
	}
}
