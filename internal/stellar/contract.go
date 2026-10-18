package stellar

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file reads contract identity and state: which contract a create
// operation produced, and which bytecode a live contract is actually running.
// The latter is what makes drift detection possible — SoroForge compares what
// it recorded against what the ledger says, rather than trusting its own
// database.

// ContractIDFromSimulation extracts the contract ID from the result of
// simulating a create-contract transaction.
//
// The host function returns the new contract's address as its return value, so
// simulation tells us the ID before submission — which is what lets a dry run
// report the address a real deploy would produce.
func ContractIDFromSimulation(sim SimulateResult) (string, error) {
	if len(sim.Results) == 0 {
		return "", fmt.Errorf("simulation returned no results; cannot determine the contract ID")
	}

	raw := sim.Results[0].ReturnValueXDR
	if raw == "" {
		return "", fmt.Errorf("simulation returned no value; cannot determine the contract ID")
	}

	var val xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(raw, &val); err != nil {
		return "", fmt.Errorf("decode simulated return value: %w", err)
	}
	return ContractIDFromScVal(val)
}

// ContractIDFromScVal renders an ScVal holding a contract address as a C...
// strkey.
func ContractIDFromScVal(val xdr.ScVal) (string, error) {
	if val.Type != xdr.ScValTypeScvAddress || val.Address == nil {
		return "", fmt.Errorf("expected a contract address, got %s", val.Type.String())
	}
	if val.Address.Type != xdr.ScAddressTypeScAddressTypeContract || val.Address.ContractId == nil {
		return "", fmt.Errorf("expected a contract address, got an account address")
	}
	id := *val.Address.ContractId
	encoded, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		return "", fmt.Errorf("encode contract id: %w", err)
	}
	return encoded, nil
}

// ContractIDFromDeployer derives the address a CreateContractV2 operation with
// this deployer and salt will produce.
//
// A contract's address commits to the network, deployer, and salt — not to the
// bytecode — so it can be computed before anything is uploaded. That is what
// lets a deploy check for a collision before paying for an upload, and it
// agrees with what ContractIDFromSimulation later reports.
func ContractIDFromDeployer(networkPassphrase, deployer string, salt [32]byte) (string, error) {
	deployerAccount, err := xdr.AddressToAccountId(deployer)
	if err != nil {
		return "", fmt.Errorf("deployer address %q: %w", deployer, err)
	}

	// The network ID is the SHA-256 of the public network passphrase, as the
	// protocol defines it; network.ID computes exactly that.
	networkID := network.ID(networkPassphrase)
	preimage := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeContractId,
		ContractId: &xdr.HashIdPreimageContractId{
			NetworkId: xdr.Hash(networkID),
			ContractIdPreimage: xdr.ContractIdPreimage{
				Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
				FromAddress: &xdr.ContractIdPreimageFromAddress{
					Address: xdr.ScAddress{
						Type:      xdr.ScAddressTypeScAddressTypeAccount,
						AccountId: &deployerAccount,
					},
					Salt: xdr.Uint256(salt),
				},
			},
		},
	}

	encoded, err := preimage.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("encode contract id preimage: %w", err)
	}
	id := sha256.Sum256(encoded)

	contractID, err := strkey.Encode(strkey.VersionByteContract, id[:])
	if err != nil {
		return "", fmt.Errorf("encode contract id: %w", err)
	}
	return contractID, nil
}

// ContractExists reports whether a contract has an instance entry on-chain —
// that is, whether its address is already occupied.
func ContractExists(ctx context.Context, client Client, contractID string) (bool, error) {
	key, err := InstanceLedgerKey(contractID)
	if err != nil {
		return false, err
	}
	encodedKey, err := xdr.MarshalBase64(key)
	if err != nil {
		return false, fmt.Errorf("encode instance ledger key: %w", err)
	}

	entries, err := client.LedgerEntries(ctx, []string{encodedKey})
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// InstanceLedgerKey builds the LedgerKey for a contract's instance entry, whose
// value holds the executable (and therefore the WASM hash) the contract is
// currently running.
func InstanceLedgerKey(contractID string) (xdr.LedgerKey, error) {
	address, err := ContractAddress(contractID)
	if err != nil {
		return xdr.LedgerKey{}, err
	}
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   address,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}, nil
}

// CodeLedgerKey builds the LedgerKey for uploaded contract bytecode, used to
// check whether a WASM hash is already on-chain.
func CodeLedgerKey(wasmHash xdr.Hash) xdr.LedgerKey {
	return xdr.LedgerKey{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.LedgerKeyContractCode{Hash: wasmHash},
	}
}

// ErrContractNotFound reports that a contract has no instance entry on the
// target network — it was never deployed there, or has expired from the ledger.
type ErrContractNotFound struct {
	ContractID string
}

func (e *ErrContractNotFound) Error() string {
	return fmt.Sprintf("contract %s has no instance entry on this network", e.ContractID)
}

// ErrStellarAssetContract reports that a contract is a Stellar Asset Contract
// and has no uploaded bytecode or WASM hash.
type ErrStellarAssetContract struct {
	ContractID string
}

func (e *ErrStellarAssetContract) Error() string {
	return fmt.Sprintf("contract %s is a Stellar Asset Contract and has no WASM hash", e.ContractID)
}

// OnChainWasmHash reports the WASM hash a deployed contract is currently
// running, read directly from the ledger.
//
// This is the ground truth in drift detection: SoroForge's database records
// what it believes it deployed, and this reports what is actually live. When
// they disagree, someone changed the contract outside SoroForge.
//
// It returns *ErrContractNotFound if the contract has no instance entry.
func OnChainWasmHash(ctx context.Context, client Client, contractID string) (xdr.Hash, error) {
	var zero xdr.Hash

	key, err := InstanceLedgerKey(contractID)
	if err != nil {
		return zero, err
	}
	encodedKey, err := xdr.MarshalBase64(key)
	if err != nil {
		return zero, fmt.Errorf("encode instance ledger key: %w", err)
	}

	entries, err := client.LedgerEntries(ctx, []string{encodedKey})
	if err != nil {
		return zero, err
	}
	if len(entries) == 0 {
		return zero, &ErrContractNotFound{ContractID: contractID}
	}

	var data xdr.LedgerEntryData
	if err := xdr.SafeUnmarshalBase64(entries[0].DataXDR, &data); err != nil {
		return zero, fmt.Errorf("decode contract instance entry: %w", err)
	}
	if data.ContractData == nil {
		return zero, fmt.Errorf("contract %s: ledger entry is not contract data", contractID)
	}

	instance := data.ContractData.Val.Instance
	if instance == nil {
		return zero, fmt.Errorf("contract %s: ledger entry holds no contract instance", contractID)
	}

	switch instance.Executable.Type {
	case xdr.ContractExecutableTypeContractExecutableWasm:
		if instance.Executable.WasmHash == nil {
			return zero, fmt.Errorf("contract %s: instance declares WASM but carries no hash", contractID)
		}
		return *instance.Executable.WasmHash, nil

	case xdr.ContractExecutableTypeContractExecutableStellarAsset:
		// Stellar Asset Contracts are built into the protocol and have no
		// uploaded bytecode, so there is nothing to compare against.
		return zero, &ErrStellarAssetContract{ContractID: contractID}

	default:
		return zero, fmt.Errorf("contract %s has an unsupported executable type %s",
			contractID, instance.Executable.Type.String())
	}
}

// WasmUploaded reports whether bytecode with this hash already exists on-chain.
//
// Upgrades use it to skip a redundant upload: re-uploading identical bytecode
// is harmless but costs a transaction and a fee for no effect.
func WasmUploaded(ctx context.Context, client Client, wasmHash xdr.Hash) (bool, error) {
	encodedKey, err := xdr.MarshalBase64(CodeLedgerKey(wasmHash))
	if err != nil {
		return false, fmt.Errorf("encode contract code ledger key: %w", err)
	}
	entries, err := client.LedgerEntries(ctx, []string{encodedKey})
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}
