// Package store owns the SQLite database: schema, connection setup, and the
// sqlc-generated queries in the db subpackage.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"math"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Open opens (creating if needed) the database and applies the schema. The
// schema is CREATE IF NOT EXISTS throughout, so this is idempotent.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writers; one connection avoids SQLITE_BUSY for a
	// workload of one insert every 20 seconds.
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(schemaSQL); err != nil {
		d.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return d, nil
}

// Float stores NaN as NULL.
func Float(v float64) sql.NullFloat64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: v, Valid: true}
}

// Value reads NULL back as NaN.
func Value(v sql.NullFloat64) float64 {
	if !v.Valid {
		return math.NaN()
	}
	return v.Float64
}
