package config

import (
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/xdr"
	"gopkg.in/yaml.v3"
)

// Value is a deferred YAML value: it captures whatever the document held under
// `value:` without interpreting it, so Arg.ToScVal can decide how to read it
// based on the sibling `type:` field.
//
// It exists because an Arg's value can be a scalar, a list, or a list of
// key/value pairs depending on the type, which a plain typed field cannot
// express. yaml.v3 gives us the node; the accessors below turn it into
// something with a useful error message attached.
type Value struct {
	node yaml.Node
	set  bool
}

// UnmarshalYAML captures the raw node.
func (v *Value) UnmarshalYAML(node *yaml.Node) error {
	v.node = *node
	v.set = true
	return nil
}

// MarshalYAML round-trips the captured node so a Config can be written back out.
func (v Value) MarshalYAML() (any, error) {
	if !v.set {
		return nil, nil
	}
	return &v.node, nil
}

// decode unmarshals the captured node into out.
func (v Value) decode(out any) error {
	if !v.set {
		return fmt.Errorf("missing value")
	}
	return v.node.Decode(out)
}

// scalar returns the value as its raw YAML text. Every scalar accessor goes
// through here so that `value: 7` and `value: "7"` behave identically — a
// distinction YAML cares about but a config author reasonably does not.
func (v Value) scalar() (string, error) {
	if !v.set {
		return "", fmt.Errorf("missing value")
	}
	if v.node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("expected a single value, got %s", kindName(v.node.Kind))
	}
	return v.node.Value, nil
}

func (v Value) asString() (string, error) {
	return v.scalar()
}

func (v Value) asBool() (bool, error) {
	s, err := v.scalar()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on":
		return true, nil
	case "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%q is not a boolean (expected true or false)", s)
	}
}

func (v Value) asUint(bits int) (uint64, error) {
	s, err := v.scalar()
	if err != nil {
		return 0, err
	}
	n, err := parseUint(s, bits)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid u%d: %w", s, bits, err)
	}
	return n, nil
}

func (v Value) asInt(bits int) (int64, error) {
	s, err := v.scalar()
	if err != nil {
		return 0, err
	}
	n, err := parseInt(s, bits)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid i%d: %w", s, bits, err)
	}
	return n, nil
}

func (v Value) asUint128() (xdr.UInt128Parts, error) {
	s, err := v.scalar()
	if err != nil {
		return xdr.UInt128Parts{}, err
	}
	hi, lo, err := parse128(s, false)
	if err != nil {
		return xdr.UInt128Parts{}, err
	}
	return xdr.UInt128Parts{Hi: xdr.Uint64(hi), Lo: xdr.Uint64(lo)}, nil
}

func (v Value) asInt128() (xdr.Int128Parts, error) {
	s, err := v.scalar()
	if err != nil {
		return xdr.Int128Parts{}, err
	}
	hi, lo, err := parse128(s, true)
	if err != nil {
		return xdr.Int128Parts{}, err
	}
	// Int128Parts.Hi is signed: reinterpreting the two's-complement high word
	// preserves negative values.
	return xdr.Int128Parts{Hi: xdr.Int64(int64(hi)), Lo: xdr.Uint64(lo)}, nil
}

func (v Value) asUint256() (xdr.UInt256Parts, error) {
	if v.node.Kind == yaml.ScalarNode && (v.node.Tag == "!!int" || v.node.Tag == "!!float") {
		return xdr.UInt256Parts{}, fmt.Errorf("256-bit values must be quoted strings in YAML to prevent precision loss (e.g. \"123\")")
	}
	s, err := v.scalar()
	if err != nil {
		return xdr.UInt256Parts{}, err
	}
	hiHi, hiLo, loHi, loLo, err := parse256(s, false)
	if err != nil {
		return xdr.UInt256Parts{}, err
	}
	return xdr.UInt256Parts{
		HiHi: xdr.Uint64(hiHi),
		HiLo: xdr.Uint64(hiLo),
		LoHi: xdr.Uint64(loHi),
		LoLo: xdr.Uint64(loLo),
	}, nil
}

func (v Value) asInt256() (xdr.Int256Parts, error) {
	if v.node.Kind == yaml.ScalarNode && (v.node.Tag == "!!int" || v.node.Tag == "!!float") {
		return xdr.Int256Parts{}, fmt.Errorf("256-bit values must be quoted strings in YAML to prevent precision loss (e.g. \"123\")")
	}
	s, err := v.scalar()
	if err != nil {
		return xdr.Int256Parts{}, err
	}
	hiHi, hiLo, loHi, loLo, err := parse256(s, true)
	if err != nil {
		return xdr.Int256Parts{}, err
	}
	return xdr.Int256Parts{
		HiHi: xdr.Int64(int64(hiHi)),
		HiLo: xdr.Uint64(hiLo),
		LoHi: xdr.Uint64(loHi),
		LoLo: xdr.Uint64(loLo),
	}, nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	default:
		return "an unknown node"
	}
}

// NewValue builds a Value from a Go value. It exists for tests and for callers
// that construct Args programmatically rather than from YAML.
func NewValue(v any) Value {
	var node yaml.Node
	if err := node.Encode(v); err != nil {
		// Encode only fails on types yaml cannot represent, which would be a
		// programming error at the call site rather than bad user input.
		panic(fmt.Sprintf("config.NewValue: %v", err))
	}
	return Value{node: node, set: true}
}
