package model

import (
	"sort"

	"monks.co/backupd/logger"
)

type Model struct {
	Datasets map[DatasetName]*Dataset
}

func New() *Model {
	return &Model{
		Datasets: make(map[DatasetName]*Dataset),
	}
}

func (model *Model) Clone() *Model {
	out := New()
	if model != nil {
		for k, ds := range model.Datasets {
			out.Datasets[k] = ds.Clone()
		}
	}
	return out
}

func (model *Model) GetDataset(name DatasetName) *Dataset {
	return model.Datasets[name]
}

func (model *Model) ListDatasets() []DatasetName {
	var names []DatasetName
	for name := range model.Datasets {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := names[i], names[j]
		if len(a) == len(b) {
			return a < b
		}
		return len(a) < len(b)
	})
	return names
}

func (model *Model) SetPlan(dataset DatasetName, plan *Plan) {
	ds := model.GetDataset(dataset)
	if ds == nil {
		panic("no such dataset")
	}
	ds.Plan = plan
}

func ReplaceDataset(name DatasetName, dataset *Dataset) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		out.Datasets[name] = dataset
		return out
	}
}

// AddLocalDataset records a read of name's local side: its snapshots,
// and its size when the read came from a listing that carries one.
func AddLocalDataset(name DatasetName, snapshots []*Snapshot, size *DatasetSize) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		ds := out.ensure(name)
		ds.Current.Local = NewSnapshots(snapshots...)
		ds.LocalKnown = true
		if size != nil {
			ds.Metrics.LocalSize = *size
			ds.Metrics.HasLocal = true
		}
		return out
	}
}

// AddRemoteDataset records a read of name's remote side, as
// AddLocalDataset does the local one. No snapshots is a read too: the
// remote holds none.
func AddRemoteDataset(name DatasetName, snapshots []*Snapshot, size *DatasetSize) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		ds := out.ensure(name)
		ds.Current.Remote = NewSnapshots(snapshots...)
		ds.RemoteKnown = true
		if size != nil {
			ds.Metrics.RemoteSize = *size
			ds.Metrics.HasRemote = true
		}
		return out
	}
}

// Listed records that a listing of loc named every dataset that side
// holds, each with all its snapshots. A dataset it did not name holds
// nothing there, so every dataset in the model has loc known: the ones
// the listing named were marked as they were added, and the rest are
// known to be empty.
func Listed(loc Location) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		for _, ds := range out.Datasets {
			if ds.Current == nil {
				ds.Current = &SnapshotInventory{Local: NewSnapshots(), Remote: NewSnapshots()}
			}
			switch loc {
			case Local:
				ds.LocalKnown = true
			case Remote:
				ds.RemoteKnown = true
			}
		}
		return out
	}
}

// KeepRemote carries prev's remote side into a model whose remote has
// not been read: every dataset whose remote prev had read keeps that
// inventory, and its size, until a read of its own replaces it. A
// remote listing that has not returned, or that failed, is not news
// that the remote emptied, and backupd is the remote's only writer, so
// what it last read there is still what is there. A dataset prev never
// read the remote of stays unread.
func KeepRemote(prev *Model) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		if prev == nil {
			return out
		}
		for name, was := range prev.Datasets {
			if !was.RemoteKnown || was.Current == nil {
				continue
			}
			ds := out.ensure(name)
			ds.Current.Remote = was.Current.Remote.Clone()
			ds.RemoteKnown = true
			ds.Metrics.RemoteSize = was.Metrics.RemoteSize
			ds.Metrics.HasRemote = was.Metrics.HasRemote
		}
		return out
	}
}

// ReplaceRemote publishes a whole remote listing, read into a model of
// its own, over a model's remote side: each dataset the listing named
// takes the listing's snapshots and size, and every other dataset holds
// nothing on the remote, since the listing named all it has. The local
// side is untouched, so a local read that landed while the remote was
// listed stands. A dataset the listing named that the model lacks is
// added with its local side as the model has it: unread, until a
// Listed(Local) says otherwise.
func ReplaceRemote(listing *Model) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		for name, ds := range out.Datasets {
			if _, named := listing.Datasets[name]; named {
				continue
			}
			if ds.Current == nil {
				ds.Current = &SnapshotInventory{Local: NewSnapshots(), Remote: NewSnapshots()}
			}
			ds.Current.Remote = NewSnapshots()
			ds.RemoteKnown = true
			ds.Metrics.RemoteSize = DatasetSize{}
			ds.Metrics.HasRemote = false
		}
		for name, read := range listing.Datasets {
			ds := out.ensure(name)
			ds.Current.Remote = read.Current.Remote.Clone()
			ds.RemoteKnown = true
			ds.Metrics.RemoteSize = read.Metrics.RemoteSize
			ds.Metrics.HasRemote = read.Metrics.HasRemote
		}
		return out
	}
}

// Retain keeps only the named datasets.
func Retain(names ...DatasetName) func(*Model) *Model {
	return func(old *Model) *Model {
		keep := make(map[DatasetName]bool, len(names))
		for _, name := range names {
			keep[name] = true
		}
		out := old.Clone()
		for name := range out.Datasets {
			if !keep[name] {
				delete(out.Datasets, name)
			}
		}
		return out
	}
}

// ensure returns name's dataset, adding it with nothing read if the
// model does not hold it. It mutates the model, so it is for a clone
// the caller owns.
func (model *Model) ensure(name DatasetName) *Dataset {
	ds, has := model.Datasets[name]
	if !has {
		ds = &Dataset{Name: name, Logs: logger.New(name.String())}
		model.Datasets[name] = ds
	}
	if ds.Current == nil {
		ds.Current = &SnapshotInventory{Local: NewSnapshots(), Remote: NewSnapshots()}
	}
	return ds
}
