package stellar

import (
	"encoding/hex"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWasmHashMatchesSHA256(t *testing.T) {
	// The hash Soroban addresses bytecode by is a plain sha256 of the file.
	// Pinning a known vector guards against anyone "improving" this into
	// something else.
	got := HashHex(WasmHash([]byte("hello")))
	assert.Equal(t,
		"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		got)
}

func TestParseHashHexRoundTrip(t *testing.T) {
	original := WasmHash([]byte("soroforge"))

	parsed, err := ParseHashHex(HashHex(original))
	require.NoError(t, err)
	assert.Equal(t, original, parsed)
}

func TestParseHashHexRejectsBadInput(t *testing.T) {
	tests := map[string]string{
		"not hex":   "zzzz",
		"too short": "abcd",
		"too long":  hex.EncodeToString(make([]byte, 33)),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHashHex(input)
			assert.Error(t, err)
		})
	}
}

func TestUploadWasmOpCarriesBytecode(t *testing.T) {
	wasm := []byte{0x00, 0x61, 0x73, 0x6d} // the WASM magic number
	source := keypair.MustRandom().Address()

	op := UploadWasmOp(wasm, source)

	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeUploadContractWasm, op.HostFunction.Type)
	require.NotNil(t, op.HostFunction.Wasm)
	assert.Equal(t, wasm, *op.HostFunction.Wasm)
	assert.Equal(t, source, op.SourceAccount)

	// The operation must survive XDR encoding, since that is what is actually
	// submitted.
	_, err := op.BuildXDR()
	require.NoError(t, err)
}

func TestCreateContractOpUsesV2WithConstructorArgs(t *testing.T) {
	deployer := keypair.MustRandom().Address()
	wasmHash := WasmHash([]byte("bytecode"))
	salt := [32]byte{1, 2, 3}

	arg := xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: ptr(xdr.Uint32(42))}

	op, err := CreateContractOp(wasmHash, deployer, salt, []xdr.ScVal{arg}, deployer)
	require.NoError(t, err)

	// V2 is the variant that carries constructor arguments.
	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeCreateContractV2, op.HostFunction.Type)
	require.NotNil(t, op.HostFunction.CreateContractV2)

	create := op.HostFunction.CreateContractV2
	require.Len(t, create.ConstructorArgs, 1)
	assert.Equal(t, xdr.ScValTypeScvU32, create.ConstructorArgs[0].Type)

	assert.Equal(t, xdr.ContractExecutableTypeContractExecutableWasm, create.Executable.Type)
	require.NotNil(t, create.Executable.WasmHash)
	assert.Equal(t, wasmHash, *create.Executable.WasmHash)

	require.NotNil(t, create.ContractIdPreimage.FromAddress)
	assert.Equal(t, xdr.Uint256(salt), create.ContractIdPreimage.FromAddress.Salt)

	_, err = op.BuildXDR()
	require.NoError(t, err)
}

func TestCreateContractOpUsesV2EvenWithNoArgs(t *testing.T) {
	// Using one host function type regardless of arity means adding a
	// constructor argument later cannot silently change which variant is sent.
	deployer := keypair.MustRandom().Address()

	op, err := CreateContractOp(WasmHash([]byte("x")), deployer, [32]byte{}, nil, deployer)
	require.NoError(t, err)

	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeCreateContractV2, op.HostFunction.Type)
	require.NotNil(t, op.HostFunction.CreateContractV2)
	assert.Empty(t, op.HostFunction.CreateContractV2.ConstructorArgs)
}

func TestCreateContractOpRejectsBadDeployer(t *testing.T) {
	_, err := CreateContractOp(WasmHash([]byte("x")), "not-an-address", [32]byte{}, nil, "")
	assert.ErrorContains(t, err, "deployer address")
}

func TestInvokeOpTargetsContractFunction(t *testing.T) {
	contractID := testContractID(t)
	source := keypair.MustRandom().Address()
	wasmHash := WasmHash([]byte("v2"))

	op, err := InvokeOp(contractID, "upgrade", []xdr.ScVal{UpgradeArg(wasmHash)}, source)
	require.NoError(t, err)

	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeInvokeContract, op.HostFunction.Type)
	require.NotNil(t, op.HostFunction.InvokeContract)

	invoke := op.HostFunction.InvokeContract
	assert.Equal(t, xdr.ScSymbol("upgrade"), invoke.FunctionName)
	require.Len(t, invoke.Args, 1)

	// The upgrade entrypoint takes BytesN<32>, which is an ScBytes holding the
	// raw hash.
	assert.Equal(t, xdr.ScValTypeScvBytes, invoke.Args[0].Type)
	require.NotNil(t, invoke.Args[0].Bytes)
	assert.Equal(t, wasmHash[:], []byte(*invoke.Args[0].Bytes))

	_, err = op.BuildXDR()
	require.NoError(t, err)
}

func TestInvokeOpRejectsBadContractID(t *testing.T) {
	_, err := InvokeOp("GNOTACONTRACT", "upgrade", nil, "")
	assert.ErrorContains(t, err, "contract id")
}

func TestContractAddressRoundTrip(t *testing.T) {
	contractID := testContractID(t)

	addr, err := ContractAddress(contractID)
	require.NoError(t, err)
	require.Equal(t, xdr.ScAddressTypeScAddressTypeContract, addr.Type)

	encoded, err := ContractIDFromScVal(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &addr,
	})
	require.NoError(t, err)
	assert.Equal(t, contractID, encoded)
}

func TestNewSaltIsRandom(t *testing.T) {
	// Distinct salts are what stop two deploys of the same alias from
	// colliding on one contract address.
	a, err := NewSalt()
	require.NoError(t, err)
	b, err := NewSalt()
	require.NoError(t, err)

	assert.NotEqual(t, a, b)
	assert.NotEqual(t, [32]byte{}, a)
}

// testContractID returns a syntactically valid C... contract address.
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

func ptr[T any](v T) *T { return &v }

func TestParseAsset(t *testing.T) {
	native, err := ParseAsset("native")
	require.NoError(t, err)
	assert.Equal(t, xdr.AssetTypeAssetTypeNative, native.Type)

	issuer := keypair.MustRandom().Address()
	credit, err := ParseAsset("USDC:" + issuer)
	require.NoError(t, err)
	assert.Equal(t, xdr.AssetTypeAssetTypeCreditAlphanum4, credit.Type)

	_, err = ParseAsset("invalid:asset:extra")
	assert.Error(t, err)
}

func TestContractIDFromAsset(t *testing.T) {
	native, err := ParseAsset("native")
	require.NoError(t, err)

	passphrase := "Test SDF Network ; September 2015"
	id, err := ContractIDFromAsset(native, passphrase)
	require.NoError(t, err)
	assert.True(t, len(id) > 0)
	assert.Equal(t, byte('C'), id[0])

	// Same asset and passphrase yields identical deterministic contract ID.
	id2, err := ContractIDFromAsset(native, passphrase)
	require.NoError(t, err)
	assert.Equal(t, id, id2)

	// Different network passphrase yields different contract ID.
	otherPassphrase := "Public Global Stellar Network ; September 2015"
	idOther, err := ContractIDFromAsset(native, otherPassphrase)
	require.NoError(t, err)
	assert.NotEqual(t, id, idOther)
}

func TestCreateSacOp(t *testing.T) {
	native, err := ParseAsset("native")
	require.NoError(t, err)

	source := keypair.MustRandom().Address()
	op := CreateSacOp(native, source)

	assert.Equal(t, xdr.HostFunctionTypeHostFunctionTypeCreateContract, op.HostFunction.Type)
	require.NotNil(t, op.HostFunction.CreateContract)

	create := op.HostFunction.CreateContract
	assert.Equal(t, xdr.ContractIdPreimageTypeContractIdPreimageFromAsset, create.ContractIdPreimage.Type)
	require.NotNil(t, create.ContractIdPreimage.FromAsset)
	assert.Equal(t, native, *create.ContractIdPreimage.FromAsset)

	assert.Equal(t, xdr.ContractExecutableTypeContractExecutableStellarAsset, create.Executable.Type)
	assert.Equal(t, source, op.SourceAccount)

	// Operation must survive XDR marshaling.
	_, err = op.BuildXDR()
	require.NoError(t, err)
}
