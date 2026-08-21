package env

import (
	"context"

	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

// Interface is everything the daemon needs from a backing environment.
// The real implementation (*Env) shells out to zfs locally and over
// ssh; the sim package implements the same surface over an in-memory
// pair of pools so the full daemon can run — and be tested — without
// root, ZFS, or a network.
type Interface interface {
	// LocalDatasets and RemoteDatasets enumerate the datasets under
	// each root, with their storage metrics.
	LocalDatasets(l *logger.Logger) ([]DatasetInfo, error)
	RemoteDatasets(l *logger.Logger) ([]DatasetInfo, error)

	// LocalSnapshots and RemoteSnapshots list a dataset's snapshots in
	// creation order.
	LocalSnapshots(l *logger.Logger, dataset model.DatasetName) ([]*model.Snapshot, error)
	RemoteSnapshots(l *logger.Logger, dataset model.DatasetName) ([]*model.Snapshot, error)

	// RemoteResumeToken returns the receive resume token for an
	// interrupted transfer into the remote dataset, or "" if none.
	RemoteResumeToken(l *logger.Logger, dataset model.DatasetName) (string, error)

	// AbortRemoteResumable discards partially-received state on the
	// remote, clearing the resume token.
	AbortRemoteResumable(l *logger.Logger, dataset model.DatasetName) error

	// Resume continues an interrupted transfer using its resume token.
	Resume(ctx context.Context, l *logger.Logger, dataset model.DatasetName, token string) error

	// Apply executes one plan operation (a transfer or deletion).
	Apply(ctx context.Context, l *logger.Logger, op model.Operation) error

	// Snapshot creates a recursive snapshot of the local root, named
	// <periodicity>-<timestamp>.
	Snapshot(ctx context.Context, l *logger.Logger, root, periodicity string) error

	// SetOnProgress installs the callback that receives cumulative
	// progress for the in-flight transfer: bytes moved so far and the
	// expected total. Transfers run one at a time, so one callback
	// serves the whole environment. The daemon installs its own on
	// construction, which is why this is part of the seam rather than a
	// field on each implementation.
	SetOnProgress(func(sent, total int64))
}

var _ Interface = &Env{}

func (env *Env) LocalDatasets(l *logger.Logger) ([]DatasetInfo, error) {
	return env.Local.GetDatasets(l)
}

func (env *Env) RemoteDatasets(l *logger.Logger) ([]DatasetInfo, error) {
	return env.Remote.GetDatasets(l)
}

func (env *Env) LocalSnapshots(l *logger.Logger, dataset model.DatasetName) ([]*model.Snapshot, error) {
	return env.Local.GetSnapshots(l, dataset)
}

func (env *Env) RemoteSnapshots(l *logger.Logger, dataset model.DatasetName) ([]*model.Snapshot, error) {
	return env.Remote.GetSnapshots(l, dataset)
}

func (env *Env) RemoteResumeToken(l *logger.Logger, dataset model.DatasetName) (string, error) {
	return env.Remote.GetResumeToken(l, dataset)
}

func (env *Env) AbortRemoteResumable(l *logger.Logger, dataset model.DatasetName) error {
	return env.Remote.AbortResumable(l, dataset)
}

func (env *Env) Snapshot(ctx context.Context, l *logger.Logger, root, periodicity string) error {
	return env.CreateSnapshotRecursively(ctx, l, root, periodicity)
}

func (env *Env) SetOnProgress(fn func(sent, total int64)) { env.OnProgress = fn }
