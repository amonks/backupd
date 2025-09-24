package env

import (
	"context"
	"fmt"
	"os/exec"

	"monks.co/backupd/config"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

type Env struct {
	Local, Remote *ZFS
}

func New(config *config.Config) *Env {
	return &Env{
		Local: NewZFS(config.Local.Root, Local),
		Remote: NewZFS(
			config.Remote.Root,
			NewRemote(
				config.Remote.SSHKey,
				config.Remote.SSHHost,
			),
		),
	}
}

func (env *Env) Resume(ctx context.Context, logger logger.Logger, dataset model.DatasetName, token string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw", "-t", token)
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s %s", env.Remote.WithPrefix(dataset)))

	size, err := env.Local.Size(logger, send)
	if err != nil {
		return fmt.Errorf("getting size of resume: %w", err)
	}

	if err := Pipe(ctx, logger, size, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferInitialSnapshot(ctx context.Context, logger logger.Logger, dataset model.DatasetName, snapshot string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), snapshot))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s %s", env.Remote.WithPrefix(dataset)))

	size, err := env.Local.Size(logger, send)
	if err != nil {
		return fmt.Errorf("getting size of transfer '%s': %w", snapshot, err)
	}

	if err := Pipe(ctx, logger, size, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshot(ctx context.Context, logger logger.Logger, dataset model.DatasetName, snapshot string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw",
		fmt.Sprintf("%s %s", env.Local.WithPrefix(dataset), snapshot))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s -F %s", env.Remote.WithPrefix(dataset)))

	size, err := env.Local.Size(logger, send)
	if err != nil {
		return fmt.Errorf("getting size of transfer '%s': %w", snapshot, err)
	}

	if err := Pipe(ctx, logger, size, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshotIncrementally(ctx context.Context, logger logger.Logger, dataset model.DatasetName, from, to string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw", "-i",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), from),
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), to))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s -F %s", env.Remote.WithPrefix(dataset)))

	size, err := env.Local.Size(logger, send)
	if err != nil {
		return fmt.Errorf("getting size of range transfer from '%s' to '%s': %w", from, to, err)
	}

	if err := Pipe(ctx, logger, size, send, recv); err != nil {
		return err
	}

	return nil
}

// CreateSnapshotRecursively creates a recursive snapshot for the configured root
func (env *Env) CreateSnapshotRecursively(ctx context.Context, logger logger.Logger, root string, periodicity string) error {
	if err := env.Local.CreateSnapshot(logger, root, periodicity); err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}
	return nil
}
