package deploy_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers `deploy --all`: one run that fills in every configured
// contract a network does not have yet, skipping the ones it does.

// multiContractConfig points three aliases at the same artifact, deliberately
// not in alias order. --all must process them in alias order, not config order.
const multiContractConfig = `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  zeta:
    wasm: ./counter.wasm
  mid:
    wasm: ./counter.wasm
  alpha:
    wasm: ./counter.wasm
`

// failedContractConfig has one broken contract in the middle of alias order,
// so a run over it must deploy alpha, fail on broken, and never reach omega.
const failedContractConfig = `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  alpha:
    wasm: ./counter.wasm
  broken:
    wasm: ./never-built.wasm
  omega:
    wasm: ./counter.wasm
`

func resultAliases(results []deploy.DeployAllResult) []string {
	aliases := make([]string, 0, len(results))
	for _, r := range results {
		aliases = append(aliases, r.Alias)
	}
	return aliases
}

func TestDeployAllDeploysUntrackedContractsInAliasOrder(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, harnessOptions{configBody: multiContractConfig, log: &logs})
	ctx := context.Background()

	// zeta is tracked already; --all must leave it alone rather than create a
	// second contract and move the alias to a new address.
	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias:           "zeta",
		Network:         "testnet",
		ContractID:      h.contractID,
		CurrentWasmHash: stellar.HashHex(h.wasmHash),
	})
	require.NoError(t, err)

	results, err := h.svc.DeployAll(ctx, deploy.DeployAllRequest{Notes: "batch"})
	require.NoError(t, err)
	require.Len(t, results, 3)

	// Alias order, not the config file's order.
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, resultAliases(results))

	assert.Equal(t, deploy.DeployAllDeployed, results[0].Status)
	require.NotNil(t, results[0].Result)
	assert.Equal(t, "alpha", results[0].Result.Alias)
	assert.Equal(t, "testnet", results[0].Result.Network)
	require.NotNil(t, results[0].Result.Deployment)
	assert.Equal(t, "batch", results[0].Result.Deployment.Notes)

	assert.Equal(t, deploy.DeployAllDeployed, results[1].Status)
	require.NotNil(t, results[1].Result)

	// The tracked contract is skipped, no result attached, and the run says
	// so rather than failing silently.
	assert.Equal(t, deploy.DeployAllSkipped, results[2].Status)
	assert.Nil(t, results[2].Result)
	assert.Empty(t, results[2].Error)
	assert.Contains(t, logs.String(), "already tracked")
	assert.Contains(t, logs.String(), "alias=zeta")

	// Both untracked contracts are now tracked, each with its own history.
	for _, alias := range []string{"alpha", "mid"} {
		tracked, err := h.store.GetContract(ctx, "testnet", alias)
		require.NoError(t, err)
		assert.Equal(t, h.contractID, tracked.ContractID)

		history, err := h.store.History(ctx, "testnet", alias)
		require.NoError(t, err)
		assert.Len(t, history, 1)
	}

	// Two transactions per deployed contract and none for the skipped one.
	// (The mock does not persist an upload, so each deploy uploads its own.)
	assert.Equal(t, 4, h.client.SendCount())
}

func TestDeployAllDryRunSubmitsNothingAndRecordsNothing(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: multiContractConfig})
	ctx := context.Background()

	results, err := h.svc.DeployAll(ctx, deploy.DeployAllRequest{DryRun: true})
	require.NoError(t, err)
	require.Len(t, results, 3)

	for _, r := range results {
		assert.Equal(t, deploy.DeployAllDryRun, r.Status)
		require.NotNil(t, r.Result)
		assert.True(t, r.Result.DryRun)
		assert.NotEmpty(t, r.Result.CreateEnvelopeXDR, "the envelope is the dry run's deliverable")
	}

	assert.Zero(t, h.client.SendCount(), "a dry run must not submit anything")
	assert.Zero(t, h.signer.SignCount(), "a dry run must not sign anything")

	_, err = h.store.GetContract(ctx, "testnet", "alpha")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeployAllStopsAtTheFirstFailureAndReportsWhatSucceeded(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: failedContractConfig})
	ctx := context.Background()

	results, err := h.svc.DeployAll(ctx, deploy.DeployAllRequest{})
	require.Error(t, err)
	assert.ErrorContains(t, err, "deploy broken on testnet")
	assert.ErrorContains(t, err, "wasm artifact not found")

	// The partial results are how the caller learns what happened before the
	// run stopped: alpha succeeded, broken failed, omega was not attempted.
	require.Len(t, results, 2)
	assert.Equal(t, "alpha", results[0].Alias)
	assert.Equal(t, deploy.DeployAllDeployed, results[0].Status)
	assert.Equal(t, "broken", results[1].Alias)
	assert.Equal(t, deploy.DeployAllFailed, results[1].Status)
	assert.Contains(t, results[1].Error, "has the contract been built?")
	assert.Nil(t, results[1].Result)

	// Only alpha reached the network.
	assert.Equal(t, 2, h.client.SendCount())

	_, err = h.store.GetContract(ctx, "testnet", "alpha")
	require.NoError(t, err)
	_, err = h.store.GetContract(ctx, "testnet", "broken")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = h.store.GetContract(ctx, "testnet", "omega")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeployAllSkipsOnlyOnTheRequestedNetwork(t *testing.T) {
	// Tracking is per-network, so a contract live on mainnet is still
	// untracked on testnet and --all on testnet must deploy it.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias:           "counter",
		Network:         "mainnet",
		ContractID:      h.contractID,
		CurrentWasmHash: stellar.HashHex(h.wasmHash),
	})
	require.NoError(t, err)

	results, err := h.svc.DeployAll(ctx, deploy.DeployAllRequest{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, deploy.DeployAllDeployed, results[0].Status)

	_, err = h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
}

func TestDeployAllRejectsUnknownNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.DeployAll(context.Background(), deploy.DeployAllRequest{Network: "staging"})
	assert.ErrorContains(t, err, "unknown network")
}
