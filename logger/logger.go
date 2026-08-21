// Package logger buffers recent log lines in memory for display while
// teeing every line to the process log. The buffer is a bounded ring: a
// daemon that runs for months writes an unbounded number of lines, and
// the durable record of all of them is the process log (stdout or
// -logfile); the in-memory copy exists only for recent-detail surfaces
// like plan-step logs, so it keeps the newest entries and drops the
// rest.
//
// Where those lines go is the host's to choose. By default they go to
// the standard log package, one line each, prefixed with the logger's
// label. A host embedding backupd as a library calls SetLogger to route
// them into its own structured pipeline instead.
package logger

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// LabelKey is the attribute a routed line carries its logger's label
// under: "global", a dataset name, or a plan operation.
const LabelKey = "backupd.log"

// sink is where lines go once buffered. Loggers are created deep in the
// model (one per dataset, one per plan step), so the destination is a
// package-level setting rather than a constructor argument — the same
// shape the standard log package has, and the reason SetLogger is
// documented as process-wide.
var sink atomic.Pointer[slog.Logger]

// SetLogger routes every line written to every Logger to l, at info
// level, with the writing logger's label attached under LabelKey.
// Passing nil restores the default, which writes "[label]\tline" to the
// standard log package.
//
// This is process-wide, not per-daemon: one process running two daemons
// shares one destination.
func SetLogger(l *slog.Logger) {
	sink.Store(l)
}

// keep bounds each logger's ring. Sized for the busiest writer — a
// long transfer's once-a-minute throughput reports — to hold several
// hours of tail.
const keep = 500

type Logger struct {
	label string

	mu   sync.Mutex
	ring []LogEntry // circular once full
	next int        // write index into ring
	full bool
}

type LogEntry struct {
	LogAt time.Time
	Log   string
}

var _ io.Writer = &Logger{}

func New(label string) *Logger {
	return &Logger{label: label}
}

func (p *Logger) Printf(s string, args ...any) {
	p.Write(fmt.Appendf(nil, s, args...))
}

func (p *Logger) Write(bs []byte) (int, error) {
	entry := LogEntry{
		LogAt: time.Now(),
		Log:   string(bs),
	}
	p.mu.Lock()
	if p.full {
		p.ring[p.next] = entry
		p.next = (p.next + 1) % keep
	} else {
		p.ring = append(p.ring, entry)
		if len(p.ring) == keep {
			p.full = true
		}
	}
	p.mu.Unlock()
	p.emit(entry.Log)
	return len(bs), nil
}

// emit hands one line to the configured destination. The slog path uses
// LogAttrs with a background context and no caller PC: these lines come
// from a daemon loop rather than a request, and the source position they
// would report is this file.
func (p *Logger) emit(line string) {
	if l := sink.Load(); l != nil {
		l.LogAttrs(context.Background(), slog.LevelInfo, line, slog.String(LabelKey, p.label))
		return
	}
	log.Println(fmt.Sprintf("[%s]\t", p.label) + line)
}

// GetLogs returns the retained entries, oldest first.
func (p *Logger) GetLogs() []LogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.full {
		out := make([]LogEntry, len(p.ring))
		copy(out, p.ring)
		return out
	}
	out := make([]LogEntry, 0, keep)
	out = append(out, p.ring[p.next:]...)
	out = append(out, p.ring[:p.next]...)
	return out
}
