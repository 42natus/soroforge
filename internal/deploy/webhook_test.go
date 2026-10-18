package deploy_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeployNotifiesWebhookWithSignature(t *testing.T) {
	var (
		mu       sync.Mutex
		received []deploy.WebhookPayload
		rawBody  []byte
		gotSig   string
	)

	secret := "my-shared-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		gotSig = r.Header.Get("X-SoroForge-Signature")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rawBody = body

		var payload deploy.WebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - url: %s
        secret: %s
contracts:
  counter:
    wasm: ./counter.wasm
`, srv.URL, secret)

	h := newHarness(t, harnessOptions{configBody: cfg})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias: "counter",
		Notes: "initial release",
	})
	require.NoError(t, err)

	require.Len(t, result.Webhooks, 1)
	assert.True(t, result.Webhooks[0].OK)
	assert.Equal(t, srv.URL, result.Webhooks[0].URL)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, received, 1)
	p := received[0]
	assert.Equal(t, "counter", p.Alias)
	assert.Equal(t, "testnet", p.Network)
	assert.Equal(t, h.contractID, p.ContractID)
	assert.Equal(t, "deploy", string(p.Action))
	assert.Equal(t, "initial release", p.Notes)

	// Verify HMAC-SHA256 signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expectedSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, expectedSig, gotSig)
}

func TestUpgradeNotifiesWebhook(t *testing.T) {
	var (
		mu       sync.Mutex
		received []deploy.WebhookPayload
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		var payload deploy.WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received = append(received, payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - %s
contracts:
  counter:
    wasm: ./counter.wasm
`, srv.URL)

	h := newHarness(t, harnessOptions{configBody: cfg, wasm: testWasm + "modified"})
	trackContract(t, h, stellar.WasmHash([]byte(testWasm)))

	result, err := h.svc.Upgrade(context.Background(), deploy.UpgradeRequest{
		Alias: "counter",
		Notes: "security patch",
	})
	require.NoError(t, err)

	require.Len(t, result.Webhooks, 1)
	assert.True(t, result.Webhooks[0].OK)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, received, 1)
	assert.Equal(t, "counter", received[0].Alias)
	assert.Equal(t, "testnet", received[0].Network)
	assert.Equal(t, "upgrade", string(received[0].Action))
	assert.Equal(t, "security patch", received[0].Notes)
}

func TestDeployWebhookFailureDoesNotFailTheDeploy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal service unavailable", http.StatusBadGateway)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - %s
contracts:
  counter:
    wasm: ./counter.wasm
`, srv.URL)

	h := newHarness(t, harnessOptions{configBody: cfg})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err, "a failing webhook must not fail the deploy")

	require.Len(t, result.Webhooks, 1)
	assert.False(t, result.Webhooks[0].OK)
	assert.Contains(t, result.Webhooks[0].Error, "502")

	// Deploy is still tracked in the store
	tracked, err := h.store.GetContract(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, h.contractID, tracked.ContractID)
}

func TestDeployDryRunDoesNotNotifyWebhook(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - %s
contracts:
  counter:
    wasm: ./counter.wasm
`, srv.URL)

	h := newHarness(t, harnessOptions{configBody: cfg})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:  "counter",
		DryRun: true,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Webhooks)
	assert.False(t, called)
}

func TestWebhookSecretNeverLeaksIntoResultsOrLogs(t *testing.T) {
	// Security: a receiver failure must report the problem without ever
	// surfacing the shared secret in statuses, JSON results, or logs.
	var logs bytes.Buffer

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	const secret = "super-sensitive-shared-secret"
	cfg := fmt.Sprintf(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
    webhooks:
      - url: %s
        secret: %s
contracts:
  counter:
    wasm: ./counter.wasm
`, srv.URL, secret)

	h := newHarness(t, harnessOptions{configBody: cfg, logOut: &logs})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err, "a failing webhook still must not fail the deploy")
	require.Len(t, result.Webhooks, 1)
	assert.False(t, result.Webhooks[0].OK)

	encoded, err := json.Marshal(result)
	require.NoError(t, err)

	for _, leak := range []string{string(encoded), logs.String()} {
		assert.NotContains(t, leak, secret, "the shared secret must never be reported")
	}
}
