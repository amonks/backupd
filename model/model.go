package model

import "sort"

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

func ReplaceDataset(name DatasetName, dataset *Dataset) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()
		out.Datasets[name] = dataset
		return out
	}
}

func AddLocalDataset(name DatasetName, snapshots []*Snapshot, size *DatasetSize) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()

		if _, has := out.Datasets[name]; !has {
			out.Datasets[name] = &Dataset{
				Name: name,
				Current: &SnapshotInventory{
					Local:  NewSnapshots(),
					Remote: NewSnapshots(),
				},
			}
		}
		if out.Datasets[name].Current == nil {
			out.Datasets[name].Current = &SnapshotInventory{
				Local:  NewSnapshots(),
				Remote: NewSnapshots(),
			}
		}
		out.Datasets[name].Current.Local = NewSnapshots(snapshots...)
		// Update metrics if size is provided
		if size != nil {
			out.Datasets[name].Metrics.LocalSize = *size
			out.Datasets[name].Metrics.HasLocal = true
		}

		return out
	}
}

func AddRemoteDataset(name DatasetName, snapshots []*Snapshot, size *DatasetSize) func(*Model) *Model {
	return func(old *Model) *Model {
		out := old.Clone()

		if _, has := out.Datasets[name]; !has {
			out.Datasets[name] = &Dataset{
				Name: name,
				Current: &SnapshotInventory{
					Local:  NewSnapshots(),
					Remote: NewSnapshots(),
				},
			}
		}
		if out.Datasets[name].Current == nil {
			out.Datasets[name].Current = &SnapshotInventory{
				Local:  NewSnapshots(),
				Remote: NewSnapshots(),
			}
		}
		out.Datasets[name].Current.Remote = NewSnapshots(snapshots...)
		// Update metrics if size is provided
		if size != nil {
			out.Datasets[name].Metrics.RemoteSize = *size
			out.Datasets[name].Metrics.HasRemote = true
		}

		return out
	}
}
