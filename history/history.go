// Package history records sync-cycle outcomes and per-dataset success
// times. It lives outside the model atom on purpose: the model is rebuilt
// from ZFS at the start of every cycle for resilience, while history must
// survive that reset. It is in-memory only and starts empty on restart.
package history

import (
	"maps"
	"time"

	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

// keep bounds the cycle ring buffer.
const keep = 50

// Cycle summarizes one sync cycle.
type Cycle struct {
	StartedAt time.Time
	StoppedAt time.Time
	OK        bool
	Paused    bool     // globally paused during this cycle
	Error     string   // refresh error that aborted the cycle, if any
	Datasets  int      // datasets processed
	Failures  []string // datasets that failed to sync
}

// Duration returns how long the cycle took.
func (c Cycle) Duration() time.Duration {
	return c.StoppedAt.Sub(c.StartedAt)
}

type record struct {
	cycles      []Cycle // newest first
	lastSuccess map[model.DatasetName]time.Time
}

type History struct {
	atom *atom.Atom[record]
}

func New() *History {
	return &History{
		atom: atom.New(record{
			lastSuccess: make(map[model.DatasetName]time.Time),
		}),
	}
}

// RecordCycle prepends a cycle summary, discarding the oldest beyond the
// ring size.
func (h *History) RecordCycle(c Cycle) {
	h.atom.Swap(func(r record) record {
		cycles := make([]Cycle, 0, len(r.cycles)+1)
		cycles = append(cycles, c)
		cycles = append(cycles, r.cycles...)
		if len(cycles) > keep {
			cycles = cycles[:keep]
		}
		return record{cycles: cycles, lastSuccess: r.lastSuccess}
	})
}

// RecordDatasetSuccess notes that a dataset fully executed its plan.
func (h *History) RecordDatasetSuccess(dataset model.DatasetName, at time.Time) {
	h.atom.Swap(func(r record) record {
		lastSuccess := make(map[model.DatasetName]time.Time, len(r.lastSuccess)+1)
		maps.Copy(lastSuccess, r.lastSuccess)
		lastSuccess[dataset] = at
		return record{cycles: r.cycles, lastSuccess: lastSuccess}
	})
}

// Cycles returns cycle summaries, newest first.
func (h *History) Cycles() []Cycle {
	return h.atom.Deref().cycles
}

// LastSuccess returns when a dataset last fully executed its plan.
func (h *History) LastSuccess(dataset model.DatasetName) (time.Time, bool) {
	at, ok := h.atom.Deref().lastSuccess[dataset]
	return at, ok
}
