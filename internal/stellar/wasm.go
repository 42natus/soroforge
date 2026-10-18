package stellar

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// ContractSpecSection is the custom section a Soroban contract's exported
// interface lives in. SoroVault catalogues it, client bindings are generated
// from it, and SoroProbe types arguments with it.
const ContractSpecSection = "contractspecv0"

// DefaultMaxContractSize is the largest contract WASM the network accepts.
//
// 131072 bytes (128 KiB) is the value of the CONFIG_SETTING_CONTRACT_MAX_SIZE_BYTES
// network setting on both mainnet and testnet, read with getLedgerEntries for
// ConfigSettingID 0 (CONFIG_SETTING_CONTRACT_MAX_SIZE_BYTES) on 2026-10-17. The
// setting is protocol-level and can be raised, so SOROFORGE_MAX_CONTRACT_SIZE
// overrides it:
//
//	https://github.com/stellar/stellar-protocol/blob/master/contents/cap-0046/Stellar-contract-config-setting.x
const DefaultMaxContractSize = 128 * 1024

// CheckWasmSize rejects bytecode larger than the network's contract size limit.
//
// Checking locally turns a rejected upload after simulation into an immediate,
// readable error. A non-positive limit disables the check.
func CheckWasmSize(wasm []byte, limit int) error {
	if limit <= 0 || len(wasm) <= limit {
		return nil
	}
	return fmt.Errorf(
		"wasm is %d bytes, over the %d-byte network limit; run `stellar contract optimize` on it, "+
			"or raise SOROFORGE_MAX_CONTRACT_SIZE if the network limit has increased",
		len(wasm), limit)
}

const (
	wasmHeaderSize    = 8
	customSectionID   = 0
	wasmVersion       = 1
	maxULEB128Bytes   = 5
	contractSpecLabel = ContractSpecSection
)

var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d} // "\0asm"

// HasContractSpec reports whether wasm contains a contractspecv0 custom section.
//
// Only the section framing is walked: the spec's contents are the contract
// SDK's business, and a malformed body inside the section is still a spec that
// was exported. Structural damage anywhere in the module is reported as an
// error rather than guessed at.
func HasContractSpec(wasm []byte) (bool, error) {
	if len(wasm) < wasmHeaderSize {
		return false, fmt.Errorf("file is %d bytes, shorter than the %d-byte WASM header", len(wasm), wasmHeaderSize)
	}
	if !bytes.Equal(wasm[:4], wasmMagic) {
		return false, fmt.Errorf("missing WASM magic bytes (\\0asm)")
	}
	if version := binary.LittleEndian.Uint32(wasm[4:wasmHeaderSize]); version != wasmVersion {
		return false, fmt.Errorf("unsupported WASM version %d (expected %d)", version, wasmVersion)
	}

	offset := wasmHeaderSize
	for offset < len(wasm) {
		sectionID := wasm[offset]
		offset++

		size, n, err := readULEB128(wasm[offset:])
		if err != nil {
			return false, fmt.Errorf("section %d: read size: %w", sectionID, err)
		}
		offset += n
		if size > uint64(len(wasm)-offset) {
			return false, fmt.Errorf(
				"section %d claims %d bytes but only %d remain", sectionID, size, len(wasm)-offset)
		}
		payload := wasm[offset : offset+int(size)]

		if sectionID == customSectionID {
			present, err := customSectionIsSpec(payload)
			if err != nil {
				return false, err
			}
			if present {
				return true, nil
			}
		}

		offset += int(size)
	}
	return false, nil
}

// customSectionIsSpec reads a custom section's name and compares it with
// ContractSpecSection.
func customSectionIsSpec(payload []byte) (bool, error) {
	nameLen, n, err := readULEB128(payload)
	if err != nil {
		return false, fmt.Errorf("custom section: read name length: %w", err)
	}
	payload = payload[n:]
	if nameLen > uint64(len(payload)) {
		return false, fmt.Errorf(
			"custom section name claims %d bytes but only %d remain", nameLen, len(payload))
	}
	return string(payload[:nameLen]) == contractSpecLabel, nil
}

// readULEB128 decodes an unsigned LEB128 value, which WASM uses for section
// sizes and name lengths. Values wider than 32 bits are not valid here.
func readULEB128(buf []byte) (uint64, int, error) {
	var value uint64
	for i, b := range buf {
		if i >= maxULEB128Bytes {
			return 0, 0, fmt.Errorf("value is longer than %d bytes", maxULEB128Bytes)
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			if value > math.MaxUint32 {
				return 0, 0, fmt.Errorf("value %d exceeds 32 bits", value)
			}
			return value, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("truncated value")
}
