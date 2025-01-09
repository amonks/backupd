package model

import (
	"sync"
)

type Model struct {
	datasets map[DatasetName]*Dataset
	mu       sync.RWMutex
}

func New() *Model {
	return &Model{
		datasets: make(map[DatasetName]*Dataset),
	}
}

func (model *Model) GetDataset(name DatasetName) *Dataset {
	model.mu.RLock()
	defer model.mu.RUnlock()

	return model.datasets[name]
}

func (model *Model) ListDatasets() []DatasetName {
	model.mu.RLock()
	defer model.mu.RUnlock()

	var names []DatasetName
	for name := range model.datasets {
		names = append(names, name)
	}

	return names
}

func (model *Model) ReplaceDataset(name DatasetName, dataset *Dataset) {
	model.mu.Lock()
	defer model.mu.Unlock()

	model.datasets[name] = dataset
}

func (model *Model) AddLocalDataset(name DatasetName, snapshots []*Snapshot) {
	if _, has := model.datasets[name]; !has {
		model.datasets[name] = &Dataset{
			Name: name,
		}
	}

	model.datasets[name].Local = NewSnapshots(snapshots...)
}

func (model *Model) AddRemoteDataset(name DatasetName, snapshots []*Snapshot) {
	if _, has := model.datasets[name]; !has {
		model.datasets[name] = &Dataset{
			Name: name,
		}
	}

	model.datasets[name].Remote = NewSnapshots(snapshots...)
}
