package stellar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// DefaultSignerTimeout bounds one external signing invocation.
//
// A signer command talks to a local device or a remote KMS, both of which can
// hang; without a deadline a deploy would wait forever on them.
const DefaultSignerTimeout = 30 * time.Second

// SignerRequest is the document written to a signer command's stdin, as one
// line of JSON followed by a newline:
//
//	{"hash":"<64 lowercase hex chars>","public_key":"G..."}
//
// Hash is the network-specific transaction hash, never the envelope. PublicKey
// is the address the command is expected to sign for, so a command serving
// several keys can select the right one.
type SignerRequest struct {
	Hash      string `json:"hash"`
	PublicKey string `json:"public_key"`
}

// CommandSignerOptions configures a CommandSigner.
type CommandSignerOptions struct {
	// Command is an executable followed by optional arguments, split on
	// whitespace. It is run directly, never through a shell, so shell syntax
	// and quoting are not interpreted.
	Command string

	// Address is the G... public key the command signs for. It cannot be
	// derived without the private key, so it is configured explicitly rather
	// than read back from the command.
	Address string

	// Timeout bounds one signing invocation. Defaults to DefaultSignerTimeout.
	Timeout time.Duration
}

// CommandSigner delegates signing to an external executable, for keys that
// cannot be read into this process — an HSM, a cloud KMS, or a remote signing
// service.
//
// SoroForge never sees the private key. It sends the transaction hash to the
// command and receives a signature, which it verifies against the configured
// public key before attaching it: a command that signs with the wrong key, or
// that corrupts its output, fails the deploy rather than producing an invalid
// transaction.
type CommandSigner struct {
	command string
	args    []string
	from    *keypair.FromAddress
	timeout time.Duration
}

var _ Signer = (*CommandSigner)(nil)

// NewCommandSigner builds a signer backed by an external command.
func NewCommandSigner(opts CommandSignerOptions) (*CommandSigner, error) {
	fields := strings.Fields(opts.Command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("signer command is empty")
	}
	if strings.TrimSpace(opts.Address) == "" {
		return nil, fmt.Errorf("signer public key is required, since it cannot be derived without the private key")
	}
	from, err := keypair.ParseAddress(opts.Address)
	if err != nil {
		return nil, fmt.Errorf("parse signer public key: invalid Stellar address (expected a G... value)")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultSignerTimeout
	}

	return &CommandSigner{
		command: fields[0],
		args:    fields[1:],
		from:    from,
		timeout: timeout,
	}, nil
}

// Address implement Signer. It is the configured public key, never read from
// the command's output.
func (s *CommandSigner) Address() string { return s.from.Address() }

// Sign asks the external command to sign the transaction hash, verifies the
// signature, and returns a copy of tx carrying it.
func (s *CommandSigner) Sign(networkPassphrase string, tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
	if tx == nil {
		return nil, fmt.Errorf("sign: nil transaction")
	}
	if networkPassphrase == "" {
		return nil, fmt.Errorf("sign: empty network passphrase")
	}

	hash, err := tx.Hash(networkPassphrase)
	if err != nil {
		return nil, fmt.Errorf("compute transaction hash: %w", err)
	}

	signature, err := s.signHash(hash)
	if err != nil {
		return nil, err
	}

	if err := s.from.Verify(hash[:], signature); err != nil {
		return nil, fmt.Errorf(
			"signer command returned a signature that does not verify for %s; is it signing with the configured key?",
			s.from.Address())
	}

	signed, err := tx.AddSignatureDecorated(xdr.DecoratedSignature{
		Hint:      xdr.SignatureHint(s.from.Hint()),
		Signature: xdr.Signature(signature),
	})
	if err != nil {
		return nil, fmt.Errorf("attach signature: %w", err)
	}
	return signed, nil
}

// signHash runs the command and decodes the signature it returns.
func (s *CommandSigner) signHash(hash [32]byte) ([]byte, error) {
	request, err := json.Marshal(SignerRequest{
		Hash:      hex.EncodeToString(hash[:]),
		PublicKey: s.from.Address(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode signer request: %w", err)
	}
	request = append(request, '\n')

	// Sign has no context parameter, so the timeout is the only backstop
	// against a hung signer.
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.command, s.args...)
	cmd.Stdin = bytes.NewReader(request)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("signer command %q timed out after %s", s.command, s.timeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail := oneLine(stderr.String())
			if detail == "" {
				detail = "no error output"
			}
			return nil, fmt.Errorf(
				"signer command %q exited with status %d: %s",
				s.command, exitErr.ExitCode(), detail)
		}
		return nil, fmt.Errorf("run signer command %q: %w", s.command, err)
	}

	return decodeSignature(stdout.Bytes())
}

// decodeSignature parses the command's stdout: the raw 64-byte Ed25519
// signature, base64-encoded, with surrounding whitespace ignored.
func decodeSignature(out []byte) ([]byte, error) {
	encoded := strings.TrimSpace(string(out))
	if encoded == "" {
		return nil, fmt.Errorf("signer command wrote no signature to stdout")
	}

	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Unpadded base64 is a common shell-tool output; accept it too.
		signature, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("signer command output is not valid base64: %w", err)
		}
	}

	if len(signature) != 64 {
		return nil, fmt.Errorf(
			"signer command returned a %d-byte signature, expected a 64-byte Ed25519 signature",
			len(signature))
	}
	return signature, nil
}

// oneLine collapses a command's stderr into a bounded single-line detail, so a
// failure stays readable inside SoroForge's error.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
