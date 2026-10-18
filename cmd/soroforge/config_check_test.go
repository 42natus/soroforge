package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadConfigForCheck(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), config.DefaultFilename)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	return cfg
}

func TestCheckConfigReportsAllArtifactAndRPCProblems(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.wasm"), []byte("not wasm"), 0o600))
	body := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://testnet.example.org
    passphrase: "Test SDF Network ; September 2015"
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
contracts:
  badmagic:
    wasm: %q
  missing:
    wasm: %q
`, filepath.Join(dir, "bad.wasm"), filepath.Join(dir, "missing.wasm"))
	cfg := loadConfigForCheck(t, body)

	testnet := stellar.NewMockClient()
	testnet.NetworkInfoPassphrase = "wrong network"
	mainnet := stellar.NewMockClient()
	mainnet.NetworkInfoErr = errors.New("endpoint unavailable")
	clients := map[string]*stellar.MockClient{"testnet": testnet, "mainnet": mainnet}
	var factoryCalls []string
	err := checkConfig(context.Background(), cfg, false, func(name string, _ config.Network) (stellar.Client, error) {
		factoryCalls = append(factoryCalls, name)
		return clients[name], nil
	})

	require.Error(t, err)
	assert.Equal(t, []string{"mainnet", "testnet"}, factoryCalls)
	assert.Contains(t, err.Error(), "contracts.badmagic.wasm")
	assert.Contains(t, err.Error(), "does not start with the WebAssembly magic bytes")
	assert.Contains(t, err.Error(), "contracts.missing.wasm")
	assert.Contains(t, err.Error(), "networks.mainnet.rpc_url")
	assert.Contains(t, err.Error(), "endpoint unavailable")
	assert.Contains(t, err.Error(), "networks.testnet.passphrase")
	assert.Equal(t, 1, mainnet.CloseCalls)
	assert.Equal(t, 1, testnet.CloseCalls)
}

func TestCheckConfigOfflineStillChecksWasmAndSkipsRPC(t *testing.T) {
	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "counter.wasm")
	require.NoError(t, os.WriteFile(wasmPath, []byte{0x00, 0x61, 0x73, 0x6d, 0x01}, 0o600))
	cfg := loadConfigForCheck(t, fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://testnet.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: %q
`, wasmPath))

	err := checkConfig(context.Background(), cfg, true, func(string, config.Network) (stellar.Client, error) {
		t.Fatal("offline check called the RPC factory")
		return nil, nil
	})
	require.NoError(t, err)
}

func TestConfigCheckCommandHasOfflineFlag(t *testing.T) {
	cmd := newRootCmd()
	configCmd, _, err := cmd.Find([]string{"config", "check"})
	require.NoError(t, err)
	require.NotNil(t, configCmd.Flags().Lookup("offline"))
}
