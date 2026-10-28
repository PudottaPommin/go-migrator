//go:build sqlite

package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

const (
	maxLeadingTokens         = 5
	minSavepointTokens       = 2
	minTxSavepointTokens     = 3
	minPragmaAssignTokens    = 3
	minQualifiedPragmaTokens = 5
	rollbackCleanupTimeout   = 5 * time.Second
)

type connectionSQLite struct {
	dtype DriverType
	conn  *sql.Conn
}

func NewConnectionSQLite(dtype DriverType, conn *sql.Conn) Driver {
	return &connectionSQLite{dtype: dtype, conn: conn}
}

func (c *connectionSQLite) Type() DriverType {
	return c.dtype
}

func (c *connectionSQLite) Conn() *sql.Conn {
	return c.conn
}

func (c *connectionSQLite) Close() error {
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			return fmt.Errorf("failed to close connection: %w", err)
		}
	}
	return nil
}

func (c *connectionSQLite) run(ctx context.Context, useTx bool, fn func(interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}) error,
) error {
	if useTx {
		tx, err := c.conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %w", err)
		}
		defer tx.Rollback()
		if err = fn(tx); err != nil {
			return fmt.Errorf("failed to execute transaction: %w", err)
		}
		return tx.Commit()
	}
	if err := fn(c.conn); err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}
	return nil
}

func (c *connectionSQLite) runSQL(ctx context.Context, query string, useTx bool) error {
	stmts, err := sqlsplit.Split(query, sqlsplit.SQLiteDialect())
	if err != nil {
		return fmt.Errorf("failed to parse SQL: %w", err)
	}
	if len(stmts) == 0 {
		return nil
	}

	if useTx {
		if err = validateSQLiteTxStatements(stmts); err != nil {
			return err
		}
		return c.runSQLInTx(ctx, stmts)
	}

	return c.runSQLWithoutTx(ctx, stmts)
}

func (c *connectionSQLite) runSQLInTx(ctx context.Context, stmts []sqlsplit.Statement) error {
	tx, err := c.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	for i, stmt := range stmts {
		if _, err = tx.ExecContext(ctx, stmt.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("statement %d (line %d) failed: %w", i+1, stmt.Line, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

func (c *connectionSQLite) runSQLWithoutTx(ctx context.Context, stmts []sqlsplit.Statement) error {
	for i, stmt := range stmts {
		if _, err := c.conn.ExecContext(ctx, stmt.SQL); err != nil {
			c.rollbackBestEffort(ctx)
			return fmt.Errorf("statement %d (line %d) failed: %w", i+1, stmt.Line, err)
		}
	}

	if _, probeErr := c.conn.ExecContext(ctx, "rollback"); probeErr == nil {
		return ErrUncommittedTx
	}

	return nil
}

func (c *connectionSQLite) rollbackBestEffort(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackCleanupTimeout)
	defer cancel()
	_, _ = c.conn.ExecContext(cleanupCtx, "rollback")
}

func (c *connectionSQLite) getCurrentVersion(ctx context.Context, tableName string) (int, error) {
	sqlString, err := renderSQLTemplate(sqliteTemplate, "current_version", tableName)
	if err != nil {
		return -1, err
	}
	var version int
	if err = c.conn.QueryRowContext(ctx, sqlString).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, err
	}
	return version, nil
}

func (c *connectionSQLite) tableExists(ctx context.Context, tableName string) (bool, error) {
	sqlString, err := renderSQLTemplate(sqliteTemplate, "exists", tableName)
	if err != nil {
		return false, err
	}

	var cid, name, dtype, notnull, pk string
	var dfltValue *string
	err = c.conn.QueryRowContext(ctx, sqlString).Scan(&cid, &name, &dtype, &notnull, &dfltValue, &pk)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return cid != "", nil
}

func (c *connectionSQLite) ensureMigrationTable(ctx context.Context, tableName string) error {
	if ok, err := c.tableExists(ctx, tableName); err != nil {
		return err
	} else if ok {
		return nil
	}

	sqlString, err := renderSQLTemplate(sqliteTemplate, "create", tableName)
	if err != nil {
		return err
	}

	_, err = c.conn.ExecContext(ctx, sqlString)
	if err != nil {
		return err
	}

	return nil
}

func validateSQLiteTxStatements(stmts []sqlsplit.Statement) error {
	dialect := sqlsplit.SQLiteDialect()
	for i, stmt := range stmts {
		tokens := sqlsplit.LeadingTokens(stmt.SQL, dialect, maxLeadingTokens)
		if len(tokens) == 0 {
			continue
		}
		if isDisallowedTxStatement(tokens) {
			return fmt.Errorf("statement %d (line %d): %w", i+1, stmt.Line, ErrManualTxInTransactedMigration)
		}
		if isPragmaForeignKeysMutation(tokens) {
			return fmt.Errorf("statement %d (line %d): %w", i+1, stmt.Line, ErrPragmaForeignKeysInTx)
		}
	}
	return nil
}

func isDisallowedTxStatement(tokens []string) bool {
	first := tokens[0]
	if strings.EqualFold(first, "BEGIN") || strings.EqualFold(first, "COMMIT") || strings.EqualFold(first, "END") {
		return true
	}
	if strings.EqualFold(first, "ROLLBACK") {
		return !isRollbackToSavepoint(tokens)
	}
	return false
}

func isRollbackToSavepoint(tokens []string) bool {
	if len(tokens) >= minSavepointTokens && strings.EqualFold(tokens[1], "TO") {
		return true
	}
	if len(tokens) >= minTxSavepointTokens &&
		(strings.EqualFold(tokens[1], "TRANSACTION") || strings.EqualFold(tokens[1], "WORK")) &&
		strings.EqualFold(tokens[2], "TO") {
		return true
	}
	return false
}

func isPragmaForeignKeysMutation(tokens []string) bool {
	if len(tokens) < minPragmaAssignTokens || !strings.EqualFold(tokens[0], "PRAGMA") {
		return false
	}
	if strings.EqualFold(tokens[1], "foreign_keys") && isPragmaAssignOp(tokens[2]) {
		return true
	}
	if len(tokens) >= minQualifiedPragmaTokens &&
		tokens[2] == "." &&
		strings.EqualFold(tokens[3], "foreign_keys") &&
		isPragmaAssignOp(tokens[4]) {
		return true
	}
	return false
}

func isPragmaAssignOp(tok string) bool {
	return tok == "=" || tok == "("
}

var sqliteTemplate = template.Must(template.New("sqlite").Parse(`
{{define "exists"}}
PRAGMA table_info({{.}});
{{end}}
{{define "create"}}
create table {{.}} (
	version integer primary key,
	migrated_at text not null default current_timestamp
);
{{end}}
{{define "current_version"}}
select version from {{.}} order by version desc;
{{end}}
`))
