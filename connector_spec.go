package migrator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

// This file is untagged: the whole execution flow is compiled and unit-tested
// without any driver tag. Tagged files only supply a driverSpec.

const cleanupTimeout = 5 * time.Second

var errInvalidMigrationsTable = errors.New("invalid migrations table name")

// queries holds the engine-specific SQL. Table names arrive already quoted.
type queries struct {
	tableExists   string // optional: 2-arg query (schema, unquoted table); sql.ErrNoRows means table does not exist
	createTable   string
	initVersion   string // idempotent: inserts the single row with version 0 if the table is empty
	selectVersion string // one row, one column
	updateVersion string // args: version; must affect exactly one row
}

type lockFunc func(ctx context.Context, conn *sql.Conn, table string) (unlock func(context.Context) error, err error)

// driverSpec is the declarative description of one engine.
type driverSpec struct {
	name       string
	split      sqlsplit.Dialect
	caps       Capabilities
	quoteIdent func(string) string                                        // nil: "double quotes"
	queries    func(quotedTable string) queries                           // required
	lock       lockFunc                                                   // nil: no locking
	checkTx    func(stmts []sqlsplit.Statement, d sqlsplit.Dialect) error // nil: no extra validation
}

func quoteDouble(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// parseTableName splits a version-table option into unquoted identifiers.
// Empty raw returns "", DefaultVersionTable, nil.
func parseTableName(raw string) (string, string, error) {
	if raw == "" {
		return "", DefaultVersionTable, nil
	}
	if strings.ContainsRune(raw, 0) {
		return "", "", errInvalidMigrationsTable
	}
	first, rest, err := nextTableIdent(raw)
	if err != nil {
		return "", "", err
	}
	if rest == "" {
		return "", first, nil
	}
	if rest[0] != '.' {
		return "", "", errInvalidMigrationsTable
	}
	second, tail, err := nextTableIdent(rest[1:])
	if err != nil || tail != "" {
		return "", "", errInvalidMigrationsTable
	}
	return first, second, nil
}

// nextTableIdent reads one part. A leading '"' is a quoted identifier
// ('""' unescapes to '"') and must be closed. Any other part runs until '.' or end.
func nextTableIdent(s string) (string, string, error) {
	if s == "" {
		return "", "", errInvalidMigrationsTable
	}
	if s[0] != '"' {
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			return s, "", nil
		}
		if dot == 0 {
			return "", "", errInvalidMigrationsTable
		}
		return s[:dot], s[dot:], nil
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		if b.Len() == 0 {
			return "", "", errInvalidMigrationsTable
		}
		return b.String(), s[i+1:], nil
	}
	return "", "", errInvalidMigrationsTable
}

// formatTableIdent quotes table, or quote(schema)+"."+quote(table).
// quote must be non-nil; Connect substitutes quoteDouble when quoteIdent is nil.
func formatTableIdent(quote func(string) string, schema, table string) string {
	if schema == "" {
		return quote(table)
	}
	return quote(schema) + "." + quote(table)
}

type specConnector struct{ spec driverSpec }

func (s specConnector) Name() string { return s.spec.name }

func (s specConnector) Connect(ctx context.Context, db *sql.DB, opts ConnOptions) (Conn, error) {
	schema, table, err := parseTableName(opts.Table)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.spec.name, errInvalidMigrationsTable)
	}
	quote := s.spec.quoteIdent
	if quote == nil {
		quote = quoteDouble
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: pin connection: %w", s.spec.name, err)
	}
	return &specConn{
		spec:   s.spec,
		conn:   conn,
		schema: schema,
		table:  table,
		q:      s.spec.queries(formatTableIdent(quote, schema, table)),
		locked: false,
	}, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type specConn struct {
	spec   driverSpec
	conn   *sql.Conn
	schema string
	table  string
	q      queries
	locked bool
}

func (c *specConn) lockIdent() string {
	if c.schema == "" {
		return c.table
	}
	return c.schema + "." + c.table
}

func (c *specConn) Capabilities() Capabilities { return c.spec.caps }

func (c *specConn) Lock(ctx context.Context) (func(context.Context) error, error) {
	if c.spec.lock == nil {
		return func(context.Context) error { return nil }, nil
	}
	unlock, err := c.spec.lock(ctx, c.conn, c.lockIdent())
	if err != nil {
		return nil, err
	}
	c.locked = true
	return func(ctx context.Context) error {
		// The run may have been cancelled; releasing the lock must still work.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if uerr := unlock(ctx); uerr != nil {
			return uerr
		}
		c.locked = false
		return nil
	}, nil
}

func (c *specConn) EnsureTable(ctx context.Context) error {
	for _, q := range []string{c.q.createTable, c.q.initVersion} {
		if _, err := c.conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: prepare version table: %w", c.spec.name, err)
		}
	}
	return nil
}

func (c *specConn) CurrentVersion(ctx context.Context) (int, error) {
	var v int
	err := c.conn.QueryRowContext(ctx, c.q.selectVersion).Scan(&v)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if c.q.tableExists != "" {
		var exists int
		if existsErr := c.conn.QueryRowContext(ctx, c.q.tableExists, c.schema, c.table).Scan(&exists); errors.Is(
			existsErr,
			sql.ErrNoRows,
		) {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("%s: read current version: %w", c.spec.name, err)
}

func (c *specConn) Apply(ctx context.Context, step Step) error {
	stmts, err := sqlsplit.Split(step.SQL, c.spec.split)
	if err != nil {
		return err
	}
	if step.UseTx {
		return c.applyTx(ctx, step, stmts)
	}
	return c.applyPlain(ctx, step, stmts)
}

func (c *specConn) applyTx(ctx context.Context, step Step, stmts []sqlsplit.Statement) error {
	if c.spec.checkTx != nil {
		if err := c.spec.checkTx(stmts, c.spec.split); err != nil {
			return err
		}
	}
	tx, err := c.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if serr := runStatements(ctx, tx, stmts); serr != nil {
		_ = tx.Rollback()
		return serr
	}
	if verr := c.setVersion(ctx, tx, step.NewVersion); verr != nil {
		_ = tx.Rollback()
		return verr
	}
	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("commit: %w", cerr)
	}
	return nil
}

// applyPlain runs without an automatic transaction. The version update is a separate
// statement, so a crash between script and update leaves the migration applied but unrecorded.
func (c *specConn) applyPlain(ctx context.Context, step Step, stmts []sqlsplit.Statement) error {
	if err := runStatements(ctx, c.conn, stmts); err != nil {
		c.rollbackQuietly(ctx)
		return err
	}
	return c.setVersion(ctx, c.conn, step.NewVersion)
}

// rollbackQuietly makes sure a script that did its own BEGIN cannot leave the pinned
// connection inside an open transaction. Errors ("no transaction is active") are expected.
func (c *specConn) rollbackQuietly(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = c.conn.ExecContext(ctx, "ROLLBACK")
}

func (c *specConn) setVersion(ctx context.Context, ex execer, v int) error {
	res, err := ex.ExecContext(ctx, c.q.updateVersion, v)
	if err != nil {
		return fmt.Errorf("update version: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update version: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("update version: version row missing or duplicated (%d rows affected)", n)
	}
	return nil
}

func runStatements(ctx context.Context, ex execer, stmts []sqlsplit.Statement) error {
	for i, s := range stmts {
		if _, err := ex.ExecContext(ctx, s.SQL); err != nil {
			return fmt.Errorf("statement %d (line %d): %w", i+1, s.Line, err)
		}
	}
	return nil
}

func (c *specConn) Close() error {
	if c.locked {
		// A session-level lock is still held (unlock failed or was skipped). Never hand
		// such a connection back to the pool: discard it so the server ends the session.
		_ = c.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	if err := c.conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return err
	}
	return nil
}
