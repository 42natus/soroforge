package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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

// UnmarshalJSON captures a JSON value the same way UnmarshalYAML captures a
// YAML node, so an --args-file accepts exactly the shapes soroforge.yaml does,
// including nested vec and map arguments.
func (v *Value) UnmarshalJSON(raw []byte) error {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	// UseNumber keeps integers exact: without it JSON decodes every number to
	// float64, and a u64 or u128 beyond 2^53 would be silently rounded before
	// ToScVal ever sees it.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	*v = NewValue(normalizeJSONNumbers(decoded))
	return nil
}

// normalizeJSONNumbers converts the json.Number values a UseNumber decode
// produces into Go integers.
//
// It exists so a number written in an --args-file keeps its numeric form when
// the arguments are recorded: yaml.Node.Encode tags the json.Number string
// type as a YAML string, which would record `value: 9` as "9" while the same
// value passed with --arg records as 9. Numbers too large for uint64 stay
// strings, which ToScVal still reads exactly.
func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if n, err := strconv.ParseUint(t.String(), 10, 64); err == nil {
			return n
		}
		return t.String()
	case []any:
		for i := range t {
			t[i] = normalizeJSONNumbers(t[i])
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSONNumbers(val)
		}
		return t
	default:
		return v
	}
}

// MarshalJSON writes the captured value back as JSON, for recording the
// constructor arguments a deployment actually used.
func (v Value) MarshalJSON() ([]byte, error) {
	if !v.set {
		return []byte("null"), nil
	}

	switch v.node.Tag {
	case "!!int":
		// Emit the digits directly. Decoding through any and re-marshalling
		// would route values beyond uint64 through float64 and lose precision,
		// and a u128 argument is exactly where that matters. A non-decimal
		// YAML form (0x10, 1_000) falls through to the decode below, which
		// resolves it to the number it denotes.
		text := strings.TrimPrefix(strings.TrimSpace(v.node.Value), "+")
		if isDecimalInt(text) {
			return json.RawMessage(text), nil
		}
	case "!!bool":
		b, err := v.asBool()
		if err != nil {
			return nil, err
		}
		return json.Marshal(b)
	}

	var decoded any
	if err := v.node.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// isDecimalInt reports whether s is a plain decimal integer literal, the only
// form that is valid JSON as-is.
func isDecimalInt(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
