package sqlsplit

import "fmt"

type sentinelError string

func (e sentinelError) Error() string {
	return string(e)
}

const (
	// ErrUnclosedString is returned when a single-quoted string literal is not closed before EOF.
	ErrUnclosedString sentinelError = "unclosed string literal"
	// ErrUnclosedQuotedIdent is returned when a double-quoted, backtick-quoted, or bracket-quoted identifier is not closed before EOF.
	ErrUnclosedQuotedIdent sentinelError = "unclosed quoted identifier"
	// ErrUnclosedBlockComment is returned when a /* ... */ block comment is not closed before EOF.
	ErrUnclosedBlockComment sentinelError = "unclosed block comment"
	// ErrUnclosedDollarQuote is returned when a Postgres $tag$...$tag$ dollar-quoted string is not closed before EOF.
	ErrUnclosedDollarQuote sentinelError = "unclosed dollar-quoted string"
	// ErrUnclosedCompound is returned when a compound statement (such as SQLite CREATE TRIGGER) reaches EOF without its closing sequence.
	ErrUnclosedCompound sentinelError = "unclosed compound block"
)

// SyntaxError represents a lexical or compound-statement error at a specific source position.
// Line and Col are 1-based; Col and Offset are measured in bytes.
// For unclosed quotes or comments, Line, Col, and Offset point to the opening delimiter.
// For ErrUnclosedCompound, Line, Col, and Offset point to the start of the statement.
type SyntaxError struct {
	Err    error
	Line   int
	Col    int
	Offset int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("sql syntax error at line %d, column %d (offset %d): %v", e.Line, e.Col, e.Offset, e.Err)
}

func (e *SyntaxError) Unwrap() error {
	return e.Err
}
