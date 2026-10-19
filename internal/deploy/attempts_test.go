package deploy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #81: a failed deploy or upgrade must leave a trace beyond a log line.
// These tests drive the real submission cycle against the mocked RPC and
// assert the shape of the recorded attempt — action, stage, hash, error text —
// and, every time, that the deployments table stayed clean: failures never
// appear as deployments.

func TestDeployIncludedThenFailedRecordsConfirmAttempt(t *testing.T) {
	// The canonical case from the issue: a transaction that was included in a
	// ledger and then failed. It was submitted and confirmed-as-failed, so the
	// stage is "confirm" and the sent hash is kept.
	h := newHarness(t, harnessOptions{})
	h.client.AwaitResponse = stellar.TxResult{
		Status: stellar.TxStatusFailed,
		Hash:   "abc",
		Ledger: 42,
	}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1, "the failed deploy leaves exactly one trace")

	a := attempts[0]
	assert.Equal(t, store.ActionDeploy, a.Action)
	assert.Equal(t, store.AttemptConfirm, a.Stage)
	assert.Equal(t, h.client.SendHash, a.TxHash)
	assert.Contains(t, a.Error, "included in ledger")

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history, "a failure never appears as a deployment")
}

func TestDeployRejectedSubmissionRecordsSubmitAttempt(t *testing.T) {
	// The network rejected the send itself: nothing was ever included, so the
	// stage is "submit" and there is no transaction hash to keep.
	h := newHarness(t, harnessOptions{})
	h.client.SendFunc = func(context.Context, string) (stellar.SendResult, error) {
		return stellar.SendResult{Status: stellar.SendStatusError, ErrorResultXDR: "AAAAA"}, nil
	}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, store.AttemptSubmit, attempts[0].Stage)
	assert.Empty(t, attempts[0].TxHash)
	assert.Contains(t, attempts[0].Error, "rejected by the network")
}

func TestDeploySimulationFailureRecordsSimulateAttempt(t *testing.T) {
	// A pre-flight stop never left the machine: stage "simulate", no hash.
	h := newHarness(t, harnessOptions{})
	h.client.SimulateResponse = stellar.SimulateResult{Error: "HostError: invalid wasm"}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, store.AttemptSimulate, attempts[0].Stage)
	assert.Empty(t, attempts[0].TxHash)
	assert.Contains(t, attempts[0].Error, "simulation failed")
}

func TestDeployWithoutSignerRecordsPreflightAttempt(t *testing.T) {
	// Config-level stops count too: the Attempt doc defines "simulate" as
	// everything before submission, pre-flight config checks included.
	h := newHarness(t, harnessOptions{noSigner: true})

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, store.AttemptSimulate, attempts[0].Stage)
	assert.Contains(t, attempts[0].Error, "no signing key")
}

func TestDryRunFailureRecordsNoAttempt(t *testing.T) {
	// A dry run submits nothing, so its failure is a build/config problem, not
	// a deployment attempt. Nothing is recorded.
	h := newHarness(t, harnessOptions{})
	h.client.SimulateResponse = stellar.SimulateResult{Error: "HostError: invalid wasm"}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, attempts)
}

func TestSuccessfulDeployRecordsNoAttempt(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, attempts, "success writes history, not attempts")

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Len(t, history, 1, "and successful history is unchanged")
}

func TestUpgradeContractRejectedRecordsConfirmAttempt(t *testing.T) {
	// The other canonical case: the upgrade transaction confirmed, but the
	// contract's entrypoint returned success without changing the code. The
	// ledger refused the upgrade — an auditable failure at the confirm stage.
	h := newHarness(t, harnessOptions{
		wasm:        upgradedWasm,
		upgradeNoop: true,
		verifyDelay: time.Millisecond,
	})
	oldHash := stellar.WasmHash([]byte(testWasm))
	trackContract(t, h, oldHash)

	ctx := context.Background()
	_, err := h.svc.Upgrade(ctx, deploy.UpgradeRequest{Alias: "counter"})
	require.Error(t, err)

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, store.ActionUpgrade, attempts[0].Action)
	assert.Equal(t, store.AttemptConfirm, attempts[0].Stage)
	assert.Equal(t, h.client.SendHash, attempts[0].TxHash)
	assert.Contains(t, attempts[0].Error, "without updating the contract's code")

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)
}

func TestDeployAttemptErrorNeverCarriesKeyMaterial(t *testing.T) {
	// The store's write path redacts secret-seed-shaped text, so an error that
	// quotes a key — an RPC echoing the signing source back in its rejection,
	// say — is still safe to print via `history --failures`.
	h := newHarness(t, harnessOptions{})
	seed := "S" + strings.Repeat("A", 55)
	h.client.SendFunc = func(context.Context, string) (stellar.SendResult, error) {
		return stellar.SendResult{}, fmt.Errorf("node rejected envelope signed by %s", seed)
	}

	ctx := context.Background()
	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter"})
	require.Error(t, err)
	// The operator's own error still names the problem (the seed survives on
	// the live error path; only the stored copy is redacted).
	assert.Contains(t, err.Error(), "rejected envelope")

	attempts, err := h.store.Attempts(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.NotContains(t, attempts[0].Error, seed)
	assert.Contains(t, attempts[0].Error, "[REDACTED-SECRET-KEY]")
}
