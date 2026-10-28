package migrator

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

func newTestMigrator(t *testing.T, opts *Options) (*Migrator, *fakeDB) {
	t.Helper()
	db, f := newFakeDB(t)
	m, err := NewMigratorEx(context.Background(), db, specConnector{spec: fakeSpec(nil)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.AppendMigration("U1", "D1")
	m.AppendMigration("U2", "D2")
	m.AppendMigration("U3", "D3")
	return m, f
}

// scripts returns only the migration scripts that were executed, in order.
func scripts(log []string) []string {
	var out []string
	for _, q := range log {
		if len(q) == 2 && (q[0] == 'U' || q[0] == 'D') {
			out = append(out, q)
		}
	}
	return out
}

func TestMigrateUpAndDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, f := newTestMigrator(t, nil)

	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, scripts(f.snapshot()), []string{"U1", "U2", "U3"})
	if f.version != 3 {
		t.Fatalf("version = %d", f.version)
	}

	if err := m.MigrateTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	eq(t, scripts(f.snapshot()), []string{"U1", "U2", "U3", "D3", "D2"})
	if f.version != 1 {
		t.Fatalf("version = %d", f.version)
	}

	before := len(f.snapshot())
	if err := m.MigrateTo(ctx, 1); err != nil { // already there
		t.Fatal(err)
	}
	for _, q := range f.snapshot()[before:] {
		if strings.HasPrefix(q, "BEGIN") || strings.HasPrefix(q, "UPDATE") {
			t.Errorf("unexpected write when already at target: %s", q)
		}
	}
}

func TestMigrateStopsAtFailureAndKeepsVersion(t *testing.T) {
	t.Parallel()
	m, f := newTestMigrator(t, nil)
	f.failOn = "U2"
	err := m.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration 2 (up)") {
		t.Fatalf("err = %v", err)
	}
	if f.version != 1 {
		t.Errorf("version = %d, want 1", f.version)
	}
}

func TestDownNotReversibleFailsBeforeAnyChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, f := newTestMigrator(t, nil)
	m.Migrations[1].Down = "" // migration 2 cannot be reversed
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(f.snapshot())
	err := m.MigrateTo(ctx, 0)
	if err == nil || !strings.Contains(err.Error(), "migration 2 is not reversible") {
		t.Fatalf("err = %v", err)
	}
	if f.version != 3 {
		t.Errorf("version changed to %d", f.version)
	}
	for _, q := range f.snapshot()[before:] {
		if strings.HasPrefix(q, "D") || q == "BEGIN" {
			t.Errorf("executed %q despite pre-check", q)
		}
	}
}

func TestTargetOutOfRangeTouchesNothing(t *testing.T) {
	t.Parallel()
	m, f := newTestMigrator(t, nil)
	if err := m.MigrateTo(context.Background(), 9); err == nil {
		t.Fatal("expected error")
	}
	eq(t, f.snapshot(), nil)
}

func TestDisableTxRunsPlain(t *testing.T) {
	t.Parallel()
	m, f := newTestMigrator(t, &Options{DisableTx: true})
	if err := m.MigrateTo(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.snapshot() {
		if q == "BEGIN" || q == "COMMIT" {
			t.Errorf("transaction used despite DisableTx: %v", f.snapshot())
		}
	}
}

func TestPerMigrationDisableTx(t *testing.T) {
	t.Parallel()

	m, f := newTestMigrator(t, nil)
	m.Migrations[1].DisableTx = true

	if err := m.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	eq(t, f.snapshot(), []string{
		`CREATE "__migrations"`,
		`INIT "__migrations"`,
		`SELECT "__migrations"`,
		"BEGIN", "U1", `UPDATE "__migrations"`, "COMMIT",
		"U2", `UPDATE "__migrations"`,
		"BEGIN", "U3", `UPDATE "__migrations"`, "COMMIT",
	})
}

func TestValidateRejectsCommentOnlyDown(t *testing.T) {
	t.Parallel()

	m, _ := newTestMigrator(t, nil)
	m.Migrations[1].Down = "-- comment only\n/* block comment */ ;"
	err := m.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration 2: down script contains no SQL") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	t.Parallel()

	t.Run("valid files with and without disable-tx directive", func(t *testing.T) {
		t.Parallel()

		m := &Migrator{}
		fsys := fstest.MapFS{
			"1_init.sql": {
				Data: []byte("-- header comment\nCREATE TABLE t1 (id INT);\n" +
					UpDownSnippet + "\nDROP TABLE t1;\n"),
			},
			"2_rebuild.sql": {
				Data: []byte(
					"\xef\xbb\xbf-- migrator:disable-tx\n-- rebuild table\nALTER TABLE t1 ADD COLUMN c INT;\n",
				),
			},
		}
		if err := m.LoadMigrations(fsys); err != nil {
			t.Fatal(err)
		}
		if len(m.Migrations) != 2 {
			t.Fatalf("len = %d", len(m.Migrations))
		}
		if m.Migrations[0].DisableTx || !m.Migrations[0].reversible() {
			t.Errorf("mig 1 = %+v", m.Migrations[0])
		}
		if !m.Migrations[1].DisableTx || m.Migrations[1].reversible() {
			t.Errorf("mig 2 = %+v", m.Migrations[1])
		}
	})

	t.Run("rejects invalid Up, Down, and directives", func(t *testing.T) {
		t.Parallel()

		cases := map[string]string{
			"comment-only up":           "-- only comment\n",
			"block-comment-only up":     "/* only block comment */ ; ;",
			"empty down with separator": "CREATE TABLE t1 (id INT);\n" + UpDownSnippet + "\n   \n",
			"comment-only down":         "CREATE TABLE t1 (id INT);\n" + UpDownSnippet + "\n-- nothing here\n/* nor here */ ;",
			"unknown directive":         "-- migrator:no-tx\nCREATE TABLE t1 (id INT);",
			"directive after SQL":       "CREATE TABLE t1 (id INT);\n-- migrator:disable-tx\n",
			"directive in down section": "CREATE TABLE t1 (id INT);\n" + UpDownSnippet + "\n-- migrator:disable-tx\nDROP TABLE t1;",
		}
		for name, content := range cases {
			m := &Migrator{}
			fsys := fstest.MapFS{"1_test.sql": {Data: []byte(content)}}
			if err := m.LoadMigrations(fsys); err == nil {
				t.Errorf("%s: expected error, got nil", name)
			}
		}
	})
}

func TestMigrateWithoutMigrationsDoesNotPanic(t *testing.T) {
	t.Parallel()
	db, _ := newFakeDB(t)
	m, err := NewMigrator(context.Background(), db, specConnector{spec: fakeSpec(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if merr := m.Migrate(context.Background()); merr != nil {
		t.Fatal(merr)
	}
}

func TestFindMigrations(t *testing.T) {
	t.Parallel()
	m := &Migrator{}
	ok := fstest.MapFS{}
	for _, n := range []string{"1_a.sql", "2_b.sql", "10_j.sql", "3_c.sql", "4_d.sql", "5_e.sql", "6_f.sql", "7_g.sql", "8_h.sql", "9_i.sql", "readme.md"} {
		ok[n] = &fstest.MapFile{Data: []byte("x")}
	}
	paths, err := m.FindMigrations(ok)
	if err != nil || len(paths) != 10 || paths[1] != "2_b.sql" || paths[9] != "10_j.sql" {
		t.Fatalf("paths = %v, err = %v", paths, err)
	}

	for name, fsys := range map[string]fstest.MapFS{
		"zero version":      {"0_init.sql": {Data: []byte("x")}},
		"timestamp version": {"20240101120000_init.sql": {Data: []byte("x")}},
		"gap":               {"1_a.sql": {Data: []byte("x")}, "3_c.sql": {Data: []byte("x")}, "4_d.sql": {Data: []byte("x")}},
		"duplicate":         {"1_a.sql": {Data: []byte("x")}, "01_b.sql": {Data: []byte("x")}},
	} {
		if _, ferr := m.FindMigrations(fsys); ferr == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
