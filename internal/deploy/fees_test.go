package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mocked simulation reports a 12,345-stroop resource fee for every
// transaction, so with the minimum per-operation base fee each transaction
// totals 12,445 stroops.

func TestDeployDryRunReportsEstimatedFees(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, result.EstimatedFees)

	for name, fee := range map[string]*deploy.FeeEstimate{
		"upload": result.EstimatedFees.Upload,
		"create": result.EstimatedFees.Create,
	} {
		require.NotNil(t, fee, "%s should carry an estimate", name)
		assert.Equal(t, int64(12_345), fee.ResourceFeeStroops)
		assert.Equal(t, int64(txnbuild.MinBaseFee), fee.InclusionFeeStroops)
		assert.Equal(t, int64(12_445), fee.TotalStroops)
		assert.Equal(t, "0.0012445", fee.TotalXLM)
	}

	assert.Nil(t, result.EstimatedFees.Upgrade)
	assert.Equal(t, int64(24_890), result.EstimatedFees.Total.TotalStroops)
	assert.Equal(t, "0.0024890", result.EstimatedFees.Total.TotalXLM)
}

func TestDeployDryRunFeeExcludesSkippedUpload(t *testing.T) {
	// A real deploy of already-uploaded bytecode would not submit the upload,
	// so the estimate must not charge for it either.
	h := newHarness(t, harnessOptions{})
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, result.EstimatedFees)

	assert.Nil(t, result.EstimatedFees.Upload, "a skipped upload costs nothing")
	require.NotNil(t, result.EstimatedFees.Create)
	assert.Equal(t, result.EstimatedFees.Create.TotalStroops, result.EstimatedFees.Total.TotalStroops)
}

func TestUpgradeDryRunReportsEstimatedFees(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
	require.NotNil(t, result.EstimatedFees)

	assert.NotNil(t, result.EstimatedFees.Upload)
	assert.NotNil(t, result.EstimatedFees.Upgrade)
	assert.Nil(t, result.EstimatedFees.Create)
	assert.Equal(t, int64(24_890), result.EstimatedFees.Total.TotalStroops)
}

func TestRealDeployDoesNotReportEstimatedFees(t *testing.T) {
	// Estimates are dry-run output; a submitted deploy reports what happened.
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)
	assert.Nil(t, result.EstimatedFees)
}
