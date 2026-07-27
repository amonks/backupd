// Package status tracks what the daemon is doing *right now*: the sync
// loop's phase, the deadline it is waiting toward, the dataset and plan
// step being executed, and live progress of the in-flight transfer.
//
// This state answers the dashboard's most important question — "what is
// happening?" — and previously lived in local variables inside the sync
// loop, invisible to the UI. Like the sync-status map it replaces, it
// is ephemeral by design: it describes the present, so it is simply
// re-derived by the loop as it runs and starts empty on boot.
package status

import (
	"time"

	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

type Phase int

const (
	// Starting: process boot, before the first cycle begins.
	Starting Phase = iota
	// Refreshing: discovering datasets and snapshots from ZFS.
	Refreshing
	// Syncing: executing a dataset's plan.
	Syncing
	// Idle: last cycle succeeded; waiting until the next one.
	Idle
	// BackingOff: last cycle failed; waiting to retry.
	BackingOff
)

func (p Phase) String() string {
	switch p {
	case Starting:
		return "starting"
	case Refreshing:
		return "refreshing"
	case Syncing:
		return "syncing"
	case Idle:
		return "idle"
	case BackingOff:
		return "backing off"
	default:
		return "unknown"
	}
}

// Transfer is the progress of an in-flight snapshot transfer.
type Transfer struct {
	StartedAt time.Time
	Sent      int64
	Total     int64
	// Rate is a smoothed estimate of recent throughput in bytes/sec.
	Rate float64
}

// Percent returns completion in [0, 100].
func (t Transfer) Percent() float64 {
	if t.Total <= 0 {
		return 0
	}
	return min(100, float64(t.Sent)/float64(t.Total)*100)
}

// ETA estimates the remaining duration from the current rate. ok is
// false until there is a usable rate.
func (t Transfer) ETA() (time.Duration, bool) {
	if t.Rate <= 0 || t.Total <= 0 || t.Sent >= t.Total {
		return 0, false
	}
	return time.Duration(float64(t.Total-t.Sent) / t.Rate * float64(time.Second)), true
}

// Activity is one consistent snapshot of the loop's current state.
type Activity struct {
	Phase Phase
	// Since is when the current phase began.
	Since time.Time
	// CycleStarted is when the current (or, when waiting, the previous)
	// cycle began. Zero before the first cycle.
	CycleStarted time.Time
	// Until is the deadline being waited toward: the next cycle when
	// Idle, the retry when BackingOff. Zero otherwise.
	Until time.Time
	// ConsecutiveFailures counts failed cycles since the last success.
	ConsecutiveFailures int
	// Dataset, Step, Steps, and Operation describe plan execution while
	// Syncing. Step is 1-based; Steps is 0 when no plan is executing.
	Dataset   model.DatasetName
	Step      int
	Steps     int
	Operation string
	// Transfer is the in-flight transfer's progress, or nil.
	Transfer *Transfer
}

// A Tracker holds the current Activity and notifies on change. Progress
// updates arrive per pipe write — far too often to notify every time —
// so they are rate-limited; phase transitions always notify.
type Tracker struct {
	atom     *atom.Atom[Activity]
	onChange func()

	// now is injectable for tests.
	now func() time.Time

	// lastNotify rate-limits progress notifications.
	lastNotify *atom.Atom[time.Time]
}

// notifyEvery is how often transfer progress may fire onChange.
const notifyEvery = 2 * time.Second

// New returns a Tracker. onChange may be nil.
func New(onChange func()) *Tracker {
	return &Tracker{
		atom:       atom.New(Activity{Phase: Starting}),
		onChange:   onChange,
		now:        time.Now,
		lastNotify: atom.New(time.Time{}),
	}
}

// Get returns the current activity snapshot.
func (t *Tracker) Get() Activity {
	return t.atom.Deref()
}

// IsSyncing reports whether the named dataset's plan is executing.
func (t *Tracker) IsSyncing(dataset model.DatasetName) bool {
	a := t.atom.Deref()
	return a.Phase == Syncing && a.Dataset == dataset
}

func (t *Tracker) swap(f func(Activity) Activity) {
	t.atom.Swap(f)
	if t.onChange != nil {
		t.onChange()
	}
}

// StartCycle marks the beginning of a cycle's refresh phase.
func (t *Tracker) StartCycle() {
	now := t.now()
	t.swap(func(a Activity) Activity {
		return Activity{
			Phase:               Refreshing,
			Since:               now,
			CycleStarted:        now,
			ConsecutiveFailures: a.ConsecutiveFailures,
		}
	})
}

// StartDataset marks the beginning of one dataset's sync.
func (t *Tracker) StartDataset(dataset model.DatasetName) {
	now := t.now()
	t.swap(func(a Activity) Activity {
		a.Phase = Syncing
		a.Since = now
		a.Dataset = dataset
		a.Step, a.Steps, a.Operation = 0, 0, ""
		a.Transfer = nil
		return a
	})
}

// StartStep marks plan-step execution within the current dataset.
// step is 1-based.
func (t *Tracker) StartStep(step, steps int, operation string) {
	t.swap(func(a Activity) Activity {
		a.Step, a.Steps, a.Operation = step, steps, operation
		a.Transfer = nil
		return a
	})
}

// Wait marks the between-cycles wait: idle after a success, backing off
// after failures (failures > 0 selects BackingOff).
func (t *Tracker) Wait(until time.Time, failures int) {
	now := t.now()
	t.swap(func(a Activity) Activity {
		phase := Idle
		if failures > 0 {
			phase = BackingOff
		}
		return Activity{
			Phase:               phase,
			Since:               now,
			CycleStarted:        a.CycleStarted,
			Until:               until,
			ConsecutiveFailures: failures,
		}
	})
}

// Progress records transfer progress. It is called for every write on
// the transfer pipe, so it must stay cheap and only occasionally
// notify.
func (t *Tracker) Progress(sent, total int64) {
	now := t.now()
	t.atom.Swap(func(a Activity) Activity {
		xfer := a.Transfer
		if xfer == nil {
			xfer = &Transfer{StartedAt: now}
		} else {
			copied := *xfer
			xfer = &copied
		}
		if dt := now.Sub(xfer.StartedAt).Seconds(); dt > 0 {
			// A smoothed whole-transfer average: stable enough for an
			// ETA, cheap enough for the write path.
			xfer.Rate = float64(sent) / dt
		}
		xfer.Sent, xfer.Total = sent, total
		a.Transfer = xfer
		return a
	})

	if t.onChange == nil {
		return
	}
	last := t.lastNotify.Deref()
	if now.Sub(last) >= notifyEvery {
		t.lastNotify.Reset(now)
		t.onChange()
	}
}
