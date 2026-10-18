package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wasmWithSpec is a structurally valid module whose only custom section is
// named contractspecv0.
const wasmWithSpec = "\x00asm\x01\x00\x00\x00\x00\x0f\x0econtractspecv0"

// wasmWithoutSpec differs from testWasm and is structurally valid, but exports
// no interface metadata.
const wasmWithoutSpec = "\x00asm\x01\x00\x00\x00\x01\x00"

func TestDeployRejectsWasmOverTheSizeLimitBeforeAnyRPC(t *testing.T) {
	h := newHarness(t, harnessOptions{maxContractSize: len(testWasm) - 1})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "wasm is 8 bytes")
	assert.ErrorContains(t, err, "7-byte network limit")
	assert.ErrorContains(t, err, "stellar contract optimize")
	assert.Zero(t, h.client.SimulateCount(), "oversized bytecode must not reach simulation")
	assert.Zero(t, h.client.SendCount())
}

func TestDeployAcceptsWasmAtTheSizeLimit(t *testing.T) {
	h := newHarness(t, harnessOptions{maxContractSize: len(testWasm)})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
}

func TestDeployWarnsWhenTheSpecSectionIsMissing(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "contractspecv0")
	assert.Contains(t, result.Warnings[0], "SoroVault")
	assert.Contains(t, result.Warnings[0], "SoroProbe")
}

func TestDeployRequireSpecFailsBeforeAnyRPC(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", RequireSpec: true})

	require.Error(t, err)
	assert.ErrorContains(t, err, "contractspecv0")
	assert.ErrorContains(t, err, "--require-spec")
	assert.Zero(t, h.client.SimulateCount())
}

func TestDeployWithSpecSectionHasNoWarnings(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: wasmWithSpec})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:       "counter",
		DryRun:      true,
		RequireSpec: true,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Warnings)
}

func TestDeployReportsMalformedWasmClearly(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: "\x00asm\x01\x00\x00\x00junk"})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})

	require.Error(t, err)
	assert.ErrorContains(t, err, "malformed wasm")
	assert.Zero(t, h.client.SimulateCount())
}

func TestUpgradeWarnsWhenTheSpecSectionIsMissing(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: wasmWithoutSpec})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "contractspecv0")
}

func TestUpgradeRequireSpecFailsBeforeAnyRPC(t *testing.T) {
	h := newHarness(t, harnessOptions{wasm: wasmWithoutSpec})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{
		Alias:       "counter",
		RequireSpec: true,
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "contractspecv0")
	assert.Zero(t, h.client.SimulateCount())
}
