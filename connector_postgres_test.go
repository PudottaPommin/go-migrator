//go:build postgres || !none

package migrator

import (
	"strings"
	"testing"
)

func TestPostgresLockKey(t *testing.T) {
	t.Parallel()
	a, b := postgresLockKey("schema_migrations"), postgresLockKey("other")
	if a < 0 || b < 0 || a == b || a != postgresLockKey("schema_migrations") {
		t.Errorf("bad keys: %d %d", a, b)
	}
	sa := postgresLockKey("schema_a.__migrations")
	sb := postgresLockKey("schema_b.__migrations")
	unq := postgresLockKey("__migrations")
	if sa < 0 || sb < 0 || unq < 0 || sa == sb || sa == unq || sb == unq {
		t.Errorf("expected distinct non-negative keys, got sa=%d sb=%d unq=%d", sa, sb, unq)
	}
}

func TestPostgresQueries(t *testing.T) {
	t.Parallel()
	q := postgresQueries(`"myschema"."__migrations"`)
	if !strings.Contains(q.tableExists, "COALESCE(NULLIF($1, ''), current_schema())") ||
		!strings.Contains(q.tableExists, "c.relname = $2") {
		t.Errorf("unexpected tableExists query: %s", q.tableExists)
	}
}
