//go:build postgres || !none

package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

var errLockNotHeld = errors.New("postgres advisory lock was not held by this session")

// Postgres returns the PostgreSQL connector. It only exists with `-tags postgres`.
//
// Locking uses a session-level advisory lock on the pinned connection. It does not
// work through PgBouncer in transaction-pooling mode.
func Postgres() Connector {
	return specConnector{spec: driverSpec{
		name:       "postgres",
		split:      sqlsplit.PostgresDialect(),
		caps:       Capabilities{TransactionalDDL: true, SessionLock: true},
		quoteIdent: nil,
		queries:    postgresQueries,
		lock:       postgresLock,
		checkTx:    nil, // extension point: reject CREATE INDEX CONCURRENTLY, VACUUM, ... in a tx
	}}
}

func postgresQueries(t string) queries {
	return queries{
		tableExists: "select 1 from pg_catalog.pg_class c " +
			"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
			"WHERE n.nspname = COALESCE(NULLIF($1, ''), current_schema()) AND c.relname = $2",
		createTable:   "CREATE TABLE IF NOT EXISTS " + t + " (version INTEGER NOT NULL)",
		initVersion:   "insert into " + t + " (version) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM " + t + ")",
		selectVersion: "select version from " + t,
		updateVersion: "UPDATE " + t + " SET version = $1",
	}
}

// postgresLockKey derives a stable, non-negative advisory-lock key from the table name.
// Advisory locks are scoped per database, so the table name is enough to separate runs.
func postgresLockKey(table string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("go-migrator:" + table))
	return int64(h.Sum64() >> 1)
}

func postgresLock(ctx context.Context, conn *sql.Conn, table string) (func(context.Context) error, error) {
	key := postgresLockKey(table)
	if _, err := conn.ExecContext(ctx, "select pg_advisory_lock($1)", key); err != nil {
		return nil, fmt.Errorf("postgres: acquire advisory lock: %w", err)
	}
	return func(ctx context.Context) error {
		var released bool
		if err := conn.QueryRowContext(ctx, "select pg_advisory_unlock($1)", key).Scan(&released); err != nil {
			return fmt.Errorf("postgres: release advisory lock: %w", err)
		}
		if !released {
			return errLockNotHeld
		}
		return nil
	}, nil
}
