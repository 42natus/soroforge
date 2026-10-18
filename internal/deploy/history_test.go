package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryUnknownNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	history, err := h.svc.History(ctx, "devnet", "counter")
	assert.Nil(t, history)
	require.Error(t, err)
	assert.ErrorContains(t, err, `unknown network "devnet"`)
	// Verify it names the configured networks in the error message
	assert.ErrorContains(t, err, "configured: ")
	assert.ErrorContains(t, err, "testnet")
	assert.ErrorContains(t, err, "mainnet")
}

func TestHistoryEmptyNetworkUsesDefault(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.store.RecordDeployment(ctx, store.Deployment{
		Alias:          "counter",
		Network:        "testnet",
		ContractID:     h.contractID,
		WasmHash:       "v1",
		Action:         store.ActionDeploy,
		DeployerPubkey: "GABC",
	})
	require.NoError(t, err)

	history, err := h.svc.History(ctx, "", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "counter", history[0].Alias)
	assert.Equal(t, "testnet", history[0].Network)
	assert.Equal(t, h.contractID, history[0].ContractID)
	assert.Equal(t, "v1", history[0].WasmHash)
}

func TestHistoryAliasWithNoHistoryReturnsEmptyList(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	history, err := h.svc.History(ctx, "testnet", "never_deployed")
	require.NoError(t, err)
	assert.Empty(t, history)
	assert.NotNil(t, history)
}
