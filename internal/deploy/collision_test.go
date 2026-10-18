package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSalt = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// fixedSaltConfig declares counter with a deterministic salt, so its address is
// known before anything is uploaded.
func fixedSaltConfig(salt string) string {
	return `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    salt: "` + salt + `"
`
}

// derivedAddress is the address a fixed-salt deploy of counter would occupy.
func derivedAddress(t *testing.T, h *harness) string {
	t.Helper()

	salt, err := config.ParseSalt(testSalt)
	require.NoError(t, err)

	address, err := stellar.ContractIDFromDeployer(
		h.cfg.Networks["testnet"].Passphrase, h.signer.Address(), salt)
	require.NoError(t, err)
	return address
}

func TestDeployStopsWhenFixedSaltAddressIsOccupied(t *testing.T) {
	// Deploying the same alias twice with a fixed salt would try to create a
	// contract that already exists. That must fail before the upload is paid
	// for, and the message must say how to proceed.
	h := newHarness(t, harnessOptions{configBody: fixedSaltConfig(testSalt)})
	address := derivedAddress(t, h)

	// A contract already lives at the deterministic address.
	require.NoError(t, h.client.SetContractInstance(address, stellar.WasmHash([]byte("old bytecode"))))

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)

	assert.ErrorContains(t, err, address, "the error must name the occupied address")
	assert.ErrorContains(t, err, "different salt")
	assert.ErrorContains(t, err, "soroforge upgrade counter")

	assert.Zero(t, h.client.SimulateCount(), "the check must run before simulation")
	assert.Zero(t, h.client.SendCount(), "nothing may be submitted")
}

func TestDeployProceedsWhenFixedSaltAddressIsFree(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: fixedSaltConfig(testSalt)})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
	assert.NotEmpty(t, result.ContractID)
	assert.Equal(t, 2, h.client.SimulateCount(), "a free address must not block the deploy")
}

func TestDeploySkipsCollisionCheckForRandomSalt(t *testing.T) {
	// Without a fixed salt every deploy gets a fresh address, so no check is
	// performed — even if the address the mocked simulation reports happens to
	// be occupied.
	h := newHarness(t, harnessOptions{})
	require.NoError(t, h.client.SetContractInstance(h.contractID, h.wasmHash))

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, h.contractID, result.ContractID)
}
