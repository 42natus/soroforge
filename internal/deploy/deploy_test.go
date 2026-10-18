package deploy_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/soroworks/soroforge/internal/catalog"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file exercises the full deploy/upgrade/status flow against a mocked RPC
// and an in-memory store. Nothing here touches a network or a database, which
// is the point: the real-network path is the same code, differing only in which
// Client implementation is injected.

const testWasm = "\x00asm\x01\x00\x00\x00" // a minimal WASM header

// harness bundles a Service with the fakes behind it, so tests can assert on
// what SoroForge did rather than only on what it returned.
type harness struct {
	svc    *deploy.Service
	client *stellar.MockClient
	signer *stellar.MockSigner
	store  *store.MemoryStore
	cfg    *config.Config

	// contractID is the address the mocked create simulation returns.
	contractID string
	wasmHash   xdr.Hash
}

type harnessOptions struct {
	// wasm overrides the contract bytecode, so tests can produce a different
	// hash for an upgrade.
	wasm string

	// configBody overrides the whole soroforge.yaml.
	configBody string

	// noSigner builds a read-only service.
	noSigner bool

	// catalog replaces the SoroVault registrar.
	catalog catalog.Registrar

	// webhooks replaces the webhook sender.
	webhooks deploy.WebhookSender

	// logOut captures the service log instead of discarding it, for tests
	// that assert on what is logged.
	logOut io.Writer
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	wasm := opts.wasm
	if wasm == "" {
		wasm = testWasm
	}

	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "counter.wasm")
	require.NoError(t, os.WriteFile(wasmPath, []byte(wasm), 0o600))

	body := opts.configBody
	if body == "" {
		body = `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
`
	}
	configPath := filepath.Join(dir, "soroforge.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))

	cfg, err := config.Load(configPath)
	require.NoError(t, err)

	contractID := testContractID(t)
	client := stellar.NewMockClient()
	client.SimulateResponse = simulationReturning(t, contractID)

	memStore := store.NewMemoryStore()

	var signer stellar.Signer
	mockSigner := stellar.NewMockSigner()
	if !opts.noSigner {
		signer = mockSigner
	}

	svc, err := deploy.New(deploy.Options{
		Config:  cfg,
		Store:   memStore,
		Signer:  signer,
		Clients: func(string, config.Network) (stellar.Client, error) { return client, nil },
		// Discard logs so test output stays readable unless a test asks
		// to capture them.
		Log:      slog.New(slog.NewTextHandler(orIoDiscard(opts.logOut), nil)),
		Catalog:  opts.catalog,
		Webhooks: opts.webhooks,
	})
	require.NoError(t, err)

	return &harness{
		svc:        svc,
		client:     client,
		signer:     mockSigner,
		store:      memStore,
		cfg:        cfg,
		contractID: contractID,
		wasmHash:   stellar.WasmHash([]byte(wasm)),
	}
}

// orIoDiscard keeps the harness log silent unless a test provides a writer.
func orIoDiscard(w io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return io.Discard
}

// simulationReturning builds a simulation response whose return value is the
// given contract address, as a real create-contract simulation would produce.
func simulationReturning(t *testing.T, contractID string) stellar.SimulateResult {
	t.Helper()

	addr, err := stellar.ContractAddress(contractID)
	require.NoError(t, err)
	returnValue, err := xdr.MarshalBase64(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &addr,
	})
	require.NoError(t, err)

	data, err := xdr.MarshalBase64(xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Instructions:  xdr.Uint32(1_000_000),
			DiskReadBytes: xdr.Uint32(500),
			WriteBytes:    xdr.Uint32(1_000),
		},
		ResourceFee: xdr.Int64(12_345),
	})
	require.NoError(t, err)

	return stellar.SimulateResult{
		TransactionDataXDR: data,
		MinResourceFee:     12_345,
		Results:            []stellar.HostFunctionResult{{ReturnValueXDR: returnValue}},
		LatestLedger:       1000,
	}
}

func testContractID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	id, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)
	return id
}

// otherContractID returns a valid contract address distinct from
// testContractID and from other seeds.
func otherContractID(t *testing.T, seed byte) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = 0xF0
	raw[1] = seed
	id, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)
	return id
}

func TestDeployRecordsContractAndHistory(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	confirmedFee := int64(54321)
	h.client.AwaitResponse.FeeCharged = &confirmedFee

	result, err := h.svc.Deploy(ctx, deploy.DeployRequest{
		Alias: "counter",
		Notes: "first release",
	})
	require.NoError(t, err)

	assert.Equal(t, h.contractID, result.ContractID)
	assert.Equal(t, stellar.HashHex(h.wasmHash), result.WasmHash)
	assert.Equal(t, h.signer.Address(), result.Deployer)
	assert.False(t, result.DryRun)

	// Deploying is two transactions: upload the bytecode, then instantiate it.
	assert.Equal(t, 2, h.client.SendCount())

	// The contract must now be tracked.
	tracked, err := h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, h.contractID, tracked.ContractID)
	assert.Equal(t, stellar.HashHex(h.wasmHash), tracked.CurrentWasmHash)

	// And the history must record who did it and when.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, store.ActionDeploy, history[0].Action)
	assert.Equal(t, h.signer.Address(), history[0].DeployerPubkey)
	assert.Equal(t, "first release", history[0].Notes)
	assert.NotEmpty(t, history[0].TxHash)
	require.NotNil(t, history[0].FeeCharged)
	assert.Equal(t, confirmedFee, *history[0].FeeCharged,
		"history records the confirmed fee, not the simulation estimate")
}

func TestDeployDryRunSubmitsNothingAndRecordsNothing(t *testing.T) {
	// This is the guarantee that makes --dry-run safe to point at mainnet.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	result, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.Zero(t, h.client.SendCount(), "a dry run must not submit anything")
	assert.Zero(t, h.signer.SignCount(), "a dry run must not sign anything")

	// Nothing recorded.
	_, err = h.store.GetContract(ctx, "testnet", "counter")
	assert.ErrorIs(t, err, store.ErrNotFound)

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)

	// But it still simulated, and it still produced assembled envelopes and the
	// address a real deploy would create — that is what makes it useful.
	assert.Equal(t, 2, h.client.SimulateCount())
	assert.NotEmpty(t, result.UploadEnvelopeXDR)
	assert.NotEmpty(t, result.CreateEnvelopeXDR)
	assert.Equal(t, h.contractID, result.ContractID)
	assert.Empty(t, result.UploadTxHash)
	assert.Empty(t, result.CreateTxHash)
}

func TestDeployDryRunEnvelopesAreValidXDR(t *testing.T) {
	// The envelopes are the deliverable of a dry run, so they must decode into
	// real transactions that another tool could inspect or submit.
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	for name, envelope := range map[string]string{
		"upload": result.UploadEnvelopeXDR,
		"create": result.CreateEnvelopeXDR,
	} {
		t.Run(name, func(t *testing.T) {
			var decoded xdr.TransactionEnvelope
			require.NoError(t, xdr.SafeUnmarshalBase64(envelope, &decoded))

			require.NotNil(t, decoded.V1)
			// Assembled means the Soroban resource data is attached.
			assert.Equal(t, int32(1), decoded.V1.Tx.Ext.V)
			require.NotNil(t, decoded.V1.Tx.Ext.SorobanData)
			assert.Equal(t, xdr.Int64(12_345), decoded.V1.Tx.Ext.SorobanData.ResourceFee)
			// Unsigned, because a dry run stops before signing.
			assert.Empty(t, decoded.Signatures())
		})
	}
}

func TestDeploySkipsUploadWhenBytecodeIsAlreadyOnChain(t *testing.T) {
	// Re-uploading identical bytecode is a no-op on-chain but still costs a
	// transaction and a fee.
	h := newHarness(t, harnessOptions{})
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.True(t, result.UploadSkipped)
	assert.Empty(t, result.UploadTxHash)
	assert.Equal(t, 1, h.client.SendCount(), "only the create transaction should be submitted")
}

func TestDeployUsesRequestedNetwork(t *testing.T) {
	// Every record is namespaced by network; deploying to mainnet must not
	// appear under testnet.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", Network: "mainnet"})
	require.NoError(t, err)

	_, err = h.store.GetContract(ctx, "mainnet", "counter")
	require.NoError(t, err)

	_, err = h.store.GetContract(ctx, "testnet", "counter")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeployRejectsUnknownAliasAndNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "nonexistent"})
	assert.ErrorContains(t, err, "unknown contract")

	_, err = h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", Network: "staging"})
	assert.ErrorContains(t, err, "unknown network")
}

func TestDeployWithoutSignerFailsClearly(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "no signing key configured")
	assert.ErrorContains(t, err, config.EnvSecretKey)
}

func TestDeployReportsMissingWasmArtifact(t *testing.T) {
	// A missing build output is the most common first-run failure; the message
	// should say what to do about it.
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./never-built.wasm
`})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "wasm artifact not found")
	assert.ErrorContains(t, err, "has the contract been built?")
}

func TestDeployPropagatesSimulationFailure(t *testing.T) {
	// A simulation error arrives inside a successful RPC response, so it must
	// be checked explicitly rather than assumed absent.
	h := newHarness(t, harnessOptions{})
	h.client.SimulateResponse = stellar.SimulateResult{Error: "HostError: invalid wasm"}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "simulation failed")
	assert.ErrorContains(t, err, "invalid wasm")
	assert.Zero(t, h.client.SendCount(), "nothing should be submitted after a failed simulation")
}

func TestDeployReportsFailedTransaction(t *testing.T) {
	// A FAILED transaction returns a nil error from polling — failure is a
	// valid terminal state — so the status must be checked, or SoroForge would
	// record a deploy that never happened.
	h := newHarness(t, harnessOptions{})
	h.client.AwaitResponse = stellar.TxResult{
		Status: stellar.TxStatusFailed,
		Hash:   "abc",
		Ledger: 42,
	}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "failed")

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history, "a failed transaction must not be recorded as a deployment")
}

func TestDeployReportsRejectedSubmission(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.client.SendFunc = func(context.Context, string) (stellar.SendResult, error) {
		return stellar.SendResult{Status: stellar.SendStatusError, ErrorResultXDR: "AAAAA"}, nil
	}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "rejected by the network")
}

func TestDeployRefusesWhenRestoreIsRequired(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	sim := h.client.SimulateResponse
	sim.RestoreRequired = true
	sim.RestoreTransactionDataXDR = sim.TransactionDataXDR
	h.client.SimulateResponse = sim

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "restored")
	assert.Zero(t, h.client.SendCount())
}

func TestDeployUsesConfiguredSaltForDeterministicAddress(t *testing.T) {
	// A configured salt is what makes a contract address reproducible.
	const salt = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    salt: "` + salt + `"
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	// Decode the create envelope and confirm the salt reached the preimage.
	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	require.Equal(t, xdr.HostFunctionTypeHostFunctionTypeCreateContractV2, op.HostFunction.Type)

	expected, err := config.ParseSalt(salt)
	require.NoError(t, err)
	assert.Equal(t, xdr.Uint256(expected),
		op.HostFunction.CreateContractV2.ContractIdPreimage.FromAddress.Salt)
}

func TestDeployPassesConstructorArguments(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 7}
      - {type: string, value: "SoroForge"}
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	args := op.HostFunction.CreateContractV2.ConstructorArgs
	require.Len(t, args, 2)
	assert.Equal(t, xdr.Uint32(7), *args[0].U32)
	assert.Equal(t, xdr.ScString("SoroForge"), *args[1].Str)
}

func TestDeployWithInitializerSuccess(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    init_fn: initialize
    init_args:
      - {type: u32, value: 42}
`})

	ctx := context.Background()
	result, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.NotEmpty(t, result.ContractID)
	assert.NotEmpty(t, result.CreateTxHash)
	assert.NotEmpty(t, result.InitTxHash)
	assert.Equal(t, 3, h.client.SendCount(), "upload + create + init")

	// Store must record the deploy
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, result.ContractID, history[0].ContractID)
}

func TestDeployWithInitializerDryRun(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    init_fn: initialize
    init_args:
      - {type: u32, value: 42}
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:  "counter",
		DryRun: true,
	})
	require.NoError(t, err)
	assert.True(t, result.DryRun)
	assert.NotEmpty(t, result.InitEnvelopeXDR)
	assert.Zero(t, h.client.SendCount())

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.InitEnvelopeXDR, &decoded))
	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	assert.Equal(t, xdr.ScSymbol("initialize"), op.HostFunction.InvokeContract.FunctionName)
	require.Len(t, op.HostFunction.InvokeContract.Args, 1)
	assert.Equal(t, xdr.Uint32(42), *op.HostFunction.InvokeContract.Args[0].U32)
}

func TestDeployWithInitializerFailureReportsUninitialised(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    init_fn: initialize
`})

	callCount := 0
	h.client.SendFunc = func(ctx context.Context, signedTxXDR string) (stellar.SendResult, error) {
		callCount++
		// Upload = 1, Create = 2, Init = 3
		if callCount == 3 {
			return stellar.SendResult{}, fmt.Errorf("initialization tx rejected")
		}
		return stellar.SendResult{Status: stellar.SendStatusPending, Hash: "txhash"}, nil
	}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "deployed at")
	assert.ErrorContains(t, err, "but uninitialised")

	// Even though initialization failed, the contract must be tracked in the store
	// because it exists on-chain.
	tracked, err := h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, h.contractID, tracked.ContractID)

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Contains(t, history[0].Notes, "uninitialised")
}
