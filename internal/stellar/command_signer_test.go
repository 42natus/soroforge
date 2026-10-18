package stellar

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fake signer command is this test binary re-invoked with the helper
// environment set. That keeps the protocol end-to-end — real process, real
// stdin, real stdout — without shipping a script or depending on a shell.
const (
	helperEnabledEnv = "SOROFORGE_TEST_SIGNER_HELPER"
	helperSeedEnv    = "SOROFORGE_TEST_SIGNER_SEED"
	helperModeEnv    = "SOROFORGE_TEST_SIGNER_MODE"
	helperCaptureEnv = "SOROFORGE_TEST_SIGNER_CAPTURE"
)

// TestCommandSignerHelperProcess is the fake signer. It only acts when
// re-invoked by a test; in a normal run it returns immediately.
func TestCommandSignerHelperProcess(t *testing.T) {
	if os.Getenv(helperEnabledEnv) != "1" {
		return
	}

	switch os.Getenv(helperModeEnv) {
	case "timeout":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "exit":
		fmt.Fprintln(os.Stderr, "HSM is unreachable")
		os.Exit(3)
	case "garbage":
		fmt.Fprintln(os.Stdout, "this is not a signature")
		os.Exit(0)
	case "short":
		fmt.Fprintln(os.Stdout, base64.StdEncoding.EncodeToString(make([]byte, 32)))
		os.Exit(0)
	case "empty":
		os.Exit(0)
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if path := os.Getenv(helperCaptureEnv); path != "" {
		if err := os.WriteFile(path, input, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	var req SignerRequest
	if err := json.Unmarshal(input, &req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	hash, err := hex.DecodeString(req.Hash)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	full, err := keypair.ParseFull(os.Getenv(helperSeedEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sig, err := full.Sign(hash)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	fmt.Fprintln(os.Stdout, base64.StdEncoding.EncodeToString(sig))

	// os.Exit rather than returning: the testing package would append "PASS" to
	// stdout, corrupting the protocol the parent is reading.
	os.Exit(0)
}

// helperCommand returns the command line that re-invokes this test binary as
// the fake signer.
func helperCommand(t *testing.T) string {
	t.Helper()
	if strings.ContainsAny(os.Args[0], " ") {
		t.Skip("test binary path contains a space; signer commands are split on whitespace")
	}
	return os.Args[0] + " -test.run=^TestCommandSignerHelperProcess$"
}

// configureHelper makes the fake signer available to a signer under test.
func configureHelper(t *testing.T, mode, seed string) {
	t.Helper()
	t.Setenv(helperEnabledEnv, "1")
	t.Setenv(helperModeEnv, mode)
	t.Setenv(helperSeedEnv, seed)
}

func TestCommandSignerSignsTheTransactionHash(t *testing.T) {
	full := keypair.MustRandom()
	capture := filepath.Join(t.TempDir(), "request.json")
	configureHelper(t, "sign", full.Seed())
	t.Setenv(helperCaptureEnv, capture)

	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: helperCommand(t),
		Address: full.Address(),
	})
	require.NoError(t, err)
	assert.Equal(t, full.Address(), signer.Address())

	tx, passphrase := buildTestTx(t)
	signed, err := signer.Sign(passphrase, tx)
	require.NoError(t, err)

	// The unsigned transaction is untouched; Sign returns a copy.
	assert.Empty(t, tx.Signatures())

	// The command was given the network-specific hash, and the public key it is
	// expected to sign for — never the envelope.
	raw, err := os.ReadFile(capture)
	require.NoError(t, err)
	var req SignerRequest
	require.NoError(t, json.Unmarshal(raw, &req))

	hash, err := tx.Hash(passphrase)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(hash[:]), req.Hash)
	assert.Equal(t, full.Address(), req.PublicKey)
	assert.NotContains(t, string(raw), "envelope")

	// The signature is attached with the right hint and verifies against the
	// configured key.
	require.Len(t, signed.Signatures(), 1)
	sig := signed.Signatures()[0]
	assert.Equal(t, xdr.SignatureHint(full.Hint()), sig.Hint)
	require.NoError(t, full.Verify(hash[:], sig.Signature))
}

func TestCommandSignerRejectsASignatureFromTheWrongKey(t *testing.T) {
	// The command is told which key to use, but a buggy or misconfigured one
	// may sign with another. SoroForge must not attach that signature.
	expected := keypair.MustRandom()
	actual := keypair.MustRandom()
	configureHelper(t, "sign", actual.Seed())

	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: helperCommand(t),
		Address: expected.Address(),
	})
	require.NoError(t, err)

	tx, passphrase := buildTestTx(t)
	_, err = signer.Sign(passphrase, tx)
	assert.ErrorContains(t, err, "does not verify")
	assert.ErrorContains(t, err, expected.Address())
}

func TestCommandSignerReportsANonZeroExit(t *testing.T) {
	full := keypair.MustRandom()
	configureHelper(t, "exit", full.Seed())

	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: helperCommand(t),
		Address: full.Address(),
	})
	require.NoError(t, err)

	tx, passphrase := buildTestTx(t)
	_, err = signer.Sign(passphrase, tx)
	assert.ErrorContains(t, err, "exited with status 3")
	assert.ErrorContains(t, err, "HSM is unreachable")
}

func TestCommandSignerTimesOut(t *testing.T) {
	full := keypair.MustRandom()
	configureHelper(t, "timeout", full.Seed())

	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: helperCommand(t),
		Address: full.Address(),
		Timeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	tx, passphrase := buildTestTx(t)
	_, err = signer.Sign(passphrase, tx)
	assert.ErrorContains(t, err, "timed out after 200ms")
}

func TestCommandSignerRejectsBadOutput(t *testing.T) {
	tests := map[string]struct {
		mode    string
		wantErr string
	}{
		"not base64":   {mode: "garbage", wantErr: "not valid base64"},
		"wrong length": {mode: "short", wantErr: "expected a 64-byte Ed25519 signature"},
		"empty":        {mode: "empty", wantErr: "no signature"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			full := keypair.MustRandom()
			configureHelper(t, tc.mode, full.Seed())

			signer, err := NewCommandSigner(CommandSignerOptions{
				Command: helperCommand(t),
				Address: full.Address(),
			})
			require.NoError(t, err)

			tx, passphrase := buildTestTx(t)
			_, err = signer.Sign(passphrase, tx)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCommandSignerReportsAMissingExecutable(t *testing.T) {
	full := keypair.MustRandom()
	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: "/nonexistent/soroforge-signer",
		Address: full.Address(),
	})
	require.NoError(t, err)

	tx, passphrase := buildTestTx(t)
	_, err = signer.Sign(passphrase, tx)
	assert.ErrorContains(t, err, "run signer command")
}

func TestNewCommandSignerRejectsInvalidOptions(t *testing.T) {
	full := keypair.MustRandom()

	t.Run("empty command", func(t *testing.T) {
		_, err := NewCommandSigner(CommandSignerOptions{Address: full.Address()})
		assert.ErrorContains(t, err, "command is empty")
	})

	t.Run("empty address", func(t *testing.T) {
		_, err := NewCommandSigner(CommandSignerOptions{Command: "/bin/sign"})
		assert.ErrorContains(t, err, "public key is required")
	})

	t.Run("invalid address", func(t *testing.T) {
		_, err := NewCommandSigner(CommandSignerOptions{Command: "/bin/sign", Address: "not-a-key"})
		assert.ErrorContains(t, err, "invalid Stellar address")
	})
}

func TestCommandSignerDefaultsTheTimeout(t *testing.T) {
	full := keypair.MustRandom()
	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: "/bin/sign",
		Address: full.Address(),
	})
	require.NoError(t, err)
	assert.Equal(t, DefaultSignerTimeout, signer.timeout)
}

func TestCommandSignerRejectsMissingInputs(t *testing.T) {
	full := keypair.MustRandom()
	signer, err := NewCommandSigner(CommandSignerOptions{
		Command: "/bin/sign",
		Address: full.Address(),
	})
	require.NoError(t, err)

	_, err = signer.Sign("Test SDF Network ; September 2015", nil)
	assert.ErrorContains(t, err, "nil transaction")

	tx, _ := buildTestTx(t)
	_, err = signer.Sign("", tx)
	assert.ErrorContains(t, err, "empty network passphrase")
}
