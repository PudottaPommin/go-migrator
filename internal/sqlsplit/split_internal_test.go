package sqlsplit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplit_SQLite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		want []Statement
	}{
		{
			name: "empty and whitespace only",
			sql:  "   \t\r\n  -- only a comment\n /* block */ ; ; ",
			want: nil,
		},
		{
			name: "comment at EOF without newline",
			sql:  "SELECT 1; -- trailing comment at EOF",
			want: []Statement{
				{SQL: "SELECT 1", Line: 1, Col: 1, Offset: 0},
			},
		},
		{
			name: "non-ASCII NBSP treated as identifier not whitespace",
			sql:  "\u00a0;",
			want: []Statement{
				{SQL: "\u00a0", Line: 1, Col: 1, Offset: 0},
			},
		},
		{
			name: "CRLF line endings and inline hint comments preserved",
			sql:  "-- header\r\nCREATE TABLE t1 (id INT);\r\nSELECT /*+ INDEX(t1) */ id FROM t1\r\nWHERE name = 'a;b'",
			want: []Statement{
				{SQL: "CREATE TABLE t1 (id INT)", Line: 2, Col: 1, Offset: 11},
				{SQL: "SELECT /*+ INDEX(t1) */ id FROM t1\r\nWHERE name = 'a;b'", Line: 3, Col: 1, Offset: 38},
			},
		},
		{
			name: "leading UTF-8 BOM before CREATE TRIGGER",
			sql:  "\xef\xbb\xbfCREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES (NEW.id); END;\nSELECT 1;",
			want: []Statement{
				{
					SQL:    "CREATE TRIGGER tr AFTER INSERT ON t1 BEGIN INSERT INTO log VALUES (NEW.id); END",
					Line:   1,
					Col:    4,
					Offset: 3,
				},
				{
					SQL:    "SELECT 1",
					Line:   2,
					Col:    1,
					Offset: 84,
				},
			},
		},
		{
			name: "trigger with unquoted end and begin column names",
			sql: `CREATE TRIGGER update_ranges AFTER UPDATE ON ranges
BEGIN
	UPDATE ranges SET end = NEW.end, begin = NEW.begin WHERE id = NEW.id;
	INSERT INTO audit (range_id, end) VALUES (NEW.id, NEW.end);
END;`,
			want: []Statement{
				{
					SQL: `CREATE TRIGGER update_ranges AFTER UPDATE ON ranges
BEGIN
	UPDATE ranges SET end = NEW.end, begin = NEW.begin WHERE id = NEW.id;
	INSERT INTO audit (range_id, end) VALUES (NEW.id, NEW.end);
END`,
					Line:   1,
					Col:    1,
					Offset: 0,
				},
			},
		},
		{
			name: "trigger with WHEN CASE...END in header and CASE...END in body",
			sql: `CREATE TEMP TRIGGER tr_status AFTER UPDATE ON users
WHEN CASE WHEN NEW.active IS NOT NULL THEN 1 ELSE 0 END = 1
BEGIN
	UPDATE audit SET label = CASE WHEN NEW.active = 1 THEN 'active;yes' ELSE 'inactive' END;
END;`,
			want: []Statement{
				{
					SQL: `CREATE TEMP TRIGGER tr_status AFTER UPDATE ON users
WHEN CASE WHEN NEW.active IS NOT NULL THEN 1 ELSE 0 END = 1
BEGIN
	UPDATE audit SET label = CASE WHEN NEW.active = 1 THEN 'active;yes' ELSE 'inactive' END;
END`,
					Line:   1,
					Col:    1,
					Offset: 0,
				},
			},
		},
		{
			name: "Slovak accented identifiers next to end case begin inside trigger",
			sql: `CREATE TEMPORARY TRIGGER IF NOT EXISTS tr_sk AFTER INSERT ON položky
BEGIN
	UPDATE položky SET šend = NEW.éCASE, čbegin = NEW.end_š WHERE id = NEW.id;
END;`,
			want: []Statement{
				{
					SQL: `CREATE TEMPORARY TRIGGER IF NOT EXISTS tr_sk AFTER INSERT ON položky
BEGIN
	UPDATE položky SET šend = NEW.éCASE, čbegin = NEW.end_š WHERE id = NEW.id;
END`,
					Line:   1,
					Col:    1,
					Offset: 0,
				},
			},
		},
		{
			name: "quoted end strings and identifiers inside trigger body",
			sql: "CREATE TRIGGER tr_quotes AFTER INSERT ON t1 BEGIN " +
				"INSERT INTO log (\"end\", `end`, [end]) VALUES ('end', '; END ;', 'it''s;end'); " +
				"END;",
			want: []Statement{
				{
					SQL: "CREATE TRIGGER tr_quotes AFTER INSERT ON t1 BEGIN " +
						"INSERT INTO log (\"end\", `end`, [end]) VALUES ('end', '; END ;', 'it''s;end'); " +
						"END",
					Line:   1,
					Col:    1,
					Offset: 0,
				},
			},
		},
		{
			name: "standalone BEGIN transaction statements split normally",
			sql:  "BEGIN IMMEDIATE TRANSACTION;\nINSERT INTO t1 VALUES (X'CAFE');\nCOMMIT;",
			want: []Statement{
				{SQL: "BEGIN IMMEDIATE TRANSACTION", Line: 1, Col: 1, Offset: 0},
				{SQL: "INSERT INTO t1 VALUES (X'CAFE')", Line: 2, Col: 1, Offset: 29},
				{SQL: "COMMIT", Line: 3, Col: 1, Offset: 62},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Split(tc.sql, SQLiteDialect())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSplit_PostgresAndMySQL(t *testing.T) {
	t.Parallel()

	t.Run("Postgres E-strings dollar quotes nested comments and positional params", func(t *testing.T) {
		t.Parallel()

		sql := `/* outer /* inner ; comment */ still outer */
SELECT E'it\'s;one', $1, $$body;1$$, $fn$BEGIN RETURN 1; END;$fn$;
SELECT 2`
		got, err := Split(sql, PostgresDialect())
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "SELECT E'it\\'s;one', $1, $$body;1$$, $fn$BEGIN RETURN 1; END;$fn$", got[0].SQL)
		assert.Equal(t, 2, got[0].Line)
		assert.Equal(t, "SELECT 2", got[1].SQL)
		assert.Equal(t, 3, got[1].Line)
	})

	t.Run("Postgres documented compound gaps for BEGIN ATOMIC and CREATE RULE", func(t *testing.T) {
		t.Parallel()

		// PostgresDialect currently uses CompoundNone; dollar-quoted functions stay intact,
		// whereas unquoted BEGIN ATOMIC and CREATE RULE split on inner semicolons until CompoundPostgres is added.
		sql := "CREATE RULE r1 AS ON INSERT TO t1 DO ALSO (INSERT INTO l1 VALUES (1); INSERT INTO l2 VALUES (2));"
		got, err := Split(sql, PostgresDialect())
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	t.Run("MySQL hash comments and backslash escapes", func(t *testing.T) {
		t.Parallel()

		sql := "# hash comment ; ignored\nSELECT 'a\\';b', `col;1`, /*!50100 1 */;\nSELECT 2;"
		got, err := Split(sql, MySQLDialect())
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "SELECT 'a\\';b', `col;1`, /*!50100 1 */", got[0].SQL)
		assert.Equal(t, "SELECT 2", got[1].SQL)
	})
}

func TestSplit_SyntaxErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sql        string
		dialect    Dialect
		wantTarget error
		wantLine   int
		wantCol    int
		wantOffset int
	}{
		{
			name:       "unclosed single-quoted string points to opening quote",
			sql:        "SELECT 1;\nSELECT 'unclosed;\nSELECT 2;",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedString,
			wantLine:   2,
			wantCol:    8,
			wantOffset: 17,
		},
		{
			name:       "unclosed Postgres E-string points to E prefix",
			sql:        "SELECT e'unclosed\\'",
			dialect:    PostgresDialect(),
			wantTarget: ErrUnclosedString,
			wantLine:   1,
			wantCol:    8,
			wantOffset: 7,
		},
		{
			name:       "unclosed double-quoted identifier after Slovak UTF-8 bytes",
			sql:        "SELECT šč, \"unclosed",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedQuotedIdent,
			wantLine:   1,
			wantCol:    14, // "SELECT " (7) + "šč" (4 UTF-8 bytes) + ", " (2) + 1 = 14
			wantOffset: 13,
		},
		{
			name:       "unclosed backtick identifier",
			sql:        "SELECT `unclosed",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedQuotedIdent,
			wantLine:   1,
			wantCol:    8,
			wantOffset: 7,
		},
		{
			name:       "unclosed bracket identifier",
			sql:        "SELECT [unclosed",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedQuotedIdent,
			wantLine:   1,
			wantCol:    8,
			wantOffset: 7,
		},
		{
			name:       "unclosed block comment points to opening slash-star",
			sql:        "SELECT 1;\n  /* unclosed comment",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedBlockComment,
			wantLine:   2,
			wantCol:    3,
			wantOffset: 12,
		},
		{
			name:       "unclosed nested block comment in Postgres",
			sql:        "/* outer /* inner */",
			dialect:    PostgresDialect(),
			wantTarget: ErrUnclosedBlockComment,
			wantLine:   1,
			wantCol:    1,
			wantOffset: 0,
		},
		{
			name:       "unclosed dollar quote points to opening tag",
			sql:        "SELECT $tag$unclosed $other$",
			dialect:    PostgresDialect(),
			wantTarget: ErrUnclosedDollarQuote,
			wantLine:   1,
			wantCol:    8,
			wantOffset: 7,
		},
		{
			name:       "unclosed trigger compound points to statement start",
			sql:        "SELECT 1;\n  CREATE TRIGGER tr BEGIN INSERT INTO t VALUES (1);",
			dialect:    SQLiteDialect(),
			wantTarget: ErrUnclosedCompound,
			wantLine:   2,
			wantCol:    3,
			wantOffset: 12,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stmts, err := Split(tc.sql, tc.dialect)
			require.Error(t, err)
			assert.Nil(t, stmts)
			require.ErrorIs(t, err, tc.wantTarget)

			var synErr *SyntaxError
			require.ErrorAs(t, err, &synErr)
			assert.Equal(t, tc.wantLine, synErr.Line)
			assert.Equal(t, tc.wantCol, synErr.Col)
			assert.Equal(t, tc.wantOffset, synErr.Offset)
			assert.Contains(t, synErr.Error(), tc.wantTarget.Error())
		})
	}
}

func TestLeadingTokens(t *testing.T) {
	t.Parallel()

	assert.Nil(t, LeadingTokens("SELECT 1", SQLiteDialect(), 0))
	assert.Equal(
		t,
		[]string{"PRAGMA", "main", ".", "foreign_keys", "=", "OFF"},
		LeadingTokens("/* c */ PRAGMA \"main\".'foreign_keys' = OFF;", SQLiteDialect(), 6),
	)
	assert.Equal(
		t,
		[]string{"PRAGMA", "foreign_keys", "(", "0", ")"},
		LeadingTokens("PRAGMA [foreign_keys](0);", SQLiteDialect(), 5),
	)
	assert.Equal(
		t,
		[]string{"PRAGMA", "foreign_keys", "="},
		LeadingTokens("PRAGMA `foreign_keys` = 1", SQLiteDialect(), 3),
	)
}

func FuzzSplit(f *testing.F) {
	seeds := []string{
		"",
		";;;",
		"\u00a0;",
		"\xef\xbb\xbfCREATE TRIGGER t BEGIN SELECT 1; END;",
		"SELECT 1;\xef\xbb\xbfSELECT 2",
		"CREATE TRIGGER tr BEGIN UPDATE ranges SET end = NEW.end, begin = NEW.begin; END;",
		"CREATE TEMP TRIGGER tr WHEN CASE WHEN 1 THEN 1 END = 1 BEGIN SELECT 'end', \"end\", [end], `end`; END;",
		"SELECT šend, éCASE, čbegin FROM položky;\r\n-- comment at EOF",
		"SELECT E'\\';', $$a;b$$, $tag$c;d$tag$, $1 /* outer /* inner */ */;",
		"# mysql comment\nSELECT 'a\\';b';",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	dialects := []Dialect{
		SQLiteDialect(),
		PostgresDialect(),
		MySQLDialect(),
	}

	f.Fuzz(func(t *testing.T, sql string) {
		for _, dialect := range dialects {
			assertSplitInvariants(t, sql, dialect)
		}
	})
}

func assertSplitInvariants(t *testing.T, sql string, dialect Dialect) {
	t.Helper()

	stmts, err := Split(sql, dialect)
	if err != nil {
		var synErr *SyntaxError
		require.ErrorAs(t, err, &synErr)
		require.GreaterOrEqual(t, synErr.Offset, 0)
		require.Less(t, synErr.Offset, len(sql))
		wantLine, wantCol := lineColAtOffset(sql, synErr.Offset)
		assert.Equal(t, wantLine, synErr.Line)
		assert.Equal(t, wantCol, synErr.Col)
		return
	}

	prevEnd := 0
	for i, stmt := range stmts {
		require.NotEmpty(t, stmt.SQL)
		require.GreaterOrEqual(t, stmt.Offset, 0)
		require.LessOrEqual(t, stmt.Offset+len(stmt.SQL), len(sql))
		assert.Equal(t, stmt.SQL, sql[stmt.Offset:stmt.Offset+len(stmt.SQL)])

		if i > 0 {
			assert.Greater(t, stmt.Offset, prevEnd)
		}
		prevEnd = stmt.Offset + len(stmt.SQL)

		wantLine, wantCol := lineColAtOffset(sql, stmt.Offset)
		assert.Equal(t, wantLine, stmt.Line)
		assert.Equal(t, wantCol, stmt.Col)

		if !strings.HasPrefix(stmt.SQL, utf8BOM) {
			resplit, resplitErr := Split(stmt.SQL, dialect)
			require.NoError(t, resplitErr)
			require.Equal(t, []Statement{
				{
					SQL:    stmt.SQL,
					Line:   1,
					Col:    1,
					Offset: 0,
				},
			}, resplit)
		}
	}
}

func lineColAtOffset(sql string, offset int) (int, int) {
	line, col := 1, 1
	for i := range offset {
		if sql[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
