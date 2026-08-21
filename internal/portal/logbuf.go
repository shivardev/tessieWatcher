package portal

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// LogBuffer keeps the last N log lines in memory so the portal's index
// page can show recent activity without shelling out to `journalctl` or
// needing any special permissions - just whatever the process itself
// already logged via log/slog. Safe for concurrent use.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewLogBuffer creates a LogBuffer holding at most max lines (oldest
// dropped first once full).
func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 1
	}
	return &LogBuffer{max: max}
}

// Lines returns a snapshot of the currently buffered lines, oldest
// first (same order they were logged in).
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

func (b *LogBuffer) append(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// Handler returns an slog.Handler that formats every record as one line
// (timestamp, level, message, attrs - the same information a plain-text
// journalctl line would show) and appends it to this buffer. Meant to
// be combined with a normal handler (see runner's teeHandler) so log
// output still also goes to stderr/journalctl exactly as before -
// LogBuffer only ever adds a second destination, never replaces one.
func (b *LogBuffer) Handler() slog.Handler {
	return &logBufHandler{buf: b}
}

type logBufHandler struct {
	buf   *LogBuffer
	attrs []slog.Attr
}

func (h *logBufHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *logBufHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Time.Format("2006-01-02 15:04:05"))
	sb.WriteByte(' ')
	sb.WriteString(r.Level.String())
	sb.WriteByte(' ')
	sb.WriteString(r.Message)
	for _, a := range h.attrs {
		sb.WriteByte(' ')
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		sb.WriteString(a.Value.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteByte(' ')
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		sb.WriteString(a.Value.String())
		return true
	})
	h.buf.append(sb.String())
	return nil
}

func (h *logBufHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &logBufHandler{buf: h.buf, attrs: merged}
}

// WithGroup is a no-op: nothing in this codebase uses slog groups, and
// this is a plain-line buffer, not a structured sink.
func (h *logBufHandler) WithGroup(string) slog.Handler { return h }
