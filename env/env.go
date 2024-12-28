package env

import (
	"context"
	"fmt"
	"os/exec"

	"monks.co/backupbot/db"
)

type Env struct {
	Local, Remote *ZFS
}

func New(db *db.DB) *Env {
	return &Env{
		Local: NewZFS("data/tank", Local),
		Remote: NewZFS(
			"data1/thor/tank",
			NewRemote(
				"/home/ajm/.ssh/id_ed25519",
				"root@57269.zfs.rsync.net",
			),
		),
	}
}

func (env *Env) Resume(ctx context.Context, dataset, token string) error {
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw", "-t", token)
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s %s", env.Remote.WithPrefix(dataset)))

	if err := Pipe(ctx, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferInitialSnapshot(ctx context.Context, dataset, snapshot string) error {
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), snapshot))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s %s", env.Remote.WithPrefix(dataset)))

	if err := Pipe(ctx, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshot(ctx context.Context, dataset, snapshot string) error {
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw",
		fmt.Sprintf("%s %s", env.Local.WithPrefix(dataset), snapshot))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s -F %s", env.Remote.WithPrefix(dataset)))

	if err := Pipe(ctx, send, recv); err != nil {
		return err
	}

	return nil
}

func (env *Env) TransferSnapshotIncrementally(ctx context.Context, dataset, from, to string) error {
	remote := env.Remote.x.(*Remote)

	send := exec.Command("zfs", "send", "--raw", "-i",
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), from),
		fmt.Sprintf("%s@%s", env.Local.WithPrefix(dataset), to))
	recv := exec.Command("ssh", "-i", remote.sshKey, remote.sshHost,
		fmt.Sprintf("zfs receive -s -F %s", env.Remote.WithPrefix(dataset)))

	if err := Pipe(ctx, send, recv); err != nil {
		return err
	}

	return nil
}
