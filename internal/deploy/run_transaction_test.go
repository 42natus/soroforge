package deploy

// These tests reach into runTransaction directly, which is why this file is an
// internal test: the external tests in deploy_test.go exercise whole operations
// through Deploy and Upgrade, and cannot see where in the submission cycle a
// failure happened. The point of each test here is that a failure at the
// account-load and signing stages stops the cycle before anything reaches the
// network.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalWasm is a plausible upload payload. Nothing here reaches a host, so
// the bytes only need to be non-empty.
const minimalWasm = "\x00asm\x01\x00\x00\x00"

// newTransactionService wires a Service to the given fakes. The zero-value
// config is enough because runTransaction never reads it.
func newTransactionService(t *testing.T, client *stellar.MockClient, signer stellar.Signer) *Service {
	t.Helper()

	svc, err := New(Options{
		Config: &config.Config{},
		Store:  store.NewMemoryStore(),
		Signer: signer,
		Clients: func(string, config.Network) (stellar.Client, error) {
			return client, nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return svc
}

// simulationCarrying returns the simulation result Assemble accepts. The
// resource fee is a fixed, recognisable number so the dry-run test can read it
// back out of the assembled envelope.
func simulationCarrying(t *testing.T) stellar.SimulateResult {
	t.Helper()

	data, err := xdr.MarshalBase64(xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Instructions:  xdr.Uint32(1_000_000),
			DiskReadBytes: xdr.Uint32(500),
			WriteBytes:    xdr.Uint32(1_000),
		},
		ResourceFee: xdr.Int64(12_345),
	})
	require.NoError(t, err)

	return stellar.SimulateResult{
		TransactionDataXDR: data,
		MinResourceFee:     12_345,
		LatestLedger:       1000,
	}
}

func TestRunTransactionStopsWhenTheAccountCannotBeLoaded(t *testing.T) {
	// An unfunded deployer is the most common first-run failure. The RPC client
	// names the account it could not load (see RPCClient.LoadAccount), and that
	// message is what tells the user which account to fund — so the cycle must
	// surface it and stop, without simulating or sending anything.
	client := stellar.NewMockClient()
	signer := stellar.NewMockSigner()
	client.LoadAccountErr = fmt.Errorf("load account %s: account not found", signer.Address())

	svc := newTransactionService(t, client, signer)

	_, err := svc.runTransaction(context.Background(),
		resolved{Client: client},
		stellar.UploadWasmOp([]byte(minimalWasm), signer.Address()),
		false)

	require.Error(t, err)
	assert.ErrorContains(t, err, signer.Address(),
		"the error must name the account that needs funding")
	assert.Zero(t, client.SimulateCount(),
		"nothing should be simulated when the account cannot be loaded")
	assert.Zero(t, client.SendCount(),
		"nothing should be sent when the account cannot be loaded")
	assert.Zero(t, signer.SignCount())
}

func TestRunTransactionStopsWhenSigningFails(t *testing.T) {
	// Signing runs after the transaction has been built, simulated and
	// assembled; a failure there must stop the cycle before Send, because an
	// unsigned transaction the deployer never authorised must not be submitted.
	client := stellar.NewMockClient()
	client.SimulateResponse = simulationCarrying(t)

	signer := stellar.NewMockSigner()
	signErr := errors.New("keystore is locked")
	signer.SignErr = signErr

	svc := newTransactionService(t, client, signer)

	_, err := svc.runTransaction(context.Background(),
		resolved{Client: client},
		stellar.UploadWasmOp([]byte(minimalWasm), signer.Address()),
		false)

	require.Error(t, err)
	assert.ErrorIs(t, err, signErr)
	assert.Equal(t, 1, client.SimulateCount(),
		"the cycle should have reached the signing step")
	assert.Zero(t, client.SendCount(),
		"a signing failure must stop the cycle before Send")
}

func TestRunTransactionDryRunSignsNothingAndSendsNothing(t *testing.T) {
	client := stellar.NewMockClient()
	client.SimulateResponse = simulationCarrying(t)

	signer := stellar.NewMockSigner()
	svc := newTransactionService(t, client, signer)

	result, err := svc.runTransaction(context.Background(),
		resolved{Client: client},
		stellar.UploadWasmOp([]byte(minimalWasm), signer.Address()),
		true)
	require.NoError(t, err)

	assert.Equal(t, 1, client.SimulateCount(),
		"a dry run still simulates, if only to discover the footprint")
	assert.Zero(t, signer.SignCount(), "a dry run must never call the signer")
	assert.Zero(t, client.SendCount(), "a dry run must never send")
	assert.Empty(t, result.Hash, "a dry run produces no transaction hash")
	assert.NotEmpty(t, result.EnvelopeXDR,
		"the assembled envelope is the deliverable of a dry run")

	// The envelope must decode into a real transaction: it is what another
	// tool would inspect or submit.
	var envelope xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.EnvelopeXDR, &envelope))

	require.NotNil(t, envelope.V1)
	assert.Empty(t, envelope.Signatures(), "a dry run stops before signing")
	require.NotNil(t, envelope.V1.Tx.Ext.SorobanData,
		"the envelope must carry the simulated footprint")
	assert.Equal(t, xdr.Int64(12_345), envelope.V1.Tx.Ext.SorobanData.ResourceFee)
}
