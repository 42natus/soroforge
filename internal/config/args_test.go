package config

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// argFromYAML decodes a single Arg from YAML, exercising the same path a real
// soroforge.yaml takes rather than constructing the struct directly.
func argFromYAML(t *testing.T, doc string) Arg {
	t.Helper()
	var arg Arg
	require.NoError(t, yaml.Unmarshal([]byte(doc), &arg))
	return arg
}

func TestArgToScValScalarTypes(t *testing.T) {
	tests := map[string]struct {
		yaml     string
		wantType xdr.ScValType
		check    func(t *testing.T, v xdr.ScVal)
	}{
		"bool true": {
			yaml:     "{type: bool, value: true}",
			wantType: xdr.ScValTypeScvBool,
			check:    func(t *testing.T, v xdr.ScVal) { assert.True(t, *v.B) },
		},
		"bool false": {
			yaml:     "{type: bool, value: false}",
			wantType: xdr.ScValTypeScvBool,
			check:    func(t *testing.T, v xdr.ScVal) { assert.False(t, *v.B) },
		},
		"void": {
			yaml:     "{type: void}",
			wantType: xdr.ScValTypeScvVoid,
		},
		"u32": {
			yaml:     "{type: u32, value: 4294967295}",
			wantType: xdr.ScValTypeScvU32,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.Uint32(4294967295), *v.U32) },
		},
		"i32 negative": {
			yaml:     "{type: i32, value: -2147483648}",
			wantType: xdr.ScValTypeScvI32,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.Int32(-2147483648), *v.I32) },
		},
		"u64 quoted beyond float precision": {
			// YAML numbers can lose precision through float64; quoting is the
			// documented way to pass a large integer exactly.
			yaml:     `{type: u64, value: "18446744073709551615"}`,
			wantType: xdr.ScValTypeScvU64,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.Uint64(18446744073709551615), *v.U64) },
		},
		"i64 negative": {
			yaml:     "{type: i64, value: -9223372036854775808}",
			wantType: xdr.ScValTypeScvI64,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.Int64(-9223372036854775808), *v.I64) },
		},
		"string": {
			yaml:     `{type: string, value: "MyToken"}`,
			wantType: xdr.ScValTypeScvString,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.ScString("MyToken"), *v.Str) },
		},
		"symbol": {
			yaml:     "{type: symbol, value: transfer}",
			wantType: xdr.ScValTypeScvSymbol,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.ScSymbol("transfer"), *v.Sym) },
		},
		"bytes hex": {
			yaml:     "{type: bytes, value: deadbeef}",
			wantType: xdr.ScValTypeScvBytes,
			check: func(t *testing.T, v xdr.ScVal) {
				assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, []byte(*v.Bytes))
			},
		},
		"bytes with 0x prefix": {
			yaml:     "{type: bytes, value: 0xdeadbeef}",
			wantType: xdr.ScValTypeScvBytes,
			check: func(t *testing.T, v xdr.ScVal) {
				assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, []byte(*v.Bytes))
			},
		},
		"unquoted integer read as string still parses": {
			yaml:     "{type: u32, value: 7}",
			wantType: xdr.ScValTypeScvU32,
			check:    func(t *testing.T, v xdr.ScVal) { assert.Equal(t, xdr.Uint32(7), *v.U32) },
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			val, err := argFromYAML(t, tc.yaml).ToScVal()
			require.NoError(t, err)
			assert.Equal(t, tc.wantType, val.Type)
			if tc.check != nil {
				tc.check(t, val)
			}
			// Everything produced here is submitted as XDR, so it must encode.
			_, err = xdr.MarshalBase64(val)
			require.NoError(t, err)
		})
	}
}

func TestArgToScVal128BitIntegers(t *testing.T) {
	// 128-bit values are the ones a token contract actually takes, and the
	// hi/lo split is easy to get wrong — particularly for negatives, which use
	// two's complement rather than sign-magnitude.
	tests := map[string]struct {
		yaml   string
		wantHi any
		wantLo xdr.Uint64
	}{
		"u128 zero": {
			yaml: `{type: u128, value: "0"}`, wantHi: xdr.Uint64(0), wantLo: 0,
		},
		"u128 fits in low word": {
			yaml:   `{type: u128, value: "18446744073709551615"}`,
			wantHi: xdr.Uint64(0), wantLo: 18446744073709551615,
		},
		"u128 spans both words": {
			yaml:   `{type: u128, value: "18446744073709551616"}`, // 2^64
			wantHi: xdr.Uint64(1), wantLo: 0,
		},
		"u128 maximum": {
			yaml:   `{type: u128, value: "340282366920938463463374607431768211455"}`,
			wantHi: xdr.Uint64(18446744073709551615), wantLo: 18446744073709551615,
		},
		"i128 positive": {
			yaml: `{type: i128, value: "1"}`, wantHi: xdr.Int64(0), wantLo: 1,
		},
		"i128 negative one": {
			// Two's complement: -1 is all bits set.
			yaml:   `{type: i128, value: "-1"}`,
			wantHi: xdr.Int64(-1), wantLo: 18446744073709551615,
		},
		"i128 minimum": {
			yaml:   `{type: i128, value: "-170141183460469231731687303715884105728"}`,
			wantHi: xdr.Int64(-9223372036854775808), wantLo: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			val, err := argFromYAML(t, tc.yaml).ToScVal()
			require.NoError(t, err)

			switch want := tc.wantHi.(type) {
			case xdr.Uint64:
				require.NotNil(t, val.U128)
				assert.Equal(t, want, val.U128.Hi)
				assert.Equal(t, tc.wantLo, val.U128.Lo)
			case xdr.Int64:
				require.NotNil(t, val.I128)
				assert.Equal(t, want, val.I128.Hi)
				assert.Equal(t, tc.wantLo, val.I128.Lo)
			}

			_, err = xdr.MarshalBase64(val)
			require.NoError(t, err)
		})
	}
}

func TestArgToScValAddresses(t *testing.T) {
	account := keypair.MustRandom().Address()

	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	contract, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)

	t.Run("account G address", func(t *testing.T) {
		val, err := Arg{Type: ArgAddress, Value: NewValue(account)}.ToScVal()
		require.NoError(t, err)
		require.Equal(t, xdr.ScValTypeScvAddress, val.Type)
		assert.Equal(t, xdr.ScAddressTypeScAddressTypeAccount, val.Address.Type)
	})

	t.Run("contract C address", func(t *testing.T) {
		val, err := Arg{Type: ArgAddress, Value: NewValue(contract)}.ToScVal()
		require.NoError(t, err)
		require.Equal(t, xdr.ScValTypeScvAddress, val.Type)
		assert.Equal(t, xdr.ScAddressTypeScAddressTypeContract, val.Address.Type)
	})
}

func TestArgToScValNestedVec(t *testing.T) {
	val, err := argFromYAML(t, `
type: vec
value:
  - {type: u32, value: 1}
  - {type: string, value: "two"}
  - {type: vec, value: [{type: bool, value: true}]}
`).ToScVal()
	require.NoError(t, err)

	require.Equal(t, xdr.ScValTypeScvVec, val.Type)
	require.NotNil(t, val.Vec)
	items := **val.Vec
	require.Len(t, items, 3)
	assert.Equal(t, xdr.ScValTypeScvU32, items[0].Type)
	assert.Equal(t, xdr.ScValTypeScvString, items[1].Type)
	assert.Equal(t, xdr.ScValTypeScvVec, items[2].Type)

	_, err = xdr.MarshalBase64(val)
	require.NoError(t, err)
}

func TestArgToScValMap(t *testing.T) {
	val, err := argFromYAML(t, `
type: map
value:
  - key: {type: symbol, value: name}
    value: {type: string, value: "SoroForge"}
  - key: {type: symbol, value: decimals}
    value: {type: u32, value: 7}
`).ToScVal()
	require.NoError(t, err)

	require.Equal(t, xdr.ScValTypeScvMap, val.Type)
	require.NotNil(t, val.Map)
	entries := **val.Map
	require.Len(t, entries, 2)
	assert.Equal(t, xdr.ScSymbol("name"), *entries[0].Key.Sym)
	assert.Equal(t, xdr.ScString("SoroForge"), *entries[0].Val.Str)
}

func TestArgToScValXDREscapeHatch(t *testing.T) {
	// The escape hatch is what keeps an unsupported type from being a hard
	// block, so it must reproduce the value exactly.
	original := xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: uint32Ptr(99)}
	encoded, err := xdr.MarshalBase64(original)
	require.NoError(t, err)

	val, err := Arg{Type: ArgXDR, Value: NewValue(encoded)}.ToScVal()
	require.NoError(t, err)
	assert.Equal(t, original, val)
}

func TestArgToScValRejectsBadInput(t *testing.T) {
	tests := map[string]struct {
		arg     Arg
		wantErr string
	}{
		"missing type": {
			arg:     Arg{Value: NewValue(1)},
			wantErr: "missing type",
		},
		"unknown type names the escape hatch": {
			arg:     Arg{Type: "u256", Value: NewValue("1")},
			wantErr: "type: xdr",
		},
		"u32 overflow": {
			arg:     Arg{Type: ArgU32, Value: NewValue("4294967296")},
			wantErr: "not a valid u32",
		},
		"u32 negative": {
			arg:     Arg{Type: ArgU32, Value: NewValue("-1")},
			wantErr: "not a valid u32",
		},
		"u128 out of range": {
			arg:     Arg{Type: ArgU128, Value: NewValue("340282366920938463463374607431768211456")},
			wantErr: "out of range",
		},
		"u128 negative": {
			arg:     Arg{Type: ArgU128, Value: NewValue("-1")},
			wantErr: "out of range",
		},
		"i128 not a number": {
			arg:     Arg{Type: ArgI128, Value: NewValue("abc")},
			wantErr: "not a valid decimal integer",
		},
		"bad bool": {
			arg:     Arg{Type: ArgBool, Value: NewValue("maybe")},
			wantErr: "not a boolean",
		},
		"bad hex bytes": {
			arg:     Arg{Type: ArgBytes, Value: NewValue("nothex")},
			wantErr: "not valid hex",
		},
		"bad address": {
			arg:     Arg{Type: ArgAddress, Value: NewValue("nonsense")},
			wantErr: "expected a G... account or C... contract address",
		},
		"symbol too long": {
			arg:     Arg{Type: ArgSymbol, Value: NewValue("this_symbol_is_far_too_long_to_be_valid")},
			wantErr: "limit is 32",
		},
		"symbol with illegal character": {
			arg:     Arg{Type: ArgSymbol, Value: NewValue("has-a-dash")},
			wantErr: "only [a-zA-Z0-9_]",
		},
		"bad base64 xdr": {
			arg:     Arg{Type: ArgXDR, Value: NewValue("!!!!")},
			wantErr: "not a valid base64 ScVal",
		},
		"missing value": {
			arg:     Arg{Type: ArgU32},
			wantErr: "missing value",
		},
		"scalar expected but list given": {
			arg:     Arg{Type: ArgU32, Value: NewValue([]int{1, 2})},
			wantErr: "expected a single value",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := tc.arg.ToScVal()
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestArgsToScValsReportsFailingIndex(t *testing.T) {
	// With several constructor arguments, knowing which one is wrong matters.
	_, err := ArgsToScVals([]Arg{
		{Type: ArgU32, Value: NewValue("1")},
		{Type: ArgU32, Value: NewValue("not a number")},
	})
	assert.ErrorContains(t, err, "arg[1]")
}

func TestArgsToScValsEmpty(t *testing.T) {
	vals, err := ArgsToScVals(nil)
	require.NoError(t, err)
	assert.Empty(t, vals)
}

func TestParseSalt(t *testing.T) {
	t.Run("valid 32 bytes", func(t *testing.T) {
		salt, err := ParseSalt("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
		require.NoError(t, err)
		assert.Equal(t, byte(0x00), salt[0])
		assert.Equal(t, byte(0xff), salt[31])
	})

	t.Run("0x prefix accepted", func(t *testing.T) {
		_, err := ParseSalt("0x00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
		require.NoError(t, err)
	})

	t.Run("wrong length rejected", func(t *testing.T) {
		_, err := ParseSalt("00112233")
		assert.ErrorContains(t, err, "32 bytes")
	})

	t.Run("non-hex rejected", func(t *testing.T) {
		_, err := ParseSalt("zz")
		assert.ErrorContains(t, err, "not valid hex")
	})
}

func uint32Ptr(v uint32) *xdr.Uint32 {
	x := xdr.Uint32(v)
	return &x
}

func TestParseCLIArg(t *testing.T) {
	tests := []struct {
		input   string
		wantTyp string
	}{
		{"u32:7", ArgU32},
		{"i32:-10", ArgI32},
		{"u64:100", ArgU64},
		{"i64:-100", ArgI64},
		{"u128:1000", ArgU128},
		{"i128:-1000", ArgI128},
		{"bool:true", ArgBool},
		{"bool:false", ArgBool},
		{"string:hello", ArgString},
		{"string:\"hello world\"", ArgString},
		{"symbol:transfer", ArgSymbol},
		{"bytes:deadbeef", ArgBytes},
		{"address:GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", ArgAddress},
		{"void", ArgVoid},
		{"{type: u32, value: 42}", ArgU32},
		{"{type: vec, value: [{type: u32, value: 1}, {type: u32, value: 2}]}", ArgVec},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			arg, err := ParseCLIArg(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTyp, arg.Type)
			_, err = arg.ToScVal()
			require.NoError(t, err)
		})
	}

	t.Run("invalid arguments", func(t *testing.T) {
		_, err := ParseCLIArg("")
		assert.Error(t, err)

		_, err = ParseCLIArg("u32:not_a_number")
		assert.Error(t, err)

		_, err = ParseCLIArg("invalidformat")
		assert.Error(t, err)
	})
}

func TestParseCLIArgs(t *testing.T) {
	args, err := ParseCLIArgs([]string{"u32:7", "string:test", "void"})
	require.NoError(t, err)
	require.Len(t, args, 3)
	assert.Equal(t, ArgU32, args[0].Type)
	assert.Equal(t, ArgString, args[1].Type)
	assert.Equal(t, ArgVoid, args[2].Type)

	_, err = ParseCLIArgs([]string{"u32:7", "bad"})
	assert.ErrorContains(t, err, "arg[1]")
}
