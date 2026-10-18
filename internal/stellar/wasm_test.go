package stellar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// module assembles a WASM binary from raw sections, so tests can build exactly
// the shape they need without a compiler.
func module(sections ...[]byte) []byte {
	out := []byte("\x00asm\x01\x00\x00\x00")
	for _, section := range sections {
		out = append(out, section...)
	}
	return out
}

// uleb encodes a small unsigned LEB128 value (test lengths fit in one or two
// bytes).
func uleb(v int) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

func customSection(name string, body []byte) []byte {
	payload := append(uleb(len(name)), []byte(name)...)
	payload = append(payload, body...)
	section := []byte{customSectionID}
	section = append(section, uleb(len(payload))...)
	return append(section, payload...)
}

func TestHasContractSpec(t *testing.T) {
	tests := map[string]struct {
		wasm []byte
		want bool
	}{
		"minimal module has no spec": {
			wasm: module(),
			want: false,
		},
		"unrelated custom section": {
			wasm: module(customSection("producers", []byte{0x00})),
			want: false,
		},
		"spec section": {
			wasm: module(customSection(ContractSpecSection, []byte{0x00, 0x01})),
			want: true,
		},
		"spec after another section": {
			// A real module has type/function/code sections before the custom
			// section that carries the spec.
			wasm: module(
				[]byte{0x01, 0x04, 0x60, 0x00, 0x00}, // type section
				customSection("producers", nil),
				customSection(ContractSpecSection, []byte{0xde, 0xad}),
			),
			want: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := HasContractSpec(tc.wasm)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHasContractSpecReportsMalformedWasm(t *testing.T) {
	validHeader := []byte("\x00asm\x01\x00\x00\x00")

	tests := map[string]struct {
		wasm []byte
		want string
	}{
		"empty file": {
			wasm: nil,
			want: "shorter than the 8-byte WASM header",
		},
		"truncated header": {
			wasm: validHeader[:7],
			want: "shorter than the 8-byte WASM header",
		},
		"wrong magic": {
			wasm: []byte("nope\x01\x00\x00\x00"),
			want: "missing WASM magic",
		},
		"unsupported version": {
			wasm: []byte("\x00asm\x02\x00\x00\x00"),
			want: "unsupported WASM version 2",
		},
		"truncated section size": {
			wasm: append(validHeader, 0x00, 0x80),
			want: "read size",
		},
		"section extends past end": {
			wasm: append(validHeader, 0x00, 0x05, 0x01),
			want: "claims 5 bytes but only 1 remain",
		},
		"custom name extends past section": {
			wasm: append(validHeader, 0x00, 0x02, 0x05, 'a'),
			want: "name claims 5 bytes but only 1 remain",
		},
		"truncated custom name length": {
			wasm: append(validHeader, 0x00, 0x01, 0x80),
			want: "read name length",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			present, err := HasContractSpec(tc.wasm)
			require.Error(t, err)
			assert.False(t, present)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestCheckWasmSize(t *testing.T) {
	const limit = 16

	tests := map[string]struct {
		wasm    []byte
		limit   int
		wantErr bool
	}{
		"under the limit": {wasm: make([]byte, limit-1), limit: limit},
		"at the limit":    {wasm: make([]byte, limit), limit: limit},
		"over the limit":  {wasm: make([]byte, limit+1), limit: limit, wantErr: true},
		"zero disables":   {wasm: make([]byte, limit+1), limit: 0},
		"negative is off": {wasm: make([]byte, limit+1), limit: -1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := CheckWasmSize(tc.wasm, tc.limit)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, "over the 16-byte network limit")
			assert.ErrorContains(t, err, "stellar contract optimize")
			assert.ErrorContains(t, err, "SOROFORGE_MAX_CONTRACT_SIZE")
		})
	}
}
