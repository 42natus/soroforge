package deploy_test

import (
	"context"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeployReportsProgressThroughTheCallback(t *testing.T) {
	// The CLI renders these events so a deploy does not look hung. The service
	// must emit them through the injected callback — never by printing itself.
	var events []deploy.ProgressEvent
	h := newHarness(t, harnessOptions{progress: func(ev deploy.ProgressEvent) {
		events = append(events, ev)
	}})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)

	require.Len(t, events, 8, "two transactions, four phases each")

	wantPhases := []string{
		deploy.ProgressSimulating, deploy.ProgressSubmitted, deploy.ProgressWaiting, deploy.ProgressConfirmed,
		deploy.ProgressSimulating, deploy.ProgressSubmitted, deploy.ProgressWaiting, deploy.ProgressConfirmed,
	}
	wantSteps := []string{"upload", "upload", "upload", "upload", "create", "create", "create", "create"}
	for i, ev := range events {
		assert.Equal(t, wantPhases[i], ev.Phase)
		assert.Equal(t, wantSteps[i], ev.Step)
		assert.Equal(t, "counter", ev.Alias)
		assert.Equal(t, "testnet", ev.Network)
	}

	// While waiting, the ledger to report is the first one the transaction can
	// land in — not the one already closed when it was accepted.
	assert.Equal(t, h.client.LedgerSeq+1, events[2].Ledger)
	// Confirmation reports the ledger that actually included it.
	assert.Equal(t, h.client.AwaitResponse.Ledger, events[3].Ledger)
}

func TestDeployDryRunReportsOnlySimulationProgress(t *testing.T) {
	// A dry run submits and waits for nothing, so the wait-related phases must
	// not be reported.
	var events []deploy.ProgressEvent
	h := newHarness(t, harnessOptions{progress: func(ev deploy.ProgressEvent) {
		events = append(events, ev)
	}})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	require.Len(t, events, 2)
	for _, ev := range events {
		assert.Equal(t, deploy.ProgressSimulating, ev.Phase)
	}
}

func TestUpgradeReportsProgressThroughTheCallback(t *testing.T) {
	var events []deploy.ProgressEvent
	h := newHarness(t, harnessOptions{
		wasm: upgradedWasm,
		progress: func(ev deploy.ProgressEvent) {
			events = append(events, ev)
		},
	})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))
	// The new bytecode is already on-chain, so only the upgrade invoke runs.
	require.NoError(t, h.client.SetWasmUploaded(stellar.WasmHash([]byte(upgradedWasm))))

	_, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{Alias: "counter"})
	require.NoError(t, err)

	require.Len(t, events, 4, "one transaction, four phases")
	for _, ev := range events {
		assert.Equal(t, "upgrade", ev.Step)
		assert.Equal(t, "counter", ev.Alias)
	}
}
