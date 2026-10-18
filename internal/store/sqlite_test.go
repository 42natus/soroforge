package store_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSQLiteStore returns a migrated, empty SQLite store in a temp file.
//
// Unlike Postgres, this needs no provisioning — the file lives under the test
// temp dir — so the shared conformance suite runs against SQLite in the
// default `go test ./...`, not behind an environment variable. That is the
// point of the backend: same semantics, nothing to stand up.
func newSQLiteStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()

	url := sqliteFileURL(t)

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	require.NoError(t, migrator.Up())
	require.NoError(t, migrator.Close())

	s, err := store.NewSQLite(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

// sqliteFileURL points a fresh sqlite:// DATABASE_URL at a temp file,
// exercising the exact scheme operators configure.
func sqliteFileURL(t *testing.T) string {
	t.Helper()
	// ToSlash keeps a Windows temp path parseable as a URL; a no-op elsewhere.
	return store.SQLiteURLPrefix + filepath.ToSlash(filepath.Join(t.TempDir(), "history.db"))
}

func TestSQLiteStore(t *testing.T) {
	runStoreSuite(t, newSQLiteStore)
}

func TestSQLiteHistory(t *testing.T) {
	runHistorySuite(t, newSQLiteStore)
}

func TestMemoryHistory(t *testing.T) {
	runHistorySuite(t, func(t *testing.T) store.Store {
		return store.NewMemoryStore()
	})
}

func TestSQLiteMigrationsAreReversible(t *testing.T) {
	url := sqliteFileURL(t)

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	defer func() { require.NoError(t, migrator.Close()) }()

	require.NoError(t, migrator.Up())

	version, dirty, err := migrator.Version()
	require.NoError(t, err)
	require.False(t, dirty, "migrations must not leave the database dirty")
	require.Equal(t, uint(4), version)

	require.NoError(t, migrator.Down())
	version, dirty, err = migrator.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, uint(3), version)

	require.NoError(t, migrator.Up())
}

func TestSQLiteUpIsIdempotent(t *testing.T) {
	url := sqliteFileURL(t)

	for i := 0; i < 2; i++ {
		migrator, err := store.NewMigrator(url)
		require.NoError(t, err)
		require.NoError(t, migrator.Up())
		require.NoError(t, migrator.Close())
	}
}

func TestNewSQLiteRejectsBadURLs(t *testing.T) {
	ctx := context.Background()

	_, err := store.NewSQLite(ctx, "")
	require.ErrorContains(t, err, "database URL is required")

	_, err = store.NewSQLite(ctx, "postgres://nope/nope")
	require.ErrorContains(t, err, "sqlite://")

	_, err = store.NewSQLite(ctx, "sqlite://")
	require.ErrorContains(t, err, "no file")
}

func TestOpenDispatchesOnScheme(t *testing.T) {
	ctx := context.Background()

	s, err := store.Open(ctx, sqliteFileURL(t))
	require.NoError(t, err)
	_, ok := s.(*store.SQLite)
	assert.True(t, ok, "sqlite:// URLs must open the SQLite store")
	require.NoError(t, s.Close())

	// A path with no file behind it must not silently succeed for Postgres;
	// anything that is not sqlite:// keeps the Postgres semantics, including
	// its eager ping failure.
	_, err = store.Open(ctx, "postgres://soroforge:soroforge@127.0.0.1:1/none?sslmode=disable")
	require.Error(t, err)
}

// --- dialect parity -------------------------------------------------------
//
// The two migration sets must describe the same schema: same tables, same
// column sets, same indexes, same migration versions. This test parses the
// embedded SQL — no database needed — and additionally verifies the SQLite
// side against the schema a real migrated file actually ends up with.

// dialectSchema parses every up migration of one dialect in version order
// and returns the final table -> columns mapping plus the set of index
// names touched. It understands the handful of statements the migrations
// use: CREATE TABLE, DROP TABLE, ALTER TABLE ... RENAME TO (the SQLite way
// of changing a constraint), and CREATE INDEX.
func dialectSchema(t *testing.T, dir string) (map[string][]string, map[string]bool) {
	t.Helper()

	files, err := fs.Glob(store.MigrationsFS(), dir+"/*.up.sql")
	require.NoError(t, err)
	sort.Strings(files)
	require.NotEmpty(t, files, "dialect %s has no up migrations", dir)

	tables := map[string][]string{}
	indexes := map[string]bool{}

	createRE := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s*\((.*?)\n\)`)
	dropRE := regexp.MustCompile(`(?i)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+)`)
	renameRE := regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+RENAME\s+TO\s+(\w+)`)
	indexRE := regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)

	for _, file := range files {
		raw, err := store.MigrationsFS().ReadFile(file)
		require.NoError(t, err)
		sql := string(raw)

		for _, m := range createRE.FindAllStringSubmatch(sql, -1) {
			tables[m[1]] = parseColumns(t, m[2])
		}
		for _, m := range renameRE.FindAllStringSubmatch(sql, -1) {
			if cols, ok := tables[m[1]]; ok {
				delete(tables, m[1])
				tables[m[2]] = cols
			}
		}
		for _, m := range dropRE.FindAllStringSubmatch(sql, -1) {
			// A rebuild drops the old table right before renaming the
			// replacement into its name; such a drop keeps the logical table,
			// only its columns now come from the rebuilt one.
			if renamedInto(sql, m[1]) {
				continue
			}
			delete(tables, m[1])
		}
		for _, m := range indexRE.FindAllStringSubmatch(sql, -1) {
			indexes[m[1]] = true
		}
		applyColumnAlters(tables, sql)
	}
	return tables, indexes
}

var (
	alterTableRE = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\w+)\s+(.*)$`)
	addColumnRE  = regexp.MustCompile(`(?i)\bADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	dropColumnRE = regexp.MustCompile(`(?i)\bDROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(\w+)`)
)

// applyColumnAlters folds ALTER TABLE ... ADD/DROP COLUMN statements into
// tables. Postgres adds several columns in one statement and SQLite one per
// statement, so statements are split on ';' and every clause is read.
func applyColumnAlters(tables map[string][]string, sql string) {
	var body strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	for _, stmt := range strings.Split(body.String(), ";") {
		m := alterTableRE.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		table := m[1]
		for _, add := range addColumnRE.FindAllStringSubmatch(m[2], -1) {
			tables[table] = append(tables[table], strings.ToLower(add[1]))
		}
		for _, drop := range dropColumnRE.FindAllStringSubmatch(m[2], -1) {
			kept := tables[table][:0]
			for _, c := range tables[table] {
				if c != strings.ToLower(drop[1]) {
					kept = append(kept, c)
				}
			}
			tables[table] = kept
		}
	}
}

var renameToRE = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+\w+\s+RENAME\s+TO\s+(\w+)`)

// renamedInto reports whether some ALTER ... RENAME TO name lands in the
// same file, meaning a DROP TABLE name is part of a rebuild, not a removal.
func renamedInto(sql, name string) bool {
	for _, m := range renameToRE.FindAllStringSubmatch(sql, -1) {
		if strings.EqualFold(m[1], name) {
			return true
		}
	}
	return false
}

var skipLinePrefixes = []string{"constraint", "primary", "unique", "check", "foreign"}

// parseColumns extracts column names from a CREATE TABLE body. Table entries
// in this project's migrations are one per line; lines that continue across
// newlines (nested parentheses) are re-joined before their first token is
// read, and table-level CONSTRAINT/PRIMARY KEY/... lines are skipped.
func parseColumns(t *testing.T, body string) []string {
	t.Helper()

	var cols []string
	var entry strings.Builder
	balance := 0

	flush := func() {
		line := strings.TrimSpace(entry.String())
		entry.Reset()
		line = strings.TrimSuffix(line, ",")
		if line == "" {
			return
		}
		if strings.HasPrefix(line, "--") {
			return
		}
		fields := strings.Fields(line)
		name := strings.ToLower(strings.Trim(fields[0], `"`))
		for _, p := range skipLinePrefixes {
			if name == p {
				return
			}
		}
		cols = append(cols, name)
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && balance == 0 {
			continue
		}
		entry.WriteString(trimmed)
		entry.WriteString("\n")
		balance += strings.Count(trimmed, "(") - strings.Count(trimmed, ")")
		if balance <= 0 {
			balance = 0
			flush()
		}
	}
	require.Equal(t, 0, balance, "unbalanced parentheses in migration body: %s", body)
	return cols
}

func TestMigrationDialectsStayInParity(t *testing.T) {
	pgTables, pgIndexes := dialectSchema(t, "migrations/postgres")
	sqliteTables, sqliteIndexes := dialectSchema(t, "migrations/sqlite")

	// Same tables, same columns per table.
	require.Contains(t, pgTables, "contracts")
	require.Contains(t, pgTables, "deployments")
	assert.Equal(t,
		setOf(pgTables["contracts"]), setOf(sqliteTables["contracts"]),
		"contracts columns diverged between dialects")
	assert.Equal(t,
		setOf(pgTables["deployments"]), setOf(sqliteTables["deployments"]),
		"deployments columns diverged between dialects")
	assert.Equal(t, setOfKeys(pgTables), setOfKeys(sqliteTables), "table sets diverged")

	// Same indexes backing the same queries.
	assert.Equal(t, pgIndexes, sqliteIndexes, "index sets diverged between dialects")

	// Same migration versions and basenames, so parity is maintained file by
	// file rather than only in the end state.
	pgFiles := migrationVersions(t, "migrations/postgres")
	sqliteFiles := migrationVersions(t, "migrations/sqlite")
	assert.Equal(t, pgFiles, sqliteFiles)

	// And the parsed SQLite schema must match what a real migrated file
	// contains — proof the parser reads what the engine executes.
	live := liveSQLiteSchema(t)
	assert.Equal(t, setOfKeys(pgTables), setOfKeys(live), "live sqlite tables differ from postgres")
	for table, cols := range pgTables {
		assert.Equal(t, setOf(cols), setOf(live[table]), "live sqlite columns differ from postgres for %s", table)
	}
}

func liveSQLiteSchema(t *testing.T) map[string][]string {
	t.Helper()

	ctx := context.Background()
	raw := newSQLiteStore(t)
	db := raw.(*store.SQLite).DB()

	out := map[string][]string{}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('contracts','deployments') ORDER BY name")
	require.NoError(t, err)
	var tableNames []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tableNames = append(tableNames, n)
	}
	rows.Close()

	for _, table := range tableNames {
		pr, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		require.NoError(t, err)
		for pr.Next() {
			var n string
			require.NoError(t, pr.Scan(&n))
			out[table] = append(out[table], n)
		}
		pr.Close()
	}
	return out
}

func migrationVersions(t *testing.T, dir string) []string {
	t.Helper()

	files, err := fs.Glob(store.MigrationsFS(), dir+"/*.up.sql")
	require.NoError(t, err)
	var names []string
	for _, f := range files {
		base := filepath.Base(f)
		names = append(names, strings.TrimSuffix(base, ".up.sql"))
	}
	sort.Strings(names)

	// Every up needs a real down.
	down, err := fs.Glob(store.MigrationsFS(), dir+"/*.down.sql")
	require.NoError(t, err)
	var downNames []string
	for _, f := range down {
		base := filepath.Base(f)
		downNames = append(downNames, strings.TrimSuffix(base, ".down.sql"))
	}
	sort.Strings(downNames)
	require.Equal(t, names, downNames, "every up migration needs a paired down in the same dialect")
	return names
}

func setOf(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[strings.ToLower(it)] = true
	}
	return out
}

func setOfKeys(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[strings.ToLower(k)] = true
	}
	return out
}
