// Describe 10 bugs in this code. Not just code smells: specific, actionable
// bugs.

package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Backup struct {
	local, remote *ZFS

	db *sql.DB
	q  *Queries
}

func NewBackup(db *sql.DB, local, remote *ZFS) *Backup {
	return &Backup{
		local:  local,
		remote: remote,

		db: db,
		q:  New(db),
	}
}

func (b *Backup) ObserveDatasets(ctx context.Context) error {
	if err := b.observeLocalDatasets(ctx); err != nil {
		return err
	}
	if err := b.observeRemoteDatasets(ctx); err != nil {
		return err
	}

	return nil
}

func (b *Backup) observeLocalDatasets(ctx context.Context) error {
	now := time.Now().Truncate(time.Second).Unix()

	datasets, err := b.local.GetDatasets()
	if err != nil {
		return fmt.Errorf("get local datasets: %w", err)
	}

	for _, name := range datasets {
		if _, err := b.q.ObserveLocalDataset(ctx, ObserveLocalDatasetParams{
			Name:      name,
			IsOnLocal: True(),
		}); err != nil {
			return err
		}
	}

	if err := b.q.RemoveOlderLocalDatasets(ctx, Int64(now)); err != nil {
		return fmt.Errorf("removing local datasets not seen since this search began (%d): %w", now, err)
	}

	return nil
}

func (b *Backup) observeRemoteDatasets(ctx context.Context) error {
	now := time.Now().Truncate(time.Second).Unix()

	datasets, err := b.remote.GetDatasets()
	if err != nil {
		return fmt.Errorf("get remote datasets: %w", err)
	}

	for _, name := range datasets {
		if _, err := b.q.ObserveRemoteDataset(ctx, ObserveRemoteDatasetParams{
			Name:       name,
			IsOnRemote: True(),
		}); err != nil {
			return err
		}
	}

	if err := b.q.RemoveOlderRemoteDatasets(ctx, Int64(now)); err != nil {
		return fmt.Errorf("removing remote datasets not seen since this search began (%d): %w", now, err)
	}

	return nil
}

func (b *Backup) ObserveSnapshots(ctx context.Context, dataset *Dataset) error {
	if err := b.observeLocalSnapshots(ctx, dataset.Name); err != nil {
		return err
	}
	if IsTrue(dataset.IsOnRemote) {
		if err := b.observeRemoteSnapshots(ctx, dataset.Name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backup) observeLocalSnapshots(ctx context.Context, dataset string) error {
	now := time.Now().Truncate(time.Second).Unix()

	snaps, err := b.local.GetSnapshots(dataset)
	if err != nil {
		return fmt.Errorf("get local snapshots: %w", err)
	}

	for _, snap := range snaps {
		if _, err := b.q.ObserveLocalSnapshot(ctx, ObserveLocalSnapshotParams{
			Dataset:   dataset,
			Name:      snap.Name,
			CreatedAt: snap.CreatedAt,
			IsOnLocal: True(),
		}); err != nil {
			return err
		}
	}

	if err := b.q.RemoveOlderLocalSnapshots(ctx, RemoveOlderLocalSnapshotsParams{
		Dataset:         dataset,
		LocalObservedAt: Int64(now),
	}); err != nil {
		return fmt.Errorf("removing local snapshots not seen since this search began at %d", now)
	}

	return nil
}

func (b *Backup) observeRemoteSnapshots(ctx context.Context, dataset string) error {
	now := time.Now().Truncate(time.Second).Unix()

	snaps, err := b.remote.GetSnapshots(dataset)
	if err != nil {
		return fmt.Errorf("get remote snapshots: %w", err)
	}

	for _, snap := range snaps {
		if _, err := b.q.ObserveRemoteSnapshot(ctx, ObserveRemoteSnapshotParams{
			Dataset:    dataset,
			Name:       snap.Name,
			CreatedAt:  snap.CreatedAt,
			IsOnRemote: True(),
		}); err != nil {
			return err
		}
	}

	if err := b.q.RemoveOlderRemoteSnapshots(ctx, RemoveOlderRemoteSnapshotsParams{
		Dataset:          dataset,
		RemoteObservedAt: Int64(now),
	}); err != nil {
		return fmt.Errorf("removing remote snapshots not seen since this search began at %d", now)
	}

	return nil
}

//go:embed policy.json
var policyJSON []byte
var policy struct {
	Local  map[string]int
	Remote map[string]int
}

func init() {
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		panic(err)
	}
}

// keep, both:
// - "high water mark": latest snapshot on both local and remote
//
// keep, local:
// - the oldest snapshot
// - snapshots matching local's policy
// - snapshots matching remote's policy
//
// keep, remote:
// - the oldest snapshot
// - snapshots matching remote's policy
// - the latest snapshot remote has
func (b *Backup) Prune(ctx context.Context, dataset string) error {
	snapshotsSlice, err := b.q.GetAllSnapshots(ctx, dataset)
	if err != nil {
		return err
	}

	snapshots := NewSnapshots(snapshotsSlice...)

	localDel := NewSnapshots()
	remoteDel := NewSnapshots()

	hasHighWaterMark := false
	hasRemoteLatest := false

	localAccum := map[string]int{}
	remoteAccum := map[string]int{}

	for snapshot := range snapshots.AllDesc() {
		typ := snapshot.Type()

		shouldKeepLocal := false
		shouldKeepRemote := false

		// remote: keep the latest snapshot remote has
		if !hasRemoteLatest {
			if IsTrue(snapshot.IsOnRemote) {
				hasRemoteLatest = true

				shouldKeepRemote = true
			}
		}

		// both: keep high water mark
		if !hasHighWaterMark {
			if IsTrue(snapshot.IsOnLocal) && IsTrue(snapshot.IsOnRemote) {
				hasHighWaterMark = true

				shouldKeepLocal = true
				shouldKeepRemote = true
			}
		}

		// local: keep snapshots matching local's policy
		if IsTrue(snapshot.IsOnLocal) {
			if localTarget, hasLocalPolicy := policy.Local[typ]; hasLocalPolicy && localAccum[typ] < localTarget {
				localAccum[typ]++

				shouldKeepLocal = true
			}
		}

		if remoteTarget, hasRemotePolicy := policy.Remote[typ]; hasRemotePolicy && remoteAccum[typ] < remoteTarget {
			// remote or local: keep snapshots matching remote's policy
			if IsTrue(snapshot.IsOnRemote) {
				remoteAccum[typ]++

				shouldKeepRemote = true
			} else if IsTrue(snapshot.IsOnLocal) {
				remoteAccum[typ]++

				shouldKeepLocal = true
			}
		}

		if IsTrue(snapshot.IsOnLocal) && !shouldKeepLocal {
			localDel.Add(snapshot)
		}
		if IsTrue(snapshot.IsOnRemote) && !shouldKeepRemote {
			remoteDel.Add(snapshot)
		}
	}

	if oldestLocal := snapshots.OldestLocal(); oldestLocal != nil {
		localDel.Del(oldestLocal.Name)
	}
	if oldestRemote := snapshots.OldestRemote(); oldestRemote != nil {
		remoteDel.Del(oldestRemote.Name)
	}

	localDeletionRanges, err := b.extractRangeDeletions(ctx, snapshots.Local(), localDel)
	if err != nil {
		return err
	}
	if err := b.deleteSnapshotRanges(ctx, localDeletionRanges, &destroyer{
		destroyOne:   b.local.DestroySnapshot,
		destroyRange: b.local.DestroySnapshotRange,
		observe: func(name string) error {
			_, err := b.q.ObserveLocalSnapshot(ctx, ObserveLocalSnapshotParams{
				Dataset:   dataset,
				Name:      name,
				IsOnLocal: False(),
			})
			return err
		},
	}); err != nil {
		return err
	}

	remoteDeletionRanges, err := b.extractRangeDeletions(ctx, snapshots.Remote(), remoteDel)
	if err != nil {
		return err
	}
	if err := b.deleteSnapshotRanges(ctx, remoteDeletionRanges, &destroyer{
		destroyOne:   b.remote.DestroySnapshot,
		destroyRange: b.remote.DestroySnapshotRange,
		observe: func(name string) error {
			_, err := b.q.ObserveRemoteSnapshot(ctx, ObserveRemoteSnapshotParams{
				Dataset:    dataset,
				Name:       name,
				IsOnRemote: False(),
			})
			return err
		},
	}); err != nil {
		return err
	}

	return nil
}

func (b *Backup) extractRangeDeletions(ctx context.Context, allSnapshots iter.Seq[*Snapshot], toDelete *Snapshots) ([][]*Snapshot, error) {
	var ranges [][]*Snapshot
	var thisRange []*Snapshot
	for candidate := range allSnapshots {
		if toDelete.Has(candidate) {
			thisRange = append(thisRange, candidate)
		} else {
			if len(thisRange) > 0 {
				ranges = append(ranges, thisRange)
				thisRange = nil
			}
		}
	}
	if len(thisRange) > 0 {
		ranges = append(ranges, thisRange)
	}
	return ranges, nil
}

type destroyer struct {
	destroyOne   func(dataset, name string) error
	destroyRange func(dataset, first, last string) error
	observe      func(name string) error
}

func (b *Backup) deleteSnapshotRanges(ctx context.Context, ranges [][]*Snapshot, d *destroyer) error {
	for _, thisRange := range ranges {
		if len(thisRange) == 0 {
			panic("bad range")
		} else if len(thisRange) == 1 {
			if err := d.destroyOne(thisRange[0].Dataset, thisRange[0].Name); err != nil {
				return fmt.Errorf("destroying range %s: %w", thisRange[0], err)
			}
			continue
		}

		start, end := thisRange[0], thisRange[len(thisRange)-1]
		if err := d.destroyRange(start.Dataset, start.Name, end.Name); err != nil {
			return fmt.Errorf("destroying range %s@%s%%%s: %w", start.Dataset, start.Name, end.Name, err)
		}
		for _, snap := range thisRange {
			if err := d.observe(snap.Name); err != nil {
				return fmt.Errorf("marking %s as destroyed: %w", snap, err)
			}
		}
	}

	return nil
}
