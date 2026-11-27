package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getloopscale/loop-sandbox/migrations"
)

// migrationLockKey is a fixed advisory-lock id that serializes migrations across
// replicas. Its value spells "GoSandbo" and must not change: replicas of different
// versions rely on taking the same lock during a rolling deploy.
const migrationLockKey int64 = 0x476F53616E64626F

// ErrMigrationChanged is returned when a migration recorded as applied no longer
// has the SQL it was applied with. An applied migration is history; a schema change
// goes in a new file.
var ErrMigrationChanged = errors.New("applied migration was modified")

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Migrate applies every migration that schema_migrations does not record yet, in
// version order, and returns how many it applied. Each migration and its record
// commit in one transaction, so a failure leaves the schema at the previous version.
//
// It is safe to call from several replicas at once: a session-level advisory lock is
// held for the whole run, so one replica applies and the others wait and then find
// nothing left to do.
//
// A database created before version tracking has the schema and no records. Every
// migration written before that point is idempotent, so the first Migrate re-applies
// them once and records them.
func Migrate(ctx context.Context, pool *pgxpool.Pool, all []migrations.Migration) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	// Advisory locks are session-scoped, so the lock and the migrations must share
	// this one connection.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return 0, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, createMigrationsTable); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}
	applied, err := appliedChecksums(ctx, conn)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, m := range all {
		if sum, done := applied[m.Version]; done {
			if sum != m.Checksum {
				return count, fmt.Errorf("%w: %s (recorded checksum %.12s, file has %.12s)",
					ErrMigrationChanged, m.Name, sum, m.Checksum)
			}
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func appliedChecksums(ctx context.Context, conn *pgxpool.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return out, nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, m migrations.Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.Name, err)
	}
	// Rollback after a successful Commit is a no-op that returns ErrTxClosed.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The simple protocol lets one Exec carry the several statements of a file.
	if _, err := tx.Exec(ctx, m.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("record migration %s: %w", m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.Name, err)
	}
	return nil
}
