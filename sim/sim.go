// Package sim is an in-memory implementation of env.Interface: a pair
// of ZFS pools (local and remote) with snapshots, transfers, resume
// tokens, and fault injection. It exists so the complete daemon — sync
// loop, planner, resume handling, dashboard — can run and be tested in
// a controlled environment, without root, ZFS, or a network.
//
// Fidelity notes: the error strings for "dataset does not exist",
// "contains partially-complete state", "no longer exists", and "out of
// space" match the substrings the daemon's recovery paths dispatch on,
// because that dispatch is exactly what the simulator is for testing.
// Transfers can be paced (bytes/sec) to exercise progress reporting,
// or instant for property tests.
package sim

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	gosync "sync"
	"time"

	"monks.co/backupd/env"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

type Sim struct {
	mu     gosync.Mutex
	local  map[model.DatasetName]*dataset
	remote map[model.DatasetName]*dataset

	// Now is the clock used for snapshot creation; injectable for
	// deterministic tests.
	Now func() time.Time

	// Rate paces transfers in bytes per second; 0 transfers instantly.
	Rate int64

	// OnProgress, when set, receives cumulative progress for the
	// in-flight transfer, mirroring env.Env.OnProgress.
	OnProgress func(sent, total int64)

	// Fault injection.
	remoteDown    bool
	remoteFull    bool
	interruptFrac float64 // 0 = don't interrupt; else fraction of the next transfer to complete

	mutations []string
}

type dataset struct {
	snaps *model.Snapshots
	// resume state (remote datasets only): a partially-received
	// transfer that can be resumed.
	resumeToken string
	resumeEnd   *model.Snapshot // the snapshot the interrupted transfer was delivering
	resumeSent  int64           // bytes already received
}

var _ env.Interface = &Sim{}

func New() *Sim {
	return &Sim{
		local:  map[model.DatasetName]*dataset{},
		remote: map[model.DatasetName]*dataset{},
		Now:    time.Now,
	}
}

// SeedLocal and SeedRemote install a dataset with the given snapshots,
// creating it if needed. Snapshots may be nil to create an empty
// dataset.
func (s *Sim) SeedLocal(name model.DatasetName, snaps ...*model.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed(s.local, name, snaps)
}

func (s *Sim) SeedRemote(name model.DatasetName, snaps ...*model.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed(s.remote, name, snaps)
}

func (s *Sim) seed(pool map[model.DatasetName]*dataset, name model.DatasetName, snaps []*model.Snapshot) {
	ds, ok := pool[name]
	if !ok {
		ds = &dataset{snaps: model.NewSnapshots()}
		pool[name] = ds
	}
	for _, snap := range snaps {
		ds.snaps.Add(snap)
	}
}

// SeedInterruptedTransfer installs partial receive state on the remote,
// as if a transfer of `end` into `name` had been cut off.
func (s *Sim) SeedInterruptedTransfer(name model.DatasetName, end *model.Snapshot, sent int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed(s.remote, name, nil)
	ds := s.remote[name]
	ds.resumeToken = fmt.Sprintf("token-%s-%s", name, end.Name)
	ds.resumeEnd = end
	ds.resumeSent = sent
}

// SetRemoteDown makes every remote operation fail, simulating an
// unreachable backup host.
func (s *Sim) SetRemoteDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remoteDown = down
}

// SetRemoteFull makes receives fail with an out-of-space error while
// leaving deletions working, simulating a full backup pool.
func (s *Sim) SetRemoteFull(full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remoteFull = full
}

// InterruptNextTransfer makes the next transfer stop after the given
// fraction, leaving resume state behind, as if the connection dropped.
func (s *Sim) InterruptNextTransfer(frac float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptFrac = frac
}

// Mutations returns a log of every state-changing operation performed,
// and ResetMutations clears it. Tests use this to assert convergence
// ("a second cycle does nothing").
func (s *Sim) Mutations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.mutations))
	copy(out, s.mutations)
	return out
}

func (s *Sim) ResetMutations() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutations = nil
}

func (s *Sim) record(format string, args ...any) {
	s.mutations = append(s.mutations, fmt.Sprintf(format, args...))
}

// LocalInventory returns the sim's ground truth for one dataset, for
// asserting against the planner's target.
func (s *Sim) Inventory(name model.DatasetName) *model.SnapshotInventory {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := &model.SnapshotInventory{Local: model.NewSnapshots(), Remote: model.NewSnapshots()}
	if ds, ok := s.local[name]; ok {
		inv.Local = ds.snaps.Clone()
	}
	if ds, ok := s.remote[name]; ok {
		inv.Remote = ds.snaps.Clone()
	}
	return inv
}

func (s *Sim) listDatasets(pool map[model.DatasetName]*dataset) []env.DatasetInfo {
	names := make([]model.DatasetName, 0, len(pool))
	for name := range pool {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]env.DatasetInfo, len(names))
	for i, name := range names {
		ds := pool[name]
		var used, logical int64
		for snap := range ds.snaps.All() {
			used += snap.LogicalReferenced
		}
		if newest := ds.snaps.Newest(); newest != nil {
			logical = newest.LogicalReferenced
		}
		out[i] = env.DatasetInfo{
			Name: name,
			Size: &model.DatasetSize{Used: used, LogicalReferenced: logical},
		}
	}
	return out
}

func (s *Sim) LocalDatasets(l *logger.Logger) ([]env.DatasetInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listDatasets(s.local), nil
}

func (s *Sim) RemoteDatasets(l *logger.Logger) ([]env.DatasetInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteDown {
		return nil, fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}
	return s.listDatasets(s.remote), nil
}

func (s *Sim) snapshots(pool map[model.DatasetName]*dataset, name model.DatasetName) ([]*model.Snapshot, error) {
	ds, ok := pool[name]
	if !ok {
		return nil, fmt.Errorf("cannot open '%s': dataset does not exist", name)
	}
	var out []*model.Snapshot
	for snap := range ds.snaps.All() {
		out = append(out, snap)
	}
	return out, nil
}

func (s *Sim) LocalSnapshots(l *logger.Logger, name model.DatasetName) ([]*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshots(s.local, name)
}

func (s *Sim) RemoteSnapshots(l *logger.Logger, name model.DatasetName) ([]*model.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteDown {
		return nil, fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}
	return s.snapshots(s.remote, name)
}

func (s *Sim) RemoteResumeToken(l *logger.Logger, name model.DatasetName) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteDown {
		return "", fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}
	ds, ok := s.remote[name]
	if !ok {
		return "", fmt.Errorf("cannot open '%s': dataset does not exist", name)
	}
	return ds.resumeToken, nil
}

func (s *Sim) AbortRemoteResumable(l *logger.Logger, name model.DatasetName) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteDown {
		return fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}
	ds, ok := s.remote[name]
	if !ok {
		return fmt.Errorf("cannot open '%s': dataset does not exist", name)
	}
	s.record("abort resumable %s", name)
	ds.resumeToken = ""
	ds.resumeEnd = nil
	ds.resumeSent = 0
	return nil
}

func (s *Sim) Resume(ctx context.Context, l *logger.Logger, name model.DatasetName, token string) error {
	s.mu.Lock()
	if s.remoteDown {
		s.mu.Unlock()
		return fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}
	ds, ok := s.remote[name]
	if !ok || ds.resumeToken == "" || ds.resumeToken != token {
		s.mu.Unlock()
		return fmt.Errorf("resume token does not match")
	}
	end := ds.resumeEnd
	sent := ds.resumeSent
	localDS, hasLocal := s.local[name]
	if !hasLocal || !localDS.snaps.Has(end) {
		s.mu.Unlock()
		return fmt.Errorf("cannot resume send: '%s@%s' used in the initial send no longer exists", name, end.Name)
	}
	if s.remoteFull {
		s.mu.Unlock()
		return fmt.Errorf("'to' command error: exit status 1 (cannot receive resume stream: out of space)")
	}
	s.mu.Unlock()

	total := transferSize(end)
	if err := s.pace(ctx, l, sent, total); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.record("resume %s@%s", name, end.Name)
	ds.snaps.Add(end)
	ds.resumeToken = ""
	ds.resumeEnd = nil
	ds.resumeSent = 0
	return nil
}

func (s *Sim) Apply(ctx context.Context, l *logger.Logger, op model.Operation) error {
	if step, ok := op.(*model.PlanStep); ok {
		op = step.Operation
	}
	switch op := op.(type) {
	case *model.SnapshotDeletion:
		return s.deleteRange(op.Location, op.Snapshot.Dataset, op.Snapshot, op.Snapshot)
	case *model.SnapshotRangeDeletion:
		return s.deleteRange(op.Location, op.Start.Dataset, op.Start, op.End)
	case *model.InitialSnapshotTransfer:
		return s.transfer(ctx, l, op.Snapshot.Dataset, nil, op.Snapshot)
	case *model.SnapshotTransfer:
		return s.transfer(ctx, l, op.Snapshot.Dataset, nil, op.Snapshot)
	case *model.SnapshotRangeTransfer:
		return s.transfer(ctx, l, op.End.Dataset, op.Start, op.End)
	default:
		return fmt.Errorf("%s is not supported", op)
	}
}

func (s *Sim) deleteRange(loc model.Location, name model.DatasetName, start, end *model.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pool map[model.DatasetName]*dataset
	switch loc {
	case model.Local:
		pool = s.local
	case model.Remote:
		if s.remoteDown {
			return fmt.Errorf("running 'ssh': exit status 255: connection refused")
		}
		pool = s.remote
	default:
		return fmt.Errorf("invalid location '%s'", loc)
	}

	ds, ok := pool[name]
	if !ok {
		return fmt.Errorf("cannot open '%s': dataset does not exist", name)
	}
	if !ds.snaps.Has(start) || !ds.snaps.Has(end) {
		return fmt.Errorf("could not find matching snapshot range %s%%%s", start.Name, end.Name)
	}

	// Destroy everything from start to end inclusive, in snapshot
	// order, like `zfs destroy ds@first%last`.
	var doomed []*model.Snapshot
	inRange := false
	for snap := range ds.snaps.All() {
		if snap.ID() == start.ID() {
			inRange = true
		}
		if inRange {
			doomed = append(doomed, snap)
		}
		if snap.ID() == end.ID() {
			break
		}
	}
	for _, snap := range doomed {
		ds.snaps.Del(snap)
		s.record("destroy %s %s@%s", loc, name, snap.Name)
	}
	return nil
}

func transferSize(snap *model.Snapshot) int64 {
	if snap.LogicalReferenced > 0 {
		return snap.LogicalReferenced
	}
	return 1 << 20
}

// transfer moves `end` into the remote dataset. A nil `start` is an
// initial (full) transfer; otherwise the remote's newest snapshot must
// be `start`, mirroring an incremental receive.
func (s *Sim) transfer(ctx context.Context, l *logger.Logger, name model.DatasetName, start, end *model.Snapshot) error {
	s.mu.Lock()
	if s.remoteDown {
		s.mu.Unlock()
		return fmt.Errorf("running 'ssh': exit status 255: connection refused")
	}

	localDS, ok := s.local[name]
	if !ok || !localDS.snaps.Has(end) {
		s.mu.Unlock()
		return fmt.Errorf("cannot open '%s@%s': snapshot does not exist locally", name, end.Name)
	}

	if start == nil {
		// Initial transfer: like `zfs create -p` + receive, parents
		// spring into existence remotely.
		if remoteDS, has := s.remote[name]; has && remoteDS.snaps.Len() > 0 {
			s.mu.Unlock()
			return fmt.Errorf("cannot receive new filesystem stream: destination '%s' exists", name)
		}
	} else {
		remoteDS, has := s.remote[name]
		if !has {
			s.mu.Unlock()
			return fmt.Errorf("cannot open '%s': dataset does not exist", name)
		}
		if remoteDS.resumeToken != "" {
			s.mu.Unlock()
			return fmt.Errorf("cannot receive incremental stream: destination %s contains partially-complete state from \"zfs receive -s\"", name)
		}
		if newest := remoteDS.snaps.Newest(); newest == nil || newest.ID() != start.ID() {
			s.mu.Unlock()
			return fmt.Errorf("cannot receive incremental stream: most recent snapshot of %s does not match incremental source", name)
		}
	}

	if s.remoteFull {
		s.mu.Unlock()
		return fmt.Errorf("'to' command error: exit status 1 (cannot receive: out of space)")
	}

	interrupt := s.interruptFrac
	s.interruptFrac = 0
	s.mu.Unlock()

	total := transferSize(end)
	if interrupt > 0 {
		partial := int64(float64(total) * interrupt)
		if err := s.pace(ctx, l, 0, partial); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.seed(s.remote, name, nil)
		ds := s.remote[name]
		ds.resumeToken = fmt.Sprintf("token-%s-%s", name, end.Name)
		ds.resumeEnd = end
		ds.resumeSent = partial
		s.record("interrupted transfer %s@%s", name, end.Name)
		return fmt.Errorf("'from' command error: exit status 1 (connection reset by peer)")
	}

	if err := s.pace(ctx, l, 0, total); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Create the dataset (and, on initial transfer, its parents, like
	// the real env's `zfs create -p`).
	if start == nil {
		for parent := path.Dir(name.Path()); parent != "." && parent != "/" && parent != ""; parent = path.Dir(parent) {
			s.seed(s.remote, model.DatasetName(parent), nil)
		}
	}
	s.seed(s.remote, name, nil)
	s.remote[name].snaps.Add(end)
	if start == nil {
		s.record("transfer initial %s@%s", name, end.Name)
	} else {
		s.record("transfer %s@%s..%s@%s", name, start.Name, name, end.Name)
	}
	return nil
}

// pace simulates the wall-clock cost of moving bytes, reporting
// progress along the way. With Rate zero it just reports completion.
func (s *Sim) pace(ctx context.Context, l *logger.Logger, sent, total int64) error {
	report := func(n int64) {
		if s.OnProgress != nil {
			s.OnProgress(n, total)
		}
	}
	if s.Rate <= 0 {
		report(total)
		return nil
	}
	report(sent)
	const tick = 100 * time.Millisecond
	perTick := max(int64(float64(s.Rate)*tick.Seconds()), 1)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for sent < total {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			sent = min(sent+perTick, total)
			report(sent)
		}
	}
	return nil
}

func (s *Sim) Snapshot(ctx context.Context, l *logger.Logger, root, periodicity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	name := fmt.Sprintf("%s-%s", periodicity, now.Format("2006-01-02-15:04:05"))
	for dsName, ds := range s.local {
		if ds.snaps.Has(&model.Snapshot{Dataset: dsName, Name: name}) {
			continue
		}
		var logical int64 = 1 << 20
		if newest := ds.snaps.Newest(); newest != nil && newest.LogicalReferenced > 0 {
			logical = newest.LogicalReferenced
		}
		ds.snaps.Add(&model.Snapshot{
			Dataset:           dsName,
			Name:              name,
			CreatedAt:         now.Unix(),
			LogicalReferenced: logical,
		})
		s.record("snapshot %s@%s", dsName, name)
	}
	return nil
}

// String summarizes the sim state, for debugging failed tests.
func (s *Sim) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	describe := func(label string, pool map[model.DatasetName]*dataset) {
		names := make([]model.DatasetName, 0, len(pool))
		for name := range pool {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fmt.Fprintf(&out, "%s %s:", label, name)
			for snap := range pool[name].snaps.All() {
				fmt.Fprintf(&out, " %s", snap.Name)
			}
			if pool[name].resumeToken != "" {
				fmt.Fprintf(&out, " [resume %s]", pool[name].resumeToken)
			}
			fmt.Fprintln(&out)
		}
	}
	describe("local", s.local)
	describe("remote", s.remote)
	return out.String()
}
