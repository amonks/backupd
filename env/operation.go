package env

import (
	"context"
	"fmt"

	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

func (env *Env) Apply(ctx context.Context, logger logger.Logger, op model.Operation) error {
	// Unwrap PlanStep if necessary
	if step, ok := op.(*model.PlanStep); ok {
		op = step.Operation
	}

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
		if err := target.DestroySnapshot(logger, op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
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
		if err := target.DestroySnapshotRange(logger, op.Start.Dataset, op.Start.Name, op.End.Name); err != nil {
			return err
		}
		return nil

	case *model.InitialSnapshotTransfer:
		if err := env.TransferInitialSnapshot(ctx, logger, op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
			return err
		}
		return nil

	case *model.SnapshotTransfer:
		if err := env.TransferSnapshot(ctx, logger, op.Snapshot.Dataset, op.Snapshot.Name); err != nil {
			return err
		}
		return nil

	case *model.SnapshotRangeTransfer:
		if err := env.TransferSnapshotIncrementally(ctx, logger, op.Start.Dataset, op.Start.Name, op.End.Name); err != nil {
			return err
		}
		return nil

	default:
		return fmt.Errorf("%s is not supported", op)
	}
}
