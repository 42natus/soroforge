package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfig = `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    passphrase: "Test SDF Network ; September 2015"
    sorovault_url: http://localhost:8080
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
contracts:
  counter:
    wasm: ./out/counter.wasm
  token:
    wasm: ./out/token.wasm
    upgrade_fn: admin_upgrade
    constructor_args:
      - {type: u32, value: 7}
`

// writeConfig writes a config file into a temp directory and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFilename)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadValidConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	assert.Equal(t, 1, cfg.Version)
	assert.Equal(t, "testnet", cfg.DefaultNetwork)
	assert.Len(t, cfg.Networks, 2)
	assert.Len(t, cfg.Contracts, 2)
	assert.Equal(t, "http://localhost:8080", cfg.Networks["testnet"].SoroVaultURL)
	assert.Empty(t, cfg.Networks["mainnet"].SoroVaultURL, "the registry is optional per network")
}

func TestNetworkLookupFallsBackToDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	name, network, err := cfg.Network("")
	require.NoError(t, err)
	assert.Equal(t, "testnet", name)
	assert.Equal(t, "https://soroban-testnet.stellar.org", network.RPCURL)

	name, _, err = cfg.Network("mainnet")
	require.NoError(t, err)
	assert.Equal(t, "mainnet", name)
}

func TestNetworkLookupListsOptionsWhenUnknown(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	_, _, err = cfg.Network("staging")
	assert.ErrorContains(t, err, "unknown network")
	// Naming the configured networks turns a typo into a one-step fix.
	assert.ErrorContains(t, err, "mainnet, testnet")
}

func TestContractLookup(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	contract, err := cfg.Contract("counter")
	require.NoError(t, err)
	assert.Equal(t, "./out/counter.wasm", contract.Wasm)

	_, err = cfg.Contract("nope")
	assert.ErrorContains(t, err, "unknown contract")
	assert.ErrorContains(t, err, "counter, token")
}

func TestUpgradeFuncDefaultAndOverride(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	counter, err := cfg.Contract("counter")
	require.NoError(t, err)
	assert.Equal(t, DefaultUpgradeFunc, counter.UpgradeFuncOrDefault())

	token, err := cfg.Contract("token")
	require.NoError(t, err)
	assert.Equal(t, "admin_upgrade", token.UpgradeFuncOrDefault())
}

func TestWasmPathResolvesRelativeToConfigFile(t *testing.T) {
	// A relative wasm path must mean "relative to the project", not "relative
	// to whatever directory the shell happens to be in".
	path := writeConfig(t, validConfig)
	cfg, err := Load(path)
	require.NoError(t, err)

	contract, err := cfg.Contract("counter")
	require.NoError(t, err)

	expected := filepath.Join(filepath.Dir(path), "out", "counter.wasm")
	assert.Equal(t, expected, cfg.WasmPath(contract))
}

func TestWasmPathKeepsAbsolutePaths(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	absolute := filepath.Join(t.TempDir(), "built.wasm")
	assert.Equal(t, absolute, cfg.WasmPath(Contract{Wasm: absolute}))
}

func TestLoadUsesEnvConfigPath(t *testing.T) {
	path := writeConfig(t, validConfig)
	t.Setenv(EnvConfigPath, path)

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, path, cfg.Path())
}

func TestLoadExplicitPathBeatsEnv(t *testing.T) {
	explicit := writeConfig(t, validConfig)
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	cfg, err := Load(explicit)
	require.NoError(t, err)
	assert.Equal(t, explicit, cfg.Path())
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	assert.ErrorContains(t, err, "read config")
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	// A silently ignored typo in a config that selects networks is exactly the
	// kind of mistake that deploys to the wrong place.
	_, err := Load(writeConfig(t, `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    pass_phrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
`))
	assert.ErrorContains(t, err, "pass_phrase")
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	// Fixing a config one error per run is miserable, so validation collects
	// all of them.
	_, err := Load(writeConfig(t, `
version: 2
default_network: staging
networks:
  testnet:
    rpc_url: ""
    passphrase: ""
contracts: {}
`))
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "version")
	assert.Contains(t, msg, "rpc_url")
	assert.Contains(t, msg, "passphrase")
	assert.Contains(t, msg, "default_network")
	assert.Contains(t, msg, "contracts")
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := map[string]struct {
		config  string
		wantErr string
	}{
		"non-http rpc scheme": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: ftp://example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter: {wasm: ./c.wasm}
`,
			wantErr: "scheme must be http or https",
		},
		"rpc url without host": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: "https://"
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter: {wasm: ./c.wasm}
`,
			wantErr: "missing host",
		},
		"sorovault url without scheme": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    sorovault_url: localhost:8080
contracts:
  counter: {wasm: ./c.wasm}
`,
			wantErr: "networks.testnet.sorovault_url",
		},
		"contract without wasm": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter: {}
`,
			wantErr: "wasm: must be set",
		},
		"invalid constructor arg": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./c.wasm
    constructor_args:
      - {type: u32, value: "not a number"}
`,
			wantErr: "constructor_args[0]",
		},
		"both constructor_args and init_fn": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./c.wasm
    constructor_args:
      - {type: u32, value: 7}
    init_fn: initialize
`,
			wantErr: "cannot configure both constructor_args and init_fn",
		},
		"init_args without init_fn": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./c.wasm
    init_args:
      - {type: u32, value: 7}
`,
			wantErr: "init_args: init_fn must be set",
		},
		"invalid init_args": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./c.wasm
    init_fn: initialize
    init_args:
      - {type: u32, value: "not a number"}
`,
			wantErr: "init_args[0]",
		},
		"invalid salt": {
			config: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./c.wasm
    salt: "abcd"
`,
			wantErr: "salt",
		},
		"default_network not configured": {
			config: `
version: 1
default_network: mainnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter: {wasm: ./c.wasm}
`,
			wantErr: "is not one of the configured networks",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.config))
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidateAcceptsMissingWasmFile(t *testing.T) {
	// Whether a contract has been built yet is not a property of the
	// configuration; requiring the artifact at load time would make `soroforge
	// list` fail on a clean checkout.
	cfg, err := Load(writeConfig(t, validConfig))
	require.NoError(t, err)

	contract, err := cfg.Contract("counter")
	require.NoError(t, err)

	_, statErr := os.Stat(cfg.WasmPath(contract))
	assert.True(t, os.IsNotExist(statErr), "the artifact genuinely does not exist")
}

func TestWebhooksParseBothForms(t *testing.T) {
	// A network may list webhooks as bare URLs or as {url, secret} mappings;
	// both must land in Network.Webhooks.
	cfg, err := Load(writeConfig(t, `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - https://hooks.example.org/a
      - url: https://internal.example.org/b
        secret: shared
contracts:
  counter: {wasm: ./c.wasm}
`))
	require.NoError(t, err)

	assert.Equal(t, []Webhook{
		{URL: "https://hooks.example.org/a"},
		{URL: "https://internal.example.org/b", Secret: "shared"},
	}, cfg.Networks["testnet"].Webhooks)
}

func TestWebhookValidation(t *testing.T) {
	tests := map[string]struct {
		entries string
		wantErr string
	}{
		"non-http scheme": {
			entries: "      - ftp://hooks.example.org/a\n",
			wantErr: "networks.testnet.webhooks[0].url: scheme must be http or https",
		},
		"missing host": {
			entries: "      - \"https://\"\n",
			wantErr: "networks.testnet.webhooks[0].url: missing host",
		},
		"empty url in mapping": {
			entries: "      - url: https://ok.example.org\n      - {secret: s}\n",
			wantErr: "networks.testnet.webhooks[1].url: must be set",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
`+tc.entries+`contracts:
  counter: {wasm: ./c.wasm}
`))
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}
