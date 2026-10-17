package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/require"
)

// The Postgres implementation is exercised against a real database only when
// TEST_DATABASE_URL is set. `go test ./...` on a clean checkout must never
// require a database — so without that variable these tests skip, and the
// shared suite still runs against MemoryStore.
//
// To run them:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL=postgres://soroforge:soroforge@localhost:5432/soroforge?sslmode=disable \
//	    go test ./internal/store/
//
// The database is migrated and truncated between tests, so it must be a
// throwaway.
const testDatabaseURLEnv = "TEST_DATABASE_URL"

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv(testDatabaseURLEnv)
	if url == "" {
		t.Skipf("set %s to run Postgres integration tests", testDatabaseURLEnv)
	}
	return url
}

// newPostgresStore returns a migrated, empty Postgres store.
func newPostgresStore(t *testing.T) store.Store {
	t.Helper()
	url := testDatabaseURL(t)
	ctx := context.Background()

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	require.NoError(t, migrator.Up())
	require.NoError(t, migrator.Close())

	truncate(t, url)

	s, err := store.NewPostgres(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

func truncate(t *testing.T, url string) {
	t.Helper()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, "TRUNCATE contracts, deployments RESTART IDENTITY")
	require.NoError(t, err)
}

func TestPostgresStore(t *testing.T) {
	// The same suite MemoryStore passes, so the fake used everywhere else is
	// held to real database semantics.
	runStoreSuite(t, newPostgresStore)
}

func TestPostgresMigrationsAreReversible(t *testing.T) {
	url := testDatabaseURL(t)

	migrator, err := store.NewMigrator(url)
	require.NoError(t, err)
	defer func() { require.NoError(t, migrator.Close()) }()

	require.NoError(t, migrator.Up())

	version, dirty, err := migrator.Version()
	require.NoError(t, err)
	require.False(t, dirty, "migrations must not leave the database dirty")
	require.Equal(t, uint(2), version)

	// The suite that runs first leaves rows behind, and reverting 000002
	// narrows the action constraint — with an 'upload' row present the ADD
	// CONSTRAINT correctly refuses, by design. Reversibility is a schema
	// property, so prove it against a clean table.
	truncate(t, url)

	require.NoError(t, migrator.Down())
	require.NoError(t, migrator.Up())
}

func TestPostgresUpIsIdempotent(t *testing.T) {
	// `soroforge migrate up` runs on every deploy in some setups; a second run
	// must be a no-op rather than an error.
	url := testDatabaseURL(t)

	for i := 0; i < 2; i++ {
		migrator, err := store.NewMigrator(url)
		require.NoError(t, err)
		require.NoError(t, migrator.Up())
		require.NoError(t, migrator.Close())
	}
}

func TestNewPostgresRejectsBadURL(t *testing.T) {
	// No database needed: this must fail before connecting.
	_, err := store.NewPostgres(context.Background(), "")
	require.ErrorContains(t, err, "database URL is required")

	_, err = store.NewPostgres(context.Background(), "://not a url")
	require.Error(t, err)
	// The URL may carry a password, so it must not appear in the error.
	require.NotContains(t, err.Error(), "not a url")
}
