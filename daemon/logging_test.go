package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/sim"
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
// the common case — has to see a failed cycle that is a fault without
// knowing backupd's attribute vocabulary, and has to see why.
func TestFailingCycleLogsAtErrorLevel(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t filesystem", err: errors.New("pool suspended")},
	}}
	remote := &fakeExecutor{name: "remote"}
	b, cap := loggingDaemon(t, local, remote)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if ok := attr(rec, CycleOKKey); ok.Bool() != false {
		t.Errorf("%s = true on a failed cycle", CycleOKKey)
	}
	if rec.Level != slog.LevelError {
		t.Errorf("failed cycle logged at %s, want ERROR", rec.Level)
	}
	if msg := attr(rec, "error.message").String(); !strings.Contains(msg, "pool suspended") {
		t.Errorf("error.message = %q, want the refresh error", msg)
	}
	if has(rec, "error.type") {
		t.Errorf("error.type = %q on a failure nothing classifies", attr(rec, "error.type"))
	}
}

// cycle returns the cycle record, failing the test if there is none.
func (c *capture) cycle(t *testing.T) slog.Record {
	t.Helper()
	rec, _, found := c.find(CycleOKKey)
	if !found {
		t.Fatalf("no record carried %s", CycleOKKey)
	}
	return rec
}

// attr returns a record's named attribute, or the zero Value.
func attr(r slog.Record, key string) slog.Value {
	var val slog.Value
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value
			return false
		}
		return true
	})
	return val
}

func has(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}

// simLoggingDaemon runs the daemon over the sim with its records going
// to a host logger, the way a host embeds it: through Options.Logger.
func simLoggingDaemon(t *testing.T, conf *config.Config, s *sim.Sim) (*Daemon, *capture) {
	t.Helper()
	cap := &capture{}
	b := New(Options{Config: conf, Env: s, Logger: slog.New(cap)})
	// New installs the host logger as the rings' sink, process-wide.
	t.Cleanup(func() { logger.SetLogger(nil) })
	b.snitch = func(context.Context, string) error { return nil }
	b.state.Reset(model.New())
	return b, cap
}

// The failures a remote over the public internet has at a base rate:
// ssh's own words for a connection that did not happen or did not last.
const (
	sshConnectTimeout = "running 'ssh': exit status 255: ssh: connect to host backup.example.net port 22: Operation timed out"
	sshBannerTimeout  = "running 'ssh': exit status 255: Fssh_kex_exchange_identification: read: Operation timed out\r; banner exchange: Connection to 192.0.2.10 port 22: Operation timed out"
	sshHostKeyChanged = "running 'ssh': exit status 255: @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@; @    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @; @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@; IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!; Host key for backup.example.net has changed and you have requested strict checking.; Host key verification failed."
)

// seedBehind installs a dataset whose remote is one snapshot behind, so
// a cycle has a transfer to make.
func seedBehind(s *sim.Sim, names ...model.DatasetName) {
	for _, name := range names {
		a, b := simSnap(name, "daily-a", 1000), simSnap(name, "daily-b", 2000)
		s.SeedLocal(name, a, b)
		s.SeedRemote(name, a)
	}
}

var dailyTwo = map[string]int{"daily": 2}

// TestRemoteDownCycleIsAnExpectedFailure: the remote is a host across
// the public internet, and its being unreachable for a cycle is weather
// that calls for nobody — the next retry is a minute away, and a remote
// that stays down withholds the snitch ping and pages that way. So the
// cycle is WARN, and still says what happened.
func TestRemoteDownCycleIsAnExpectedFailure(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	s.SetRemoteDown(true)
	b, cap := simLoggingDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("remote-down cycle logged at %s, want WARN", rec.Level)
	}
	if ok := attr(rec, CycleOKKey); ok.Bool() != false {
		t.Errorf("%s = true on a failed cycle", CycleOKKey)
	}
	if msg := attr(rec, "error.message").String(); !strings.Contains(msg, "Connection refused") {
		t.Errorf("error.message = %q, want ssh's refusal", msg)
	}
	if kind := attr(rec, "error.type").String(); kind != "connection-refused" {
		t.Errorf("error.type = %q, want connection-refused", kind)
	}
}

// TestDatasetConnectionFailuresAreAnExpectedFailure: the same weather
// arriving on per-dataset calls — a resume-token lookup or a transfer
// whose ssh connection timed out — is just as expected, and the cycle
// record carries each dataset's error, not only its name.
func TestDatasetConnectionFailuresAreAnExpectedFailure(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/a", "/b", "/ok")
	s.SetDatasetError("/a", sshConnectTimeout)
	s.SetDatasetError("/b", sshBannerTimeout)
	b, cap := simLoggingDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelWarn {
		t.Errorf("connection-failure cycle logged at %s, want WARN", rec.Level)
	}
	msg := attr(rec, "error.message").String()
	for _, want := range []string{"'/a'", "connect to host backup.example.net", "'/b'", "banner exchange"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error.message = %q, want it to mention %s", msg, want)
		}
	}
	if kind := attr(rec, "error.type").String(); kind != "timeout" {
		t.Errorf("error.type = %q, want timeout", kind)
	}
	if got, ok := attr(rec, CycleFailuresKey).Any().([]string); !ok || len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("%s = %v, want [/a /b]", CycleFailuresKey, attr(rec, CycleFailuresKey))
	}
}

// TestHostKeyChangedCycleIsAFault: ssh exits 255 here too, but it
// reached a host and refused it — someone has to look at the key.
func TestHostKeyChangedCycleIsAFault(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/osm")
	s.SetDatasetError("/osm", sshHostKeyChanged)
	b, cap := simLoggingDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelError {
		t.Errorf("host-key cycle logged at %s, want ERROR", rec.Level)
	}
	if msg := attr(rec, "error.message").String(); !strings.Contains(msg, "Host key verification failed") {
		t.Errorf("error.message = %q, want ssh's refusal", msg)
	}
	if kind := attr(rec, "error.type").String(); kind != "host-key-mismatch" {
		t.Errorf("error.type = %q, want host-key-mismatch", kind)
	}
}

// TestRefreshHostKeyChangedIsAFault: the refresh is where a changed key
// usually bites, through the same logCycle record as a dataset failure.
func TestRefreshHostKeyChangedIsAFault(t *testing.T) {
	local, _ := steadyStateExecutors()
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "-t filesystem", err: errors.New(sshHostKeyChanged)},
	}}
	b, cap := loggingDaemon(t, local, remote)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelError {
		t.Errorf("host-key refresh logged at %s, want ERROR", rec.Level)
	}
	if kind := attr(rec, "error.type").String(); kind != "host-key-mismatch" {
		t.Errorf("error.type = %q, want host-key-mismatch", kind)
	}
}

// TestPersistentReceiveFailureIsAFault: a dataset the remote will not
// receive is ours to fix, whatever the network is doing.
func TestPersistentReceiveFailureIsAFault(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/bad", "/ok")
	s.SetDatasetError("/bad", "invalid backup stream (checksum mismatch)")
	b, cap := simLoggingDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelError {
		t.Errorf("receive-failure cycle logged at %s, want ERROR", rec.Level)
	}
	if msg := attr(rec, "error.message").String(); !strings.Contains(msg, "'/bad'") || !strings.Contains(msg, "checksum mismatch") {
		t.Errorf("error.message = %q, want /bad's receive error", msg)
	}
}

// TestOneFaultAmongConnectionFailuresIsAFault: weather on one dataset
// does not excuse a fault on another — the cycle is only expected when
// every failure in it is.
func TestOneFaultAmongConnectionFailuresIsAFault(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/a", "/bad")
	s.SetDatasetError("/a", sshConnectTimeout)
	s.SetDatasetError("/bad", "invalid backup stream (checksum mismatch)")
	b, cap := simLoggingDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runOneCycle(t, b)

	rec := cap.cycle(t)
	if rec.Level != slog.LevelError {
		t.Errorf("mixed cycle logged at %s, want ERROR", rec.Level)
	}
	if has(rec, "error.type") {
		t.Errorf("error.type = %q, want none: the fault deciding the level is unclassified", attr(rec, "error.type"))
	}
	if msg := attr(rec, "error.message").String(); !strings.Contains(msg, "connect to host") || !strings.Contains(msg, "checksum mismatch") {
		t.Errorf("error.message = %q, want both failures", msg)
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

// snitchLoggingDaemon is a healthy daemon, configured with a snitch,
// whose check-in is the given func and whose records are captured.
func snitchLoggingDaemon(t *testing.T, check func(context.Context, string) error) (*Daemon, *capture) {
	t.Helper()
	local, remote := steadyStateExecutors()
	conf := testConf()
	conf.SnitchID = "test-snitch"
	b := newTestDaemon(conf, local, remote)
	cap := &capture{}
	b.log = slog.New(cap)
	b.snitch = check
	return b, cap
}

// refusedCheckIn is a check-in to an address nothing listens on, made
// with net/http, so the daemon sees the error chain a real transport
// failure has.
func refusedCheckIn(t *testing.T) func(context.Context, string) error {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return func(ctx context.Context, id string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/"+id, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return fmt.Errorf("check-in to a closed server answered %s", resp.Status)
	}
}

// TestFailedSnitchPingIsAnExpectedFailure: Dead Man's Snitch is a
// service across the public internet, and a check-in that does not
// arrive is what pages — so a failed ping, by itself, calls for nobody.
// It is WARN, says why under OpenTelemetry's pair, and stays in the
// journal as the warning incident it already was.
func TestFailedSnitchPingIsAnExpectedFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   func(*testing.T) func(context.Context, string) error
		message string
		kind    string
	}{
		{
			name:    "the connection was refused",
			check:   refusedCheckIn,
			message: "connection refused",
			kind:    "connection-refused",
		},
		{
			name: "the snitch answered other than 2xx",
			check: func(*testing.T) func(context.Context, string) error {
				return func(context.Context, string) error {
					return errors.New("snitch test-snitch: 503 Service Unavailable")
				}
			},
			message: "503 Service Unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, cap := snitchLoggingDaemon(t, tc.check(t))
			runOneCycle(t, b)

			rec, ok, found := cap.find(SnitchOKKey)
			if !found {
				t.Fatalf("no record carried %s", SnitchOKKey)
			}
			if ok.Bool() {
				t.Errorf("%s = true on a failed ping", SnitchOKKey)
			}
			if rec.Level != slog.LevelWarn {
				t.Errorf("failed ping logged at %s, want WARN", rec.Level)
			}
			if msg := attr(rec, "error.message").String(); !strings.Contains(msg, tc.message) {
				t.Errorf("error.message = %q, want it to say %q", msg, tc.message)
			}
			if tc.kind == "" && has(rec, "error.type") {
				t.Errorf("error.type = %q on a failure the chain does not classify", attr(rec, "error.type"))
			}
			if kind := attr(rec, "error.type").String(); tc.kind != "" && kind != tc.kind {
				t.Errorf("error.type = %q, want %q", kind, tc.kind)
			}
			if has(rec, "error") {
				t.Errorf("record carries the legacy error key: %q", attr(rec, "error"))
			}
			if !journalHas(b, history.Warning, "snitch ping failing") {
				t.Errorf("journal = %+v, want a snitch warning incident", b.history.Events())
			}
		})
	}
}

// TestSnitchPingCutShortByShutdownIsNoFailure: a check-in our own stop
// cancels did not fail — nothing about the snitch or the path to it
// went wrong — so it is neither logged nor journaled as a failure.
func TestSnitchPingCutShortByShutdownIsNoFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, cap := snitchLoggingDaemon(t, func(ctx context.Context, _ string) error {
		cancel()
		<-ctx.Done()
		return fmt.Errorf("Get \"https://nosnch.in/test-snitch\": %w", ctx.Err())
	})
	b.wait = func(context.Context, time.Duration) (*syncRequest, error) {
		return nil, context.Canceled
	}
	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if rec, _, found := cap.find(SnitchOKKey); found {
		t.Errorf("a cancelled ping was logged: %s at %s", rec.Message, rec.Level)
	}
	if journalHas(b, history.Warning, "snitch ping failing") {
		t.Errorf("a cancelled ping was journaled: %+v", b.history.Events())
	}
}

func journalHas(b *Daemon, level history.Level, prefix string) bool {
	for _, e := range b.history.Events() {
		if e.Level == level && strings.HasPrefix(e.Message, prefix) {
			return true
		}
	}
	return false
}
