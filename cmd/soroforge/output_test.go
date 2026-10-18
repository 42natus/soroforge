package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProgressPrinterNonInteractiveWritesOneLinePerEvent(t *testing.T) {
	// In a pipe or CI log there is nowhere to rewrite a line, so every event
	// must stand on its own — and it goes to the writer given, never stdout.
	var buf bytes.Buffer
	print := progressPrinter(&buf, false)

	print(deploy.ProgressEvent{
		Alias: "counter", Network: "testnet", Step: "upload", Phase: deploy.ProgressSimulating,
	})
	print(deploy.ProgressEvent{
		Alias: "counter", Network: "testnet", Step: "upload", Phase: deploy.ProgressWaiting, Ledger: 1234,
	})
	print(deploy.ProgressEvent{
		Alias: "counter", Network: "testnet", Step: "upload", Phase: deploy.ProgressConfirmed, Ledger: 1234,
	})

	got := buf.String()
	assert.Contains(t, got, "simulating")
	assert.Contains(t, got, "waiting for ledger 1234")
	assert.Contains(t, got, "confirmed in ledger 1234")
	assert.Equal(t, 3, bytes.Count(buf.Bytes(), []byte("\n")), "each event should be its own line")
	assert.NotContains(t, got, "\r\033[K", "escape sequences belong in interactive output only")
}

func TestProgressPrinterInteractiveRewritesOneLine(t *testing.T) {
	var buf bytes.Buffer
	print := progressPrinter(&buf, true)

	print(deploy.ProgressEvent{
		Alias: "counter", Network: "testnet", Step: "create", Phase: deploy.ProgressSimulating,
	})
	print(deploy.ProgressEvent{
		Alias: "counter", Network: "testnet", Step: "create", Phase: deploy.ProgressConfirmed, Ledger: 4321,
	})

	got := buf.String()
	assert.Contains(t, got, "\r\033[K", "a terminal line is rewritten in place")
	assert.True(t, bytes.HasSuffix(buf.Bytes(), []byte("confirmed in ledger 4321\n")),
		"confirmation should end the line, got %q", got)
	assert.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("\n")), "one transaction, one finished line")
}

func TestPrintDeployResultJSONIncludesEstimatedFees(t *testing.T) {
	flags = globalFlags{}
	defer func() { flags = globalFlags{} }()
	flags.jsonOutput = true

	result := &deploy.DeployResult{
		Alias:      "counter",
		Network:    "testnet",
		ContractID: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
		WasmHash:   "00",
		DryRun:     true,
		EstimatedFees: &deploy.DryRunFees{
			Create: &deploy.FeeEstimate{
				InclusionFeeStroops: 100,
				ResourceFeeStroops:  12_345,
				TotalStroops:        12_445,
				TotalXLM:            "0.0012445",
			},
			Total: deploy.FeeEstimate{
				InclusionFeeStroops: 100,
				ResourceFeeStroops:  12_345,
				TotalStroops:        12_445,
				TotalXLM:            "0.0012445",
			},
		},
	}

	out := captureStdout(t, func() error { return printDeployResult(result) })

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	fees, ok := got["estimated_fees"].(map[string]any)
	require.True(t, ok, "JSON must carry an estimated_fees object")

	create, ok := fees["create"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(12_445), create["total_stroops"])
	assert.Equal(t, "0.0012445", create["total_xlm"])
	assert.Equal(t, float64(100), create["inclusion_fee_stroops"])
	assert.Equal(t, float64(12_345), create["resource_fee_stroops"])

	total, ok := fees["total"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(12_445), total["total_stroops"])
	assert.Equal(t, "0.0012445", total["total_xlm"])
}
