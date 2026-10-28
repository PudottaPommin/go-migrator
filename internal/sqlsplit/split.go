package sqlsplit

import "strings"

const (
	utf8BOM          = "\xef\xbb\xbf"
	minCommentMarker = 2
	asciiHighBit     = 0x80
)

// Statement represents a single executable SQL statement sliced from the original input.
// Line and Col are 1-based; Col and Offset are measured in bytes.
type Statement struct {
	SQL    string
	Line   int
	Col    int
	Offset int
}

// Split scans sql using the given dialect rules and returns the individual executable SQL statements.
func Split(sql string, dialect Dialect) ([]Statement, error) {
	s := newScanner(sql, dialect)
	return s.split()
}

// LeadingTokens scans up to maxTokens leading non-trivia tokens (words, unquoted string/identifier
// literals, or single-byte punctuation symbols) from sql using the given dialect rules.
func LeadingTokens(sql string, dialect Dialect, maxTokens int) []string {
	if maxTokens <= 0 {
		return nil
	}
	s := newScanner(sql, dialect)
	return s.leadingTokens(maxTokens)
}

type triggerHeaderState int

const (
	headerStart triggerHeaderState = iota
	headerSawCreate
	headerSawTemp
	headerDone
)

type triggerTracker struct {
	headerState triggerHeaderState
	enabled     bool
	inTrigger   bool
	semi        bool
	sawEnd      bool
}

func newTriggerTracker(rule CompoundRule) triggerTracker {
	return triggerTracker{
		headerState: headerStart,
		enabled:     rule == CompoundSQLite,
		inTrigger:   false,
		semi:        false,
		sawEnd:      false,
	}
}

func (t *triggerTracker) reset() {
	t.headerState = headerStart
	t.inTrigger = false
	t.semi = false
	t.sawEnd = false
}

func (t *triggerTracker) onWord(word string) {
	if !t.enabled {
		return
	}
	switch t.headerState {
	case headerStart:
		if strings.EqualFold(word, "CREATE") {
			t.headerState = headerSawCreate
		} else {
			t.headerState = headerDone
		}
	case headerSawCreate:
		switch {
		case strings.EqualFold(word, "TEMP") || strings.EqualFold(word, "TEMPORARY"):
			t.headerState = headerSawTemp
		case strings.EqualFold(word, "TRIGGER"):
			t.inTrigger = true
			t.headerState = headerDone
		default:
			t.headerState = headerDone
		}
	case headerSawTemp:
		if strings.EqualFold(word, "TRIGGER") {
			t.inTrigger = true
		}
		t.headerState = headerDone
	case headerDone:
		// Already past the statement header.
	}

	if t.inTrigger {
		t.sawEnd = t.semi && strings.EqualFold(word, "END")
		t.semi = false
	}
}

func (t *triggerTracker) onOther() {
	if !t.enabled {
		return
	}
	t.headerState = headerDone
	t.semi = false
	t.sawEnd = false
}

func (t *triggerTracker) onSemicolon() bool {
	if !t.enabled || !t.inTrigger {
		return true
	}
	if t.sawEnd {
		return true
	}
	t.semi = true
	t.sawEnd = false
	return false
}

func (t *triggerTracker) isUnclosedAtEOF() bool {
	return t.enabled && t.inTrigger && !t.sawEnd
}

type scanner struct {
	sql       string
	dialect   Dialect
	pos       int
	line      int
	col       int
	stmtStart int
	stmtLine  int
	stmtCol   int
	tracker   triggerTracker
}

func newScanner(sql string, dialect Dialect) *scanner {
	s := &scanner{
		sql:       sql,
		dialect:   dialect,
		pos:       0,
		line:      1,
		col:       1,
		stmtStart: -1,
		stmtLine:  0,
		stmtCol:   0,
		tracker:   newTriggerTracker(dialect.Compound),
	}
	if strings.HasPrefix(sql, utf8BOM) {
		s.pos = len(utf8BOM)
		s.col = len(utf8BOM) + 1
	}
	return s
}

func (s *scanner) split() ([]Statement, error) {
	var stmts []Statement

	for s.pos < len(s.sql) {
		skipped, err := s.trySkipTrivia()
		if err != nil {
			return nil, err
		}
		if skipped {
			continue
		}

		if s.sql[s.pos] == ';' {
			if s.stmtStart < 0 {
				s.advance(1)
				s.tracker.reset()
				continue
			}
			if s.tracker.onSemicolon() {
				stmts = s.appendStatement(stmts, s.pos)
				s.advance(1)
				continue
			}
			s.advance(1)
			continue
		}

		s.markStmtStart()
		if err = s.scanNonTriviaToken(); err != nil {
			return nil, err
		}
	}

	if s.stmtStart >= 0 {
		if s.tracker.isUnclosedAtEOF() {
			return nil, &SyntaxError{
				Err:    ErrUnclosedCompound,
				Line:   s.stmtLine,
				Col:    s.stmtCol,
				Offset: s.stmtStart,
			}
		}
		stmts = s.appendStatement(stmts, len(s.sql))
	}

	return stmts, nil
}

func (s *scanner) markStmtStart() {
	if s.stmtStart < 0 {
		s.stmtStart = s.pos
		s.stmtLine = s.line
		s.stmtCol = s.col
	}
}

func (s *scanner) appendStatement(stmts []Statement, endOffset int) []Statement {
	raw := trimRightSpace(s.sql[s.stmtStart:endOffset])
	if raw != "" {
		stmts = append(stmts, Statement{
			SQL:    raw,
			Line:   s.stmtLine,
			Col:    s.stmtCol,
			Offset: s.stmtStart,
		})
	}
	s.stmtStart = -1
	s.stmtLine = 0
	s.stmtCol = 0
	s.tracker.reset()
	return stmts
}

func (s *scanner) trySkipTrivia() (bool, error) {
	b := s.sql[s.pos]
	if isSpace(b) {
		for s.pos < len(s.sql) && isSpace(s.sql[s.pos]) {
			s.advance(1)
		}
		return true, nil
	}
	if b == '-' && s.pos+1 < len(s.sql) && s.sql[s.pos+1] == '-' {
		s.skipToEndOfLine(minCommentMarker)
		return true, nil
	}
	if s.dialect.HashComments && b == '#' {
		s.skipToEndOfLine(1)
		return true, nil
	}
	if b == '/' && s.pos+1 < len(s.sql) && s.sql[s.pos+1] == '*' {
		return true, s.skipBlockComment()
	}
	return false, nil
}

func (s *scanner) skipToEndOfLine(prefixLen int) {
	s.advance(prefixLen)
	for s.pos < len(s.sql) && s.sql[s.pos] != '\n' {
		s.advance(1)
	}
	if s.pos < len(s.sql) && s.sql[s.pos] == '\n' {
		s.advance(1)
	}
}

func (s *scanner) skipBlockComment() error {
	openOffset, openLine, openCol := s.pos, s.line, s.col
	s.advance(minCommentMarker)
	depth := 1

	for s.pos < len(s.sql) {
		if s.dialect.NestedComments && s.sql[s.pos] == '/' && s.pos+1 < len(s.sql) && s.sql[s.pos+1] == '*' {
			depth++
			s.advance(minCommentMarker)
			continue
		}
		if s.sql[s.pos] == '*' && s.pos+1 < len(s.sql) && s.sql[s.pos+1] == '/' {
			depth--
			s.advance(minCommentMarker)
			if depth == 0 {
				return nil
			}
			continue
		}
		s.advance(1)
	}

	return &SyntaxError{
		Err:    ErrUnclosedBlockComment,
		Line:   openLine,
		Col:    openCol,
		Offset: openOffset,
	}
}

func (s *scanner) scanNonTriviaToken() error {
	b := s.sql[s.pos]

	if b == '\'' {
		s.tracker.onOther()
		return s.scanSingleQuote(s.dialect.BackslashEscapes, s.pos, s.line, s.col, 1)
	}
	if b == '"' {
		s.tracker.onOther()
		return s.scanDoubledQuote('"')
	}
	if s.dialect.BacktickQuotes && b == '`' {
		s.tracker.onOther()
		return s.scanDoubledQuote('`')
	}
	if s.dialect.BracketQuotes && b == '[' {
		s.tracker.onOther()
		return s.scanBracketQuote()
	}
	if s.dialect.DollarQuotes && b == '$' {
		matched, err := s.tryScanDollarQuote()
		if err != nil {
			return err
		}
		if matched {
			s.tracker.onOther()
			return nil
		}
	}
	if isIdentStart(b) {
		return s.scanWordOrEString()
	}

	s.tracker.onOther()
	s.advance(1)
	return nil
}

func (s *scanner) scanWordOrEString() error {
	openOffset, openLine, openCol := s.pos, s.line, s.col
	for s.pos < len(s.sql) && isIdentContinue(s.sql[s.pos]) {
		s.advance(1)
	}
	word := s.sql[openOffset:s.pos]

	if s.dialect.EStrings && (word == "e" || word == "E") && s.pos < len(s.sql) && s.sql[s.pos] == '\'' {
		s.tracker.onOther()
		return s.scanSingleQuote(true, openOffset, openLine, openCol, 1)
	}

	s.tracker.onWord(word)
	return nil
}

func (s *scanner) scanSingleQuote(backslashEscapes bool, openOffset, openLine, openCol, prefixAdvance int) error {
	s.advance(prefixAdvance)
	for s.pos < len(s.sql) {
		ch := s.sql[s.pos]
		if backslashEscapes && ch == '\\' {
			s.advance(1)
			if s.pos < len(s.sql) {
				s.advance(1)
			}
			continue
		}
		if ch == '\'' {
			s.advance(1)
			if s.pos < len(s.sql) && s.sql[s.pos] == '\'' {
				s.advance(1)
				continue
			}
			return nil
		}
		s.advance(1)
	}
	return &SyntaxError{
		Err:    ErrUnclosedString,
		Line:   openLine,
		Col:    openCol,
		Offset: openOffset,
	}
}

func (s *scanner) scanDoubledQuote(quote byte) error {
	openOffset, openLine, openCol := s.pos, s.line, s.col
	s.advance(1)
	for s.pos < len(s.sql) {
		if s.sql[s.pos] == quote {
			s.advance(1)
			if s.pos < len(s.sql) && s.sql[s.pos] == quote {
				s.advance(1)
				continue
			}
			return nil
		}
		s.advance(1)
	}
	return &SyntaxError{
		Err:    ErrUnclosedQuotedIdent,
		Line:   openLine,
		Col:    openCol,
		Offset: openOffset,
	}
}

func (s *scanner) scanBracketQuote() error {
	openOffset, openLine, openCol := s.pos, s.line, s.col
	s.advance(1)
	for s.pos < len(s.sql) {
		if s.sql[s.pos] == ']' {
			s.advance(1)
			return nil
		}
		s.advance(1)
	}
	return &SyntaxError{
		Err:    ErrUnclosedQuotedIdent,
		Line:   openLine,
		Col:    openCol,
		Offset: openOffset,
	}
}

func (s *scanner) tryScanDollarQuote() (bool, error) {
	tagLen := s.matchDollarTag()
	if tagLen == 0 {
		return false, nil
	}
	openOffset, openLine, openCol := s.pos, s.line, s.col
	tag := s.sql[s.pos : s.pos+tagLen]
	s.advance(tagLen)

	for s.pos < len(s.sql) {
		if s.sql[s.pos] == '$' && strings.HasPrefix(s.sql[s.pos:], tag) {
			s.advance(tagLen)
			return true, nil
		}
		s.advance(1)
	}
	return true, &SyntaxError{
		Err:    ErrUnclosedDollarQuote,
		Line:   openLine,
		Col:    openCol,
		Offset: openOffset,
	}
}

func (s *scanner) matchDollarTag() int {
	rem := s.sql[s.pos:]
	if len(rem) < minCommentMarker || rem[0] != '$' {
		return 0
	}
	if rem[1] == '$' {
		return minCommentMarker
	}
	if !isIdentStart(rem[1]) {
		return 0
	}
	i := 2
	for i < len(rem) && isIdentContinue(rem[i]) {
		i++
	}
	if i < len(rem) && rem[i] == '$' {
		return i + 1
	}
	return 0
}

func (s *scanner) leadingTokens(maxTokens int) []string {
	tokens := make([]string, 0, maxTokens)
	for s.pos < len(s.sql) && len(tokens) < maxTokens {
		skipped, err := s.trySkipTrivia()
		if err != nil {
			break
		}
		if skipped {
			continue
		}
		tok, ok := s.nextLeadingToken()
		if !ok {
			break
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

func (s *scanner) nextLeadingToken() (string, bool) {
	b := s.sql[s.pos]
	if b == '\'' {
		return s.extractQuotedContent('\'', s.dialect.BackslashEscapes)
	}
	if b == '"' {
		return s.extractQuotedContent('"', false)
	}
	if s.dialect.BacktickQuotes && b == '`' {
		return s.extractQuotedContent('`', false)
	}
	if s.dialect.BracketQuotes && b == '[' {
		return s.extractBracketContent()
	}
	if isIdentStart(b) {
		start := s.pos
		for s.pos < len(s.sql) && isIdentContinue(s.sql[s.pos]) {
			s.advance(1)
		}
		word := s.sql[start:s.pos]
		if s.dialect.EStrings && (word == "e" || word == "E") && s.pos < len(s.sql) && s.sql[s.pos] == '\'' {
			return s.extractQuotedContent('\'', true)
		}
		return word, true
	}
	s.advance(1)
	return s.sql[s.pos-1 : s.pos], true
}

func (s *scanner) extractQuotedContent(quote byte, backslashEscapes bool) (string, bool) {
	s.advance(1)
	var buf strings.Builder
	for s.pos < len(s.sql) {
		ch := s.sql[s.pos]
		if backslashEscapes && ch == '\\' {
			s.advance(1)
			if s.pos < len(s.sql) {
				buf.WriteByte(s.sql[s.pos])
				s.advance(1)
			}
			continue
		}
		if ch == quote {
			s.advance(1)
			if s.pos < len(s.sql) && s.sql[s.pos] == quote {
				buf.WriteByte(quote)
				s.advance(1)
				continue
			}
			return buf.String(), true
		}
		buf.WriteByte(ch)
		s.advance(1)
	}
	return "", false
}

func (s *scanner) extractBracketContent() (string, bool) {
	s.advance(1)
	start := s.pos
	for s.pos < len(s.sql) {
		if s.sql[s.pos] == ']' {
			content := s.sql[start:s.pos]
			s.advance(1)
			return content, true
		}
		s.advance(1)
	}
	return "", false
}

func (s *scanner) advance(n int) {
	for range n {
		if s.pos >= len(s.sql) {
			return
		}
		if s.sql[s.pos] == '\n' {
			s.line++
			s.col = 1
		} else {
			s.col++
		}
		s.pos++
	}
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b >= asciiHighBit
}

func isIdentContinue(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func trimRightSpace(s string) string {
	end := len(s)
	for end > 0 && isSpace(s[end-1]) {
		end--
	}
	return s[:end]
}
