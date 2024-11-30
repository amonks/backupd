package main

type Dataset struct {
	name   string
	local  *ZFS
	remote *ZFS
}

func NewDataset(name string, local *ZFS, remote *ZFS) *Dataset {
	return &Dataset{
		name:   name,
		local:  local,
		remote: remote,
	}
}

func (ds *Dataset) Sync() error {
	// localSnaps, err := ds.local.GetSnapshots(ds.name)
	// if err != nil {
	// 	return err
	// }

	// remoteSnaps, err := ds.remote.GetSnapshots(ds.name)
	// if err != nil {
	// 	return err
	// }

	return nil
}
