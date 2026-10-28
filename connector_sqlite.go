//go:build sqlite || !none

package migrator

import (
	"fmt"
	"strings"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

// SQLite returns the SQLite connector. It only exists with `-tags sqlite`.
//
// Locking: SQLite already serializes writers at file level, so Lock is a no-op.
// Configure busy_timeout (and, if you run concurrent migrators, immediate
// transactions) in the DSN of your driver.
func SQLite() Connector {
	return specConnector{spec: driverSpec{
		name:       "sqlite",
		split:      sqlsplit.SQLiteDialect(),
		caps:       Capabilities{TransactionalDDL: true, SessionLock: false},
		quoteIdent: nil,
		queries:    sqliteQueries,
		lock:       nil,
		checkTx:    sqliteCheckTx,
	}}
}

func sqliteQueries(t string) queries {
	return queries{
		tableExists:   "select 1 from pragma_table_list where schema = coalesce(nullif(?, ''), 'main') and name = ?",
		createTable:   "CREATE TABLE IF NOT EXISTS " + t + " (version INTEGER NOT NULL)",
		initVersion:   "insert into " + t + " (version) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM " + t + ")",
		selectVersion: "select version from " + t,
		updateVersion: "UPDATE " + t + " SET version = ?",
	}
}

const maxLeadingTokens = 5

// sqliteCheckTx rejects statements that cannot work inside the migrator's transaction.
// A COMMIT inside tx.Exec would silently end the real transaction and leave the
// migration half-applied; PRAGMA foreign_keys is a silent no-op inside a transaction.
func sqliteCheckTx(stmts []sqlsplit.Statement, d sqlsplit.Dialect) error {
	for i, s := range stmts {
		t := sqlsplit.LeadingTokens(s.SQL, d, maxLeadingTokens)
		if len(t) == 0 {
			continue
		}
		var err error
		switch strings.ToUpper(t[0]) {
		case "BEGIN", "COMMIT", "END":
			err = ErrManualTxInTransactedMigration
		case "ROLLBACK":
			if !isRollbackTo(t) {
				err = ErrManualTxInTransactedMigration
			}
		case "PRAGMA":
			if isForeignKeysPragma(t) {
				err = ErrPragmaForeignKeysInTx
			}
		}
		if err != nil {
			return fmt.Errorf("statement %d (line %d): %w", i+1, s.Line, err)
		}
	}
	return nil
}

// isRollbackTo: ROLLBACK TO x / ROLLBACK [TRANSACTION|WORK] TO x are legal inside a transaction.
func isRollbackTo(t []string) bool {
	return (len(t) > 1 && strings.EqualFold(t[1], "TO")) ||
		(len(t) > 2 && strings.EqualFold(t[2], "TO"))
}

// isForeignKeysPragma matches the setting forms only; PRAGMA foreign_keys (query),
// defer_foreign_keys and foreign_key_check are fine inside a transaction.
func isForeignKeysPragma(t []string) bool {
	if len(t) >= 3 && strings.EqualFold(t[1], "foreign_keys") && isAssign(t[2]) {
		return true
	}
	return len(t) >= 5 && t[2] == "." && strings.EqualFold(t[3], "foreign_keys") && isAssign(t[4])
}

func isAssign(tok string) bool { return tok == "=" || tok == "(" }
