package db

import (
	"database/sql"
)

func True() sql.NullBool  { return sql.NullBool{Bool: true, Valid: true} }
func False() sql.NullBool { return sql.NullBool{Bool: false, Valid: true} }

func Int64(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }

func IsTrue(b sql.NullBool) bool  { return b.Valid && b.Bool }
func IsFalse(b sql.NullBool) bool { return b.Valid && !b.Bool }
