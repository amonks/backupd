package db

import (
	"context"
	"fmt"

	"monks.co/backupbot/model"
)

func (db *DB) Record(ctx context.Context, op model.Operation) error {
	switch op := op.(type) {

	case *model.SnapshotRangeDeletion:
		if err := db.RemoveSnapshotRange(ctx, op.Location, op.Start, op.End); err != nil {
			return err
		}
		return nil

	case *model.SnapshotDeletion:
		if err := db.RemoveSnapshot(ctx, op.Location, op.Snapshot); err != nil {
			return err
		}
		return nil

	case *model.InitialSnapshotTransfer:
		if err := db.TransferSnapshot(ctx, op.Snapshot); err != nil {
			return err
		}
		return nil

	case *model.SnapshotTransfer:
		if err := db.TransferSnapshot(ctx, op.Snapshot); err != nil {
			return err
		}
		return nil

	case *model.SnapshotRangeTransfer:
		if err := db.TransferSnapshot(ctx, op.End); err != nil {
			return err
		}
		return nil

	default:
		return fmt.Errorf("not supported")
	}
}
