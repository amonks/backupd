package env

import (
	"context"
	"fmt"
	"path"

	"monks.co/backupd/config"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

type Env struct {
	Local, Remote *ZFS

	// OnProgress, when set, receives cumulative progress for the
	// in-flight transfer: bytes piped so far and the expected total.
	// Transfers run one at a time, so a single callback suffices.
	OnProgress func(sent, total int64)
}

func New(config *config.Config) *Env {
	return &Env{
		Local: NewZFS(config.Local.Root, &LocalExecutor{Escalate: config.Local.Escalate}),
		Remote: NewZFS(
			config.Remote.Root,
			NewRemote(
				config.Remote.SSHKey,
				config.Remote.SSHHost,
			),
		),
	}
}

func (env *Env) Resume(ctx context.Context, logger *logger.Logger, dataset model.DatasetName, token string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	sendArgs := []string{"zfs", "send", "--raw", "-t", token}
	send := env.Local.Command(sendArgs...)
	recv := env.Remote.Command("zfs", "receive", "-s", env.Remote.WithPrefix(dataset))

	size, err := env.Local.Size(logger, sendArgs)
	if err != nil {
		return fmt.Errorf("getting size of resume: %w", err)
	}

	if err := Pipe(ctx, logger, size, env.OnProgress, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferInitialSnapshot(ctx context.Context, logger *logger.Logger, dataset model.DatasetName, snapshot string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	// Ensure parent dataset exists on the remote so zfs receive can create
	// the leaf dataset. Without this, receives into nested paths like
	// /home/thor fail because the intermediate /home dataset doesn't exist.
	if parent := path.Dir(dataset.Path()); parent != "." && parent != "/" {
		if err := env.Remote.CreateDataset(logger, model.DatasetName(parent)); err != nil {
			return fmt.Errorf("creating parent dataset '%s' on remote: %w", parent, err)
		}
	}

	sendArgs := []string{"zfs", "send", "--raw",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), snapshot)}
	send := env.Local.Command(sendArgs...)
	recv := env.Remote.Command("zfs", "receive", "-s", env.Remote.WithPrefix(dataset))

	size, err := env.Local.Size(logger, sendArgs)
	if err != nil {
		return fmt.Errorf("getting size of transfer '%s': %w", snapshot, err)
	}

	if err := Pipe(ctx, logger, size, env.OnProgress, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshot(ctx context.Context, logger *logger.Logger, dataset model.DatasetName, snapshot string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	sendArgs := []string{"zfs", "send", "--raw",
		fmt.Sprintf("%s %s", env.Local.WithPrefix(dataset), snapshot)}
	send := env.Local.Command(sendArgs...)
	recv := env.Remote.Command("zfs", "receive", "-s", "-F", env.Remote.WithPrefix(dataset))

	size, err := env.Local.Size(logger, sendArgs)
	if err != nil {
		return fmt.Errorf("getting size of transfer '%s': %w", snapshot, err)
	}

	if err := Pipe(ctx, logger, size, env.OnProgress, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshotIncrementally(ctx context.Context, logger *logger.Logger, dataset model.DatasetName, from, to string) error {
	if env.Local.readOnly || env.Remote.readOnly {
		panic("read only")
	}
	sendArgs := []string{"zfs", "send", "--raw", "-i",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), from),
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), to)}
	send := env.Local.Command(sendArgs...)
	recv := env.Remote.Command("zfs", "receive", "-s", "-F", env.Remote.WithPrefix(dataset))

	size, err := env.Local.Size(logger, sendArgs)
	if err != nil {
		return fmt.Errorf("getting size of range transfer from '%s' to '%s': %w", from, to, err)
	}

	if err := Pipe(ctx, logger, size, env.OnProgress, send, recv); err != nil {
		return err
	}

	return nil
}

// CreateSnapshotRecursively creates a recursive snapshot for the configured root
func (env *Env) CreateSnapshotRecursively(ctx context.Context, logger *logger.Logger, root string, periodicity string) error {
	if err := env.Local.CreateSnapshot(logger, root, periodicity); err != nil {
		return fmt.Errorf("creating snapshot: %w", err)
	}
	return nil
}
