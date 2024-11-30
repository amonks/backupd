package main

import "strings"

type Executor interface {
	Exec(cmd ...string) ([]string, error)
	Execf(cmd string, args ...any) ([]string, error)
}

type ZFS struct {
	prefix string
	x      Executor
}

func NewZFS(prefix string, x Executor) *ZFS {
	return &ZFS{prefix, x}
}

func (zfs *ZFS) WithPrefix(dataset string) string {
	return zfs.prefix + dataset
}

func (zfs *ZFS) WithoutPrefix(dataset string) string {
	return strings.TrimPrefix(dataset, zfs.prefix)
}

func (zfs *ZFS) GetResumeToken(dataset string) (string, error) {
	out, err := zfs.x.Execf("zfs list -o receive_resume_token -S name -d 0 %s", zfs.WithPrefix(dataset))
	if err != nil {
		return "", err
	}

	value := out[len(out)-1]
	if value == "-" {
		return "", nil
	}

	return value, nil
}

func (zfs *ZFS) AbortResumable(dataset string) error {
	_, err := zfs.x.Execf("zfs receive -A %s", zfs.WithPrefix(dataset))
	if err != nil {
		return err
	}

	return nil
}

func (zfs *ZFS) GetDatasets() ([]string, error) {
	datasets, err := zfs.x.Execf("zfs list -t filesystem -o name")
	if err != nil {
		return nil, err
	}
	for i, d := range datasets {
		datasets[i] = zfs.WithoutPrefix(d)
	}
	return datasets, nil
}

func (zfs *ZFS) GetLatestSnapshot(dataset string) (string, error) {
	snaps, err := zfs.GetSnapshots(dataset)
	if err != nil {
		return "", err
	}
	return snaps[len(snaps)-1], nil
}

func (zfs *ZFS) GetSnapshots(dataset string) ([]string, error) {
	out, err := zfs.x.Execf("zfs list -t snapshot -o name -s creation -d 1 %s", zfs.WithPrefix(dataset))
	if err != nil {
		return nil, err
	}

	return out[1:], nil
}

func (zfs *ZFS) SendRangeTo(dest *ZFS, dataset, firstsnap, lastsnap string) error {
	return nil
}

func (zfs *ZFS) SendResumeTo(dest *ZFS, dataset, resumeToken string) error {
	return nil
}

func (zfs *ZFS) SendSnapshotTo(dest *ZFS, dataset, snap string) error {
	return nil
}

func (zfs *ZFS) SendNewSnapshotTo(dest *ZFS, dataset, snap string) error {
	return nil
}
