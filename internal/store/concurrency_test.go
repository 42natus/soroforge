package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrentDeploys is how many record-and-upsert pairs race for one alias.
const concurrentDeploys = 20

// runConcurrencySuite races N record-and-upsert pairs for a single alias, the
// shape of two HTTP deploys of the same alias arriving together. However they
// interleave, no history row may be lost, and the contract row must end up
// pointing at a deployment that was actually recorded.
func runConcurrencySuite(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()
	s := newStore(t)

	var wg sync.WaitGroup
	errs := make(chan error, concurrentDeploys*2)
	for i := 0; i < concurrentDeploys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash := fmt.Sprintf("hash-%02d", i)
			if _, err := s.RecordDeployment(ctx, store.Deployment{
				Alias: "counter", Network: "testnet", ContractID: "CCOUNTER", WasmHash: hash,
				Action: store.ActionDeploy, DeployerPubkey: "GABC",
			}); err != nil {
				errs <- err
			}
			if _, err := s.UpsertContract(ctx, store.Contract{
				Alias: "counter", Network: "testnet", ContractID: "CCOUNTER", CurrentWasmHash: hash,
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	history, err := s.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Len(t, history, concurrentDeploys, "every concurrent deploy keeps its history row")

	recorded := make(map[string]bool, len(history))
	for _, d := range history {
		recorded[d.WasmHash] = true
	}
	assert.Len(t, recorded, concurrentDeploys, "no history row was overwritten by another")

	contract, err := s.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.True(t, recorded[contract.CurrentWasmHash],
		"the contract row points at %q, which is not a recorded deployment", contract.CurrentWasmHash)

	contracts, err := s.ListContracts(ctx, "testnet")
	require.NoError(t, err)
	assert.Len(t, contracts, 1, "racing upserts must not duplicate the contract")
}

func TestMemoryStoreConcurrentDeploys(t *testing.T) {
	runConcurrencySuite(t, func(t *testing.T) store.Store { return store.NewMemoryStore() })
}

func TestSQLiteStoreConcurrentDeploys(t *testing.T) {
	runConcurrencySuite(t, newSQLiteStore)
}

// Runs against Postgres when TEST_DATABASE_URL is set, as the other store
// tests do.
func TestPostgresStoreConcurrentDeploys(t *testing.T) {
	runConcurrencySuite(t, newPostgresStore)
}
