package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupLoggingTest isolates the global flags and default logger that
// setupLogging manipulates.
func setupLoggingTest(t *testing.T) {
	t.Helper()

	savedFlags := flags
	savedLogger := slog.Default()
	t.Cleanup(func() {
		flags = savedFlags
		slog.SetDefault(savedLogger)
	})
}

func TestSetupLoggingJSONEmitsParseableLines(t *testing.T) {
	setupLoggingTest(t)
	flags.logFormat = logFormatJSON
	flags.logLevel = "info"

	var buf bytes.Buffer
	require.NoError(t, setupLogging(&buf))

	// What a real command logs: fields an aggregator can filter on.
	slog.Info("deploy confirmed", "alias", "counter", "network", "testnet", "ledger", 42)

	line := strings.TrimSpace(buf.String())
	require.NotEmpty(t, line)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &entry), "each line must be JSON: %s", line)
	assert.Equal(t, "deploy confirmed", entry["msg"])
	assert.Equal(t, "INFO", entry["level"])
	assert.Equal(t, "counter", entry["alias"])
	assert.Equal(t, "testnet", entry["network"])
	assert.Equal(t, float64(42), entry["ledger"])
	assert.Contains(t, entry, "time")
}

func TestSetupLoggingDefaultsToText(t *testing.T) {
	setupLoggingTest(t)
	flags.logFormat = ""
	flags.logLevel = "info"
	t.Setenv(config.EnvLogFormat, "")

	var buf bytes.Buffer
	require.NoError(t, setupLogging(&buf))

	slog.Info("hello", "alias", "counter")

	out := buf.String()
	assert.Contains(t, out, "msg=hello")
	assert.Contains(t, out, "alias=counter")

	var entry map[string]any
	assert.Error(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &entry),
		"the default handler must not be JSON")
}

func TestSetupLoggingFormatFromEnv(t *testing.T) {
	setupLoggingTest(t)
	flags.logFormat = ""
	flags.logLevel = "info"
	t.Setenv(config.EnvLogFormat, "json")

	var buf bytes.Buffer
	require.NoError(t, setupLogging(&buf))

	slog.Info("from env")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry))
	assert.Equal(t, "from env", entry["msg"])
}

func TestSetupLoggingUnknownFormatRejected(t *testing.T) {
	setupLoggingTest(t)
	flags.logFormat = "yaml"
	flags.logLevel = "info"

	err := setupLogging(&bytes.Buffer{})

	require.Error(t, err)
	assert.ErrorContains(t, err, "yaml")
	assert.ErrorContains(t, err, logFormatText)
	assert.ErrorContains(t, err, logFormatJSON)
}

func TestSetupLoggingUnknownLevelRejected(t *testing.T) {
	setupLoggingTest(t)
	flags.logFormat = ""
	flags.logLevel = "loud"
	t.Setenv(config.EnvLogFormat, "")

	err := setupLogging(&bytes.Buffer{})

	require.Error(t, err)
	assert.ErrorContains(t, err, "debug")
}

func TestBuildSignerUsesTheExternalCommand(t *testing.T) {
	full := keypair.MustRandom()

	signer, err := buildSigner(&config.Config{Env: config.Env{
		SignerCommand:   "/usr/local/bin/sign",
		SignerPublicKey: full.Address(),
	}}, true)
	require.NoError(t, err)

	// Building the signer must not run the command; that happens on Sign.
	assert.Equal(t, full.Address(), signer.Address())
}

func TestBuildSignerRejectsMixedKeySources(t *testing.T) {
	full := keypair.MustRandom()

	_, err := buildSigner(&config.Config{Env: config.Env{
		SignerCommand:   "/usr/local/bin/sign",
		SignerPublicKey: full.Address(),
		SecretKey:       full.Seed(),
	}}, true)
	assert.ErrorContains(t, err, "cannot be combined")
}

func TestBuildSignerWithoutAKey(t *testing.T) {
	signer, err := buildSigner(&config.Config{}, false)
	require.NoError(t, err)
	assert.Nil(t, signer)

	_, err = buildSigner(&config.Config{}, true)
	assert.ErrorContains(t, err, "no signing key configured")
	assert.ErrorContains(t, err, config.EnvSignerCommand)
}
