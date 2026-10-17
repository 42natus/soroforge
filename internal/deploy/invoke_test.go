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

func TestInvokeContractSuccess(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	wasmHash := stellar.WasmHash([]byte(testWasm))
	trackContract(t, h, wasmHash)

	ctx := context.Background()
	result, err := h.svc.Invoke(ctx, deploy.InvokeRequest{
		Alias:    "counter",
		Function: "set_admin",
		Args:     []string{"address:GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", "u32:10"},
		Notes:    "rotate admin key",
	})
	require.NoError(t, err)

	assert.Equal(t, h.contractID, result.ContractID)
	assert.Equal(t, "set_admin", result.Function)
	assert.Equal(t, 1, h.client.SendCount())
	assert.NotEmpty(t, result.TxHash)

	// Invocations must be recorded in the store and visible in history.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, store.ActionInvoke, history[0].Action)
	assert.Equal(t, "set_admin", history[0].FnName)
	assert.Equal(t, "address:GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF u32:10", history[0].FnArgs)
	assert.Equal(t, "rotate admin key", history[0].Notes)
	assert.Equal(t, result.TxHash, history[0].TxHash)
}

func TestInvokeContractDryRun(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	ctx := context.Background()
	result, err := h.svc.Invoke(ctx, deploy.InvokeRequest{
		Alias:    "counter",
		Function: "pause",
		DryRun:   true,
	})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.Zero(t, h.client.SendCount())
	assert.NotEmpty(t, result.EnvelopeXDR)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.EnvelopeXDR, &decoded))
	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	assert.Equal(t, xdr.ScSymbol("pause"), op.HostFunction.InvokeContract.FunctionName)

	// Dry run records nothing.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)
}

func TestInvokeContractUntrackedFails(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Invoke(context.Background(), deploy.InvokeRequest{
		Alias:    "counter",
		Function: "pause",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "not tracked")
	assert.ErrorContains(t, err, "soroforge deploy counter")
}

func TestInvokeContractInvalidArgs(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	_, err := h.svc.Invoke(context.Background(), deploy.InvokeRequest{
		Alias:    "counter",
		Function: "set_fee",
		Args:     []string{"u32:not_a_number"},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "parse arguments")
}

func TestInvokeContractMissingFunction(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	_, err := h.svc.Invoke(context.Background(), deploy.InvokeRequest{
		Alias:    "counter",
		Function: "",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "function name is required")
}
