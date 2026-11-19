package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"monks.co/backupd/model"
)

// Tracer records the daemon's units of work — sync cycles, transfers,
// snapshot deletions, single-dataset syncs, snapshots taken — as spans.
// It is how a host's tracing sees the daemon's work without this module
// importing anything of the host's: the daemon names the work and says
// how it ended, and the host decides what a span becomes. See
// Options.Tracer.
type Tracer interface {
	// Start starts a span named name, a child of whatever span ctx
	// carries. The returned context replaces ctx for the work: spans
	// started under it nest beneath this one, and a host's outbound
	// calls made with it parent to it.
	//
	// Start is the only reader of the context a sync request hands
	// over (Daemon.TriggerSync). A requested sync's span starts under
	// the request's context, values and all — an HTTP host's request
	// carries its authority and its own event — without its
	// cancellation; the work then runs under Run's deadline and
	// cancellation, seeing the values of the context Start returned
	// ahead of Run's. So what of the request reaches the work is what
	// Start passes on, and a host that wants a narrower parent may wrap
	// Start, handing on only the request's trace.
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is one unit of work in flight.
type Span interface {
	// Set records an attribute, replacing an earlier value under the
	// same key.
	Set(key string, value any)

	// End ends the span, exactly once. A nil err is success, and level
	// is ignored. An err that is a cancellation (errors.Is(err,
	// context.Canceled)) is not a failure: the work was cut short by its
	// context ending — the daemon's stop, or a host's deadline on Run,
	// which the daemon reports as a cancellation too. Any other err is a
	// failure at level: slog.LevelError a fault, something someone has
	// to fix; slog.LevelWarn an expected failure, weather that matters
	// only if it keeps happening.
	//
	// A failure's err is the one that decided its level: what a host
	// may classify where the daemon Set no error.type. A span whose
	// failure is several — a cycle in which more than one dataset
	// failed — Sets error.message to all of them, so a host keeps an
	// error.message or error.type the daemon Set, and takes neither
	// from err in its place.
	End(level slog.Level, err error)
}

// startSpan starts a span on the host's Tracer, or, with none, on the
// default one writing a record per span to the daemon's logger. The
// default is built per call rather than once in New so it always
// writes where b.log points.
func (b *Daemon) startSpan(ctx context.Context, name string) (context.Context, Span) {
	if b.tracer != nil {
		return b.tracer.Start(ctx, name)
	}
	return logTracer{log: b.log}.Start(ctx, name)
}

// logTracer is the Tracer a daemon uses when the host supplies none:
// one structured record per ended span, so the standalone daemon keeps
// a record of every unit of work, and a host with a Tracer of its own
// gets spans without a duplicate record of each.
type logTracer struct {
	log *slog.Logger
}

func (t logTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	return ctx, &logSpan{log: t.log, name: name, started: time.Now()}
}

// logSpan collects a span's attributes until End writes them: the
// message is the span's name, the attributes are those Set, in the
// order first set, then StatusKey and DurationKey, and on a failure
// error.message, End's err, unless the daemon Set one. A failure's
// error.type is the daemon's to Set where it classified one. The record
// is INFO for success and cancellation, and the level End was given for
// a failure.
type logSpan struct {
	log     *slog.Logger
	name    string
	started time.Time

	mu    sync.Mutex
	attrs []slog.Attr
	ended bool
}

func (s *logSpan) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.attrs, func(a slog.Attr) bool { return a.Key == key }); i >= 0 {
		s.attrs[i].Value = slog.AnyValue(value)
		return
	}
	s.attrs = append(s.attrs, slog.Any(key, value))
}

func (s *logSpan) End(level slog.Level, err error) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	attrs := slices.Clone(s.attrs)
	s.mu.Unlock()

	status, recordLevel := statusOK, slog.LevelInfo
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		status = statusCancelled
	default:
		status, recordLevel = statusError, level
	}
	attrs = append(attrs,
		slog.String(StatusKey, status),
		slog.Int64(DurationKey, time.Since(s.started).Milliseconds()))
	if status == statusError && !slices.ContainsFunc(attrs, func(a slog.Attr) bool { return a.Key == errorMessageKey }) {
		attrs = append(attrs, slog.String(errorMessageKey, err.Error()))
	}
	s.log.LogAttrs(context.Background(), recordLevel, s.name, attrs...)
}

// The values the default tracer writes under StatusKey.
const (
	statusOK        = "ok"
	statusError     = "error"
	statusCancelled = "cancelled"
)

// endClassified ends a span whose failure is read the way a cycle's
// failures are (classifyFailure): WARN when ssh failed to reach the
// remote, ERROR otherwise, with the error.type where ssh's words say
// it. Once ctx has ended the work is no outcome, whatever error it
// returned — a transfer killed by the daemon's stop fails in the words
// of whichever half of the pipe died first — so it ends as the
// cancellation, as syncDataset and Run already treat it.
func endClassified(ctx context.Context, span Span, err error) {
	switch {
	case err == nil:
		span.End(slog.LevelInfo, nil)
	case ctx.Err() != nil:
		span.End(slog.LevelInfo, cancellation(ctx))
	default:
		kind, expected := classifyFailure(err.Error())
		if kind != "" {
			span.Set(errorTypeKey, kind)
		}
		level := slog.LevelError
		if expected {
			level = slog.LevelWarn
		}
		span.End(level, err)
	}
}

// cancellation is the error a span ends with when its context ended:
// one that is context.Canceled, which Span.End reads as no failure,
// whatever ended ctx. A host may run Run under a deadline, whose
// context ends with context.DeadlineExceeded; the work that deadline
// cut short failed no more than work the daemon's stop cuts short, so
// it says Canceled too, with the deadline beside it.
func cancellation(ctx context.Context) error {
	err := ctx.Err()
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %w", context.Canceled, err)
}

// The transfer kinds a TransferSpan carries under TransferKindKey, named
// for what crosses the wire, since that is what decides a transfer's
// size and duration:
//
//   - initial: InitialSnapshotTransfer, a full stream of a dataset's
//     first snapshot into a remote that does not have the dataset yet;
//     the receive creates it. Its size is the dataset's whole contents.
//   - incremental: SnapshotRangeTransfer, `zfs send -i` from the
//     remote's newest snapshot to a newer local one — the steady state,
//     which sends only the blocks that changed between them.
//   - full: SnapshotTransfer, a full stream of one snapshot received
//     with -F into a remote dataset that holds no snapshots. The
//     planner never emits it, but env executes it, and a full stream is
//     neither a dataset's first nor a delta, so it is named for what it
//     sends rather than folded into either.
//   - resume: the remainder of an interrupted transfer, sent from its
//     receive resume token (handleIncompleteTransfer). It is not a plan
//     step, and its size is whatever the interruption left unsent.
const (
	TransferInitial     = "initial"
	TransferIncremental = "incremental"
	TransferFull        = "full"
	TransferResume      = "resume"
)

// transferKind names a transfer step's kind, and reports false for an
// operation that is not a transfer.
func transferKind(op model.Operation) (string, bool) {
	switch op.(type) {
	case *model.InitialSnapshotTransfer:
		return TransferInitial, true
	case *model.SnapshotRangeTransfer:
		return TransferIncremental, true
	case *model.SnapshotTransfer:
		return TransferFull, true
	}
	return "", false
}

// startStep starts the span of a plan step about to execute, and
// returns the context the step runs under and the function that ends
// the span with the step's outcome. A transfer's span is a
// TransferSpan, its meter reset now and read at the end; a deletion's
// is a DestroySpan, with where it deletes and, once it has succeeded,
// how many snapshots it destroyed. The count is taken now, from the
// dataset's in-memory inventory as the step finds it, which earlier
// steps have already advanced, so a range counts what is in it; a
// deletion zfs refuses destroyed nothing, and records no count.
func (b *Daemon) startStep(ctx context.Context, dataset model.DatasetName, op model.Operation) (context.Context, func(error)) {
	if kind, ok := transferKind(op); ok {
		ctx, span := b.startSpan(ctx, TransferSpan)
		span.Set(DatasetKey, dataset.String())
		span.Set(TransferKindKey, kind)
		b.meter.begin()
		return ctx, func(err error) {
			b.measureTransfer(span)
			endClassified(ctx, span, err)
		}
	}
	ctx, span := b.startSpan(ctx, DestroySpan)
	span.Set(DatasetKey, dataset.String())
	var loc model.Location
	switch op := op.(type) {
	case *model.SnapshotDeletion:
		loc = op.Location
	case *model.SnapshotRangeDeletion:
		loc = op.Location
	}
	switch loc {
	case model.Local:
		span.Set(DestroyLocationKey, "local")
	case model.Remote:
		span.Set(DestroyLocationKey, "remote")
	}
	destroys, counted := 0, false
	if ds := b.state.Deref().GetDataset(dataset); ds != nil && ds.Current != nil {
		if after, err := op.Apply(ds.Current); err == nil {
			destroys, counted = side(ds.Current, loc).Len()-side(after, loc).Len(), true
		}
	}
	return ctx, func(err error) {
		if err == nil && counted {
			span.Set(DestroySnapshotsKey, destroys)
		}
		endClassified(ctx, span, err)
	}
}

// side returns one location's snapshots of an inventory.
func side(inv *model.SnapshotInventory, loc model.Location) *model.Snapshots {
	switch loc {
	case model.Local:
		return inv.Local
	case model.Remote:
		return inv.Remote
	}
	return nil
}

// measureTransfer sets a transfer span's byte count and rate from the
// meter: TransferSentBytesKey always, zero when nothing moved, and
// TransferBytesPerSecondKey when bytes moved over a measurable time.
func (b *Daemon) measureTransfer(span Span) {
	sent, timed, over := b.meter.read()
	span.Set(TransferSentBytesKey, sent)
	if timed > 0 && over > 0 {
		span.Set(TransferBytesPerSecondKey, int64(float64(timed)*float64(time.Second)/float64(over)))
	}
}

// transferMeter measures the transfer in flight from the progress
// callback the environment reports to: how many bytes went through the
// pipe, and how fast. The rate is the bytes moved between the pipe's
// first report of bytes moved and its last, over the time between
// them. That is the pipe's own rate, the link's, with both ends cut
// the same way: the dry-run that sizes the send and the ssh
// connection's setup come before any byte moves, and the receive's
// finalization after the last, and neither counts against it. The
// bytes the first report counts moved before its clock started, from
// whenever the pipe opened, so they count toward what was sent and not
// toward the rate.
//
// One meter serves every transfer because transfers never overlap (see
// Daemon.meter); the callback runs on the pipe's goroutine while the
// sync loop reads, which is what the mutex is for.
type transferMeter struct {
	mu sync.Mutex
	// done is what earlier pipes of the same transfer moved, and pipe
	// what the current one has: a resume that aborts partial state and
	// retries runs a second pipe, whose count starts over. timed is
	// what the earlier pipes moved between their first and last
	// reports, and busy the time between those, so the time between
	// pipes — the abort and the reconnect — is no pipe's. first and
	// last are when the current pipe first and last reported bytes
	// moved, and opened the count its first report said.
	done, pipe  int64
	timed       int64
	busy        time.Duration
	first, last time.Time
	opened      int64
}

// begin starts measuring a new transfer.
func (m *transferMeter) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done, m.pipe, m.timed, m.busy = 0, 0, 0, 0
	m.first, m.last, m.opened = time.Time{}, time.Time{}, 0
}

// nextPipe keeps what the current pipe moved and starts counting
// another for the same transfer.
func (m *transferMeter) nextPipe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done += m.pipe
	m.timed += m.pipe - m.opened
	m.busy += m.last.Sub(m.first)
	m.pipe, m.first, m.last, m.opened = 0, time.Time{}, time.Time{}, 0
}

// progress is the environment's progress callback: sent is the pipe's
// cumulative count. A report of nothing moved — a paced transfer's
// opening report — starts no clock.
func (m *transferMeter) progress(sent, _ int64) {
	if sent <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.pipe = sent
	if m.first.IsZero() {
		m.first, m.opened = now, sent
	}
	m.last = now
}

// read returns the bytes the transfer's pipes moved in all (sent), and
// what they moved between each one's first report of bytes moved and
// its last (timed) over the time between those (over): zero when no
// pipe has reported twice.
func (m *transferMeter) read() (sent, timed int64, over time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done + m.pipe, m.timed + m.pipe - m.opened, m.busy + m.last.Sub(m.first)
}
