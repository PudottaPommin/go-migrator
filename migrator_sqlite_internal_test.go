//go:build sqlite

package migrator

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteMigrator_Migrate(t *testing.T) {
	t.Parallel()

	t.Run("manually added migration", func(t *testing.T) {
		m := newSQLiteMigrator(t)
		defer func() {
			err := m.Close()
			require.NoError(t, err)
		}()

		m.AppendMigration("create table t1 (id integer primary key);", "delete table if exists t1;")
		err := m.Migrate(t.Context())
		require.NoError(t, err)

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, v)
	})
}

func TestSQLiteMigrator_GetCurrentVersion(t *testing.T) {
	t.Parallel()

	m := newSQLiteMigrator(t)
	defer func() {
		err := m.Close()
		require.NoError(t, err)
	}()
	t.Run("without migration table", func(t *testing.T) {
		v, err := m.GetCurrentVersion(t.Context())
		assert.ErrorContains(t, err, "no such table: __migrations")
		assert.Equal(t, -1, v)
	})
	t.Run("without migration", func(t *testing.T) {
		err := m.ensureSchemaVersionTableExists(t.Context())
		require.NoError(t, err)

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, -1, v)
	})
	t.Run("with one migration", func(t *testing.T) {
		err := m.ensureSchemaVersionTableExists(t.Context())
		require.NoError(t, err)

		result, err := m.driver.Conn().ExecContext(t.Context(), "insert into __migrations (version) values (1);")
		require.NoError(t, err)
		assert.NotNil(t, result)
		affected, err := result.RowsAffected()
		require.NoError(t, err)
		assert.Equal(t, int64(1), affected)

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, v)
	})
	t.Run("with two migrations", func(t *testing.T) {
		err := m.ensureSchemaVersionTableExists(t.Context())
		require.NoError(t, err)

		result, err := m.driver.Conn().ExecContext(t.Context(), "insert into __migrations (version) values (2);")
		require.NoError(t, err)
		assert.NotNil(t, result)
		affected, err := result.RowsAffected()
		require.NoError(t, err)
		assert.Equal(t, int64(1), affected)

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 2, v)
	})
}

func newSQLiteMigrator(t testing.TB) *Migrator {
	t.Helper()
	m, err := NewMigrator(t.Context(), newSQLiteConnection(t))
	require.NoError(t, err)
	return m
}

func newSQLiteConnection(t testing.TB) *connectionSQLite {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	assert.NotNil(t, db)

	con, err := db.Conn(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, con)
	return NewConnectionSQLite(DriverSQLite, con).(*connectionSQLite)
}
