// Command signer is a minimal external signer for SoroForge.
//
// It implements the stdin/stdout protocol documented under "External signers"
// in the README: one JSON request on stdin, one base64 Ed25519 signature on
// stdout.
//
// A production signer replaces the local keypair below with a call to an HSM
// or KMS, so the private key never enters this process. This example keeps a
// seed in an environment variable purely to show the protocol shape:
//
//	go build -o /tmp/soroforge-example-signer ./examples/signer
//	SOROFORGE_EXAMPLE_SIGNER_SEED=S... /tmp/soroforge-example-signer
package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/stellar/go-stellar-sdk/keypair"
)

type signerRequest struct {
	Hash      string `json:"hash"`
	PublicKey string `json:"public_key"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	seed := os.Getenv("SOROFORGE_EXAMPLE_SIGNER_SEED")
	if seed == "" {
		return fmt.Errorf("SOROFORGE_EXAMPLE_SIGNER_SEED is not set")
	}
	full, err := keypair.ParseFull(seed)
	if err != nil {
		return fmt.Errorf("SOROFORGE_EXAMPLE_SIGNER_SEED is not a valid Stellar secret seed")
	}

	var req signerRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		return fmt.Errorf("decode signer request: %w", err)
	}
	if req.PublicKey != full.Address() {
		return fmt.Errorf("request is for %s but this signer holds %s", req.PublicKey, full.Address())
	}

	hash, err := hex.DecodeString(req.Hash)
	if err != nil {
		return fmt.Errorf("hash is not hexadecimal: %w", err)
	}
	if len(hash) != 32 {
		return fmt.Errorf("hash is %d bytes, expected 32", len(hash))
	}

	signature, err := full.Sign(hash)
	if err != nil {
		return fmt.Errorf("sign hash: %w", err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(signature))
	return nil
}
