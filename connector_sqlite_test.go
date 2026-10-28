//go:build sqlite || !none

package migrator

import "testing"

func TestSQLitePragmaAndRollbackHelpers(t *testing.T) {
	t.Parallel()
	fk := map[string]bool{
		"PRAGMA foreign_keys = OFF":         true,
		"PRAGMA FOREIGN_KEYS ( 0 )":         true,
		"PRAGMA main . foreign_keys = OFF":  true,
		"PRAGMA foreign_keys":               false, // read-only query
		"PRAGMA defer_foreign_keys = ON":    false,
		"PRAGMA foreign_key_check":          false,
		"PRAGMA main . foreign_key_check ;": false,
	}
	for in, want := range fk {
		if got := isForeignKeysPragma(fields(in)); got != want {
			t.Errorf("isForeignKeysPragma(%q) = %v, want %v", in, got, want)
		}
	}
	rb := map[string]bool{
		"ROLLBACK":                   false,
		"ROLLBACK TRANSACTION":       false,
		"ROLLBACK TO sp":             true,
		"ROLLBACK TRANSACTION TO sp": true,
	}
	for in, want := range rb {
		if got := isRollbackTo(fields(in)); got != want {
			t.Errorf("isRollbackTo(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSQLiteQueries(t *testing.T) {
	t.Parallel()
	q := sqliteQueries(`"aux"."__migrations"`)
	want := "select 1 from pragma_table_list where schema = coalesce(nullif(?, ''), 'main') and name = ?"
	if q.tableExists != want {
		t.Errorf("tableExists = %q, want %q", q.tableExists, want)
	}
}

func fields(s string) []string {
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, r := range s {
		switch r {
		case ' ':
			flush()
		case '=', '(', ')', '.', ';':
			flush()
			out = append(out, string(r))
		default:
			cur += string(r)
		}
	}
	flush()
	return out
}
