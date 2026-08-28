// Package history records sync-cycle outcomes, per-dataset success
// times, and a journal of notable transitions. It lives outside the
// model atom on purpose: the model is rebuilt from ZFS at the start of
// every cycle for resilience, while history must survive that reset.
// It is in-memory only and starts empty on restart.
package history

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

// keep bounds the cycle ring buffer (at an hourly interval, several
// weeks); keepOps bounds the executed-operations feed; keepEvents
// bounds the journal.
const (
	keep       = 1000
	keepOps    = 500
	keepEvents = 500
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
// has backupd actually done" answer. Kind buckets the operation for
// filtering ("transfer", "deletion", "resume", "snapshot"), recorded
// from the operation's type rather than sniffed from its string.
type Op struct {
	At        time.Time
	Dataset   model.DatasetName
	Operation string
	Kind      string
	Duration  time.Duration
	Error     string // empty on success
}

// Level classifies a journal event.
type Level string

const (
	Info    Level = "info"
	Warning Level = "warning"
	Error   Level = "error"
)

// Event is one journal entry. The journal records incidents and
// operator actions — never steady state — so a healthy month of
// hourly cycles journals almost nothing while a broken week journals
// exactly when and how things broke and recovered. A failing
// condition gets one entry when it begins; while it persists the
// entry is updated in place with a count and the latest error (the
// error text is allowed to drift — snapshot names, attempt counters,
// byte counts vary cycle to cycle — without producing new entries);
// recovery gets one entry naming when the streak began. The record
// methods below journal those transitions themselves; callers use
// RecordEvent for actions (config saves, pause toggles).
type Event struct {
	At    time.Time
	Level Level
	// Dataset is the affected dataset, nil for daemon-level events.
	Dataset *model.DatasetName
	Message string
}

// incident tracks one open failing condition: when it began and how
// many failures it has absorbed.
type incident struct {
	Since time.Time
	Count int
}

type record struct {
	cycles      []Cycle // newest first
	ops         []Op    // newest first
	events      []Event // newest first
	lastSuccess map[model.DatasetName]time.Time
	lastFailure map[model.DatasetName]Failure
	lastSnitch  time.Time
	// incidents holds the open failing conditions, keyed "cycle",
	// "snitch", or "dataset:<path>".
	incidents map[string]incident
}

// journal prepends an event, discarding the oldest beyond the ring
// size.
func journal(r record, e Event) record {
	events := make([]Event, 0, len(r.events)+1)
	events = append(events, e)
	events = append(events, r.events...)
	if len(events) > keepEvents {
		events = events[:keepEvents]
	}
	r.events = events
	return r
}

// updateNewestEvent copies the ring and updates the newest event
// matching the predicate. Reports whether a match was found.
func updateNewestEvent(r record, match func(Event) bool, update func(*Event)) (record, bool) {
	for i, e := range r.events {
		if match(e) {
			events := make([]Event, len(r.events))
			copy(events, r.events)
			update(&events[i])
			r.events = events
			return r, true
		}
	}
	return r, false
}

func (r record) setIncident(key string, inc incident) record {
	incidents := make(map[string]incident, len(r.incidents)+1)
	maps.Copy(incidents, r.incidents)
	incidents[key] = inc
	r.incidents = incidents
	return r
}

func (r record) clearIncident(key string) record {
	incidents := make(map[string]incident, len(r.incidents))
	maps.Copy(incidents, r.incidents)
	delete(incidents, key)
	r.incidents = incidents
	return r
}

func sameDataset(a, b *model.DatasetName) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// withIncidentFailure journals a failing condition: the transition
// into failing gets its own entry (keeping its At as the incident's
// start); while the condition persists, that entry is updated in
// place with the count and the latest error. If the entry has been
// pushed off the ring, a fresh one is journaled with the running
// count.
func (r record) withIncidentFailure(key string, ds *model.DatasetName, at time.Time, level Level, prefix, errMsg string) record {
	inc, open := r.incidents[key]
	if !open {
		r = r.setIncident(key, incident{Since: at, Count: 1})
		return journal(r, Event{At: at, Level: level, Dataset: ds, Message: prefix + ": " + errMsg})
	}
	inc.Count++
	r = r.setIncident(key, inc)
	msg := fmt.Sprintf("%s (×%d): %s", prefix, inc.Count, errMsg)
	r, found := updateNewestEvent(r,
		func(e Event) bool { return sameDataset(e.Dataset, ds) && strings.HasPrefix(e.Message, prefix) },
		func(e *Event) { e.Message = msg })
	if !found {
		r = journal(r, Event{At: at, Level: level, Dataset: ds, Message: msg})
	}
	return r
}

// withIncidentRecovery closes an open incident with an Info entry; a
// recovery with no open incident journals nothing.
func (r record) withIncidentRecovery(key string, ds *model.DatasetName, at time.Time, message func(incident) string) record {
	inc, open := r.incidents[key]
	if !open {
		return r
	}
	r = r.clearIncident(key)
	return journal(r, Event{At: at, Level: Info, Dataset: ds, Message: message(inc)})
}

func incidentKey(dataset model.DatasetName) string {
	return "dataset:" + dataset.Path()
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

// RecordCycle prepends a cycle summary, discarding the oldest beyond
// the ring size. The cycle-level error — the refresh failing, then
// recovering — is journaled as an incident; cycles that fail because
// datasets failed are not, since the dataset incidents already cover
// those.
func (h *History) RecordCycle(c Cycle) {
	h.atom.Swap(func(r record) record {
		if !c.OK && c.Error != "" {
			r = r.withIncidentFailure("cycle", nil, c.StoppedAt, Error, "cycle failed", c.Error)
		} else {
			r = r.withIncidentRecovery("cycle", nil, c.StoppedAt, func(incident) string { return "cycle recovered" })
		}

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

// RecordEvent appends a journal entry directly. The incident-driven
// entries are recorded by the other Record methods; this is for
// operator actions and daemon-level notices.
func (h *History) RecordEvent(e Event) {
	h.atom.Swap(func(r record) record {
		return journal(r, e)
	})
}

// RecordDatasetSuccess notes that a dataset fully executed its plan,
// journaling the recovery if the dataset had an open failing incident.
func (h *History) RecordDatasetSuccess(dataset model.DatasetName, at time.Time) {
	h.atom.Swap(func(r record) record {
		name := dataset
		r = r.withIncidentRecovery(incidentKey(dataset), &name, at, func(inc incident) string {
			// A duration rather than the incident's start: a message is
			// frozen text with no zone of its own, and every instant a
			// page shows is rendered for the viewer's zone at read time.
			return fmt.Sprintf("sync recovered (had been failing for %s)", failingFor(at.Sub(inc.Since)))
		})
		lastSuccess := make(map[model.DatasetName]time.Time, len(r.lastSuccess)+1)
		maps.Copy(lastSuccess, r.lastSuccess)
		lastSuccess[dataset] = at
		r.lastSuccess = lastSuccess
		return r
	})
}

// RecordDatasetFailure notes that a dataset's sync failed and why,
// opening (or extending) its failing incident in the journal.
func (h *History) RecordDatasetFailure(dataset model.DatasetName, at time.Time, errMsg string) {
	h.atom.Swap(func(r record) record {
		name := dataset
		r = r.withIncidentFailure(incidentKey(dataset), &name, at, Error, "sync failing", errMsg)
		lastFailure := make(map[model.DatasetName]Failure, len(r.lastFailure)+1)
		maps.Copy(lastFailure, r.lastFailure)
		lastFailure[dataset] = Failure{At: at, Error: errMsg}
		r.lastFailure = lastFailure
		return r
	})
}

// RecordSnitch notes a successful Dead Man's Snitch ping, journaling
// the recovery if pings had been failing.
func (h *History) RecordSnitch(at time.Time) {
	h.atom.Swap(func(r record) record {
		r = r.withIncidentRecovery("snitch", nil, at, func(incident) string { return "snitch ping recovered" })
		r.lastSnitch = at
		return r
	})
}

// RecordSnitchError notes a failed Dead Man's Snitch ping, opening
// (or extending) the snitch incident.
func (h *History) RecordSnitchError(at time.Time, errMsg string) {
	h.atom.Swap(func(r record) record {
		return r.withIncidentFailure("snitch", nil, at, Warning, "snitch ping failing", errMsg)
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

// Events returns journal entries, newest first.
func (h *History) Events() []Event {
	return h.atom.Deref().events
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

// failingFor words an incident's length for its recovery message —
// "3h12m", "2d4h", "<1m" — with no zone in it, so the message is as
// true in one place as another.
func failingFor(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) - h*60; m != 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) - days*24; h != 0 {
			return fmt.Sprintf("%dd%dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	}
}
