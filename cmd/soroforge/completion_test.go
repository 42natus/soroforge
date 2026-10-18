package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The completion tests call the completion functions directly rather than
// generating and sourcing shell scripts: what must be proven is the contract
// the issue sets out — suggestions come from soroforge.yaml only, nothing
// remote or database-backed is touched, and a broken config fails quietly.

const completionConfigYAML = `version: 1
default_network: testnet

networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    passphrase: "Test SDF Network ; September 2015"
  mainnet:
    rpc_url: https://mainnet.sorobanrpc.com
    passphrase: "Public Global Stellar Network ; September 2015"
  local:
    rpc_url: http://localhost:8000/soroban/rpc
    passphrase: "Standalone Network ; February 2017"

contracts:
  zeta:
    wasm: ./zeta.wasm
  counter:
    wasm: ./counter.wasm
  alpha:
    wasm: ./alpha.wasm
`

// useCompletionConfig points the global --config flag at a temporary
// soroforge.yaml and restores the flags afterwards. Completion reads
// flags.configPath exactly like real commands do, so the test exercises the
// same path rules (--config, then $SOROFORGE_CONFIG, then ./soroforge.yaml).
func useCompletionConfig(t *testing.T, contents string) {
	t.Helper()

	saved := flags
	t.Cleanup(func() { flags = saved })
	flags = globalFlags{}

	// Isolate from the developer's environment: an unset (empty) config path
	// means only the explicit flags.configPath below matters.
	t.Setenv(config.EnvConfigPath, "")

	path := filepath.Join(t.TempDir(), "soroforge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
	flags.configPath = path
}

func TestAliasCompletionSuggestsConfiguredAliasesSorted(t *testing.T) {
	useCompletionConfig(t, completionConfigYAML)

	suggestions, directive := aliasCompletion(&cobra.Command{}, nil, "")

	assert.Equal(t, []string{"alpha", "counter", "zeta"}, suggestions,
		"every configured alias, in sorted order")
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive,
		"file completion must be suppressed: an alias is not a filename")
}

func TestNetworkFlagCompletionSuggestsConfiguredNetworksSorted(t *testing.T) {
	useCompletionConfig(t, completionConfigYAML)

	suggestions, directive := networkFlagCompletion(&cobra.Command{}, nil, "")

	assert.Equal(t, []string{"local", "mainnet", "testnet"}, suggestions)
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

// A completion runs on every keystroke in a shell; a broken or missing config
// must produce silence, not suggestions and not an error the shell prints.
func TestCompletionFailsQuietlyOnBadConfig(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T)
	}{
		{
			name: "missing file",
			setup: func(t *testing.T) {
				saved := flags
				t.Cleanup(func() { flags = saved })
				flags = globalFlags{}
				t.Setenv(config.EnvConfigPath, "")
				flags.configPath = filepath.Join(t.TempDir(), "does-not-exist.yaml")
			},
		},
		{
			name: "invalid config",
			// version 2 does not exist and unknown keys are rejected.
			setup: func(t *testing.T) { useCompletionConfig(t, "version: 2\nbogus_key: true\n") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)

			aliasSuggestions, aliasDirective := aliasCompletion(&cobra.Command{}, nil, "")
			assert.Nil(t, aliasSuggestions)
			assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, aliasDirective)

			networkSuggestions, networkDirective := networkFlagCompletion(&cobra.Command{}, nil, "")
			assert.Nil(t, networkSuggestions)
			assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, networkDirective)
		})
	}
}

// Completion must work with no DATABASE_URL, no signing key, and unreachable
// networks — it reads soroforge.yaml and nothing else. The config above points
// at hosts that do not exist in a test run, and the tests never set any
// environment secrets, so suggestions arriving at all proves the seam.
func TestCompletionNeedsOnlyTheConfigFile(t *testing.T) {
	useCompletionConfig(t, completionConfigYAML)
	t.Setenv(config.EnvDatabaseURL, "")
	t.Setenv(config.EnvSecretKey, "")

	suggestions, _ := aliasCompletion(&cobra.Command{}, nil, "")
	assert.NotEmpty(t, suggestions)
}

func TestAliasCompletionWiredOntoAliasCommands(t *testing.T) {
	// The commands whose first positional argument is an alias must all carry
	// the completion; a refactor that rebuilds one of them can drop it.
	want := map[string]bool{
		"deploy":  false,
		"upgrade": false,
		"upload":  false,
		"history": false,
		"status":  false,
	}

	for name := range want {
		cmd, _, err := newRootCmd().Find([]string{name})
		require.NoError(t, err, "command %q must exist", name)
		require.NotNil(t, cmd.ValidArgsFunction,
			"%q needs a ValidArgsFunction for alias completion", name)
		want[name] = true
	}
	for _, found := range want {
		assert.True(t, found)
	}
}

// End-to-end through cobra's hidden __complete command, the exact path a
// generated shell script takes. SOROFORGE_CONFIG supplies the config because
// the __complete invocation itself does not parse --config.
func runComplete(t *testing.T, args ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "soroforge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(completionConfigYAML), 0o644))

	saved := flags
	t.Cleanup(func() { flags = saved })
	t.Setenv(config.EnvConfigPath, path)

	root := newRootCmd() // re-registers the flag defaults onto the globals
	var buf, errBuf bytes.Buffer
	root.SetOut(&buf)
	// The "Completion ended with directive..." trailer goes to stderr; keep it
	// out of the captured stdout the shell script actually parses.
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"__complete"}, args...))
	require.NoError(t, root.Execute())

	return buf.String()
}

func TestCompleteDeploySuggestsAliases(t *testing.T) {
	out := runComplete(t, "deploy", "")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.NotEmpty(t, lines)
	assert.Equal(t, ":4", lines[len(lines)-1], "directive must be NoFileComp")

	assert.Equal(t, []string{"alpha", "counter", "zeta"}, lines[:len(lines)-1])
}

func TestCompleteNetworkFlagSuggestsNetworks(t *testing.T) {
	// The --network flag is registered on the root as a persistent flag; the
	// completion must reach it from a subcommand through inheritance.
	out := runComplete(t, "deploy", "--network", "")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.NotEmpty(t, lines)
	assert.Equal(t, ":4", lines[len(lines)-1])

	assert.Equal(t, []string{"local", "mainnet", "testnet"}, lines[:len(lines)-1])
}

func TestCompleteStatusSuggestsAliases(t *testing.T) {
	out := runComplete(t, "status", "")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.NotEmpty(t, lines)
	assert.Equal(t, ":4", lines[len(lines)-1])
	assert.Equal(t, []string{"alpha", "counter", "zeta"}, lines[:len(lines)-1])
}
