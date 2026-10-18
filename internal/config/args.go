package config

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"gopkg.in/yaml.v3"
)

// Arg is a contract argument written in soroforge.yaml.
//
// Soroban takes arguments as XDR ScVal values, which are not something anyone
// wants to hand-write. An Arg is the readable form:
//
//	constructor_args:
//	  - {type: address, value: GABC...}
//	  - {type: u32, value: 7}
//	  - {type: string, value: "MyToken"}
//	  - {type: vec, value: [{type: u32, value: 1}, {type: u32, value: 2}]}
//
// The supported type names cover the values contracts actually take in their
// constructors. Anything outside that set can still be passed with the "xdr"
// escape hatch, which accepts a base64 ScVal directly — so a missing type is
// never a hard block:
//
//   - {type: xdr, value: "AAAAAQ=="}
//
// To add a type, extend the switch in ToScVal and add a case to the table test
// in args_test.go. Nothing else needs to change.
type Arg struct {
	Type  string `yaml:"type"`
	Value Value  `yaml:"value"`
}

// Supported Arg type names.
const (
	ArgBool      = "bool"
	ArgVoid      = "void"
	ArgU32       = "u32"
	ArgI32       = "i32"
	ArgU64       = "u64"
	ArgI64       = "i64"
	ArgU128      = "u128"
	ArgI128      = "i128"
	ArgU256      = "u256"
	ArgI256      = "i256"
	ArgTimepoint = "timepoint"
	ArgDuration  = "duration"
	ArgString    = "string"
	ArgSymbol    = "symbol"
	ArgBytes     = "bytes"
	ArgAddress   = "address"
	ArgVec       = "vec"
	ArgMap       = "map"
	ArgXDR       = "xdr"
)

// SupportedArgTypes lists every recognised Arg type, for error messages and docs.
var SupportedArgTypes = []string{
	ArgBool, ArgVoid, ArgU32, ArgI32, ArgU64, ArgI64, ArgU128, ArgI128,
	ArgU256, ArgI256, ArgTimepoint, ArgDuration,
	ArgString, ArgSymbol, ArgBytes, ArgAddress, ArgVec, ArgMap, ArgXDR,
}

// MapEntry is one key/value pair of a `map` argument.
type MapEntry struct {
	Key   Arg `yaml:"key"`
	Value Arg `yaml:"value"`
}

// ToScVal converts the argument to its XDR representation.
//
// Errors describe the offending value and the expected shape, since a config
// author reading them has no visibility into Soroban's type system.
func (a Arg) ToScVal() (xdr.ScVal, error) {
	switch strings.ToLower(strings.TrimSpace(a.Type)) {
	case "":
		return xdr.ScVal{}, fmt.Errorf("missing type (supported: %s)", strings.Join(SupportedArgTypes, ", "))

	case ArgVoid:
		return xdr.ScVal{Type: xdr.ScValTypeScvVoid}, nil

	case ArgBool:
		b, err := a.Value.asBool()
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b}, nil

	case ArgU32:
		n, err := a.Value.asUint(32)
		if err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.Uint32(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}, nil

	case ArgI32:
		n, err := a.Value.asInt(32)
		if err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.Int32(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvI32, I32: &v}, nil

	case ArgU64:
		n, err := a.Value.asUint(64)
		if err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.Uint64(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}, nil

	case ArgI64:
		n, err := a.Value.asInt(64)
		if err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.Int64(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &v}, nil

	case ArgU128:
		parts, err := a.Value.asUint128()
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &parts}, nil

	case ArgI128:
		parts, err := a.Value.asInt128()
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}, nil

	case ArgU256:
		parts, err := a.Value.asUint256()
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &parts}, nil

	case ArgI256:
		parts, err := a.Value.asInt256()
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvI256, I256: &parts}, nil

	case ArgTimepoint:
		n, err := a.Value.asUint(64)
		if err != nil {
			return xdr.ScVal{}, err
		}
		tp := xdr.TimePoint(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvTimepoint, Timepoint: &tp}, nil

	case ArgDuration:
		n, err := a.Value.asUint(64)
		if err != nil {
			return xdr.ScVal{}, err
		}
		dur := xdr.Duration(n)
		return xdr.ScVal{Type: xdr.ScValTypeScvDuration, Duration: &dur}, nil

	case ArgString:
		s, err := a.Value.asString()
		if err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.ScString(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}, nil

	case ArgSymbol:
		s, err := a.Value.asString()
		if err != nil {
			return xdr.ScVal{}, err
		}
		if err := validateSymbol(s); err != nil {
			return xdr.ScVal{}, err
		}
		v := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}, nil

	case ArgBytes:
		s, err := a.Value.asString()
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("bytes: expected a hex string: %w", err)
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("bytes: %q is not valid hex: %w", s, err)
		}
		v := xdr.ScBytes(raw)
		return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &v}, nil

	case ArgAddress:
		s, err := a.Value.asString()
		if err != nil {
			return xdr.ScVal{}, err
		}
		addr, err := ParseAddress(s)
		if err != nil {
			return xdr.ScVal{}, err
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}, nil

	case ArgVec:
		var items []Arg
		if err := a.Value.decode(&items); err != nil {
			return xdr.ScVal{}, fmt.Errorf("vec: expected a list of args: %w", err)
		}
		vec := make(xdr.ScVec, 0, len(items))
		for i, item := range items {
			val, err := item.ToScVal()
			if err != nil {
				return xdr.ScVal{}, fmt.Errorf("vec[%d]: %w", i, err)
			}
			vec = append(vec, val)
		}
		// ScVal.Vec is a **ScVec: the outer pointer marks union presence, the
		// inner one carries XDR's optional-vector encoding.
		ptr := &vec
		return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &ptr}, nil

	case ArgMap:
		var entries []MapEntry
		if err := a.Value.decode(&entries); err != nil {
			return xdr.ScVal{}, fmt.Errorf("map: expected a list of {key, value} pairs: %w", err)
		}
		scMap := make(xdr.ScMap, 0, len(entries))
		for i, entry := range entries {
			key, err := entry.Key.ToScVal()
			if err != nil {
				return xdr.ScVal{}, fmt.Errorf("map[%d].key: %w", i, err)
			}
			val, err := entry.Value.ToScVal()
			if err != nil {
				return xdr.ScVal{}, fmt.Errorf("map[%d].value: %w", i, err)
			}
			scMap = append(scMap, xdr.ScMapEntry{Key: key, Val: val})
		}
		ptr := &scMap
		return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &ptr}, nil

	case ArgXDR:
		s, err := a.Value.asString()
		if err != nil {
			return xdr.ScVal{}, fmt.Errorf("xdr: expected a base64 string: %w", err)
		}
		var val xdr.ScVal
		if err := xdr.SafeUnmarshalBase64(s, &val); err != nil {
			return xdr.ScVal{}, fmt.Errorf("xdr: %q is not a valid base64 ScVal: %w", s, err)
		}
		return val, nil

	default:
		return xdr.ScVal{}, fmt.Errorf(
			"unsupported type %q (supported: %s; use type: xdr with a base64 ScVal for anything else)",
			a.Type, strings.Join(SupportedArgTypes, ", "))
	}
}

// ArgsToScVals converts a list of arguments, reporting which one failed.
func ArgsToScVals(args []Arg) ([]xdr.ScVal, error) {
	out := make([]xdr.ScVal, 0, len(args))
	for i, arg := range args {
		val, err := arg.ToScVal()
		if err != nil {
			return nil, fmt.Errorf("arg[%d]: %w", i, err)
		}
		out = append(out, val)
	}
	return out, nil
}

// ParseAddress converts a Stellar account (G...) or contract (C...) strkey into
// an ScAddress.
func ParseAddress(s string) (xdr.ScAddress, error) {
	s = strings.TrimSpace(s)
	switch {
	case strkey.IsValidEd25519PublicKey(s):
		accountID, err := xdr.AddressToAccountId(s)
		if err != nil {
			return xdr.ScAddress{}, fmt.Errorf("address %q: %w", s, err)
		}
		return xdr.ScAddress{
			Type:      xdr.ScAddressTypeScAddressTypeAccount,
			AccountId: &accountID,
		}, nil

	case strings.HasPrefix(s, "C"):
		decoded, err := strkey.Decode(strkey.VersionByteContract, s)
		if err != nil {
			return xdr.ScAddress{}, fmt.Errorf("contract address %q: %w", s, err)
		}
		if len(decoded) != 32 {
			return xdr.ScAddress{}, fmt.Errorf("contract address %q: expected 32 bytes, got %d", s, len(decoded))
		}
		var id xdr.ContractId
		copy(id[:], decoded)
		return xdr.ScAddress{
			Type:       xdr.ScAddressTypeScAddressTypeContract,
			ContractId: &id,
		}, nil

	default:
		return xdr.ScAddress{}, fmt.Errorf(
			"address %q: expected a G... account or C... contract address", s)
	}
}

// ParseSalt decodes a 32-byte hex salt used to derive a deterministic contract ID.
func ParseSalt(s string) ([32]byte, error) {
	var salt [32]byte
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil {
		return salt, fmt.Errorf("salt %q is not valid hex: %w", s, err)
	}
	if len(raw) != 32 {
		return salt, fmt.Errorf("salt must be 32 bytes (64 hex chars), got %d bytes", len(raw))
	}
	copy(salt[:], raw)
	return salt, nil
}

// maxSymbolLen is the Soroban limit on ScSymbol length.
const maxSymbolLen = 32

func validateSymbol(s string) error {
	if len(s) > maxSymbolLen {
		return fmt.Errorf("symbol %q is %d characters, limit is %d", s, len(s), maxSymbolLen)
	}
	for _, r := range s {
		valid := r == '_' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z')
		if !valid {
			return fmt.Errorf("symbol %q contains %q; only [a-zA-Z0-9_] are allowed", s, r)
		}
	}
	return nil
}

// parse128 converts a decimal string into the hi/lo halves Soroban uses for
// 128-bit integers. signed selects the valid range.
func parse128(s string, signed bool) (hi uint64, lo uint64, err error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if !ok {
		return 0, 0, fmt.Errorf("%q is not a valid decimal integer", s)
	}

	var min, max *big.Int
	if signed {
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
		min = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
	} else {
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
		min = big.NewInt(0)
	}
	if n.Cmp(min) < 0 || n.Cmp(max) > 0 {
		return 0, 0, fmt.Errorf("%s is out of range for a 128-bit %s integer",
			n.String(), map[bool]string{true: "signed", false: "unsigned"}[signed])
	}

	// Two's complement for negatives, so the hi/lo split matches the XDR
	// encoding rather than a sign-magnitude reading.
	if n.Sign() < 0 {
		n = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 128), n)
	}

	loMask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	lo = new(big.Int).And(n, loMask).Uint64()
	hi = new(big.Int).Rsh(n, 64).Uint64()
	return hi, lo, nil
}

// parse256 converts a decimal string into the 64-bit words Soroban uses for
// 256-bit integers (HiHi, HiLo, LoHi, LoLo). signed selects the valid range.
//
// Follows SoroProbe's internal/scval/encode.go two's-complement split into
// HiHi/HiLo/LoHi/LoLo words.
func parse256(s string, signed bool) (hiHi, hiLo, loHi, loLo uint64, err error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("%q is not a valid decimal integer", s)
	}

	var min, max *big.Int
	if signed {
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(1))
		min = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255))
	} else {
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		min = big.NewInt(0)
	}
	if n.Cmp(min) < 0 || n.Cmp(max) > 0 {
		return 0, 0, 0, 0, fmt.Errorf("%s is out of range for a 256-bit %s integer",
			n.String(), map[bool]string{true: "signed", false: "unsigned"}[signed])
	}

	// Two's complement for negatives, matching the XDR encoding.
	// Approach follows SoroProbe's internal/scval/encode.go.
	if n.Sign() < 0 {
		n = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), n)
	}

	mask64 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	loLo = new(big.Int).And(n, mask64).Uint64()
	loHi = new(big.Int).And(new(big.Int).Rsh(n, 64), mask64).Uint64()
	hiLo = new(big.Int).And(new(big.Int).Rsh(n, 128), mask64).Uint64()
	hiHi = new(big.Int).Rsh(n, 192).Uint64()
	return hiHi, hiLo, loHi, loLo, nil
}

// parseUint and parseInt accept the decimal forms YAML may hand us for an
// integer field, including a quoted string (which is how anyone writing a value
// beyond float64's exact range must write it).
func parseUint(s string, bits int) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(s), 10, bits)
}

func parseInt(s string, bits int) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, bits)
}

// ParseCLIArg parses a command-line string into an Arg.
//
// It accepts two forms:
//  1. Compact type:value syntax, e.g. "u32:7", "string:hello", "address:GABC...", "bool:true", "void"
//  2. Full YAML/JSON syntax, e.g. "{type: u32, value: 7}", "{type: vec, value: [{type: u32, value: 1}]}"
func ParseCLIArg(s string) (Arg, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Arg{}, fmt.Errorf("argument cannot be empty")
	}

	// Case 1: Full YAML/JSON object
	if strings.HasPrefix(s, "{") {
		var a Arg
		if err := yaml.Unmarshal([]byte(s), &a); err == nil && a.Type != "" {
			if _, err := a.ToScVal(); err != nil {
				return Arg{}, err
			}
			return a, nil
		}
	}

	// Case 2: Special single-keyword "void"
	if strings.EqualFold(s, ArgVoid) {
		return Arg{Type: ArgVoid}, nil
	}

	// Case 3: Compact type:value syntax
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 2 {
		typePart := strings.ToLower(strings.TrimSpace(parts[0]))
		valPart := strings.TrimSpace(parts[1])

		var yamlStr string
		switch typePart {
		case ArgString, ArgU128, ArgI128:
			if !strings.HasPrefix(valPart, "\"") && !strings.HasPrefix(valPart, "'") {
				yamlStr = fmt.Sprintf("{type: %s, value: %q}", typePart, valPart)
			} else {
				yamlStr = fmt.Sprintf("{type: %s, value: %s}", typePart, valPart)
			}
		default:
			yamlStr = fmt.Sprintf("{type: %s, value: %s}", typePart, valPart)
		}

		var a Arg
		if err := yaml.Unmarshal([]byte(yamlStr), &a); err == nil {
			if _, err := a.ToScVal(); err == nil {
				return a, nil
			} else {
				return Arg{}, fmt.Errorf("invalid %s value %q: %w", typePart, valPart, err)
			}
		}
	}

	return Arg{}, fmt.Errorf("invalid typed argument %q (expected type:value e.g. u32:7 or {type: ..., value: ...})", s)
}

// ParseCLIArgs converts a slice of command-line argument strings into typed Args.
func ParseCLIArgs(args []string) ([]Arg, error) {
	out := make([]Arg, 0, len(args))
	for i, a := range args {
		arg, err := ParseCLIArg(a)
		if err != nil {
			return nil, fmt.Errorf("arg[%d]: %w", i, err)
		}
		out = append(out, arg)
	}
	return out, nil
}
