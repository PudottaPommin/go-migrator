package migrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

func newTestConn(t *testing.T, check func([]sqlsplit.Statement, sqlsplit.Dialect) error) (Conn, *fakeDB) {
	t.Helper()
	db, f := newFakeDB(t)
	conn, err := specConnector{spec: fakeSpec(check)}.Connect(context.Background(), db, ConnOptions{Table: `we"ird`})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, f
}

func TestApplyTxCommits(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(t, nil)
	if err := c.Apply(context.Background(), Step{SQL: "S1; S2;", NewVersion: 1, UseTx: true}); err != nil {
		t.Fatal(err)
	}
	eq(t, f.snapshot(), []string{"BEGIN", "S1", "S2", `UPDATE "we""ird"`, "COMMIT"})
	if f.version != 1 {
		t.Errorf("version = %d", f.version)
	}
}

func TestApplyTxRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(t, nil)
	f.failOn = "FAIL"
	err := c.Apply(context.Background(), Step{SQL: "S1; FAIL; S3", NewVersion: 1, UseTx: true})
	if err == nil || !strings.Contains(err.Error(), "statement 2") {
		t.Fatalf("err = %v", err)
	}
	eq(t, f.snapshot(), []string{"BEGIN", "S1", "FAIL", "ROLLBACK"})
}

func TestApplyTxRollsBackWhenVersionRowMissing(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(t, nil)
	f.noRows = true
	if err := c.Apply(context.Background(), Step{SQL: "S1", NewVersion: 1, UseTx: true}); err == nil {
		t.Fatal("expected error")
	}
	eq(t, f.snapshot(), []string{"BEGIN", "S1", `UPDATE "we""ird"`, "ROLLBACK"})
}

func TestApplyPlainCleansUpOnFailure(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(t, nil)
	f.failOn = "FAIL"
	if err := c.Apply(context.Background(), Step{SQL: "BEGIN; FAIL", NewVersion: 1, UseTx: false}); err == nil {
		t.Fatal("expected error")
	}
	eq(t, f.snapshot(), []string{"BEGIN", "FAIL", "ROLLBACK"})
}

func TestCheckTxRunsBeforeAnythingExecutes(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(
		t,
		func([]sqlsplit.Statement, sqlsplit.Dialect) error { return ErrManualTxInTransactedMigration },
	)
	err := c.Apply(context.Background(), Step{SQL: "S1", NewVersion: 1, UseTx: true})
	if !errors.Is(err, ErrManualTxInTransactedMigration) {
		t.Fatalf("err = %v", err)
	}
	eq(t, f.snapshot(), nil)
}

func TestEnsureTableAndCurrentVersion(t *testing.T) {
	t.Parallel()
	c, f := newTestConn(t, nil)
	f.version = 7
	if err := c.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := c.CurrentVersion(context.Background())
	if err != nil || v != 7 {
		t.Fatalf("v = %d, err = %v", v, err)
	}
	eq(t, f.snapshot(), []string{`CREATE "we""ird"`, `INIT "we""ird"`, `SELECT "we""ird"`})
}

func TestQuoteDouble(t *testing.T) {
	t.Parallel()
	if got := quoteDouble(`a"b`); got != `"a""b"` {
		t.Errorf("got %s", got)
	}
}

func TestParseTableName(t *testing.T) {
	t.Parallel()
	valid := []struct {
		in         string
		wantSchema string
		wantTable  string
	}{
		{in: "", wantSchema: "", wantTable: DefaultVersionTable},
		{in: "__migrations", wantSchema: "", wantTable: "__migrations"},
		{in: `"__migrations"`, wantSchema: "", wantTable: "__migrations"},
		{in: `we"ird`, wantSchema: "", wantTable: `we"ird`},
		{in: "myschema.__migrations", wantSchema: "myschema", wantTable: "__migrations"},
		{in: `"myschema"."__migrations"`, wantSchema: "myschema", wantTable: "__migrations"},
		{in: `myschema."__migrations"`, wantSchema: "myschema", wantTable: "__migrations"},
		{in: `"my.schema"."my.table"`, wantSchema: "my.schema", wantTable: "my.table"},
		{in: `"we""ird"."ta""ble"`, wantSchema: `we"ird`, wantTable: `ta"ble`},
	}
	for _, tc := range valid {
		schema, table, err := parseTableName(tc.in)
		if err != nil {
			t.Errorf("parseTableName(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if schema != tc.wantSchema || table != tc.wantTable {
			t.Errorf("parseTableName(%q) = (%q, %q), want (%q, %q)", tc.in, schema, table, tc.wantSchema, tc.wantTable)
		}
	}

	invalid := []string{
		".table",
		"schema.",
		".",
		"a.b.c",
		`"a"."b".c`,
		`""."table"`,
		`"schema".""`,
		`""`,
		`"unclosed`,
		`"schema"table`,
		`"a"."b"extra`,
		"bad\x00name",
	}
	for _, in := range invalid {
		if _, _, err := parseTableName(in); !errors.Is(err, errInvalidMigrationsTable) {
			t.Errorf("parseTableName(%q) err = %v, want %v", in, err, errInvalidMigrationsTable)
		}
	}
}

func TestFormatTableIdent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		schema string
		table  string
		want   string
	}{
		{schema: "", table: "__migrations", want: `"__migrations"`},
		{schema: "myschema", table: "__migrations", want: `"myschema"."__migrations"`},
		{schema: "my.schema", table: "my.table", want: `"my.schema"."my.table"`},
		{schema: `we"ird`, table: `ta"ble`, want: `"we""ird"."ta""ble"`},
	}
	for _, tc := range cases {
		if got := formatTableIdent(quoteDouble, tc.schema, tc.table); got != tc.want {
			t.Errorf("formatTableIdent(%q, %q) = %s, want %s", tc.schema, tc.table, got, tc.want)
		}
	}
}

func TestLockIdent(t *testing.T) {
	t.Parallel()
	unqualified := &specConn{table: "__migrations"}
	if got := unqualified.lockIdent(); got != "__migrations" {
		t.Errorf("unqualified lockIdent() = %q, want %q", got, "__migrations")
	}
	qualified := &specConn{schema: "myschema", table: "__migrations"}
	if got := qualified.lockIdent(); got != "myschema.__migrations" {
		t.Errorf("qualified lockIdent() = %q, want %q", got, "myschema.__migrations")
	}
}

func TestParseAndQuoteSchemaTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		table      string
		wantQuoted string
	}{
		{table: "myschema.__migrations", wantQuoted: `"myschema"."__migrations"`},
		{table: `"myschema"."__migrations"`, wantQuoted: `"myschema"."__migrations"`},
		{table: `myschema."__migrations"`, wantQuoted: `"myschema"."__migrations"`},
		{table: `"my.schema"."my.table"`, wantQuoted: `"my.schema"."my.table"`},
		{table: `"we""ird"."ta""ble"`, wantQuoted: `"we""ird"."ta""ble"`},
	}
	for _, tc := range cases {
		db, f := newFakeDB(t)
		conn, err := specConnector{spec: fakeSpec(nil)}.Connect(context.Background(), db, ConnOptions{Table: tc.table})
		if err != nil {
			t.Fatalf("Connect(%q): %v", tc.table, err)
		}
		if e := conn.EnsureTable(context.Background()); e != nil {
			t.Fatalf("EnsureTable(%q): %v", tc.table, e)
		}
		if _, e := conn.CurrentVersion(context.Background()); e != nil {
			t.Fatalf("CurrentVersion(%q): %v", tc.table, e)
		}
		if e := conn.Apply(context.Background(), Step{SQL: "S1", NewVersion: 1, UseTx: false}); e != nil {
			t.Fatalf("Apply(%q): %v", tc.table, e)
		}
		_ = conn.Close()
		eq(t, f.snapshot(), []string{
			"CREATE " + tc.wantQuoted,
			"INIT " + tc.wantQuoted,
			"SELECT " + tc.wantQuoted,
			"S1",
			"UPDATE " + tc.wantQuoted,
		})
	}

	invalid := []string{
		".table",
		"schema.",
		".",
		"a.b.c",
		`""."table"`,
		`"schema".""`,
		`"unclosed`,
		"bad\x00name",
	}
	for _, in := range invalid {
		db, f := newFakeDB(t)
		_, err := specConnector{spec: fakeSpec(nil)}.Connect(context.Background(), db, ConnOptions{Table: in})
		if !errors.Is(err, errInvalidMigrationsTable) {
			t.Errorf("Connect(%q) err = %v, want %v", in, err, errInvalidMigrationsTable)
		}
		eq(t, f.snapshot(), nil)
	}
}
