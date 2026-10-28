//go:build sqlite || !none

package migrator

import (
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestSQLiteMigrator_Migrate(t *testing.T) {
	t.Parallel()

	t.Run("manually added migration up and down", func(t *testing.T) {
		t.Parallel()

		m, conn := newSQLiteMigrator(t)

		m.AppendMigration(
			"create table t1 (id integer primary key, begin TEXT, end TEXT);",
			"drop table IF exists t1;",
		)
		require.NoError(t, m.Migrate(t.Context()))

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, v)

		require.NoError(t, m.MigrateTo(t.Context(), 0))

		v, err = m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 0, v)

		var count int
		err = conn.QueryRowContext(
			t.Context(),
			"select count(*) from sqlite_master where type = 'table' and name = 't1'",
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("loaded migrations with per-migration disable-tx and trigger", func(t *testing.T) {
		t.Parallel()

		m, conn := newSQLiteMigrator(t)

		fsys := fstest.MapFS{
			"1_create_ranges.sql": {
				Data: []byte(`
create table ranges (
	id integer primary key,
	begin TEXT not null,
	end TEXT not null
);
create table audit_log (
	id integer primary key AUTOINCREMENT,
	range_id integer not null,
	end TEXT not null
);
create trigger tr_ranges after update on ranges
begin
	update ranges set end = NEW.end, begin = NEW.begin where id = NEW.id;
	insert into audit_log (range_id, end) values (NEW.id, NEW.end);
end;
---- up above / down below ----
drop trigger tr_ranges;
drop table audit_log;
drop table ranges;
`),
			},
			"2_rebuild_with_pragma_fk_off.sql": {
				Data: []byte(`-- migrator:disable-tx
PRAGMA foreign_keys = OFF;
BEGIN;
INSERT INTO ranges (id, begin, end) VALUES (1, '2026-01-01', '2026-06-01');
UPDATE ranges SET end = '2026-12-31' WHERE id = 1;
COMMIT;
PRAGMA foreign_keys = ON;
`),
			},
		}

		require.NoError(t, m.LoadMigrations(fsys))
		require.NoError(t, m.Migrate(t.Context()))

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 2, v)

		var gotEnd string
		err = conn.QueryRowContext(t.Context(), "select end from audit_log where range_id = 1").Scan(&gotEnd)
		require.NoError(t, err)
		assert.Equal(t, "2026-12-31", gotEnd)
	})
}

func TestSQLiteMigrator_GetCurrentVersion(t *testing.T) {
	t.Parallel()

	t.Run("without migration table returns 0 without creating table", func(t *testing.T) {
		t.Parallel()

		m, conn := newSQLiteMigrator(t)

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 0, v)

		var count int
		err = conn.QueryRowContext(
			t.Context(),
			"select count(*) from sqlite_master where type = 'table' and name = ?",
			DefaultVersionTable,
		).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("after EnsureTable with no applied migrations", func(t *testing.T) {
		t.Parallel()

		m, _ := newSQLiteMigrator(t)

		require.NoError(t, m.conn.EnsureTable(t.Context()))

		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 0, v)
	})

	t.Run("with one and two migrations applied", func(t *testing.T) {
		t.Parallel()

		m, _ := newSQLiteMigrator(t)
		m.AppendMigration("create table t1 (id integer primary key);", "drop table t1;")
		m.AppendMigration("create table t2 (id integer primary key);", "drop table t2;")

		require.NoError(t, m.MigrateTo(t.Context(), 1))
		v, err := m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, v)

		require.NoError(t, m.MigrateTo(t.Context(), 2))
		v, err = m.GetCurrentVersion(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 2, v)
	})
}

func TestSQLiteMigrator_SchemaQualifiedVersionTable(t *testing.T) {
	t.Parallel()

	for _, versionTable := range []string{`"aux"."__migrations"`, "aux.__migrations"} {
		t.Run(versionTable, func(t *testing.T) {
			t.Parallel()

			db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, db.Close())
			})

			m, err := NewMigratorEx(t.Context(), db, SQLite(), &Options{
				VersionTable: versionTable,
				DisableTx:    false,
			})
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, m.Close())
			})

			sc, ok := m.conn.(*specConn)
			require.True(t, ok)

			_, err = sc.conn.ExecContext(t.Context(), "ATTACH DATABASE ':memory:' AS aux")
			require.NoError(t, err)

			v, err := m.GetCurrentVersion(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 0, v)

			var mainCount, auxCount int
			err = sc.conn.QueryRowContext(
				t.Context(),
				"select count(*) from main.sqlite_master where type = 'table' and name = '__migrations'",
			).Scan(&mainCount)
			require.NoError(t, err)
			assert.Equal(t, 0, mainCount)

			err = sc.conn.QueryRowContext(
				t.Context(),
				"select count(*) from aux.sqlite_master where type = 'table' and name = '__migrations'",
			).Scan(&auxCount)
			require.NoError(t, err)
			assert.Equal(t, 0, auxCount)

			m.AppendMigration("create table t1 (id integer primary key);", "drop table t1;")
			require.NoError(t, m.Migrate(t.Context()))

			err = sc.conn.QueryRowContext(
				t.Context(),
				"select count(*) from main.sqlite_master where type = 'table' and name = '__migrations'",
			).Scan(&mainCount)
			require.NoError(t, err)
			assert.Equal(t, 0, mainCount)

			err = sc.conn.QueryRowContext(
				t.Context(),
				"select count(*) from aux.sqlite_master where type = 'table' and name = '__migrations'",
			).Scan(&auxCount)
			require.NoError(t, err)
			assert.Equal(t, 1, auxCount)

			v, err = m.GetCurrentVersion(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, v)
		})
	}
}

func newSQLiteMigrator(t testing.TB) (*Migrator, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	m, err := NewMigrator(t.Context(), db, SQLite())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, m.Close())
	})
	sc, ok := m.conn.(*specConn)
	require.True(t, ok)
	return m, sc.conn
}
