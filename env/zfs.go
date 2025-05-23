package env

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

const readOnly = false

type Executor interface {
	Exec(logger logger.Logger, cmd ...string) ([]string, error)
	Execf(logger logger.Logger, cmd string, args ...any) ([]string, error)
}

type ZFS struct {
	prefix   string
	x        Executor
	readOnly bool
}

func NewZFS(prefix string, x Executor) *ZFS {
	return &ZFS{prefix, x, readOnly}
}

func (zfs *ZFS) WithPrefix(dataset model.DatasetName) string {
	return zfs.prefix + dataset.Path()
}

func (zfs *ZFS) WithoutPrefix(path string) model.DatasetName {
	return model.DatasetName(strings.TrimPrefix(path, zfs.prefix))
}

func (zfs *ZFS) GetResumeToken(logger logger.Logger, dataset model.DatasetName) (string, error) {
	out, err := zfs.x.Execf(logger, "zfs list -H -o receive_resume_token -S name -d 0 %s", zfs.WithPrefix(dataset))
	if err != nil {
		return "", fmt.Errorf("zfs list: %w\n%s", err, strings.Join(out, "\n"))
	}

	value := out[0]
	if value == "-" {
		return "", nil
	}

	return value, nil
}

func (zfs *ZFS) Size(logger logger.Logger, cmd *exec.Cmd) (int64, error) {
	if cmd.Args[0] != "zfs" || cmd.Args[1] != "send" {
		return 0, fmt.Errorf("must be a zfs send command")
	}

	args := append(cmd.Args[:], "--dryrun", "--verbose", "--parsable")
	out, err := zfs.x.Exec(logger, args...)
	if err != nil {
		return 0, fmt.Errorf("getting size of '%s': %w", strings.Join(args, " "), err)
	}
	lastLine := out[len(out)-1]
	sizeField := strings.Fields(lastLine)[1]
	size, err := strconv.ParseInt(sizeField, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing size from '%s': %w", sizeField, err)
	}

	return size, nil
}

func (zfs *ZFS) AbortResumable(logger logger.Logger, dataset model.DatasetName) error {
	if zfs.readOnly {
		panic("read only")
	}
	_, err := zfs.x.Execf(logger, "zfs receive -A %s", zfs.WithPrefix(dataset))
	if err != nil {
		return err
	}

	return nil
}

func (zfs *ZFS) GetDatasets(logger logger.Logger) ([]model.DatasetName, error) {
	datasets, err := zfs.x.Execf(logger, "zfs list -H -t filesystem -o name -d 1000 %s", zfs.prefix)
	if err != nil {
		return nil, err
	}
	out := make([]model.DatasetName, len(datasets))
	for i, d := range datasets {
		out[i] = zfs.WithoutPrefix(d)
	}
	return out, nil
}

func (zfs *ZFS) CreateDataset(logger logger.Logger, dataset model.DatasetName) error {
	if zfs.readOnly {
		panic("read only")
	}
	if _, err := zfs.x.Execf(logger, "zfs create -p %s", zfs.WithPrefix(dataset)); err != nil {
		return err
	}
	return nil
}

func (zfs *ZFS) GetLatestSnapshot(logger logger.Logger, dataset model.DatasetName) (*model.Snapshot, error) {
	snaps, err := zfs.GetSnapshots(logger, dataset)
	if err != nil {
		return nil, err
	}
	return snaps[len(snaps)-1], nil
}

func (zfs *ZFS) DestroySnapshot(logger logger.Logger, dataset model.DatasetName, snapshot string) error {
	if zfs.readOnly {
		panic("read only")
	}
	if _, err := zfs.x.Execf(logger, "zfs destroy %s@%s", zfs.WithPrefix(dataset), snapshot); err != nil {
		return err
	}
	return nil
}

func (zfs *ZFS) DestroySnapshotRange(logger logger.Logger, dataset model.DatasetName, first, last string) error {
	if zfs.readOnly {
		panic("read only")
	}
	if _, err := zfs.x.Execf(logger, "zfs destroy %s@%s%%%s", zfs.WithPrefix(dataset), first, last); err != nil {
		return err
	}
	return nil
}

func (zfs *ZFS) GetSnapshots(logger logger.Logger, dataset model.DatasetName) ([]*model.Snapshot, error) {
	rows, err := zfs.x.Execf(logger, "zfs list -H -p -t snapshot -o name,creation -s creation -d 1 %s", zfs.WithPrefix(dataset))
	if err != nil {
		return nil, fmt.Errorf("zfs list: %w", err)
	}
	snaps := make([]*model.Snapshot, len(rows))
	for i, row := range rows {
		cols := strings.Split(row, "\t")
		seconds, err := strconv.ParseInt(cols[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing timestamp '%s' (from '%s')", cols[0], cols[1])
		}
		snaps[i] = &model.Snapshot{
			Dataset:   dataset,
			Name:      strings.SplitN(cols[0], "@", 2)[1],
			CreatedAt: seconds,
		}
	}
	return snaps, nil
}
