// Package db wraps a pgx pool with transactions, migrations and small query helpers.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Q is satisfied by both *pgxpool.Pool and pgx.Tx, so domain code runs inside or outside a transaction.
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type DB struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if cfg.MaxConns < 10 {
		cfg.MaxConns = 10
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	var pool *pgxpool.Pool
	// Retry briefly: on Render/docker the database can come up a few seconds after the app.
	for attempt := 0; ; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		if attempt >= 20 {
			return nil, fmt.Errorf("db: connect: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(250*(attempt+1)) * time.Millisecond):
		}
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() { d.Pool.Close() }

// Tx runs fn in a transaction, retrying on serialization/deadlock failures.
func (d *DB) Tx(ctx context.Context, fn func(q Q) error) error {
	for attempt := 0; ; attempt++ {
		err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error { return fn(tx) })
		if err == nil || attempt >= 4 || !retryable(err) {
			return err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// IsUniqueViolation reports whether err is a unique-constraint violation (used for idempotency).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Migrate applies embedded migrations in filename order, each exactly once, under an advisory lock
// so the web service and worker can both start safely.
func (d *DB) Migrate(ctx context.Context) error {
	return d.Tx(ctx, func(q Q) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(727274)`); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (id text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		entries, err := fs.ReadDir(migrationsFS, "migrations")
		if err != nil {
			return err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			var exists bool
			if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=$1)`, name).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
			if err != nil {
				return err
			}
			if _, err := q.Exec(ctx, string(sqlBytes)); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := q.Exec(ctx, `INSERT INTO schema_migrations (id) VALUES ($1)`, name); err != nil {
				return err
			}
		}
		return nil
	})
}
