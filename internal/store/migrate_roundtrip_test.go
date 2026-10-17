package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/require"
)

// TestMigrationsRoundTrip proves that every down migration really reverses its
// up migration: migrate all the way up, all the way back down to zero, then up
// again, and require the second up to produce the same schema as the first. A
// down migration that drops too little (or too much) fails here rather than in
// an incident.
//
// The comparison reads information_schema.columns and pg_indexes, so it picks
// up every migration — including ones added later — without edits.
//
// Like the rest of the Postgres suite it runs only when TEST_DATABASE_URL is
// set, against a throwaway database.
func TestMigrationsRoundTrip(t *testing.T) {
	url := testDatabaseURL(t)

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	defer func() { require.NoError(t, migrator.Close()) }()

	require.NoError(t, migrator.Up())
	afterFirstUp := schemaSnapshot(t, url)
	require.NotEmpty(t, afterFirstUp, "the up migrations must create a schema")

	downToZero(t, migrator)

	require.Empty(t, tableNames(t, url), "a full down must leave no tables behind")

	require.NoError(t, migrator.Up())
	afterSecondUp := schemaSnapshot(t, url)

	require.Equal(t, afterFirstUp, afterSecondUp, "up-down-up must reproduce the same schema")
}

// downToZero steps the migration chain back one migration at a time until
// nothing is applied, asserting that no step leaves the database dirty.
func downToZero(t *testing.T, migrator *store.Migrator) {
	t.Helper()

	for {
		version, dirty, err := migrator.Version()
		require.NoError(t, err)
		require.False(t, dirty, "a down migration left the database dirty")
		if version == 0 {
			return
		}
		require.NoError(t, migrator.Down(), "reverting version %d", version)
	}
}

// schemaSnapshot captures every column and index in the public schema as
// stable, human-readable lines that require.Equal can diff meaningfully when
// the two snapshots disagree.
//
// golang-migrate's own schema_migrations bookkeeping table is excluded; it
// survives a full down by design and is not part of the application schema.
func schemaSnapshot(t *testing.T, url string) []string {
	t.Helper()

	columns := queryStrings(t, url, `
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type ||
		       ' nullable=' || is_nullable ||
		       ' default=' || coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY table_name, column_name
	`)

	indexes := queryStrings(t, url, `
		SELECT 'index ' || tablename || ' ' || indexname || ' ' || indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		ORDER BY tablename, indexname
	`)

	return append(columns, indexes...)
}

// tableNames lists the user tables in the public schema, so a down migration
// that forgets to drop one is caught even when no index depends on it.
func tableNames(t *testing.T, url string) []string {
	t.Helper()

	return queryStrings(t, url, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_type = 'BASE TABLE'
		  AND table_name <> 'schema_migrations'
		ORDER BY table_name
	`)
}

func queryStrings(t *testing.T, url, sql string) []string {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	rows, err := pool.Query(ctx, sql)
	require.NoError(t, err)

	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return values
}
