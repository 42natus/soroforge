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

const upgradedWasm = "\x00asm\x01\x00\x00\x00upgraded"

// trackContract seeds the store as if a deploy had already happened, and points
// the mocked ledger at the same hash.
func trackContract(t *testing.T, h *harness, wasmHash xdr.Hash) {
	t.Helper()
	ctx := context.Background()

	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias:           "counter",
		Network:         "testnet",
		ContractID:      h.contractID,
		CurrentWasmHash: stellar.HashHex(wasmHash),
	})
	require.NoError(t, err)
	require.NoError(t, h.client.SetContractInstance(h.contractID, wasmHash))
}

func TestUpgradeReplacesBytecodeAndRecordsTransition(t *testing.T) {
	// The contract is live running the original bytecode; the config now points
	// at a newer build.
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})
	oldHash := stellar.WasmHash([]byte(testWasm))
	trackContract(t, h, oldHash)

	ctx := context.Background()
	result, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{
		Alias: "counter",
		Notes: "patch release",
	})
	require.NoError(t, err)

	assert.Equal(t, h.contractID, result.ContractID, "upgrading keeps the contract address")
	assert.Equal(t, stellar.HashHex(oldHash), result.PreviousWasmHash)
	assert.Equal(t, stellar.HashHex(h.wasmHash), result.WasmHash)
	assert.Equal(t, "upgrade", result.UpgradeFunc)
	assert.False(t, result.NoChange)

	// Upload the new bytecode, then invoke the contract's upgrade entrypoint.
	assert.Equal(t, 2, h.client.SendCount())

	// The history must record the transition, not just the new state.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, store.ActionUpgrade, history[0].Action)
	assert.Equal(t, stellar.HashHex(h.wasmHash), history[0].WasmHash)
	assert.Equal(t, "patch release", history[0].Notes)

	// And the tracked current hash must move forward.
	tracked, err := h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, stellar.HashHex(h.wasmHash), tracked.CurrentWasmHash)
}

func TestUpgradeInvokesTheConfiguredEntrypointWithTheNewHash(t *testing.T) {
	// Soroban has no upgrade operation: SoroForge calls a function the contract
	// exposes, passing the new hash as BytesN<32>.
	h := newHarness(t, harnessOptions{
		wasm: upgradedWasm,
		configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    upgrade_fn: admin_upgrade
`,
	})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{
		Alias:  "counter",
		DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "admin_upgrade", result.UpgradeFunc)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.UpgradeEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	require.Equal(t, xdr.HostFunctionTypeHostFunctionTypeInvokeContract, op.HostFunction.Type)

	invoke := op.HostFunction.InvokeContract
	assert.Equal(t, xdr.ScSymbol("admin_upgrade"), invoke.FunctionName)
	require.Len(t, invoke.Args, 1)
	require.NotNil(t, invoke.Args[0].Bytes)
	assert.Equal(t, h.wasmHash[:], []byte(*invoke.Args[0].Bytes),
		"the argument must be the raw 32-byte hash of the new bytecode")
}

func TestUpgradeIsANoOpWhenBytecodeIsUnchanged(t *testing.T) {
	// Rebuilding without changing the source produces the same hash; upgrading
	// to what is already live should cost nothing.
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, h.wasmHash)

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.True(t, result.NoChange)
	assert.Zero(t, h.client.SendCount())

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history, "a no-op upgrade must not create a history entry")
}

func TestUpgradeComparesAgainstTheLedgerNotTheDatabase(t *testing.T) {
	// If someone upgraded the contract outside SoroForge, the database is
	// stale. The previous hash reported must be what is genuinely live.
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})

	driftedHash := stellar.WasmHash([]byte("deployed by someone else"))
	ctx := context.Background()

	// The database claims one hash...
	_, err := h.store.UpsertContract(ctx, store.Contract{
		Alias: "counter", Network: "testnet", ContractID: h.contractID,
		CurrentWasmHash: stellar.HashHex(stellar.WasmHash([]byte(testWasm))),
	})
	require.NoError(t, err)
	// ...while the ledger says another.
	require.NoError(t, h.client.SetContractInstance(h.contractID, driftedHash))

	result, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.Equal(t, stellar.HashHex(driftedHash), result.PreviousWasmHash,
		"the previous hash must come from the ledger, not the stale record")
}

func TestUpgradeDryRunSubmitsNothingAndRecordsNothing(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	ctx := context.Background()
	result, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.Zero(t, h.client.SendCount())
	assert.Zero(t, h.signer.SignCount())
	assert.NotEmpty(t, result.UpgradeEnvelopeXDR)

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)

	// The tracked hash must be untouched.
	tracked, err := h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, stellar.HashHex(stellar.WasmHash([]byte(testWasm))), tracked.CurrentWasmHash)
}

func TestUpgradeRequiresATrackedContract(t *testing.T) {
	// Upgrading needs to know which address to upgrade; an untracked contract
	// is a deploy, and the error should say so.
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "not tracked")
	assert.ErrorContains(t, err, "soroforge deploy counter")
}

func TestUpgradeSkipsUploadWhenBytecodeIsAlreadyOnChain(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.True(t, result.UploadSkipped)
	assert.Equal(t, 1, h.client.SendCount(), "only the upgrade invocation should be submitted")
}

func TestUpgradeWithoutSignerFailsClearly(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})

	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "no signing key configured")
}

func TestUpgradeSurfacesContractRejection(t *testing.T) {
	// A contract that does not expose an upgrade entrypoint, or that rejects
	// the caller's authorization, fails at simulation. The message should carry
	// the contract's own error rather than hiding it.
	h := newHarness(t, harnessOptions{wasm: upgradedWasm})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	h.client.SimulateResponse = stellar.SimulateResult{
		Error: "HostError: Error(Contract, #1) - Unauthorized",
	}

	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "Unauthorized")
	assert.ErrorContains(t, err, "invoke upgrade on")
}

func TestUpgradeToWasmHashSuccess(t *testing.T) {
	// Upgrade directly to a known on-chain hash, without local WASM build.
	h := newHarness(t, harnessOptions{})
	currentHash := stellar.WasmHash([]byte(testWasm))
	trackContract(t, h, currentHash)

	targetHash := stellar.WasmHash([]byte(upgradedWasm))
	targetHashHex := stellar.HashHex(targetHash)
	require.NoError(t, h.client.SetWasmUploaded(targetHash))

	ctx := context.Background()
	result, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{
		Alias:    "counter",
		WasmHash: targetHashHex,
		Notes:    "rollback to previous hash",
	})
	require.NoError(t, err)

	assert.Equal(t, h.contractID, result.ContractID)
	assert.Equal(t, stellar.HashHex(currentHash), result.PreviousWasmHash)
	assert.Equal(t, targetHashHex, result.WasmHash)
	assert.True(t, result.UploadSkipped)
	assert.Equal(t, 1, h.client.SendCount(), "only the upgrade invocation should be submitted")

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, targetHashHex, history[0].WasmHash)
	assert.Equal(t, "rollback to previous hash", history[0].Notes)
}

func TestUpgradeToWasmHashShortPrefix(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	currentHash := stellar.WasmHash([]byte("live-code"))
	trackContract(t, h, currentHash)

	targetHash := stellar.WasmHash([]byte("historical-code"))
	targetHashHex := stellar.HashHex(targetHash)
	require.NoError(t, h.client.SetWasmUploaded(targetHash))

	// Seed targetHash in history
	ctx := context.Background()
	_, err := h.store.RecordDeployment(ctx, store.Deployment{
		Alias:          "counter",
		Network:        "testnet",
		ContractID:     h.contractID,
		WasmHash:       targetHashHex,
		Action:         store.ActionDeploy,
		DeployerPubkey: "GABC",
	})
	require.NoError(t, err)

	prefix := targetHashHex[:12]
	result, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{
		Alias:    "counter",
		WasmHash: prefix,
	})
	require.NoError(t, err)
	assert.Equal(t, targetHashHex, result.WasmHash)
}

func TestUpgradeToWasmHashAmbiguousPrefix(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte("live-code")))

	ctx := context.Background()
	_, err := h.store.RecordDeployment(ctx, store.Deployment{
		Alias:          "counter",
		Network:        "testnet",
		ContractID:     h.contractID,
		WasmHash:       "aaaa1111222233334444555566667777888899990000aaaabbbbccccddddeeee01",
		Action:         store.ActionDeploy,
		DeployerPubkey: "GABC",
	})
	require.NoError(t, err)
	_, err = h.store.RecordDeployment(ctx, store.Deployment{
		Alias:          "counter",
		Network:        "testnet",
		ContractID:     h.contractID,
		WasmHash:       "aaaa1111222233334444555566667777888899990000aaaabbbbccccddddeeee02",
		Action:         store.ActionUpgrade,
		DeployerPubkey: "GABC",
	})
	require.NoError(t, err)

	_, err = h.svc.Upgrade(ctx, deploy.UpgradeRequest{
		Alias:    "counter",
		WasmHash: "aaaa11",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "ambiguous wasm hash prefix")
}

func TestUpgradeToWasmHashUnknownPrefix(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte("live-code")))

	ctx := context.Background()
	_, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{
		Alias:    "counter",
		WasmHash: "bbbb",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "no wasm hash starting with \"bbbb\" found in history")
}

func TestUpgradeToWasmHashMissingOnChain(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	trackContract(t, h, stellar.WasmHash([]byte("live-code")))

	missingHash := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{
		Alias:    "counter",
		WasmHash: missingHash,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "does not exist on-chain")
}
