package sqlsplit

// CompoundRule specifies how a SQL dialect handles compound blocks containing internal semicolons.
type CompoundRule int

const (
	// CompoundNone splits statements at every top-level semicolon outside quotes and comments.
	CompoundNone CompoundRule = iota
	// CompoundSQLite keeps semicolons inside CREATE [TEMP|TEMPORARY] TRIGGER blocks until the closing "; END ;" sequence.
	CompoundSQLite
)

// Dialect configures the lexical and compound-statement rules of a SQL engine.
type Dialect struct {
	BacktickQuotes   bool
	BracketQuotes    bool
	BackslashEscapes bool
	EStrings         bool
	DollarQuotes     bool
	NestedComments   bool
	HashComments     bool
	Compound         CompoundRule
}

// SQLiteDialect returns the Dialect configuration for SQLite.
func SQLiteDialect() Dialect {
	return Dialect{
		BacktickQuotes:   true,
		BracketQuotes:    true,
		BackslashEscapes: false,
		EStrings:         false,
		DollarQuotes:     false,
		NestedComments:   false,
		HashComments:     false,
		Compound:         CompoundSQLite,
	}
}

// PostgresDialect returns the Dialect configuration for PostgreSQL.
//
// Note on compound statements: Dollar-quoted bodies ($$...$$ and $tag$...$tag$) used in
// CREATE FUNCTION, CREATE PROCEDURE, and DO blocks are handled via DollarQuotes.
// Two PostgreSQL constructs that contain top-level semicolons outside dollar quotes are not
// yet tracked by CompoundNone:
//  1. SQL-standard CREATE FUNCTION / PROCEDURE ... BEGIN ATOMIC ...; ... END; (PostgreSQL 14+),
//     which can be supported with the "; END ;" compound rule when needed.
//  2. CREATE RULE ... DO [ALSO | INSTEAD] (stmt1; stmt2), which requires parenthesis-depth tracking.
func PostgresDialect() Dialect {
	return Dialect{
		BacktickQuotes:   false,
		BracketQuotes:    false,
		BackslashEscapes: false,
		EStrings:         true,
		DollarQuotes:     true,
		NestedComments:   true,
		HashComments:     false,
		Compound:         CompoundNone,
	}
}

// MySQLDialect returns the Dialect configuration for MySQL / MariaDB.
func MySQLDialect() Dialect {
	return Dialect{
		BacktickQuotes:   true,
		BracketQuotes:    false,
		BackslashEscapes: true,
		EStrings:         false,
		DollarQuotes:     false,
		NestedComments:   false,
		HashComments:     true,
		Compound:         CompoundNone,
	}
}
