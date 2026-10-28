package migrator

import (
	"context"
	"database/sql"
	"strings"
	"text/template"
)

type DriverType string

const (
	DriverSQLite DriverType = "sqlite"
)

type sentinelError string

func (e sentinelError) Error() string {
	return string(e)
}

const (
	// ErrManualTxInTransactedMigration is returned when a migration contains explicit transaction control
	// statements (BEGIN, COMMIT, END, or non-savepoint ROLLBACK) while automatic transactions are enabled.
	ErrManualTxInTransactedMigration sentinelError = "manual transaction statement is not allowed when useTx is enabled"
	// ErrPragmaForeignKeysInTx is returned when a migration modifies PRAGMA foreign_keys while automatic
	// transactions are enabled, because SQLite silently ignores PRAGMA foreign_keys inside a transaction.
	ErrPragmaForeignKeysInTx sentinelError = "PRAGMA foreign_keys cannot be modified inside a transaction"
	// ErrUncommittedTx is returned when a non-transacted migration finishes with an open transaction.
	ErrUncommittedTx sentinelError = "migration finished with an uncommitted transaction"
)

type Driver interface {
	Type() DriverType
	Close() error
	Conn() *sql.Conn
	run(ctx context.Context, useTx bool, fn func(interface {
		ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	}) error) error
	runSQL(ctx context.Context, query string, useTx bool) error
	getCurrentVersion(ctx context.Context, tableName string) (int, error)
	tableExists(ctx context.Context, tableName string) (bool, error)
	ensureMigrationTable(ctx context.Context, tableName string) error
}

func renderSQLTemplate(tmpl *template.Template, name string, data any) (string, error) {
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
