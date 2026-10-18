package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runHistorySuite exercises history export/import against every Store
// implementation. The guarantee under test is losslessness: an export, a JSON
// round trip, and an import into a fresh database must reproduce the original
// ids and timestamps exactly — a history that gets re-stamped on the way
// through a file has stopped being a history.
func runHistorySuite(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	ctx := context.Background()

	seed := func(t *testing.T) store.Store {
		t.Helper()
		s := newStore(t)

		for _, c := range []store.Contract{
			{Alias: "counter", Network: "testnet", ContractID: "CTEST", CurrentWasmHash: "aaaa"},
			{Alias: "token", Network: "testnet", ContractID: "CTOKEN", CurrentWasmHash: "bbbb"},
			{Alias: "counter", Network: "mainnet", ContractID: "CMAIN", CurrentWasmHash: "cccc"},
		} {
			_, err := s.UpsertContract(ctx, c)
			require.NoError(t, err)
		}
		for _, d := range []store.Deployment{
			{Alias: "counter", Network: "testnet", ContractID: "CTEST", WasmHash: "v1",
				Action: store.ActionDeploy, DeployerPubkey: "GAAA", TxHash: "tx1", Ledger: 100},
			{Alias: "counter", Network: "testnet", ContractID: "CTEST", WasmHash: "v2",
				Action: store.ActionUpgrade, DeployerPubkey: "GAAA", TxHash: "tx2", Ledger: 200,
				Notes: "fix rounding"},
			// An upload for an alias no longer tracked as a contract: the
			// export must not lose it just because its (network, alias) has
			// no live row.
			{Alias: "removed", Network: "testnet", WasmHash: "deadbeef",
				Action: store.ActionUpload, DeployerPubkey: "GZZZ"},
			{Alias: "counter", Network: "mainnet", ContractID: "CMAIN", WasmHash: "v1",
				Action: store.ActionDeploy, DeployerPubkey: "GBBB", TxHash: "tx3", Ledger: 5},
		} {
			_, err := s.RecordDeployment(ctx, d)
			require.NoError(t, err)
		}
		return s
	}

	normalize := func(snap *store.Snapshot) {
		// Postgres keeps microsecond precision; in-memory keeps whatever the
		// clock gave. Truncate to the common denominator so the suite can
		// compare exact bytes across backends.
		for i := range snap.Contracts {
			snap.Contracts[i].CreatedAt = snap.Contracts[i].CreatedAt.UTC().Round(time.Microsecond)
			snap.Contracts[i].UpdatedAt = snap.Contracts[i].UpdatedAt.UTC().Round(time.Microsecond)
		}
		for i := range snap.Deployments {
			snap.Deployments[i].CreatedAt = snap.Deployments[i].CreatedAt.UTC().Round(time.Microsecond)
		}
		snap.GeneratedAt = time.Time{}
	}

	canonical := func(t *testing.T, snap *store.Snapshot) string {
		t.Helper()
		cp := *snap
		normalize(&cp)
		data, err := cp.Marshal()
		require.NoError(t, err)
		return string(data)
	}

	t.Run("export then import into a fresh store is lossless", func(t *testing.T) {
		source := seed(t)

		snap, err := store.BuildSnapshot(ctx, source)
		require.NoError(t, err)
		assert.Equal(t, store.SnapshotVersion, snap.Version)
		require.Len(t, snap.Contracts, 3)
		require.Len(t, snap.Deployments, 4)

		data, err := snap.Marshal()
		require.NoError(t, err)
		parsed, err := store.ParseSnapshot(data)
		require.NoError(t, err)

		target := newStore(t)
		mustImport(t, ctx, target, parsed, store.ImportOptions{})

		rebuilt, err := store.BuildSnapshot(ctx, target)
		require.NoError(t, err)

		// Byte-for-byte once metadata and sub-microsecond precision are set
		// aside: ids, timestamps, notes, nullable fields all survive exactly.
		assert.Equal(t, canonical(t, snap), canonical(t, rebuilt))
	})

	t.Run("imported ids and timestamps are the originals, not re-stamped", func(t *testing.T) {
		source := seed(t)
		snap, err := store.BuildSnapshot(ctx, source)
		require.NoError(t, err)
		parsed, err := store.ParseSnapshot(mustMarshal(t, snap))
		require.NoError(t, err)

		target := newStore(t)
		mustImport(t, ctx, target, parsed, store.ImportOptions{})

		got, err := target.GetContract(ctx, "testnet", "counter")
		require.NoError(t, err)
		var wantID int64
		var wantCreated time.Time
		for _, c := range parsed.Contracts {
			if c.Network == "testnet" && c.Alias == "counter" {
				wantID, wantCreated = c.ID, c.CreatedAt
			}
		}
		assert.Equal(t, wantID, got.ID, "the contract keeps its id")
		assert.True(t, wantCreated.Round(time.Microsecond).Equal(got.CreatedAt.Round(time.Microsecond)),
			"the contract keeps its original created_at")

		all, err := target.ListAllDeployments(ctx)
		require.NoError(t, err)
		require.Len(t, all, len(parsed.Deployments))
		for i, d := range all {
			assert.Equal(t, parsed.Deployments[i].ID, d.ID)
			assert.True(t, parsed.Deployments[i].CreatedAt.Round(time.Microsecond).Equal(d.CreatedAt.Round(time.Microsecond)))
		}

		// And the sequence continues past the imported ids, so a post-import
		// deploy cannot collide with the history it just joined.
		before := all[len(all)-1].ID
		next, err := target.RecordDeployment(ctx, store.Deployment{
			Alias: "counter", Network: "testnet", ContractID: "CTEST", WasmHash: "v3",
			Action: store.ActionUpgrade, DeployerPubkey: "GAAA",
		})
		require.NoError(t, err)
		assert.Greater(t, next.ID, before, "ids continue past the imported range")
	})

	t.Run("import refuses a non-empty database unless merge is set", func(t *testing.T) {
		source := seed(t)
		snap, err := store.BuildSnapshot(ctx, source)
		require.NoError(t, err)
		parsed, err := store.ParseSnapshot(mustMarshal(t, snap))
		require.NoError(t, err)

		target := newStore(t)
		mustImport(t, ctx, target, parsed, store.ImportOptions{})

		// Now it is not empty: the same file must be refused without
		// --merge, and accepted (as exact duplicates) with it.
		_, err = store.Import(ctx, target, parsed, store.ImportOptions{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "not empty")
		assert.ErrorContains(t, err, "--merge")

		mustImport(t, ctx, target, parsed, store.ImportOptions{Merge: true})
	})

	t.Run("merge skips exact duplicates", func(t *testing.T) {
		source := seed(t)
		snap, err := store.BuildSnapshot(ctx, source)
		require.NoError(t, err)
		parsed, err := store.ParseSnapshot(mustMarshal(t, snap))
		require.NoError(t, err)

		// Half the history is already there: importing the whole file must
		// end with exactly the file, having added only what was missing.
		target := newStore(t)
		mustImport(t, ctx, target, &store.Snapshot{
			Version:     store.SnapshotVersion,
			Contracts:   parsed.Contracts[:1],
			Deployments: parsed.Deployments[:2],
		}, store.ImportOptions{})

		mustImport(t, ctx, target, parsed, store.ImportOptions{Merge: true})

		rebuilt, err := store.BuildSnapshot(ctx, target)
		require.NoError(t, err)
		assert.Equal(t, canonical(t, parsed), canonical(t, rebuilt))

		// Re-running the identical import is a no-op.
		mustImport(t, ctx, target, parsed, store.ImportOptions{Merge: true})
		rebuilt2, err := store.BuildSnapshot(ctx, target)
		require.NoError(t, err)
		assert.Equal(t, canonical(t, parsed), canonical(t, rebuilt2))
	})

	t.Run("merge fails loudly on identity conflicts", func(t *testing.T) {
		source := seed(t)
		snap, err := store.BuildSnapshot(ctx, source)
		require.NoError(t, err)
		parsed, err := store.ParseSnapshot(mustMarshal(t, snap))
		require.NoError(t, err)

		// Same id, different content: the file and the database disagree
		// about what a record *is*, which no merge policy may resolve.
		target := newStore(t)
		mustImport(t, ctx, target, parsed, store.ImportOptions{})

		clash := *parsed
		clash.Deployments = append([]store.Deployment{}, parsed.Deployments...)
		clash.Deployments[0].WasmHash = "tampered"
		_, err = store.Import(ctx, target, &clash, store.ImportOptions{Merge: true})
		require.Error(t, err)
		assert.ErrorContains(t, err, "different content")

		// Same (network, alias), different id: refusing to renumber a
		// contract, because every deployment history joins on it.
		renumbered := *parsed
		renumbered.Contracts = append([]store.SnapshotContract{}, parsed.Contracts...)
		renumbered.Contracts[0].ID = maxContractID(parsed) + 100
		_, err = store.Import(ctx, target, &renumbered, store.ImportOptions{Merge: true})
		require.Error(t, err)
		assert.ErrorContains(t, err, "already tracked")
	})
}

func maxContractID(snap *store.Snapshot) int64 {
	var max int64
	for _, c := range snap.Contracts {
		if c.ID > max {
			max = c.ID
		}
	}
	return max
}

func mustImport(t *testing.T, ctx context.Context, s store.Store, snap *store.Snapshot, opts store.ImportOptions) *store.ImportReport {
	t.Helper()
	report, err := store.Import(ctx, s, snap, opts)
	require.NoError(t, err)
	return report
}

func mustMarshal(t *testing.T, snap *store.Snapshot) []byte {
	t.Helper()
	data, err := snap.Marshal()
	require.NoError(t, err)
	return data
}

func TestSnapshotValidation(t *testing.T) {
	good := store.Snapshot{
		Version:     store.SnapshotVersion,
		GeneratedAt: time.Now().UTC(),
		Contracts: []store.SnapshotContract{{
			ID: 1, Alias: "counter", Network: "testnet", ContractID: "C1",
			CurrentWasmHash: "aa", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}},
		Deployments: []store.Deployment{{
			ID: 2, Alias: "counter", Network: "testnet", ContractID: "C1", WasmHash: "aa",
			Action: store.ActionDeploy, DeployerPubkey: "GAAA", CreatedAt: time.Now().UTC(),
		}},
	}

	data, err := good.Marshal()
	require.NoError(t, err)
	parsed, err := store.ParseSnapshot(data)
	require.NoError(t, err)
	assert.Len(t, parsed.Contracts, 1)

	t.Run("unknown version is rejected", func(t *testing.T) {
		bad := good
		bad.Version = 99
		_, err := store.ParseSnapshot(mustMarshal(t, &bad))
		require.Error(t, err)
		assert.ErrorContains(t, err, "unsupported format version 99")

		_, err = store.ParseSnapshot([]byte(`{"contracts":[],"deployments":[]}`))
		require.Error(t, err)
		assert.ErrorContains(t, err, "version")
	})

	t.Run("zero and duplicate ids are rejected", func(t *testing.T) {
		bad := good
		bad.Contracts = append(bad.Contracts, store.SnapshotContract{
			Alias: "token", Network: "testnet", ContractID: "C2", CurrentWasmHash: "bb",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		_, err := store.ParseSnapshot(mustMarshal(t, &bad))
		require.Error(t, err)
		assert.ErrorContains(t, err, "id must be positive")

		dup := good
		dup.Deployments = append(dup.Deployments, dup.Deployments[0])
		_, err = store.ParseSnapshot(mustMarshal(t, &dup))
		require.Error(t, err)
		assert.ErrorContains(t, err, "duplicate deployment id")
	})

	t.Run("missing timestamps are rejected rather than re-stamped", func(t *testing.T) {
		bad := good
		bad.Deployments[0].CreatedAt = time.Time{}
		_, err := store.ParseSnapshot(mustMarshal(t, &bad))
		require.Error(t, err)
		assert.ErrorContains(t, err, "created_at is required")
	})

	t.Run("invalid records are rejected", func(t *testing.T) {
		bad := good
		bad.Deployments[0].Action = "sideways"
		_, err := store.ParseSnapshot(mustMarshal(t, &bad))
		require.Error(t, err)
		assert.ErrorContains(t, err, "action")
	})

	t.Run("unknown fields are rejected", func(t *testing.T) {
		_, err := store.ParseSnapshot([]byte(
			`{"version":1,"contracts":[],"deployments":[],"surprise":true}`))
		require.Error(t, err)
		assert.ErrorContains(t, err, "surprise")
	})

	t.Run("two documents in one file are rejected", func(t *testing.T) {
		_, err := store.ParseSnapshot(append(data, data...))
		require.Error(t, err)
		assert.ErrorContains(t, err, "more than one JSON document")
	})

	t.Run("import rejects a store that cannot preserve identities", func(t *testing.T) {
		// A foreign Store implementation that lacks the snapshot import hook
		// must fail clearly instead of quietly re-stamping ids and
		// timestamps through RecordDeployment.
		var s store.Store = &noImportStore{Store: store.NewMemoryStore()}
		_, err := store.Import(context.Background(), s, &good, store.ImportOptions{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot import")
	})
}

// noImportStore wraps a Store in an interface-typed field, so none of the
// package-internal snapshot import hook is promoted onto it.
type noImportStore struct {
	store.Store
}
