# go-migrator

`go-migrator` is a lightweight, dialect-aware database migration library for Go. It pins a single connection to guarantee safe transaction management, provides session-level locking, accurately splits complex SQL scripts (triggers, nested blocks, comments, dollar quotes), and gates database drivers behind `go:build` tags to keep your binaries and CI builds lean.

---

## Features

- **Dialect-Aware SQL Splitter**: Accurately splits migration files statement-by-statement. Robust against multi-line comments, trigger compound bodies (`CREATE TRIGGER ... BEGIN ... END;`), columns named `begin` or `end`, dollar-quoted strings (`$$`, `$tag$`), Postgres `E'...'` strings, and UTF-8 identifiers.
- **Pluggable Engine Connectors via Build Tags**:
  - `migrator.SQLite()` under `//go:build sqlite || all`
  - `migrator.Postgres()` under `//go:build postgres || all`
  - Untagged core and parser can be compiled and unit-tested without driver dependencies.
- **Schema-Qualified Version Tables**: Version tables can be placed in custom schemas (e.g. `myschema.__migrations`, `"myschema"."__migrations"`, or SQLite attached databases `aux.__migrations`) with automatic individual identifier quoting and isolated PostgreSQL advisory locks.
- **Safe Transaction Management**:
  - Automatically wraps each migration in a transaction (if supported by the engine).
  - Pre-checks statements to prevent silent bugs (e.g. rejecting manual `BEGIN`/`COMMIT` inside transactions or `PRAGMA foreign_keys` inside SQLite transactions).
  - Per-migration transaction opt-out via `-- migrator:disable-tx`.
- **Reversible Migrations**: Supports `Up` and `Down` migrations separated by `---- up above / down below ----`. Pre-validates reversibility before executing any down steps.
- **Standard `io/fs` Support**: Seamlessly loads migrations from Go `embed.FS`, `os.DirFS`, or any custom virtual filesystem.

---

## Installation

```bash
go get github.com/pudottapommin/go-migrator
```

### Build Tags

To include engine-specific connectors, specify the corresponding build tag when building or running tests:

- **SQLite**: `go build -tags sqlite` or `go test -tags sqlite ./...`
- **PostgreSQL**: `go build -tags postgres` or `go test -tags postgres ./...`
- **All Engines**: `go build -tags all` or `go test -tags all ./...`

---

## Migration File Format

Migration files must be numbered sequentially starting from `1` without gaps and match the pattern `<N>_<description>.sql`:

```text
migrations/
├── 1_create_users.sql
├── 2_create_profiles.sql
└── 3_rebuild_table.sql
```

### Up & Down Delimiter

Separate `Up` and optional `Down` migrations using the delimiter `---- up above / down below ----`:

```sql
-- 1_create_users.sql
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

---- up above / down below ----

DROP TABLE users;
```

### Header Directives (`-- migrator:disable-tx`)

Certain operations (such as PostgreSQL `CREATE INDEX CONCURRENTLY` or SQLite table rebuilds requiring `PRAGMA foreign_keys = OFF`) cannot execute inside an active transaction. Use the `-- migrator:disable-tx` directive in the file header:

```sql
-- 3_rebuild_table.sql
-- migrator:disable-tx

PRAGMA foreign_keys = OFF;

BEGIN;
CREATE TABLE new_users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL
);
INSERT INTO new_users SELECT id, username FROM users;
DROP TABLE users;
ALTER TABLE new_users RENAME TO users;
COMMIT;

PRAGMA foreign_keys = ON;
```

> **Note**: Header directives must appear before any executable SQL statements in the file.

---

## Examples

### 1. Basic Usage with SQLite and `embed.FS`

```go
package main

import (
	"context"
	"database/sql"
	"embed"
	"log"

	"github.com/pudottapommin/go-migrator"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func main() {
	ctx := context.Background()

	db, err := sql.Open("sqlite", "app.db?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Initialize migrator with the SQLite connector
	m, err := migrator.NewMigrator(ctx, db, migrator.SQLite())
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()

	// Load migration files from embedded filesystem
	if err := m.LoadMigrations(migrationFS); err != nil {
		log.Fatal(err)
	}

	// Apply all pending migrations
	if err := m.Migrate(ctx); err != nil {
		log.Fatalf("migration failed: %v", err)
	}

	version, err := m.GetCurrentVersion(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Current database version: %d\n", version)
}
```

### 2. Schema-Qualified Version Table with PostgreSQL

When running migrations against a custom schema or sharing a database across services, configure a schema-qualified table name in `Options.VersionTable`:

```go
package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	"github.com/pudottapommin/go-migrator"
	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()

	db, err := sql.Open("postgres", "postgres://user:pass@localhost:5432/mydb?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Schema-qualified version table; advisory lock will automatically
	// hash "my_schema.__migrations" to avoid lock collisions
	opts := &migrator.Options{
		VersionTable: `"my_schema"."__migrations"`,
		DisableTx:    false,
	}

	m, err := migrator.NewMigratorEx(ctx, db, migrator.Postgres(), opts)
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()

	if err := m.LoadMigrations(os.DirFS("migrations")); err != nil {
		log.Fatal(err)
	}

	if err := m.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
}
```

### 3. Rolling Back Migrations (`MigrateTo`)

To roll back or migrate to a specific target version, call `MigrateTo`:

```go
// Roll back to version 1
if err := m.MigrateTo(ctx, 1); err != nil {
    log.Fatalf("rollback failed: %v", err)
}

// Roll back all migrations (to version 0)
if err := m.MigrateTo(ctx, 0); err != nil {
    log.Fatalf("full rollback failed: %v", err)
}
```

If any migration between the current version and the target version is not reversible (missing a `Down` section), `MigrateTo` fails immediately before touching the database.

### 4. Programmatic Migrations

You can also append migrations programmatically without filesystem files:

```go
m, err := migrator.NewMigrator(ctx, db, migrator.SQLite())
if err != nil {
    log.Fatal(err)
}
defer m.Close()

m.AppendMigration(
    "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);",
    "DROP TABLE items;",
)

if err := m.Migrate(ctx); err != nil {
    log.Fatal(err)
}
```

---

## API Overview

### Constructors & Options

- `NewMigrator(ctx, db, connector)`: Creates a migrator using default options (`VersionTable: "__migrations"`).
- `NewMigratorEx(ctx, db, connector, opts)`: Creates a migrator with custom options:
  - `VersionTable string`: Unqualified (e.g. `__migrations`) or schema-qualified (e.g. `schema.__migrations`, `"schema"."table"`).
  - `DisableTx bool`: Globally disables automatic transactions.

### Methods

- `m.LoadMigrations(vfs fs.FS) error`: Finds, validates, and loads migration files from any `fs.FS`.
- `m.AppendMigration(upSQL, downSQL string)`: Appends an in-memory migration.
- `m.Migrate(ctx) error`: Applies all pending migrations up to the latest available version.
- `m.MigrateTo(ctx, targetVersion int) error`: Migrates up or down to the requested target version.
- `m.GetCurrentVersion(ctx) (int, error)`: Returns current applied migration version (`0` if no migrations have been applied or the version table does not exist yet).
- `m.Close() error`: Releases the pinned database connection and session locks.

### Sentinel Errors

- `migrator.ErrManualTxInTransactedMigration`: Returned when a migration executed inside an automatic transaction contains explicit `BEGIN`, `COMMIT`, `END`, or bare `ROLLBACK` statements.
- `migrator.ErrPragmaForeignKeysInTx`: Returned when `PRAGMA foreign_keys` assignment is detected inside a transacted SQLite migration (where SQLite would ignore it).

---

## Running Tests

Run all unit and integration tests across dialects:

```bash
# Core splitter and spec connector tests (no CGO / DB driver required)
go test -v ./...

# SQLite integration tests
go test -tags sqlite -v ./...

# PostgreSQL integration tests
go test -tags postgres -v ./...

# All engines combined
go test -tags all -v ./...

# Run SQL splitter fuzz tests
go test -fuzz=FuzzSplit -fuzztime=10s ./internal/sqlsplit/...
```

---

## License

This project is licensed under the MIT License.
