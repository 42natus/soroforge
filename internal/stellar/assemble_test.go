package stellar

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestTx returns an unsimulated upload transaction to assemble.
func buildTestTx(t *testing.T) (*txnbuild.Transaction, string) {
	t.Helper()

	address := keypair.MustRandom().Address()
	tx, err := Build(BuildParams{
		Source:    &txnbuild.SimpleAccount{AccountID: address, Sequence: 41},
		Operation: UploadWasmOp([]byte{0x00, 0x61, 0x73, 0x6d}, address),
		BaseFee:   txnbuild.MinBaseFee,
	})
	require.NoError(t, err)
	return tx, address
}

// simulatedData encodes SorobanTransactionData with the given resource fee,
// standing in for what simulateTransaction returns.
func simulatedData(t *testing.T, resourceFee int64) string {
	t.Helper()

	data := xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Footprint:     xdr.LedgerFootprint{},
			Instructions:  xdr.Uint32(1_000_000),
			DiskReadBytes: xdr.Uint32(500),
			WriteBytes:    xdr.Uint32(1_200),
		},
		ResourceFee: xdr.Int64(resourceFee),
	}
	encoded, err := xdr.MarshalBase64(data)
	require.NoError(t, err)
	return encoded
}

func TestBuildIncrementsSequenceNumber(t *testing.T) {
	tx, _ := buildTestTx(t)
	// Building consumes the next sequence number, not the account's current one.
	assert.Equal(t, int64(42), tx.SequenceNumber())
}

func TestBuildRejectsMissingInputs(t *testing.T) {
	_, err := Build(BuildParams{Operation: UploadWasmOp(nil, "")})
	assert.ErrorContains(t, err, "no source account")

	_, err = Build(BuildParams{Source: &txnbuild.SimpleAccount{AccountID: "G", Sequence: 1}})
	assert.ErrorContains(t, err, "no operation")
}

func TestAssembleAppliesSorobanDataAndResourceFee(t *testing.T) {
	// This is the behaviour the whole submission path depends on: the Go SDK
	// has no assembleTransaction helper, so SoroForge folds the simulation
	// result in by hand, and txnbuild adds the resource fee on top of the base
	// fee when the transaction is rebuilt.
	const resourceFee = 87_654

	tx, _ := buildTestTx(t)
	before := tx.ToXDR().Fee()

	assembled, err := Assemble(tx, SimulateResult{
		TransactionDataXDR: simulatedData(t, resourceFee),
		MinResourceFee:     resourceFee,
	})
	require.NoError(t, err)

	// One operation at MinBaseFee, plus the resource fee.
	assert.Equal(t, uint32(txnbuild.MinBaseFee+resourceFee), uint32(assembled.ToXDR().Fee()))
	assert.Greater(t, assembled.ToXDR().Fee(), before)

	// The Soroban data must be attached to the operation's transaction ext,
	// or the network cannot know the footprint.
	envelope := assembled.ToXDR()
	require.NotNil(t, envelope.V1)
	require.Equal(t, int32(1), envelope.V1.Tx.Ext.V)
	require.NotNil(t, envelope.V1.Tx.Ext.SorobanData)
	assert.Equal(t, xdr.Int64(resourceFee), envelope.V1.Tx.Ext.SorobanData.ResourceFee)
	assert.Equal(t, xdr.Uint32(1_000_000), envelope.V1.Tx.Ext.SorobanData.Resources.Instructions)
}

func TestAssemblePreservesSequenceNumber(t *testing.T) {
	// Assembly rebuilds the transaction, and rebuilding must not consume a
	// second sequence number — that would produce a transaction the account
	// cannot yet submit.
	tx, _ := buildTestTx(t)
	original := tx.SequenceNumber()

	assembled, err := Assemble(tx, SimulateResult{TransactionDataXDR: simulatedData(t, 100)})
	require.NoError(t, err)

	assert.Equal(t, original, assembled.SequenceNumber())
}

func TestAssemblePreservesHostFunction(t *testing.T) {
	tx, address := buildTestTx(t)

	assembled, err := Assemble(tx, SimulateResult{TransactionDataXDR: simulatedData(t, 100)})
	require.NoError(t, err)

	ops := assembled.Operations()
	require.Len(t, ops, 1)
	invoke, ok := ops[0].(*txnbuild.InvokeHostFunction)
	require.True(t, ok)

	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm, invoke.HostFunction.Type)
	assert.Equal(t, address, invoke.SourceAccount)
}

func TestAssembleAttachesSimulatedAuthEntries(t *testing.T) {
	// Authorization entries are discovered during simulation and must be
	// carried into the submitted transaction, or anything requiring auth fails.
	entry := xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount,
		},
		RootInvocation: xdr.SorobanAuthorizedInvocation{
			Function: xdr.SorobanAuthorizedFunction{
				Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
				ContractFn: &xdr.InvokeContractArgs{
					ContractAddress: mustContractAddress(testContractID(t)),
					FunctionName:    "upgrade",
					Args:            []xdr.ScVal{},
				},
			},
		},
	}
	encodedEntry, err := xdr.MarshalBase64(entry)
	require.NoError(t, err)

	tx, _ := buildTestTx(t)
	assembled, err := Assemble(tx, SimulateResult{
		TransactionDataXDR: simulatedData(t, 100),
		Results:            []HostFunctionResult{{AuthXDR: []string{encodedEntry}}},
	})
	require.NoError(t, err)

	invoke := assembled.Operations()[0].(*txnbuild.InvokeHostFunction)
	require.Len(t, invoke.Auth, 1)
	assert.Equal(t,
		xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount,
		invoke.Auth[0].Credentials.Type)
}

func TestAssembleRejectsSimulationError(t *testing.T) {
	tx, _ := buildTestTx(t)

	_, err := Assemble(tx, SimulateResult{Error: "HostError: contract call failed"})
	assert.ErrorContains(t, err, "simulation failed")
	assert.ErrorContains(t, err, "HostError")
}

func TestAssembleRefusesWhenRestoreIsRequired(t *testing.T) {
	// Submitting anyway would spend a fee on a transaction that cannot succeed,
	// so this stops with an actionable message instead.
	tx, _ := buildTestTx(t)

	_, err := Assemble(tx, SimulateResult{
		TransactionDataXDR:        simulatedData(t, 100),
		RestoreRequired:           true,
		RestoreTransactionDataXDR: simulatedData(t, 50),
	})
	assert.ErrorContains(t, err, "restored")
}

func TestAssembleRejectsMissingTransactionData(t *testing.T) {
	tx, _ := buildTestTx(t)

	_, err := Assemble(tx, SimulateResult{})
	assert.ErrorContains(t, err, "no transaction data")
}

func TestAssembleRejectsMultipleHostFunctionResults(t *testing.T) {
	// Soroban permits one host function per transaction, so more than one
	// result means the response does not describe what we sent.
	tx, _ := buildTestTx(t)

	_, err := Assemble(tx, SimulateResult{
		TransactionDataXDR: simulatedData(t, 100),
		Results:            []HostFunctionResult{{}, {}},
	})
	assert.ErrorContains(t, err, "only 1")
}

func TestAssembleRejectsNilTransaction(t *testing.T) {
	_, err := Assemble(nil, SimulateResult{})
	assert.ErrorContains(t, err, "nil transaction")
}

func TestResourceFeeReadsAuthoritativeValue(t *testing.T) {
	// MinResourceFee is advisory; the fee that matters is inside the encoded
	// SorobanTransactionData. Deliberately disagree to prove which is read.
	fee, err := ResourceFee(SimulateResult{
		TransactionDataXDR: simulatedData(t, 4242),
		MinResourceFee:     999,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(4242), fee)
}

func TestStroopsToXLM(t *testing.T) {
	tests := map[string]struct {
		stroops int64
		want    string
	}{
		"zero":               {0, "0.0000000"},
		"one stroop":         {1, "0.0000001"},
		"min base fee":       {100, "0.0000100"},
		"one lumen":          {10_000_000, "1.0000000"},
		"typical fee":        {12_445, "0.0012445"},
		"more than one XLM":  {25_000_000, "2.5000000"},
		"large amount":       {123_456_789_012, "12345.6789012"},
		"negative is signed": {-200, "-0.0000200"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, StroopsToXLM(tc.stroops))
		})
	}
}

func TestAssembledTransactionIsSignableAndEncodable(t *testing.T) {
	// The end product must be a real, signable envelope — not just a struct
	// that passed field assertions.
	const passphrase = "Test SDF Network ; September 2015"

	signer := NewMockSigner()
	tx, err := Build(BuildParams{
		Source:    &txnbuild.SimpleAccount{AccountID: signer.Address(), Sequence: 1},
		Operation: UploadWasmOp([]byte{0x00, 0x61, 0x73, 0x6d}, signer.Address()),
		BaseFee:   txnbuild.MinBaseFee,
	})
	require.NoError(t, err)

	assembled, err := Assemble(tx, SimulateResult{TransactionDataXDR: simulatedData(t, 1000)})
	require.NoError(t, err)

	signed, err := signer.Sign(passphrase, assembled)
	require.NoError(t, err)

	envelope, err := signed.Base64()
	require.NoError(t, err)
	assert.NotEmpty(t, envelope)

	// It must decode back into a transaction envelope.
	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(envelope, &decoded))
	assert.Len(t, decoded.Signatures(), 1)
}
