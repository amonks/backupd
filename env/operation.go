package env

import (
	"context"
	"fmt"

	"monks.co/backupbot/model"
)

func (env *Env) Apply(ctx context.Context, op model.Operation) error {
	switch op := op.(type) {

	case *model.SnapshotDeletion:
		var target *ZFS
		switch op.Location {
		case model.Local:
			target = env.Local
		case model.Remote:
			target = env.Remote
		default:
			return fmt.Errorf("invalid location '%s'", op.Location)
		}
		if err := target.DestroySnapshot(op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
			return err
		}
		return nil

	case *model.SnapshotRangeDeletion:
		var target *ZFS
		switch op.Location {
		case model.Local:
			target = env.Local
		case model.Remote:
			target = env.Remote
		default:
			return fmt.Errorf("invalid location '%s'", op.Location)
		}
		if err := target.DestroySnapshotRange(op.Start.Dataset, op.Start.Name, op.End.Name); err != nil {
			return err
		}
		return nil

	case *model.InitialSnapshotTransfer:
		if err := env.TransferInitialSnapshot(ctx, op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
			return err
		}
		return nil

	case *model.SnapshotTransfer:
		if err := env.TransferSnapshot(ctx, op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
			return err
		}
		return nil

	case *model.SnapshotRangeTransfer:
		if err := env.TransferSnapshotIncrementally(ctx, op.Start.Dataset, op.Start.Name, op.End.Name); err != nil {
			return err
		}
		return nil

	default:
		return fmt.Errorf("not supported")
	}
}
