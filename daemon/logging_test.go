package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"monks.co/backupd/history"
	"monks.co/backupd/model"
)

// The daemon's structured record is what a host alerts on: "did the last
// cycle succeed" should be a query against a named attribute, not a
// substring match against a log message. These tests pin the attributes
// that promise holds by.

// capture is a slog.Handler that keeps every record for inspection.
type capture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture) WithGroup(string) slog.Handler            { return c }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}

// find returns the first record carrying the named attribute, with that
// attribute's value.
func (c *capture) find(key string) (slog.Record, slog.Value, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		var found bool
		var val slog.Value
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				found, val = true, a.Value
				return false
			}
			return true
		})
		if found {
			return r, val, true
		}
	}
	return slog.Record{}, slog.Value{}, false
}

// loggingDaemon builds a daemon whose records are captured, with the
// fixture's environment behind it.
func loggingDaemon(t *testing.T, local, remote *fakeExecutor) (*Daemon, *capture) {
	t.Helper()
	cap := &capture{}
	conf := testConf()
	b := newTestDaemon(conf, local, remote)
	b.log = slog.New(cap)
	return b, cap
}

// runOneCycle runs the sync loop for exactly one cycle by cancelling at
// the between-cycle wait.
func runOneCycle(t *testing.T, b *Daemon) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.wait = func(context.Context, time.Duration) (*syncRequest, error) {
		cancel()
		return nil, context.Canceled
	}
	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
}

// TestCycleOutcomeIsLoggedStructured: every completed cycle emits one
// record naming its outcome, so "backups are running" is a query rather
// than an inference.
func TestCycleOutcomeIsLoggedStructured(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, cap := loggingDaemon(t, local, remote)
	runOneCycle(t, b)

	rec, ok, found := cap.find(CycleOKKey)
	if !found {
		t.Fatalf("no record carried %s", CycleOKKey)
	}
	if ok.Bool() != true {
		t.Errorf("%s = false on a healthy cycle", CycleOKKey)
	}
	if rec.Level != slog.LevelInfo {
		t.Errorf("healthy cycle logged at %s, want INFO", rec.Level)
	}
	if _, _, found := cap.find(CycleDurationKey); !found {
		t.Errorf("no record carried %s", CycleDurationKey)
	}
}

// TestFailingCycleLogsAtErrorLevel: a host that routes on level alone —
// the common case — has to see a failed cycle without knowing backupd's
// attribute vocabulary.
func TestFailingCycleLogsAtErrorLevel(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t filesystem", err: errors.New("pool suspended")},
	}}
	remote := &fakeExecutor{name: "remote"}
	b, cap := loggingDaemon(t, local, remote)
	runOneCycle(t, b)

	rec, ok, found := cap.find(CycleOKKey)
	if !found {
		t.Fatalf("no record carried %s", CycleOKKey)
	}
	if ok.Bool() != false {
		t.Errorf("%s = true on a failed cycle", CycleOKKey)
	}
	if rec.Level != slog.LevelError {
		t.Errorf("failed cycle logged at %s, want ERROR", rec.Level)
	}
}

// TestJournalEntriesAreLoggedWithTheirDataset: the journal is the
// operator's incident record, and a host collecting it needs the dataset
// as a field to filter on rather than as prose in the message.
func TestJournalEntriesAreLoggedWithTheirDataset(t *testing.T) {
	local, remote := steadyStateExecutors()
	b, cap := loggingDaemon(t, local, remote)

	name := model.DatasetName("/foo")
	b.event(history.Error, &name, "sync failed: %s", "pool suspended")

	rec, val, found := cap.find(DatasetKey)
	if !found {
		t.Fatalf("no record carried %s", DatasetKey)
	}
	if val.String() != name.String() {
		t.Errorf("%s = %q, want %q", DatasetKey, val.String(), name.String())
	}
	if rec.Level != slog.LevelError {
		t.Errorf("error-level journal entry logged at %s, want ERROR", rec.Level)
	}
	if _, _, found := cap.find(EventKey); !found {
		t.Errorf("no record carried %s", EventKey)
	}
}
