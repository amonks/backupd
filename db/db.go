// Review this.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"monks.co/backupbot/model"
)

type DB struct {
	q  *Queries
	db *sql.DB
}

//go:embed db_schema.sql
var ddl string

func Open(filename string) (*DB, error) {
	ctx := context.Background()

	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return nil, err
	}

	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return nil, err
	}

	return &DB{New(db), db}, nil
}

func (db *DB) Close() error {
	return db.db.Close()
}

func (db *DB) GetModel(ctx context.Context) (*model.Model, error) {
	names, err := db.q.GetLocalDatasets(ctx)
	if err != nil {
		return nil, err
	}
	datasets := make(map[string]*model.Dataset, len(names))
	for _, name := range names {
		dataset, err := db.getDatasetModel(ctx, name)
		if err != nil {
			return nil, err
		}
		datasets[dataset.Name] = dataset
	}
	model := &model.Model{
		Datasets: datasets,
	}
	return model, nil
}

func (db *DB) getDatasetModel(ctx context.Context, dataset string) (*model.Dataset, error) {
	snaps, err := db.q.GetAllSnapshots(ctx, dataset)
	if err != nil {
		return nil, err
	}

	local, remote := model.NewSnapshots(), model.NewSnapshots()

	for _, row := range snaps {
		snap := &model.Snapshot{
			Dataset:   dataset,
			Name:      row.Name,
			CreatedAt: row.CreatedAt,
		}
		if IsTrue(row.IsOnLocal) {
			local.Add(snap)
		}
		if IsTrue(row.IsOnRemote) {
			remote.Add(snap)
		}
	}

	return &model.Dataset{
		Name:   dataset,
		Local:  local,
		Remote: remote,
	}, nil
}

func (db *DB) ObserveDatasets(ctx context.Context, location model.Location, datasets []string, observedAt time.Time) error {
	switch location {
	case model.Local, model.Remote:
	default:
		return fmt.Errorf("invalid location '%s'", location)
	}

	tx, err := db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := New(tx)

	switch location {
	case model.Local:
		for _, dataset := range datasets {
			if _, err := q.ObserveLocalDataset(ctx, ObserveLocalDatasetParams{
				Name:            dataset,
				IsOnLocal:       True(),
				LocalObservedAt: Int64(observedAt.Unix()),
			}); err != nil {
				return err
			}
		}
		if err := q.RemoveOlderLocalDatasets(ctx, Int64(observedAt.Unix())); err != nil {
			return err
		}

	case model.Remote:
		for _, dataset := range datasets {
			if _, err := q.ObserveRemoteDataset(ctx, ObserveRemoteDatasetParams{
				Name:             dataset,
				IsOnRemote:       True(),
				RemoteObservedAt: Int64(observedAt.Unix()),
			}); err != nil {
				return err
			}
		}
		if err := q.RemoveOlderRemoteDatasets(ctx, Int64(observedAt.Unix())); err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid location '%s'", location)
	}

	return tx.Commit()
}

func (db *DB) ObserveSnapshots(ctx context.Context, location model.Location, dataset string, snapshots []*model.Snapshot, observedAt time.Time) error {
	switch location {
	case model.Local, model.Remote:
	default:
		return fmt.Errorf("invalid location '%s'", location)
	}

	tx, err := db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := New(tx)

	switch location {
	case model.Local:
		for _, snap := range snapshots {
			if _, err := q.ObserveLocalSnapshot(ctx, ObserveLocalSnapshotParams{
				Dataset:         dataset,
				Name:            snap.Name,
				CreatedAt:       snap.CreatedAt,
				IsOnLocal:       True(),
				LocalObservedAt: Int64(observedAt.Unix()),
			}); err != nil {
				return err
			}
		}
		if err := q.RemoveOlderLocalSnapshots(ctx, RemoveOlderLocalSnapshotsParams{
			Dataset:         dataset,
			LocalObservedAt: Int64(observedAt.Unix()),
		}); err != nil {
			return err
		}

	case model.Remote:
		for _, snap := range snapshots {
			if _, err := q.ObserveRemoteSnapshot(ctx, ObserveRemoteSnapshotParams{
				Dataset:          dataset,
				Name:             snap.Name,
				CreatedAt:        snap.CreatedAt,
				IsOnRemote:       True(),
				RemoteObservedAt: Int64(observedAt.Unix()),
			}); err != nil {
				return err
			}
		}
		if err := q.RemoveOlderRemoteSnapshots(ctx, RemoveOlderRemoteSnapshotsParams{
			Dataset:          dataset,
			RemoteObservedAt: Int64(observedAt.Unix()),
		}); err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid location '%s'", location)
	}

	return tx.Commit()
}

func (db *DB) CreateRemoteDataset(ctx context.Context, name string) error {
	if err := db.q.CreateRemoteDataset(ctx, name); err != nil {
		return err
	}
	return nil
}

func (db *DB) RemoveSnapshot(ctx context.Context, location model.Location, snapshot *model.Snapshot) error {
	switch location {
	case model.Local:
		if _, err := db.q.ObserveLocalSnapshot(ctx, ObserveLocalSnapshotParams{
			Dataset:         snapshot.Dataset,
			Name:            snapshot.Name,
			CreatedAt:       snapshot.CreatedAt,
			IsOnLocal:       False(),
			LocalObservedAt: Int64(time.Now().Unix()),
		}); err != nil {
			return err
		}

	case model.Remote:
		if _, err := db.q.ObserveRemoteSnapshot(ctx, ObserveRemoteSnapshotParams{
			Dataset:          snapshot.Dataset,
			Name:             snapshot.Name,
			CreatedAt:        snapshot.CreatedAt,
			IsOnRemote:       False(),
			RemoteObservedAt: Int64(time.Now().Unix()),
		}); err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid location '%s'", location)
	}
	return nil
}

func (db *DB) RemoveSnapshotRange(ctx context.Context, location model.Location, start, end *model.Snapshot) error {
	switch location {
	case model.Local:
		if err := db.q.DestroyLocalSnapshotRange(ctx, DestroyLocalSnapshotRangeParams{
			LocalObservedAt: Int64(time.Now().Unix()),
			Dataset:         start.Dataset,
			CreatedAt:       start.CreatedAt,
			CreatedAt_2:     end.CreatedAt,
		}); err != nil {
			return err
		}

	case model.Remote:
		if err := db.q.DestroyRemoteSnapshotRange(ctx, DestroyRemoteSnapshotRangeParams{
			RemoteObservedAt: Int64(time.Now().Unix()),
			Dataset:          start.Dataset,
			CreatedAt:        start.CreatedAt,
			CreatedAt_2:      end.CreatedAt,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) TransferSnapshot(ctx context.Context, snapshot *model.Snapshot) error {
	if _, err := db.q.ObserveRemoteSnapshot(ctx, ObserveRemoteSnapshotParams{
		Dataset:          snapshot.Dataset,
		Name:             snapshot.Name,
		CreatedAt:        snapshot.CreatedAt,
		IsOnRemote:       True(),
		RemoteObservedAt: Int64(time.Now().Unix()),
	}); err != nil {
		return err
	}
	return nil
}
