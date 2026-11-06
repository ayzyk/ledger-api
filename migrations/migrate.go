// Package migrations applies the SQL files from this folder in name order.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

// random number, only has to be the same for every instance of the service
const lockID = 727401

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// several instances can start at the same time
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := applyOne(ctx, tx, name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

func applyOne(ctx context.Context, tx pgx.Tx, name string) error {
	var done bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", name).Scan(&done)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	sql, err := files.ReadFile(name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name)
	return err
}
