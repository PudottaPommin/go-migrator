//go:build sqlite

package migrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

func TestSQLiteConnection_tableExists(t *testing.T) {
	t.Parallel()

	con := newSQLiteConnection(t)
	defer func() {
		require.NoError(t, con.Close())
	}()

	ok, err := con.tableExists(t.Context(), DefaultVersionTable)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestSQLiteConnection_ensureMigrationTable(t *testing.T) {
	t.Parallel()

	con := newSQLiteConnection(t)
	defer func() {
		require.NoError(t, con.Close())
	}()

	ok, err := con.tableExists(t.Context(), DefaultVersionTable)
	require.NoError(t, err)
	assert.False(t, ok)

	err = con.ensureMigrationTable(t.Context(), DefaultVersionTable)
	require.NoError(t, err)

	ok, err = con.tableExists(t.Context(), DefaultVersionTable)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestSQLiteConnection_runSQL_TriggerAndSchema(t *testing.T) {
	t.Parallel()

	con := newSQLiteConnection(t)
	defer func() {
		require.NoError(t, con.Close())
	}()

	migrationSQL := `
-- Create ranges table with unquoted begin/end columns
CREATE TABLE ranges (
	id INTEGER PRIMARY KEY,
	begin TEXT NOT NULL,
	end TEXT NOT NULL
);

CREATE TABLE audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	range_id INTEGER NOT NULL,
	end TEXT NOT NULL,
	label TEXT NOT NULL
);

CREATE INDEX idx_ranges_end ON ranges (end);

CREATE TRIGGER tr_ranges_update AFTER UPDATE ON ranges
WHEN CASE WHEN NEW.end != OLD.end THEN 1 ELSE 0 END = 1
BEGIN
	UPDATE ranges SET end = NEW.end, begin = NEW.begin WHERE id = NEW.id;
	INSERT INTO audit_log (range_id, end, label)
	VALUES (NEW.id, NEW.end, CASE WHEN NEW.end > NEW.begin THEN 'valid;range' ELSE 'empty' END);
END;

INSERT INTO ranges (id, begin, end) VALUES (1, '2026-01-01', '2026-01-02');
UPDATE ranges SET end = '2026-12-31' WHERE id = 1;
`

	err := con.runSQL(t.Context(), migrationSQL, true)
	require.NoError(t, err)

	var objectCount int
	err = con.conn.QueryRowContext(
		t.Context(),
		"SELECT COUNT(*) FROM sqlite_master WHERE name IN ('ranges', 'audit_log', 'idx_ranges_end', 'tr_ranges_update')",
	).Scan(&objectCount)
	require.NoError(t, err)
	assert.Equal(t, 4, objectCount)

	var gotEnd, gotLabel string
	err = con.conn.QueryRowContext(
		t.Context(),
		"SELECT end, label FROM audit_log WHERE range_id = 1",
	).Scan(&gotEnd, &gotLabel)
	require.NoError(t, err)
	assert.Equal(t, "2026-12-31", gotEnd)
	assert.Equal(t, "valid;range", gotLabel)
}

func TestSQLiteConnection_runSQL_ValidationAndTransactions(t *testing.T) {
	t.Parallel()

	t.Run("empty or comment-only query is a no-op", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		err := con.runSQL(t.Context(), " -- just a comment \n /* block */ ; ", true)
		require.NoError(t, err)
	})

	t.Run("syntax error is returned from splitter", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		err := con.runSQL(t.Context(), "SELECT 'unclosed", true)
		require.ErrorIs(t, err, sqlsplit.ErrUnclosedString)
	})

	t.Run("useTx allows SAVEPOINT, ROLLBACK TO, defer_foreign_keys, and foreign_key_check", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		query := `
PRAGMA defer_foreign_keys = ON;
PRAGMA foreign_key_check;
CREATE TABLE items (id INTEGER PRIMARY KEY);
SAVEPOINT sp1;
INSERT INTO items (id) VALUES (1);
ROLLBACK TRANSACTION TO SAVEPOINT sp1;
INSERT INTO items (id) VALUES (2);
RELEASE SAVEPOINT sp1;
`
		err := con.runSQL(t.Context(), query, true)
		require.NoError(t, err)

		var count int
		err = con.conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM items WHERE id = 2").Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})

	t.Run("useTx rejects manual transaction control statements", func(t *testing.T) {
		t.Parallel()

		rejected := []string{
			"SELECT 1;\nBEGIN TRANSACTION;",
			"SELECT 1;\nCOMMIT;",
			"SELECT 1;\nEND;",
			"SELECT 1;\nROLLBACK;",
			"SELECT 1;\nROLLBACK WORK;",
		}

		for _, sqlText := range rejected {
			con := newSQLiteConnection(t)
			err := con.runSQL(t.Context(), sqlText, true)
			require.ErrorIs(t, err, ErrManualTxInTransactedMigration)
			assert.Contains(t, err.Error(), "statement 2 (line 2)")
			require.NoError(t, con.Close())
		}
	})

	t.Run("useTx rejects PRAGMA foreign_keys across quoting, casing, and schema qualification", func(t *testing.T) {
		t.Parallel()

		rejected := []string{
			"SELECT 1;\nPRAGMA foreign_keys = OFF;",
			"SELECT 1;\nPRAGMA 'FOREIGN_KEYS' = ON;",
			"SELECT 1;\nPRAGMA [Foreign_Keys] = OFF;",
			"SELECT 1;\nPRAGMA \"foreign_keys\"(0);",
			"SELECT 1;\nPRAGMA `foreign_keys` = 1;",
			"SELECT 1;\nPRAGMA /* comment */ main.'foreign_keys' = OFF;",
		}

		for _, sqlText := range rejected {
			con := newSQLiteConnection(t)
			err := con.runSQL(t.Context(), sqlText, true)
			require.ErrorIs(t, err, ErrPragmaForeignKeysInTx)
			assert.Contains(t, err.Error(), "statement 2 (line 2)")
			require.NoError(t, con.Close())
		}
	})

	t.Run("useTx rolls back earlier statements on failure", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		err := con.runSQL(t.Context(), "CREATE TABLE t_rb (id INT);\nINSERT INTO nonexistent_table VALUES (1);", true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "statement 2 (line 2) failed")

		ok, err := con.tableExists(t.Context(), "t_rb")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("useTx false executes manual table rebuild with PRAGMA foreign_keys", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		rebuildSQL := `
PRAGMA 'foreign_keys' = OFF;
BEGIN;
CREATE TABLE users_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
INSERT INTO users_new (id, name) VALUES (1, 'Ada');
COMMIT;
PRAGMA [foreign_keys] = ON;
`
		err := con.runSQL(t.Context(), rebuildSQL, false)
		require.NoError(t, err)

		ok, err := con.tableExists(t.Context(), "users_new")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("useTx false detects uncommitted transaction left open by script", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		err := con.runSQL(t.Context(), "BEGIN;\nCREATE TABLE dangling (id INT);", false)
		require.ErrorIs(t, err, ErrUncommittedTx)

		// Verify the connection was rolled back and can start a new transaction cleanly.
		err = con.runSQL(t.Context(), "BEGIN;\nCREATE TABLE clean_after_dangling (id INT);\nCOMMIT;", false)
		require.NoError(t, err)
	})

	t.Run("useTx false cleans up open transaction even when context is cancelled", func(t *testing.T) {
		t.Parallel()

		con := newSQLiteConnection(t)
		defer func() {
			require.NoError(t, con.Close())
		}()

		// Start a transaction on the pinned connection, then invoke runSQL with an already-cancelled
		// context so statement execution fails with context.Canceled while inside the open transaction.
		_, err := con.conn.ExecContext(t.Context(), "BEGIN")
		require.NoError(t, err)

		canceledCtx, cancel := context.WithCancel(t.Context())
		cancel()

		err = con.runSQL(canceledCtx, "CREATE TABLE should_not_run (id INT);", false)
		require.ErrorIs(t, err, context.Canceled)

		// Because rollbackBestEffort uses context.WithoutCancel(ctx), the open transaction on
		// con.conn must have been rolled back, allowing a new transaction to begin cleanly.
		err = con.runSQL(t.Context(), "BEGIN;\nCREATE TABLE after_cancel (id INT);\nCOMMIT;", false)
		require.NoError(t, err)
	})
}
