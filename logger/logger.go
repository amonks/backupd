// Package logger buffers recent log lines in memory for display while
// teeing every line to the process log. The buffer is a bounded ring: a
// daemon that runs for months writes an unbounded number of lines, and
// the durable record of all of them is the process log (stdout or
// -logfile); the in-memory copy exists only for recent-detail surfaces
// like plan-step logs, so it keeps the newest entries and drops the
// rest.
package logger

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

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
	log.Println(fmt.Sprintf("[%s]\t", p.label) + string(bs))
	return len(bs), nil
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
