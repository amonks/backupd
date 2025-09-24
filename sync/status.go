package sync

import (
	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

// Status represents the sync status of datasets
type Status struct {
	*atom.Atom[map[model.DatasetName]bool]
}

// New creates a new sync status tracker
func New() *Status {
	return &Status{
		atom.New(make(map[model.DatasetName]bool)),
	}
}

// SetSyncing marks a dataset as currently syncing
func (s *Status) SetSyncing(dataset model.DatasetName, syncing bool) {
	s.Swap(func(old map[model.DatasetName]bool) map[model.DatasetName]bool {
		out := make(map[model.DatasetName]bool, len(old))
		for k, v := range old {
			out[k] = v
		}
		if syncing {
			out[dataset] = true
		} else {
			delete(out, dataset)
		}
		return out
	})
}

// IsSyncing returns true if the dataset is currently syncing
func (s *Status) IsSyncing(dataset model.DatasetName) bool {
	status := s.Deref()
	return status[dataset]
}

// GetSyncingDatasets returns a slice of all currently syncing datasets
func (s *Status) GetSyncingDatasets() []model.DatasetName {
	status := s.Deref()
	var syncing []model.DatasetName
	for dataset, isSyncing := range status {
		if isSyncing {
			syncing = append(syncing, dataset)
		}
	}
	return syncing
}