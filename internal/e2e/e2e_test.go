//go:build e2e

// Package e2e runs SoroForge's real deploy and upgrade flow against a
// standalone Stellar network (the stellar/quickstart image in local mode).
//
// Every other test runs against MockClient, and live_test.go only reads. This
// is the one place a full deploy -> status -> upgrade -> drift cycle is proven
// against a real node, so a change in how the RPC or the host behaves shows up
// here instead of in someone's first production deploy.
//
// It is behind the e2e build tag so `make check` stays offline:
//
//	make test-e2e
//
// See README.md in this directory for the pinned image and the contract.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// quickstart's standalone network passphrase.
	passphrase = "Standalone Network ; February 2017"

	defaultRPCURL       = "http://localhost:8000/rpc"
	defaultFriendbotURL = "http://localhost:8000/friendbot"

	alias   = "upgradeable"
	network = "local"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// fund asks Friendbot for a funded account, retrying while the node finishes
// starting: quickstart reports its container up well before Friendbot works.
func fund(t *testing.T, ctx context.Context, friendbot, address string) {
	t.Helper()

	var lastErr error
	for ctx.Err() == nil {
		resp, err := http.Get(friendbot + "?addr=" + url.QueryEscape(address))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("friendbot status %d: %.200s", resp.StatusCode, body)
		} else {
			lastErr = err
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("could not fund %s: %v", address, lastErr)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, data, 0o600))
}

func TestDeployUpgradeAndDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	rpcURL := envOr("E2E_RPC_URL", defaultRPCURL)
	friendbot := envOr("E2E_FRIENDBOT_URL", defaultFriendbotURL)

	deployer := keypair.MustRandom()
	fund(t, ctx, friendbot, deployer.Address())
	signer, err := stellar.NewKeypairSigner(deployer.Seed())
	require.NoError(t, err)

	// The project directory holds one config and one artifact path. Swapping
	// the file behind that path between steps is how an operator ships a new
	// build, and is what Upgrade reads.
	dir := t.TempDir()
	artifact := filepath.Join(dir, "contract.wasm")
	configPath := filepath.Join(dir, "soroforge.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(`version: 1
default_network: %s
networks:
  %s:
    rpc_url: %s
    passphrase: %q
contracts:
  %s:
    wasm: ./contract.wasm
`, network, network, rpcURL, passphrase, alias)), 0o600))
	cfg, err := config.Load(configPath)
	require.NoError(t, err)

	memStore := store.NewMemoryStore()
	svc, err := deploy.New(deploy.Options{
		Config: cfg,
		Store:  memStore,
		Signer: signer,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	v1 := filepath.Join("testdata", "upgradeable", "v1.wasm")
	v2 := filepath.Join("testdata", "upgradeable", "v2.wasm")

	// Deploy v1.
	copyFile(t, v1, artifact)
	deployed, err := svc.Deploy(ctx, deploy.DeployRequest{Alias: alias, Network: network})
	require.NoError(t, err)
	require.NotEmpty(t, deployed.ContractID)
	v1Hash := deployed.WasmHash

	status, err := svc.Status(ctx, deploy.StatusRequest{Alias: alias, Network: network})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateInSync, status.State, status.Detail)

	// Upgrade to v2 through SoroForge.
	copyFile(t, v2, artifact)
	upgraded, err := svc.Upgrade(ctx, deploy.UpgradeRequest{Alias: alias, Network: network})
	require.NoError(t, err)
	require.False(t, upgraded.NoChange)
	assert.Equal(t, v1Hash, upgraded.PreviousWasmHash)
	assert.NotEqual(t, v1Hash, upgraded.WasmHash)

	status, err = svc.Status(ctx, deploy.StatusRequest{Alias: alias, Network: network})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateInSync, status.State, status.Detail)
	assert.Equal(t, upgraded.WasmHash, status.ActualWasmHash)

	history, err := svc.History(ctx, network, alias)
	require.NoError(t, err)
	require.Len(t, history, 2, "history has both the deploy and the upgrade")
	actions := []store.Action{history[0].Action, history[1].Action}
	assert.ElementsMatch(t, []store.Action{store.ActionDeploy, store.ActionUpgrade}, actions)

	// Upgrade outside SoroForge, back to the v1 bytecode that is already on
	// the ledger. SoroForge's record still says v2, so Status must report drift.
	client, err := stellar.NewRPCClient(stellar.RPCOptions{URL: rpcURL, NetworkPassphrase: passphrase})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	v1Raw, err := stellar.ParseHashHex(v1Hash)
	require.NoError(t, err)
	upgradeOutsideSoroForge(t, ctx, client, signer, deployed.ContractID, v1Raw)

	status, err = svc.Status(ctx, deploy.StatusRequest{Alias: alias, Network: network})
	require.NoError(t, err)
	assert.Equal(t, deploy.StateDrift, status.State, status.Detail)
	assert.Equal(t, upgraded.WasmHash, status.ExpectedWasmHash)
	assert.Equal(t, v1Hash, status.ActualWasmHash)
}

// upgradeOutsideSoroForge invokes the contract's upgrade entrypoint directly,
// the way an operator using another tool would, bypassing SoroForge's history.
func upgradeOutsideSoroForge(
	t *testing.T,
	ctx context.Context,
	client *stellar.RPCClient,
	signer stellar.Signer,
	contractID string,
	hash xdr.Hash,
) {
	t.Helper()

	op, err := stellar.InvokeOp(contractID, "upgrade", []xdr.ScVal{stellar.UpgradeArg(hash)}, signer.Address())
	require.NoError(t, err)
	source, err := client.LoadAccount(ctx, signer.Address())
	require.NoError(t, err)
	tx, err := stellar.Build(stellar.BuildParams{Source: source, Operation: op, BaseFee: 100})
	require.NoError(t, err)
	unsigned, err := tx.Base64()
	require.NoError(t, err)
	sim, err := client.Simulate(ctx, unsigned)
	require.NoError(t, err)
	assembled, err := stellar.Assemble(tx, sim)
	require.NoError(t, err)
	signed, err := signer.Sign(client.NetworkPassphrase(), assembled)
	require.NoError(t, err)
	signedXDR, err := signed.Base64()
	require.NoError(t, err)

	sent, err := client.Send(ctx, signedXDR)
	require.NoError(t, err)
	_, err = client.AwaitTransaction(ctx, sent.Hash)
	require.NoError(t, err)
}
