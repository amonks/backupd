package main

type Backup struct {
	datasets      map[string]*Dataset
	local, remote *ZFS
}

func NewBackup(local, remote *ZFS) *Backup {
	return &Backup{local: local, remote: remote}
}

func (b *Backup) Init() error {
	datasets, err := b.local.GetDatasets()
	if err != nil {
		return err
	}

	b.datasets = make(map[string]*Dataset, len(datasets))
	for _, ds := range datasets {
		b.datasets[ds] = NewDataset(ds, b.local, b.remote)
	}

	return nil
}
