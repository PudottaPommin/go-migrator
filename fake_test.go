package migrator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

// Minimal fake database/sql driver: records every statement, keeps the version row,
// and can be told to fail on statements containing a marker.

type fakeDB struct {
	mu      sync.Mutex
	log     []string
	failOn  string
	version int64
	noRows  bool // UPDATE affects 0 rows
}

func (f *fakeDB) add(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func (f *fakeDB) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeDB) exec(q string, args []driver.NamedValue) (driver.Result, error) {
	f.add(q)
	if f.failOn != "" && strings.Contains(q, f.failOn) {
		return nil, errors.New("boom")
	}
	if strings.HasPrefix(q, "UPDATE") {
		if f.noRows {
			return driver.RowsAffected(0), nil
		}
		f.mu.Lock()
		f.version = args[0].Value.(int64)
		f.mu.Unlock()
	}
	return driver.RowsAffected(1), nil
}

type fakeConnector struct{ db *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c.db}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return nil }

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { c.db.add("BEGIN"); return fakeTx{c.db}, nil }

func (c *fakeConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.db.exec(q, a)
}

func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.db.add(q)
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return &fakeRows{v: c.db.version}, nil
}

type fakeTx struct{ db *fakeDB }

func (t fakeTx) Commit() error   { t.db.add("COMMIT"); return nil }
func (t fakeTx) Rollback() error { t.db.add("ROLLBACK"); return nil }

type fakeRows struct {
	v    int64
	done bool
}

func (r *fakeRows) Columns() []string { return []string{"version"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.v
	return nil
}

func fakeSpec(check func([]sqlsplit.Statement, sqlsplit.Dialect) error) driverSpec {
	return driverSpec{
		name:       "fake",
		split:      sqlsplit.SQLiteDialect(),
		caps:       Capabilities{TransactionalDDL: true, SessionLock: false},
		quoteIdent: nil,
		queries: func(tbl string) queries {
			return queries{
				createTable:   "CREATE " + tbl,
				initVersion:   "INIT " + tbl,
				selectVersion: "SELECT " + tbl,
				updateVersion: "UPDATE " + tbl,
			}
		},
		lock:    nil,
		checkTx: check,
	}
}

func newFakeDB(t *testing.T) (*sql.DB, *fakeDB) {
	t.Helper()
	f := &fakeDB{}
	db := sql.OpenDB(fakeConnector{f})
	t.Cleanup(func() { _ = db.Close() })
	return db, f
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}
