package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the governance flow that no other operation covers:
// install bytecode first, let a separate party upgrade later. Nothing here
// creates a contract, so every assertion about the contracts table is an
// assertion that it was left alone.

func TestUploadInstallsBytecodeAndRecordsItAsItsOwnAction(t *testing.T) {
	// The alias is deliberately untracked: the governance flow uploads before
	// any deploy has happened, so requiring a tracked contract — as Upgrade
	// does — would make the command useless for its main use case.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	result, err := h.svc.Upload(ctx, deploy.UploadRequest{
		Alias: "counter",
		Notes: "v2 candidate for proposal",
	})
	require.NoError(t, err)

	assert.Equal(t, stellar.HashHex(h.wasmHash), result.WasmHash)
	assert.Equal(t, h.signer.Address(), result.Deployer)
	assert.False(t, result.DryRun)
	assert.False(t, result.UploadSkipped)
	assert.NotEmpty(t, result.UploadTxHash)

	// Exactly one transaction — the upload. There is nothing to instantiate.
	assert.Equal(t, 1, h.client.SendCount())

	// Recorded as an upload, with no contract address: content-addressed
	// bytecode has no address of its own.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, store.ActionUpload, history[0].Action)
	assert.Empty(t, history[0].ContractID)
	assert.Equal(t, stellar.HashHex(h.wasmHash), history[0].WasmHash)
	assert.Equal(t, "v2 candidate for proposal", history[0].Notes)
	assert.NotEmpty(t, history[0].TxHash)

	// Crucially, no contract was created by any of this. An upload that
	// silently registered a contract would make `status` check a contract that
	// does not exist.
	_, err = h.store.GetContract(ctx, "testnet", "counter")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUploadSkipsWhenTheBytecodeIsAlreadyOnChain(t *testing.T) {
	// A second upload of identical bytecode is a no-op that still costs a
	// transaction, so it is skipped — and nothing is recorded, because nothing
	// happened. The hash is still reported, since that is the useful part of
	// the answer for a proposal.
	h := newHarness(t, harnessOptions{})
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	result, err := h.svc.Upload(context.Background(), deploy.UploadRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.True(t, result.UploadSkipped)
	assert.Equal(t, stellar.HashHex(h.wasmHash), result.WasmHash)
	assert.Empty(t, result.UploadTxHash)
	assert.Zero(t, h.client.SendCount())
	assert.Zero(t, h.client.SimulateCount())

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history, "a skipped upload must not be recorded as an upload")
}

func TestUploadDryRunSimulatesAndReportsTheFeeWithoutSubmitting(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Upload(context.Background(), deploy.UploadRequest{
		Alias:  "counter",
		DryRun: true,
	})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.Zero(t, h.client.SendCount(), "a dry run must not submit anything")
	assert.Zero(t, h.signer.SignCount(), "a dry run must not sign anything")
	assert.Equal(t, 1, h.client.SimulateCount())

	// The fee and the assembled envelope are the dry run's deliverable.
	assert.Equal(t, int64(12_345), result.MinResourceFee)
	require.NotEmpty(t, result.UploadEnvelopeXDR)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.UploadEnvelopeXDR, &decoded))
	require.NotNil(t, decoded.V1)
	require.NotNil(t, decoded.V1.Tx.Ext.SorobanData)
	assert.Equal(t, xdr.Int64(12_345), decoded.V1.Tx.Ext.SorobanData.ResourceFee)
	assert.Empty(t, decoded.Signatures())

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)
}

func TestUploadRejectsUnknownAlias(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Upload(context.Background(), deploy.UploadRequest{Alias: "nonexistent"})
	assert.ErrorContains(t, err, "unknown contract")
}

func TestUploadWithoutSignerFailsClearly(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})

	_, err := h.svc.Upload(context.Background(), deploy.UploadRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "no signing key configured")
}
