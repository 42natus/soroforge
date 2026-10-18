package stellar

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContractIDFromSimulation(t *testing.T) {
	// A create-contract host function returns the new contract's address, which
	// is how SoroForge learns the ID — including on a dry run, before anything
	// is submitted.
	contractID := testContractID(t)
	addr, err := ContractAddress(contractID)
	require.NoError(t, err)

	encoded, err := xdr.MarshalBase64(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &addr,
	})
	require.NoError(t, err)

	got, err := ContractIDFromSimulation(SimulateResult{
		Results: []HostFunctionResult{{ReturnValueXDR: encoded}},
	})
	require.NoError(t, err)
	assert.Equal(t, contractID, got)
}

func TestContractIDFromSimulationRejectsUnusableResults(t *testing.T) {
	accountAddr, err := ParseAccountAddressForTest("GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF")
	require.NoError(t, err)
	accountVal, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &accountAddr})
	require.NoError(t, err)

	u32Val, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: ptr(xdr.Uint32(1))})
	require.NoError(t, err)

	tests := map[string]struct {
		sim     SimulateResult
		wantErr string
	}{
		"no results": {
			sim:     SimulateResult{},
			wantErr: "no results",
		},
		"empty return value": {
			sim:     SimulateResult{Results: []HostFunctionResult{{}}},
			wantErr: "no value",
		},
		"undecodable": {
			sim:     SimulateResult{Results: []HostFunctionResult{{ReturnValueXDR: "!!!not base64!!!"}}},
			wantErr: "decode",
		},
		"not an address": {
			sim:     SimulateResult{Results: []HostFunctionResult{{ReturnValueXDR: u32Val}}},
			wantErr: "expected a contract address",
		},
		"account address rather than contract": {
			sim:     SimulateResult{Results: []HostFunctionResult{{ReturnValueXDR: accountVal}}},
			wantErr: "account address",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ContractIDFromSimulation(tc.sim)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestOnChainWasmHashReadsInstanceExecutable(t *testing.T) {
	// This read is the authority in drift detection: it reports what the
	// contract is genuinely running, independent of SoroForge's database.
	contractID := testContractID(t)
	wasmHash := WasmHash([]byte("live bytecode"))

	client := NewMockClient()
	require.NoError(t, client.SetContractInstance(contractID, wasmHash))

	got, err := OnChainWasmHash(context.Background(), client, contractID)
	require.NoError(t, err)
	assert.Equal(t, wasmHash, got)
}

func TestOnChainWasmHashReportsMissingContract(t *testing.T) {
	// A contract with no instance entry must be distinguishable from an error,
	// so `status` can say "not deployed here" rather than "something broke".
	client := NewMockClient()

	_, err := OnChainWasmHash(context.Background(), client, testContractID(t))

	var notFound *ErrContractNotFound
	require.True(t, errors.As(err, &notFound), "expected ErrContractNotFound, got %v", err)
	assert.Contains(t, err.Error(), "no instance entry")
}

func TestOnChainWasmHashRejectsStellarAssetContract(t *testing.T) {
	// Stellar Asset Contracts are built into the protocol and have no uploaded
	// bytecode, so there is no hash to compare and no upgrade to perform.
	contractID := testContractID(t)
	key, err := InstanceLedgerKey(contractID)
	require.NoError(t, err)
	encodedKey, err := xdr.MarshalBase64(key)
	require.NoError(t, err)

	entry, err := xdr.MarshalBase64(xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   mustContractAddress(contractID),
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val: xdr.ScVal{
				Type: xdr.ScValTypeScvContractInstance,
				Instance: &xdr.ScContractInstance{
					Executable: xdr.ContractExecutable{
						Type: xdr.ContractExecutableTypeContractExecutableStellarAsset,
					},
				},
			},
		},
	})
	require.NoError(t, err)

	client := NewMockClient()
	client.Entries[encodedKey] = entry

	_, err = OnChainWasmHash(context.Background(), client, contractID)
	assert.ErrorContains(t, err, "Stellar Asset Contract")
}

func TestWasmUploadedDetectsPresenceAndAbsence(t *testing.T) {
	// Skipping a redundant upload saves a transaction and its fee, so this
	// check needs to be right in both directions.
	hash := WasmHash([]byte("some bytecode"))
	client := NewMockClient()

	uploaded, err := WasmUploaded(context.Background(), client, hash)
	require.NoError(t, err)
	assert.False(t, uploaded, "bytecode should not be reported as uploaded before it is")

	require.NoError(t, client.SetWasmUploaded(hash))

	uploaded, err = WasmUploaded(context.Background(), client, hash)
	require.NoError(t, err)
	assert.True(t, uploaded)

	// A different hash must not be confused for the uploaded one.
	uploaded, err = WasmUploaded(context.Background(), client, WasmHash([]byte("other")))
	require.NoError(t, err)
	assert.False(t, uploaded)
}

func TestInstanceAndCodeLedgerKeysDiffer(t *testing.T) {
	// The instance key locates a deployed contract; the code key locates
	// uploaded bytecode. Confusing them would make both lookups wrong.
	contractID := testContractID(t)
	hash := WasmHash([]byte("x"))

	instanceKey, err := InstanceLedgerKey(contractID)
	require.NoError(t, err)
	assert.Equal(t, xdr.LedgerEntryTypeContractData, instanceKey.Type)
	require.NotNil(t, instanceKey.ContractData)
	assert.Equal(t, xdr.ContractDataDurabilityPersistent, instanceKey.ContractData.Durability)

	codeKey := CodeLedgerKey(hash)
	assert.Equal(t, xdr.LedgerEntryTypeContractCode, codeKey.Type)
	require.NotNil(t, codeKey.ContractCode)
	assert.Equal(t, hash, codeKey.ContractCode.Hash)
}

func TestContractIDFromDeployerMatchesStellarDerivation(t *testing.T) {
	// The expected value was derived independently with the JavaScript SDK
	// (@stellar/stellar-base), so this pins the protocol derivation rather
	// than restating the Go implementation. Getting it wrong would mean
	// checking a different address than the one a deploy creates.
	const (
		seed     = "SCN4VT2ELMFTALLNPPJNCK6OGQEWLERZIMEPARTRVDXXL2IJAPWSXZQI"
		saltHex  = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
		expected = "CCMSQAPY4AIPCZ65CLOM4VELIQZR73C7VS6RZQSRILB2EJZ644YHAEGC"
	)

	full, err := keypair.ParseFull(seed)
	require.NoError(t, err)

	rawSalt, err := hex.DecodeString(saltHex)
	require.NoError(t, err)
	var salt [32]byte
	copy(salt[:], rawSalt)

	got, err := ContractIDFromDeployer("Test SDF Network ; September 2015", full.Address(), salt)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestContractIDFromDeployerRejectsInvalidAddress(t *testing.T) {
	_, err := ContractIDFromDeployer("Test SDF Network ; September 2015", "not-an-address", [32]byte{})
	assert.ErrorContains(t, err, "deployer address")
}

func TestContractExistsDetectsPresenceAndAbsence(t *testing.T) {
	// The deploy collision check turns on this lookup, so it must be right in
	// both directions.
	contractID := testContractID(t)
	client := NewMockClient()

	exists, err := ContractExists(context.Background(), client, contractID)
	require.NoError(t, err)
	assert.False(t, exists, "a contract with no instance entry must not be reported as occupied")

	require.NoError(t, client.SetContractInstance(contractID, WasmHash([]byte("bytecode"))))

	exists, err = ContractExists(context.Background(), client, contractID)
	require.NoError(t, err)
	assert.True(t, exists)
}

// ParseAccountAddressForTest builds an account ScAddress, used to prove that an
// account address is rejected where a contract address is required.
func ParseAccountAddressForTest(address string) (xdr.ScAddress, error) {
	accountID, err := xdr.AddressToAccountId(address)
	if err != nil {
		return xdr.ScAddress{}, err
	}
	return xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &accountID,
	}, nil
}
