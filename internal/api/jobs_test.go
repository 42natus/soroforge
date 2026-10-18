package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type jobBody struct {
	ID         string          `json:"id"`
	State      string          `json:"state"`
	Alias      string          `json:"alias"`
	Network    string          `json:"network"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Error      string          `json:"error"`
	Result     json.RawMessage `json:"result"`
}

func waitForJob(t *testing.T, router http.Handler, id string) jobBody {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := do(t, router, http.MethodGet, "/v1/jobs/"+id, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var job jobBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))

		switch job.State {
		case api.JobSucceeded, api.JobFailed:
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish; last state %q", id, job.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func submitAsync(t *testing.T, router http.Handler, path string, body map[string]any) jobBody {
	t.Helper()

	rec := do(t, router, http.MethodPost, path+"?async=true", body, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var job jobBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))
	require.NotEmpty(t, job.ID)

	assert.Equal(t, "/v1/jobs/"+job.ID, rec.Header().Get("Location"))
	return job
}

func TestAsyncDeployReturnsAcceptedAndCompletesWithTheSyncResult(t *testing.T) {
	svc := &fakeService{deployResult: &deploy.DeployResult{
		Alias:      "counter",
		Network:    "testnet",
		ContractID: "CCOUNTER",
		WasmHash:   "abc123",
	}}
	router := newTestRouter(t, svc)

	submitted := submitAsync(t, router, "/v1/deploy",
		map[string]any{"alias": "counter", "network": "testnet", "notes": "async release"})
	assert.Equal(t, api.JobQueued, submitted.State, "the job must not run before the response is written")

	job := waitForJob(t, router, submitted.ID)
	assert.Equal(t, api.JobSucceeded, job.State)
	assert.Equal(t, "counter", job.Alias)
	assert.Equal(t, "testnet", job.Network)
	require.NotNil(t, job.StartedAt)
	require.NotNil(t, job.FinishedAt)
	assert.Empty(t, job.Error)

	// The result body is exactly what the synchronous endpoint returns.
	var result deploy.DeployResult
	require.NoError(t, json.Unmarshal(job.Result, &result))
	assert.Equal(t, "CCOUNTER", result.ContractID)
	assert.Equal(t, "abc123", result.WasmHash)

	assert.Equal(t, "counter", svc.lastDeploy.Alias)
	assert.Equal(t, "testnet", svc.lastDeploy.Network)
	assert.Equal(t, "async release", svc.lastDeploy.Notes)
	assert.Equal(t, 1, svc.calls)
}

func TestAsyncUpgradeReturnsAcceptedAndCompletes(t *testing.T) {
	svc := &fakeService{upgradeResult: &deploy.UpgradeResult{
		Alias:            "counter",
		ContractID:       "CCOUNTER",
		PreviousWasmHash: "old",
		WasmHash:         "new",
	}}
	router := newTestRouter(t, svc)

	submitted := submitAsync(t, router, "/v1/upgrade", map[string]any{"alias": "counter"})
	job := waitForJob(t, router, submitted.ID)

	assert.Equal(t, api.JobSucceeded, job.State)
	var result deploy.UpgradeResult
	require.NoError(t, json.Unmarshal(job.Result, &result))
	assert.Equal(t, "old", result.PreviousWasmHash)
	assert.Equal(t, "new", result.WasmHash)
	assert.Equal(t, "counter", svc.lastUpgrade.Alias)
}

func TestAsyncJobFailureIsReportedInTheJob(t *testing.T) {
	svc := &fakeService{err: errors.New("simulateTransaction: connection refused")}
	router := newTestRouter(t, svc)

	submitted := submitAsync(t, router, "/v1/deploy", map[string]any{"alias": "counter"})
	job := waitForJob(t, router, submitted.ID)

	assert.Equal(t, api.JobFailed, job.State)
	assert.Contains(t, job.Error, "connection refused")
	assert.Empty(t, job.Result)
}

func TestAsyncJobsForTheSameAliasDoNotOverlap(t *testing.T) {
	// Two simultaneous upgrades of one contract would race on-chain; the second
	// must wait until the first has finished.
	svc := newGatedService()
	router := newTestRouter(t, svc)

	first := submitAsync(t, router, "/v1/upgrade", map[string]any{"alias": "counter"})

	select {
	case <-svc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first job never reached the service")
	}

	second := submitAsync(t, router, "/v1/upgrade", map[string]any{"alias": "counter"})

	// While the first holds the alias, the second stays queued rather than
	// entering the service.
	select {
	case <-svc.started:
		t.Fatal("a second job started while the first still held the alias")
	case <-time.After(100 * time.Millisecond):
	}

	queued := do(t, router, http.MethodGet, "/v1/jobs/"+second.ID, nil, nil)
	require.Equal(t, http.StatusOK, queued.Code)
	var queuedBody jobBody
	require.NoError(t, json.Unmarshal(queued.Body.Bytes(), &queuedBody))
	assert.Equal(t, api.JobQueued, queuedBody.State)

	// Release the first; the second runs next.
	svc.release <- struct{}{}
	select {
	case <-svc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued job never started after the first finished")
	}
	svc.release <- struct{}{}

	assert.Equal(t, api.JobSucceeded, waitForJob(t, router, first.ID).State)
	assert.Equal(t, api.JobSucceeded, waitForJob(t, router, second.ID).State)
	assert.Equal(t, 1, svc.maxConcurrent())
}

func TestSyncRequestsAreUnchangedByAsyncSupport(t *testing.T) {
	svc := &fakeService{deployResult: &deploy.DeployResult{Alias: "counter", Network: "testnet"}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy?async=false",
		map[string]any{"alias": "counter"}, nil)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
	assert.Equal(t, 1, svc.calls)
}

func TestAsyncRejectsAnInvalidValue(t *testing.T) {
	svc := &fakeService{}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy?async=maybe",
		map[string]any{"alias": "counter"}, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid async value")
	assert.Zero(t, svc.calls)
}

func TestUnknownJobIsNotFound(t *testing.T) {
	router := newTestRouter(t, &fakeService{})

	rec := do(t, router, http.MethodGet, "/v1/jobs/does-not-exist", nil, nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

func TestFinishedJobsExpire(t *testing.T) {
	svc := &fakeService{deployResult: &deploy.DeployResult{Alias: "counter"}}
	router, err := api.NewRouter(api.Options{
		Service:      svc,
		Tokens:       testTokens(),
		Readiness:    healthyReadiness{},
		JobRetention: time.Second,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	submitted := submitAsync(t, router, "/v1/deploy", map[string]any{"alias": "counter"})
	require.Equal(t, api.JobSucceeded, waitForJob(t, router, submitted.ID).State)

	time.Sleep(1100 * time.Millisecond)

	rec := do(t, router, http.MethodGet, "/v1/jobs/"+submitted.ID, nil, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// gatedService lets an upgrade block until the test releases it, and records
// how many upgrades ran at once.
type gatedService struct {
	*fakeService

	mu      sync.Mutex
	active  int
	maxSeen int

	started chan struct{}
	release chan struct{}
}

func newGatedService() *gatedService {
	return &gatedService{
		fakeService: &fakeService{upgradeResult: &deploy.UpgradeResult{Alias: "counter", WasmHash: "new"}},
		started:     make(chan struct{}, 2),
		release:     make(chan struct{}, 2),
	}
}

func (g *gatedService) Upgrade(ctx context.Context, req deploy.UpgradeRequest) (*deploy.UpgradeResult, error) {
	g.mu.Lock()
	g.active++
	if g.active > g.maxSeen {
		g.maxSeen = g.active
	}
	g.mu.Unlock()

	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	g.mu.Lock()
	g.active--
	g.mu.Unlock()

	return g.fakeService.Upgrade(ctx, req)
}

func (g *gatedService) maxConcurrent() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxSeen
}

// TestAsyncDeployLifecycleWithMockClient drives a real deploy.Service through
// the API using the same MockClient the deploy package uses, so the job's
// result is a genuine deploy result rather than a canned one.
func TestAsyncDeployLifecycleWithMockClient(t *testing.T) {
	dir := t.TempDir()
	contractID := testContractID(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "counter.wasm"),
		[]byte("\x00asm\x01\x00\x00\x00"), 0o600))
	configPath := filepath.Join(dir, "soroforge.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
`), 0o600))

	cfg, err := config.Load(configPath)
	require.NoError(t, err)

	client := stellar.NewMockClient()
	client.SimulateResponse = simulationReturning(t, contractID)

	svc, err := deploy.New(deploy.Options{
		Config:  cfg,
		Store:   store.NewMemoryStore(),
		Signer:  stellar.NewMockSigner(),
		Clients: func(string, config.Network) (stellar.Client, error) { return client, nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	router, err := api.NewRouter(api.Options{
		Service:   svc,
		Tokens:    testTokens(),
		Readiness: healthyReadiness{},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	submitted := submitAsync(t, router, "/v1/deploy", map[string]any{"alias": "counter"})
	job := waitForJob(t, router, submitted.ID)
	require.Equal(t, api.JobSucceeded, job.State, job.Error)

	var result deploy.DeployResult
	require.NoError(t, json.Unmarshal(job.Result, &result))
	assert.Equal(t, contractID, result.ContractID)
	assert.False(t, result.DryRun)
	assert.NotEmpty(t, result.CreateTxHash)

	// The real deploy went through both transactions and was recorded.
	assert.Equal(t, 2, client.SendCount())
	tracked, err := svc.List(context.Background(), "testnet")
	require.NoError(t, err)
	require.Len(t, tracked, 1)
	assert.Equal(t, contractID, tracked[0].ContractID)
}

func testContractID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	id, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)
	return id
}

func simulationReturning(t *testing.T, contractID string) stellar.SimulateResult {
	t.Helper()

	addr, err := stellar.ContractAddress(contractID)
	require.NoError(t, err)
	returnValue, err := xdr.MarshalBase64(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &addr,
	})
	require.NoError(t, err)

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
		Results:            []stellar.HostFunctionResult{{ReturnValueXDR: returnValue}},
		LatestLedger:       1000,
	}
}
