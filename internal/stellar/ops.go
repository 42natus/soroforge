package stellar

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file builds the three InvokeHostFunction operations SoroForge needs.
// Every function here is pure: given the same inputs it produces the same
// operation, with no network access and no hidden state. That is what lets the
// deploy path be tested end to end against a mocked RPC.
//
// Soroban allows exactly one InvokeHostFunction operation per transaction,
// which is why deploying is two transactions (upload, then create) rather than
// one.

// WasmHash returns the sha256 hash that identifies a contract's bytecode
// on-chain. It is computed locally, so SoroForge always knows the hash it
// intends to deploy before it talks to any network.
func WasmHash(wasm []byte) xdr.Hash {
	return sha256.Sum256(wasm)
}

// HashHex renders a hash as lowercase hex, the form used in the CLI, the HTTP
// API, and the deployment history.
func HashHex(h xdr.Hash) string {
	return hex.EncodeToString(h[:])
}

// ParseHashHex parses a 32-byte hex hash, as stored in the deployment history.
func ParseHashHex(s string) (xdr.Hash, error) {
	var h xdr.Hash
	raw, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return h, fmt.Errorf("wasm hash %q is not valid hex: %w", s, err)
	}
	if len(raw) != 32 {
		return h, fmt.Errorf("wasm hash must be 32 bytes, got %d", len(raw))
	}
	copy(h[:], raw)
	return h, nil
}

// UploadWasmOp builds the operation that uploads contract bytecode to the
// ledger. Uploading is idempotent on-chain: re-uploading identical bytecode
// yields the same hash and leaves the existing entry in place.
func UploadWasmOp(wasm []byte, source string) *txnbuild.InvokeHostFunction {
	// The XDR union holds *[]byte, so the slice needs an addressable copy.
	payload := wasm
	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm,
			Wasm: &payload,
		},
		SourceAccount: source,
	}
}

// CreateContractOp builds the operation that instantiates a contract from
// already-uploaded bytecode.
//
// It uses CreateContractV2 unconditionally, including when args is empty:
// V2 is the variant that carries constructor arguments, and using one code path
// avoids a class of bug where adding a constructor argument silently changes
// which host function is invoked.
func CreateContractOp(
	wasmHash xdr.Hash,
	deployer string,
	salt [32]byte,
	args []xdr.ScVal,
	source string,
) (*txnbuild.InvokeHostFunction, error) {
	deployerAccount, err := xdr.AddressToAccountId(deployer)
	if err != nil {
		return nil, fmt.Errorf("deployer address %q: %w", deployer, err)
	}

	if args == nil {
		// A nil slice marshals differently from an empty one in some XDR
		// paths; normalise so the encoding is stable.
		args = []xdr.ScVal{}
	}

	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeCreateContractV2,
			CreateContractV2: &xdr.CreateContractArgsV2{
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
				Executable: xdr.ContractExecutable{
					Type:     xdr.ContractExecutableTypeContractExecutableWasm,
					WasmHash: &wasmHash,
				},
				ConstructorArgs: args,
			},
		},
		SourceAccount: source,
	}, nil
}

// ParseAsset parses a classic asset string ("native" or "CODE:ISSUER") into an xdr.Asset.
func ParseAsset(s string) (xdr.Asset, error) {
	parsed, err := txnbuild.ParseAssetString(strings.TrimSpace(s))
	if err != nil {
		return xdr.Asset{}, fmt.Errorf("invalid asset %q: %w", s, err)
	}
	return parsed.ToXDR()
}

// ContractIDFromAsset returns the deterministic C... strkey contract address
// for an asset's Stellar Asset Contract on the given network.
func ContractIDFromAsset(asset xdr.Asset, networkPassphrase string) (string, error) {
	raw, err := asset.ContractID(networkPassphrase)
	if err != nil {
		return "", fmt.Errorf("derive asset contract id: %w", err)
	}
	return strkey.Encode(strkey.VersionByteContract, raw[:])
}

// CreateSacOp builds the operation that deploys the built-in Stellar Asset
// Contract (SAC) for a classic asset.
func CreateSacOp(asset xdr.Asset, source string) *txnbuild.InvokeHostFunction {
	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeCreateContract,
			CreateContract: &xdr.CreateContractArgs{
				ContractIdPreimage: xdr.ContractIdPreimage{
					Type:      xdr.ContractIdPreimageTypeContractIdPreimageFromAsset,
					FromAsset: &asset,
				},
				Executable: xdr.ContractExecutable{
					Type: xdr.ContractExecutableTypeContractExecutableStellarAsset,
				},
			},
		},
		SourceAccount: source,
	}
}

// InvokeOp builds the operation that calls a function on a deployed contract.
// SoroForge uses it for upgrades; it is exported because it is the obvious
// building block for a future `soroforge invoke`.
func InvokeOp(
	contractID string,
	function string,
	args []xdr.ScVal,
	source string,
) (*txnbuild.InvokeHostFunction, error) {
	address, err := ContractAddress(contractID)
	if err != nil {
		return nil, err
	}

	if args == nil {
		args = []xdr.ScVal{}
	}

	return &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: address,
				FunctionName:    xdr.ScSymbol(function),
				Args:            args,
			},
		},
		SourceAccount: source,
	}, nil
}

// UpgradeArg encodes a WASM hash as the BytesN<32> argument an upgrade
// entrypoint expects.
func UpgradeArg(wasmHash xdr.Hash) xdr.ScVal {
	bytes := xdr.ScBytes(wasmHash[:])
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &bytes}
}

// ContractAddress converts a C... strkey contract ID into an ScAddress.
func ContractAddress(contractID string) (xdr.ScAddress, error) {
	decoded, err := strkey.Decode(strkey.VersionByteContract, strings.TrimSpace(contractID))
	if err != nil {
		return xdr.ScAddress{}, fmt.Errorf("contract id %q: %w", contractID, err)
	}
	if len(decoded) != 32 {
		return xdr.ScAddress{}, fmt.Errorf("contract id %q: expected 32 bytes, got %d", contractID, len(decoded))
	}
	var id xdr.ContractId
	copy(id[:], decoded)
	return xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &id,
	}, nil
}

// NewSalt returns a random 32-byte salt.
//
// Contract IDs are derived from (deployer address, salt), so a random salt
// means deploying the same alias twice produces two distinct contracts rather
// than a collision. Configure an explicit salt when a reproducible address
// matters.
func NewSalt() ([32]byte, error) {
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return salt, fmt.Errorf("generate salt: %w", err)
	}
	return salt, nil
}
