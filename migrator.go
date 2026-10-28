package migrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/pudottapommin/go-migrator/internal/sqlsplit"
)

type Direction string

func (d Direction) number() int {
	switch d {
	case DirectionUp:
		return 1
	case DirectionDown:
		return -1
	default:
		return 0
	}
}

const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
)

const (
	DefaultVersionTable = "__migrations"
	UpDownSnippet       = "---- up above / down below ----"

	utf8BOM            = "\xef\xbb\xbf"
	directivePrefix    = "migrator:"
	directiveDisableTx = "migrator:disable-tx"
	maxUpDownPieces    = 2
)

var migrationPattern = regexp.MustCompile(`\A(\d+)_.+\.sql\z`)

type Migrator struct {
	conn         Conn
	Migrations   []*Migration
	versionTable string
	disableTx    bool
}

// NewMigrator creates a Migrator for db using the given engine connector,
// e.g. migrator.SQLite() or migrator.Postgres() (available with the matching build tag).
// The Migrator pins one connection from db until Close is called.
func NewMigrator(ctx context.Context, db *sql.DB, connector Connector) (*Migrator, error) {
	return NewMigratorEx(ctx, db, connector, &Options{
		VersionTable: "",
		DisableTx:    false,
	})
}

func NewMigratorEx(ctx context.Context, db *sql.DB, connector Connector, opts *Options) (*Migrator, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	m := &Migrator{
		conn:         nil,
		Migrations:   make([]*Migration, 0),
		versionTable: DefaultVersionTable,
		disableTx:    false,
	}
	if opts != nil {
		if opts.VersionTable != "" {
			m.versionTable = opts.VersionTable
		}
		m.disableTx = opts.DisableTx
	}
	conn, err := connector.Connect(ctx, db, ConnOptions{Table: m.versionTable})
	if err != nil {
		return nil, fmt.Errorf("failed to connect (%s): %w", connector.Name(), err)
	}
	m.conn = conn
	return m, nil
}

func (m *Migrator) LoadMigrations(vfs fs.FS) error {
	paths, err := m.FindMigrations(vfs)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no migrations found")
	}

	for _, p := range paths {
		body, ferr := fs.ReadFile(vfs, p)
		if ferr != nil {
			return fmt.Errorf("failed to read migration file %s: %w", p, ferr)
		}

		upSQL, downSQL, disableTx, perr := parseMigrationFile(string(body), p)
		if perr != nil {
			return perr
		}

		m.Migrations = append(m.Migrations, &Migration{
			Version:   len(m.Migrations) + 1,
			Up:        upSQL,
			Down:      downSQL,
			DisableTx: disableTx,
		})
	}
	return nil
}

func parseMigrationFile(content, path string) (string, string, bool, error) {
	trimmedContent := strings.TrimPrefix(content, utf8BOM)

	disableTx, err := parseHeaderDirectives(trimmedContent, path)
	if err != nil {
		return "", "", false, err
	}

	pieces := strings.SplitN(trimmedContent, UpDownSnippet, maxUpDownPieces)
	upSQL := strings.TrimSpace(pieces[0])
	if !hasExecutableSQL(upSQL) {
		return "", "", false, fmt.Errorf("migration file %s does not contain SQL", path)
	}

	var downSQL string
	if len(pieces) == maxUpDownPieces {
		downSQL = strings.TrimSpace(pieces[1])
		if !hasExecutableSQL(downSQL) {
			return "", "", false, fmt.Errorf("migration file %s: down section does not contain SQL", path)
		}
	}

	return upSQL, downSQL, disableTx, nil
}

func parseHeaderDirectives(content, path string) (bool, error) {
	var disableTx bool
	inHeader := true

	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == UpDownSnippet {
			inHeader = false
			continue
		}
		if !strings.HasPrefix(trimmed, "--") {
			inHeader = false
			continue
		}

		commentBody := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
		if !strings.HasPrefix(commentBody, directivePrefix) {
			continue
		}
		if !inHeader {
			return false, fmt.Errorf(
				"migration file %s: directive %q must appear in the file header before any SQL",
				path,
				commentBody,
			)
		}
		if commentBody == directiveDisableTx {
			disableTx = true
			continue
		}
		return false, fmt.Errorf("migration file %s: unknown directive %q", path, commentBody)
	}

	return disableTx, nil
}

func hasExecutableSQL(sqlText string) bool {
	stmts, err := sqlsplit.Split(sqlText, sqlsplit.SQLiteDialect())
	if err != nil {
		return !errors.Is(err, sqlsplit.ErrUnclosedBlockComment) || len(stmts) > 0
	}
	return len(stmts) > 0
}

func (m *Migrator) AppendMigration(upSQL, downSQL string) {
	m.Migrations = append(m.Migrations, &Migration{
		Version:   len(m.Migrations) + 1,
		Up:        upSQL,
		Down:      downSQL,
		DisableTx: false,
	})
}

func (m *Migrator) FindMigrations(vfs fs.FS) ([]string, error) {
	fileInfos, err := fs.ReadDir(vfs, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to load migrations: %w", err)
	}

	paths := make([]string, 0, len(fileInfos))
	for _, fi := range fileInfos {
		if fi.IsDir() {
			continue
		}
		matches := migrationPattern.FindStringSubmatch(fi.Name())
		if len(matches) != 2 { //nolint:mnd // expected 2 matches
			continue
		}

		var n int64
		n, err = strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse migration version: %w", err)
		}
		// Versions are 1..N without gaps, so anything outside 1..len(files) is invalid.
		// This also keeps setAt from allocating for huge numbers (e.g. timestamp prefixes).
		if n < 1 || n > int64(len(fileInfos)) {
			return nil, fmt.Errorf(
				"migration version %d out of range (files must be numbered 1..N without gaps): %s",
				n,
				fi.Name(),
			)
		}
		if n-1 < int64(len(paths)) && paths[n-1] != "" {
			return nil, fmt.Errorf("duplicate migration version: %d", n)
		}
		paths = setAt(paths, fi.Name(), n-1)
	}

	for i, p := range paths {
		if p == "" {
			return nil, fmt.Errorf("missing migration version: %d", i+1)
		}
	}
	return paths, nil
}

func (m *Migrator) Migrate(ctx context.Context) error {
	return m.MigrateTo(ctx, m.highestMigration())
}

func (m *Migrator) MigrateTo(ctx context.Context, targetVersion int) error {
	if err := m.validate(); err != nil {
		return err
	}
	if targetVersion < 0 || len(m.Migrations) < targetVersion {
		return fmt.Errorf("target version %d is out of range", targetVersion)
	}

	unlock, err := m.conn.Lock(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire migration lock: %w", err)
	}
	runErr := m.migrateLocked(ctx, targetVersion)
	if uerr := unlock(ctx); uerr != nil {
		return errors.Join(runErr, fmt.Errorf("failed to release migration lock: %w", uerr))
	}
	return runErr
}

func (m *Migrator) migrateLocked(ctx context.Context, targetVersion int) error {
	if err := m.conn.EnsureTable(ctx); err != nil {
		return err
	}
	currentVersion, err := m.conn.CurrentVersion(ctx)
	if err != nil {
		return err
	}
	if currentVersion < 0 || len(m.Migrations) < currentVersion {
		return fmt.Errorf("current version %d is out of range", currentVersion)
	}
	if currentVersion == targetVersion {
		return nil
	}

	direction := DirectionUp
	if currentVersion > targetVersion {
		direction = DirectionDown
		// Fail before touching the database rather than stopping half-way down.
		for v := currentVersion; v > targetVersion; v-- {
			if mig := m.Migrations[v-1]; !mig.reversible() {
				return fmt.Errorf("migration %d is not reversible", mig.Version)
			}
		}
	}

	canUseTx := !m.disableTx && m.conn.Capabilities().TransactionalDDL
	for currentVersion != targetVersion {
		var mig *Migration
		var sqlText string
		if direction == DirectionUp {
			mig = m.Migrations[currentVersion]
			sqlText = mig.Up
		} else {
			mig = m.Migrations[currentVersion-1]
			sqlText = mig.Down
		}

		step := Step{
			SQL:        sqlText,
			NewVersion: currentVersion + direction.number(),
			UseTx:      canUseTx && !mig.DisableTx,
		}
		if err = m.conn.Apply(ctx, step); err != nil {
			return fmt.Errorf("failed to execute migration %d (%s): %w", mig.Version, direction, err)
		}
		currentVersion = step.NewVersion
	}

	return nil
}

func (m *Migrator) Close() error {
	if m.conn != nil {
		return m.conn.Close()
	}
	return nil
}

func (m *Migrator) GetCurrentVersion(ctx context.Context) (int, error) {
	return m.conn.CurrentVersion(ctx)
}

func (m *Migrator) validate() error {
	for _, mig := range m.Migrations {
		if !hasExecutableSQL(mig.Up) {
			return fmt.Errorf("migration %d: up script is empty", mig.Version)
		}
		if mig.Down != "" && !hasExecutableSQL(mig.Down) {
			return fmt.Errorf("migration %d: down script contains no SQL", mig.Version)
		}
	}
	return nil
}

func (m *Migrator) highestMigration() int {
	if len(m.Migrations) == 0 {
		return 0
	}
	return m.Migrations[len(m.Migrations)-1].Version
}

type Options struct {
	// VersionTable is the version table name (e.g. "__migrations", "schema.table", or `"schema"."table"`).
	// Each identifier is quoted by the connector; defaults to DefaultVersionTable when empty.
	VersionTable string
	DisableTx    bool
}

func setAt(arr []string, value string, pos int64) []string {
	if pos > int64(len(arr))-1 {
		arr = append(arr, make([]string, pos-int64(len(arr))+1)...)
	}
	arr[pos] = value
	return arr
}
