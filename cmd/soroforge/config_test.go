package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const configShowFixture = `version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./target/wasm32v1-none/release/counter.wasm
`

// captureStdout runs fn with os.Stdout redirected and returns what was
// written. It makes CLI output assertions possible without a subprocess.
func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = previous }()

	runErr := fn()

	require.NoError(t, w.Close())
	captured, readErr := io.ReadAll(r)
	require.NoError(t, readErr)
	require.NoError(t, r.Close())
	require.NoError(t, runErr)

	return string(captured)
}

// runCLI executes the root command with args and returns everything written to
// stdout. The global flags are reset first so one test cannot leak into the
// next.
func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	flags = globalFlags{}

	return captureStdout(t, func() error {
		cmd := newRootCmd()
		cmd.SetArgs(args)
		return cmd.Execute()
	})
}

func writeConfigShowFixture(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "soroforge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(configShowFixture), 0o600))
	return dir, path
}

func TestConfigShowPrintsResolvedConfigAndNeverPrintsSecrets(t *testing.T) {
	const (
		secret = "SFAKESECRETTHATMUSTNEVERAPPEARINOUTPUT"
		dbURL  = "postgres://user:hunter2@localhost:5432/soroforge"
		token  = "super-secret-api-token"
	)
	t.Setenv(config.EnvSecretKey, secret)
	t.Setenv(config.EnvDatabaseURL, dbURL)
	t.Setenv(config.EnvAPIToken, token)

	dir, path := writeConfigShowFixture(t)
	out := runCLI(t, "config", "show", "--config", path)

	// The resolved parts a deploy would actually use.
	assert.Contains(t, out, "testnet")
	assert.Contains(t, out, "https://soroban-testnet.stellar.org")
	assert.Contains(t, out, filepath.Join(dir, "target/wasm32v1-none/release/counter.wasm"),
		"WASM paths must be printed resolved and absolute")

	// Secrets are summarised by presence only.
	assert.Contains(t, out, "(set)")
	assert.Contains(t, out, "(unset)")
	for _, secretValue := range []string{secret, dbURL, token, "hunter2"} {
		assert.NotContains(t, out, secretValue, "the secret value must never be printed")
	}
}

func TestConfigShowJSONMatchesTheTextAndRedactsSecrets(t *testing.T) {
	const secret = "SFAKESECRETTHATMUSTNEVERAPPEARINOUTPUT"
	t.Setenv(config.EnvSecretKey, secret)
	t.Setenv(config.EnvDatabaseURL, "")

	dir, path := writeConfigShowFixture(t)
	out := runCLI(t, "config", "show", "--config", path, "--json")

	var view resolvedConfig
	require.NoError(t, json.Unmarshal([]byte(out), &view))

	assert.Equal(t, "testnet", view.DefaultNetwork)
	assert.Equal(t, path, view.Path)
	require.Contains(t, view.Networks, "testnet")
	assert.Equal(t, "https://soroban-testnet.stellar.org", view.Networks["testnet"].RPCURL)

	require.Contains(t, view.Contracts, "counter")
	assert.Equal(t, "./target/wasm32v1-none/release/counter.wasm",
		view.Contracts["counter"].Wasm)
	assert.Equal(t,
		filepath.Join(dir, "target/wasm32v1-none/release/counter.wasm"),
		view.Contracts["counter"].ResolvedWasm)

	assert.Equal(t, "(set)", view.Environment[config.EnvSecretKey])
	assert.Equal(t, "(unset)", view.Environment[config.EnvDatabaseURL])
	assert.NotContains(t, out, secret)
}
