package store

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Registers the postgres driver that NewWithSourceInstance resolves from
	// the postgres:// scheme in DATABASE_URL.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"

	// Registers the sqlite driver resolved from the sqlite:// scheme. It is
	// built on modernc.org/sqlite, the pure-Go driver, so CGO stays off.
	_ "github.com/golang-migrate/migrate/v4/database/sqlite"
)

// Two migration sets live side by side, one per SQL dialect:
//
//	migrations/postgres/  the production schema
//	migrations/sqlite/    the same tables and columns, phrased for SQLite
//
// They share version numbers and file basenames — migration N is applied
// under whichever directory the URL selects — and a test in this package
// asserts the two dialects end up with the same table and column sets, so a
// migration added for one backend cannot silently skip the other.
//
//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationsFS embed.FS

// SQLiteURLPrefix is the DATABASE_URL scheme that selects the SQLite store.
// A URL like sqlite:///home/me/history.db opens (and migrates) that file.
const SQLiteURLPrefix = "sqlite://"

// IsSQLiteURL reports whether a DATABASE_URL selects SQLite.
func IsSQLiteURL(databaseURL string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(databaseURL)), SQLiteURLPrefix)
}

// Migrator applies schema migrations.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator prepares migrations against the given database URL.
//
// It takes a URL rather than a Store because golang-migrate needs its own
// database/sql handle; sharing the pgx pool is not supported.
func NewMigrator(databaseURL string) (*Migrator, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("migrate: database URL is required")
	}

	sourceDir := "migrations/postgres"
	if IsSQLiteURL(databaseURL) {
		sourceDir = "migrations/sqlite"
	}

	source, err := iofs.New(migrationsFS, sourceDir)
	if err != nil {
		return nil, fmt.Errorf("migrate: load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("migrate: open database: %w", err)
	}
	return &Migrator{m: m}, nil
}

// Up applies all pending migrations. Applying an already-current schema is not
// an error, so this is safe to run on every start.
func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down reverts the most recent migration.
//
// It steps back one migration rather than tearing everything down, because
// `migrate down` on a database holding real deployment history should not be a
// single command away from dropping all of it.
func (m *Migrator) Down() error {
	if err := m.m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Version reports the current schema version and whether the last migration
// left the database dirty (failed partway and needs manual attention).
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migrate version: %w", err)
	}
	return version, dirty, nil
}

// Close releases the migrator's database handles.
func (m *Migrator) Close() error {
	sourceErr, dbErr := m.m.Close()
	return errors.Join(sourceErr, dbErr)
}

// MigrationsFS exposes the embedded migrations for tests that build a schema
// directly and for the dialect-parity check.
func MigrationsFS() embed.FS { return migrationsFS }
