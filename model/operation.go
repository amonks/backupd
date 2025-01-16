package model

import (
	"fmt"
)

type Operation interface {
	String() string
	Apply(*Dataset) (*Dataset, error)
}

var _ Operation = &SnapshotRangeDeletion{}

type SnapshotRangeDeletion struct {
	Location Location
	Start    *Snapshot
	End      *Snapshot
}

func (op *SnapshotRangeDeletion) String() string {
	return fmt.Sprintf("destroy %s %s@%s%%%s",
		op.Location, op.Start.Dataset, op.Start.Name, op.End.Name)
}

func (op *SnapshotRangeDeletion) Apply(ds *Dataset) (*Dataset, error) {
	out := ds.Clone()

	var target *Snapshots
	switch op.Location {
	case Local:
		target = out.Local
	case Remote:
		target = out.Remote
	default:
		return nil, fmt.Errorf("invalid location '%s'", op.Location)
	}

	didStart, didEnd := false, false
	var dels []*Snapshot
	for snap := range target.All() {
		if snap.ID() == op.Start.ID() {
			didStart = true
			dels = append(dels, snap)
			continue
		}
		if !didStart {
			continue
		}

		if snap.ID() == op.End.ID() {
			dels = append(dels, snap)
			didEnd = true
			break
		} else {
			dels = append(dels, snap)
			continue
		}
	}

	if !didStart {
		return nil, fmt.Errorf("bad range: start snapshot does not exist")
	}
	if !didEnd {
		return nil, fmt.Errorf("bad range: end snapshot does not exist")
	}

	for _, del := range dels {
		var dupedels []*Snapshot
		if dupes := target.GetDuplicates(del); len(dupes) > 0 {
			for _, dupe := range dupes {
				dupedels = append(dupedels, dupe)
			}
		}
		for _, dupe := range dupedels {
			target.Del(dupe)
		}
		target.Del(del)
	}

	return out, nil
}

var _ Operation = &SnapshotDeletion{}

type SnapshotDeletion struct {
	Location Location
	Snapshot *Snapshot
}

func (op *SnapshotDeletion) String() string {
	return fmt.Sprintf("destroy %s %s@%s", op.Location, op.Snapshot.Dataset, op.Snapshot.Name)
}

func (op *SnapshotDeletion) Apply(ds *Dataset) (*Dataset, error) {
	out := ds.Clone()

	var target *Snapshots
	switch op.Location {
	case Local:
		target = out.Local
	case Remote:
		target = out.Remote
	default:
		return nil, fmt.Errorf("invalid location '%s'", op.Location)
	}

	if !target.Has(op.Snapshot) {
		return nil, fmt.Errorf("invalid deletion (snapshot not present")
	}

	target.Del(op.Snapshot)
	return out, nil
}

var _ Operation = &InitialSnapshotTransfer{}

type InitialSnapshotTransfer struct {
	Snapshot *Snapshot
}

func (op *InitialSnapshotTransfer) String() string {
	return fmt.Sprintf("transfer initial %s", op.Snapshot)
}

func (op *InitialSnapshotTransfer) Apply(ds *Dataset) (*Dataset, error) {
	if ds.Remote.Len() > 0 {
		return nil, fmt.Errorf("too late for initial transfer of %s, remote already has %d snapshots, including %s",
			op.Snapshot, ds.Remote.Len(), ds.Remote.Newest())
	}

	out := ds.Clone()
	out.Remote.Add(op.Snapshot)

	return out, nil
}

var _ Operation = &SnapshotTransfer{}

type SnapshotTransfer struct {
	Snapshot *Snapshot
}

func (op *SnapshotTransfer) String() string {
	return fmt.Sprintf("transfer %s", op.Snapshot)
}

func (op *SnapshotTransfer) Apply(ds *Dataset) (*Dataset, error) {
	if ds.Remote.Len() > 0 {
		return nil, fmt.Errorf("should use range transfer: %s", op.Snapshot)
	}

	out := ds.Clone()
	out.Remote.Add(op.Snapshot)

	return out, nil
}

var _ Operation = &SnapshotRangeTransfer{}

type SnapshotRangeTransfer struct {
	Start *Snapshot
	End   *Snapshot
}

func (op *SnapshotRangeTransfer) String() string {
	return fmt.Sprintf("transfer range from %s to %s", op.Start, op.End.Name)
}

func (op *SnapshotRangeTransfer) Apply(dataset *Dataset) (*Dataset, error) {
	if op.Start.Eq(op.End) {
		return nil, fmt.Errorf("invalid range (same start and end)")
	}

	if dataset.Remote.Len() == 0 {
		return nil, fmt.Errorf("cannot range-transfer into empty dataset")
	}
	if !op.Start.Eq(dataset.Remote.Newest()) {
		return nil, fmt.Errorf("too late to transfer %s", op.Start)
	}
	if op.Start.CreatedAt >= op.End.CreatedAt {
		return nil, fmt.Errorf("invalid range %s to %s", op.Start, op.End)
	}
	if !dataset.Remote.Has(op.Start) {
		return nil, fmt.Errorf("remote doesn't have range-start %s", op.Start)
	}
	if dataset.Remote.Has(op.End) {
		return nil, fmt.Errorf("remote already has range-end %s", op.Start)
	}
	if !dataset.Local.Has(op.Start) {
		return nil, fmt.Errorf("local doesn't have range-start %s", op.Start)
	}
	if !dataset.Local.Has(op.End) {
		return nil, fmt.Errorf("local doesn't have range-end %s", op.Start)
	}

	out := dataset.Clone()
	out.Remote.Add(op.End)

	return out, nil
}
