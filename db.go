package main

import (
	"context"
	"database/sql"
	_ "embed"
)

//go:embed schema.sql
var ddl string

func OpenDB(filename string) (*sql.DB, error) {
	ctx := context.Background()

	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return nil, err
	}

	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return nil, err
	}

	return db, nil
}
