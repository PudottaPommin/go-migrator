package migrator

import (
	"context"
	"database/sql"
	"errors"
)

var (
	// ErrManualTxInTransactedMigration is returned when a migration that runs inside
	// a transaction contains its own BEGIN / COMMIT / END / ROLLBACK.
	ErrManualTxInTransactedMigration = errors.New(
		"explicit transaction control inside a transacted migration; set Options.DisableTx for scripts that manage their own",
	)

	// ErrPragmaForeignKeysInTx is returned for PRAGMA foreign_keys inside a transaction,
	// where SQLite silently ignores it.
	ErrPragmaForeignKeysInTx = errors.New(
		"PRAGMA foreign_keys has no effect inside a transaction; set Options.DisableTx for this migration")
)

// Capabilities describes engine properties the migrator core may branch on.
type Capabilities struct {
	TransactionalDDL bool // DDL can be rolled back (SQLite, PostgreSQL: yes; MySQL: no)
	SessionLock      bool // Lock really serializes concurrent migrators
}

// Step is one unit of work handed to a Conn: run SQL, then store NewVersion.
type Step struct {
	SQL        string
	NewVersion int  // version stored once SQL succeeded (up: v+1, down: v-1)
	UseTx      bool // run SQL and the version update in one transaction
}

// ConnOptions configures a Conn.
type ConnOptions struct {
	// Table is the version table name (e.g. "__migrations", "schema.table", or `"schema"."table"`).
	// Each identifier is quoted by the connector; defaults to DefaultVersionTable when empty.
	Table string
}

// Connector is the stateless, per-engine factory. One value per supported engine.
type Connector interface {
	Name() string
	// Connect pins ONE connection from db. Session state (advisory locks, PRAGMAs,
	// manual BEGIN) only works if the whole migration run uses the same connection.
	Connect(ctx context.Context, db *sql.DB, opts ConnOptions) (Conn, error)
}

// Conn is a pinned connection plus everything engine-specific the migrator needs.
type Conn interface {
	Capabilities() Capabilities
	// Lock serializes concurrent migrators. Call it before EnsureTable. The returned
	// unlock func is safe to call with an already-cancelled context.
	Lock(ctx context.Context) (unlock func(context.Context) error, err error)
	// EnsureTable creates the version table and its single row (version 0) if missing.
	EnsureTable(ctx context.Context) error
	CurrentVersion(ctx context.Context) (int, error)
	Apply(ctx context.Context, step Step) error
	Close() error
}
