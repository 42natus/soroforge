package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mainnet() config.Network {
	return config.Network{
		RPCURL:     "https://mainnet.sorobanrpc.com",
		Passphrase: "Public Global Stellar Network ; September 2015",
		Protected:  true,
	}
}

func testnet() config.Network {
	return config.Network{
		RPCURL:     "https://soroban-testnet.stellar.org",
		Passphrase: "Test SDF Network ; September 2015",
	}
}

// readProbe fails the test if the gate reads stdin when it should not.
type readProbe struct {
	t    *testing.T
	text string
	saw  bool
}

func (p *readProbe) Read(buf []byte) (int, error) {
	p.t.Helper()
	p.saw = true
	if p.text == "" {
		return 0, io.EOF
	}
	n := copy(buf, p.text)
	p.text = p.text[n:]
	return n, nil
}

func TestConfirmProtectedPromptAccepted(t *testing.T) {
	var out bytes.Buffer
	err := confirmProtected(&out, strings.NewReader("mainnet\n"), true,
		"mainnet", mainnet(), "deploy", "counter", false, false)
	require.NoError(t, err, "typing the network name exactly is the accepted answer")
	assert.Contains(t, out.String(), "Type the network name (mainnet)")
	assert.Contains(t, out.String(), "spends real XLM", "the operator must see what will happen")
	assert.Contains(t, out.String(), "Public Global Stellar Network", "the passphrase names the real chain")
}

func TestConfirmProtectedPromptRejected(t *testing.T) {
	for _, answer := range []string{"testnet\n", "mainnet typo\n", "MAINNET\n", "\n"} {
		t.Run(answer, func(t *testing.T) {
			var out bytes.Buffer
			err := confirmProtected(&out, strings.NewReader(answer), true,
				"mainnet", mainnet(), "deploy", "counter", false, false)
			require.Error(t, err, "anything but the exact network name refuses: %q", answer)
			assert.Contains(t, err.Error(), "confirmation rejected")
			assert.Contains(t, err.Error(), "nothing submitted")
		})
	}
}

func TestConfirmProtectedEmptyStdinRefuses(t *testing.T) {
	// An immediately-empty stdin (EOF, no bytes) must refuse, not hang and
	// not treat emptiness as consent.
	var out bytes.Buffer
	err := confirmProtected(&out, strings.NewReader(""), true,
		"mainnet", mainnet(), "deploy", "counter", false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing submitted", "EOF is not consent")
}

func TestConfirmProtectedYesSkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	probe := &readProbe{t: t}
	err := confirmProtected(&out, probe, true,
		"mainnet", mainnet(), "deploy", "counter", true, false)
	require.NoError(t, err, "--yes is the CI opt-out")
	assert.False(t, probe.saw, "--yes must not read stdin")
	assert.Contains(t, out.String(), "skipped with --yes", "the skip is recorded on stderr")
}

func TestConfirmProtectedNonInteractiveRefusal(t *testing.T) {
	// The fail-closed path: CI or piped stdin, no terminal, no --yes. The
	// command must refuse rather than block forever on a prompt nobody can
	// answer.
	var out bytes.Buffer
	probe := &readProbe{t: t, text: "mainnet\n"}
	err := confirmProtected(&out, probe, false,
		"mainnet", mainnet(), "deploy", "counter", false, false)
	require.Error(t, err, "without a terminal and without --yes the command refuses")
	assert.False(t, probe.saw, "a refusal must not consume stdin, correct answer or not")
	assert.Contains(t, err.Error(), "not a terminal")
	assert.Contains(t, err.Error(), "--yes", "the error names the way out")
}

func TestConfirmProtectedDryRunNeverPrompts(t *testing.T) {
	var out bytes.Buffer
	probe := &readProbe{t: t}
	err := confirmProtected(&out, probe, false,
		"mainnet", mainnet(), "deploy", "counter", false, true)
	require.NoError(t, err, "--dry-run submits nothing, so there is nothing to confirm")
	assert.False(t, probe.saw)
	assert.Empty(t, out.String())
}

func TestConfirmProtectedUnprotectedNetworkProceeds(t *testing.T) {
	var out bytes.Buffer
	probe := &readProbe{t: t}
	err := confirmProtected(&out, probe, false,
		"testnet", testnet(), "deploy", "counter", false, false)
	require.NoError(t, err, "testnet keeps its one-command CI flow")
	assert.False(t, probe.saw)
	assert.Empty(t, out.String())
}

func TestConfirmProtectedUpgradeUsesActionInCopy(t *testing.T) {
	var out bytes.Buffer
	err := confirmProtected(&out, strings.NewReader("staging\n"), true,
		"staging", config.Network{Passphrase: "P", Protected: true},
		"upgrade", "token", false, false)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "About to upgrade token")
}

// The prompt gate must fail closed for an unreadable stdin even when the
// descriptor claims to be a terminal: a scan error is a refusal, not consent.
func TestConfirmProtectedReadErrorRefuses(t *testing.T) {
	var out bytes.Buffer
	err := confirmProtected(&out, errReader{}, true,
		"mainnet", mainnet(), "deploy", "counter", false, false)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errProbe), "the underlying read error is reported")
	assert.Contains(t, err.Error(), "aborted")
}

var errProbe = errors.New("read boom")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errProbe }
