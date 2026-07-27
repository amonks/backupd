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

// keep bounds the cycle ring buffer; keepOps bounds the executed-
// operations feed.
const (
	keep    = 50
	keepOps = 200
)

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

// Failure records a dataset's most recent sync failure. It is kept
// alongside — not erased by — later successes, so the dashboard can
// compare timestamps to decide whether the dataset is currently
// failing.
type Failure struct {
	At    time.Time
	Error string
}

// Op is one executed operation: a transfer, deletion, resume, or
// snapshot creation. The feed of recent Ops is the dashboard's "what
// has backupd actually done" answer.
type Op struct {
	At        time.Time
	Dataset   model.DatasetName
	Operation string
	Duration  time.Duration
	Error     string // empty on success
}

type record struct {
	cycles      []Cycle // newest first
	ops         []Op    // newest first
	lastSuccess map[model.DatasetName]time.Time
	lastFailure map[model.DatasetName]Failure
	lastSnitch  time.Time
}

type History struct {
	atom *atom.Atom[record]
}

func New() *History {
	return &History{
		atom: atom.New(record{
			lastSuccess: make(map[model.DatasetName]time.Time),
			lastFailure: make(map[model.DatasetName]Failure),
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
		r.cycles = cycles
		return r
	})
}

// RecordOp prepends an executed operation, discarding the oldest beyond
// the feed size.
func (h *History) RecordOp(op Op) {
	h.atom.Swap(func(r record) record {
		ops := make([]Op, 0, len(r.ops)+1)
		ops = append(ops, op)
		ops = append(ops, r.ops...)
		if len(ops) > keepOps {
			ops = ops[:keepOps]
		}
		r.ops = ops
		return r
	})
}

// RecordDatasetSuccess notes that a dataset fully executed its plan.
func (h *History) RecordDatasetSuccess(dataset model.DatasetName, at time.Time) {
	h.atom.Swap(func(r record) record {
		lastSuccess := make(map[model.DatasetName]time.Time, len(r.lastSuccess)+1)
		maps.Copy(lastSuccess, r.lastSuccess)
		lastSuccess[dataset] = at
		r.lastSuccess = lastSuccess
		return r
	})
}

// RecordDatasetFailure notes that a dataset's sync failed and why.
func (h *History) RecordDatasetFailure(dataset model.DatasetName, at time.Time, errMsg string) {
	h.atom.Swap(func(r record) record {
		lastFailure := make(map[model.DatasetName]Failure, len(r.lastFailure)+1)
		maps.Copy(lastFailure, r.lastFailure)
		lastFailure[dataset] = Failure{At: at, Error: errMsg}
		r.lastFailure = lastFailure
		return r
	})
}

// RecordSnitch notes a successful Dead Man's Snitch ping.
func (h *History) RecordSnitch(at time.Time) {
	h.atom.Swap(func(r record) record {
		r.lastSnitch = at
		return r
	})
}

// Cycles returns cycle summaries, newest first.
func (h *History) Cycles() []Cycle {
	return h.atom.Deref().cycles
}

// Ops returns executed operations, newest first.
func (h *History) Ops() []Op {
	return h.atom.Deref().ops
}

// LastSuccess returns when a dataset last fully executed its plan.
func (h *History) LastSuccess(dataset model.DatasetName) (time.Time, bool) {
	at, ok := h.atom.Deref().lastSuccess[dataset]
	return at, ok
}

// LastFailure returns a dataset's most recent sync failure, if any.
func (h *History) LastFailure(dataset model.DatasetName) (Failure, bool) {
	f, ok := h.atom.Deref().lastFailure[dataset]
	return f, ok
}

// LastSnitch returns when the snitch was last successfully pinged.
func (h *History) LastSnitch() (time.Time, bool) {
	at := h.atom.Deref().lastSnitch
	return at, !at.IsZero()
}
