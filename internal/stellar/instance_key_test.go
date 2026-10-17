package stellar

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InstanceLedgerKey builds the lookup behind every status check and every
// upload-skip decision. A key built from a non-contract string would make the
// ledger report "no entry", which status would surface as "missing on-chain" —
// a data problem disguised as a finding. Invalid input must therefore fail
// here, loudly, rather than produce a key that quietly matches nothing.

func TestInstanceLedgerKeyRejectsInvalidContractIDs(t *testing.T) {
	tests := map[string]string{
		"malformed":       "not-a-contract-id",
		"account address": "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF",
		"empty":           "",
	}

	for name, contractID := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := InstanceLedgerKey(contractID)
			assert.ErrorContains(t, err, "contract id")
		})
	}
}

func TestInstanceLedgerKeySurvivesItsXDREncoding(t *testing.T) {
	// The key is marshalled to base64 before it goes to the RPC, so the round
	// trip is the form that actually matters: a field lost here would make the
	// lookup miss entries that exist.
	contractID := testContractID(t)

	key, err := InstanceLedgerKey(contractID)
	require.NoError(t, err)

	encoded, err := xdr.MarshalBase64(key)
	require.NoError(t, err)

	var decoded xdr.LedgerKey
	require.NoError(t, xdr.SafeUnmarshalBase64(encoded, &decoded))

	assert.Equal(t, xdr.LedgerEntryTypeContractData, decoded.Type)
	require.NotNil(t, decoded.ContractData)

	address, err := ContractAddress(contractID)
	require.NoError(t, err)
	assert.Equal(t, address, decoded.ContractData.Contract)
	assert.Equal(t, xdr.ScValTypeScvLedgerKeyContractInstance, decoded.ContractData.Key.Type)
	assert.Equal(t, xdr.ContractDataDurabilityPersistent, decoded.ContractData.Durability)
}
