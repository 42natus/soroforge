package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "test-token-long-enough-to-pass"

// Rotation tests need two distinct tokens and one that matches neither.
const (
	testTokenA = "test-token-rotation-old-0001"
	testTokenB = "test-token-rotation-new-0002"
	testTokenC = "test-token-rotation-nope-0003"
)

// fakeService records calls and returns canned results, so the handlers are
// tested for what they do with HTTP rather than for lifecycle behaviour — that
// is covered in the deploy package.
type fakeService struct {
	deployResult  *deploy.DeployResult
	upgradeResult *deploy.UpgradeResult
	statusResult  *deploy.StatusResult
	networkStatus *deploy.NetworkStatus
	contracts     []store.Contract
	deployments   []store.Deployment
	err           error

	lastDeploy  deploy.DeployRequest
	lastUpgrade deploy.UpgradeRequest
	lastStatus  deploy.StatusRequest
	lastNetwork string
	lastAlias   string

	// calls counts mutating operations, so tests can prove that a rejected
	// request never reached the service.
	calls int
}

func (f *fakeService) Deploy(_ context.Context, req deploy.DeployRequest) (*deploy.DeployResult, error) {
	f.calls++
	f.lastDeploy = req
	if f.err != nil {
		return nil, f.err
	}
	return f.deployResult, nil
}

func (f *fakeService) Upgrade(_ context.Context, req deploy.UpgradeRequest) (*deploy.UpgradeResult, error) {
	f.calls++
	f.lastUpgrade = req
	if f.err != nil {
		return nil, f.err
	}
	return f.upgradeResult, nil
}

func (f *fakeService) Status(_ context.Context, req deploy.StatusRequest) (*deploy.StatusResult, error) {
	f.lastStatus = req
	if f.err != nil {
		return nil, f.err
	}
	return f.statusResult, nil
}

func (f *fakeService) StatusAll(_ context.Context, network string) (*deploy.NetworkStatus, error) {
	f.lastNetwork = network
	if f.err != nil {
		return nil, f.err
	}
	return f.networkStatus, nil
}

func (f *fakeService) List(_ context.Context, network string) ([]store.Contract, error) {
	f.lastNetwork = network
	if f.err != nil {
		return nil, f.err
	}
	return f.contracts, nil
}

func (f *fakeService) History(_ context.Context, network, alias string) ([]store.Deployment, error) {
	f.lastNetwork = network
	f.lastAlias = alias
	if f.err != nil {
		return nil, f.err
	}
	return f.deployments, nil
}

func newTestRouter(t *testing.T, svc api.Service) http.Handler {
	t.Helper()
	router, err := api.NewRouter(api.Options{
		Service: svc,
		Token:   testToken,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return router
}

func newTestRouterWithTokens(t *testing.T, svc api.Service, tokens ...string) http.Handler {
	t.Helper()
	router, err := api.NewRouter(api.Options{
		Service: svc,
		Tokens:  tokens,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return router
}

func newRateLimitedRouter(t *testing.T, svc api.Service, opts api.RateLimitOptions) http.Handler {
	t.Helper()
	router, err := api.NewRouter(api.Options{
		Service:   svc,
		Token:     testToken,
		RateLimit: opts,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	return router
}

// doFrom issues an authenticated request from a specific client address, so
// per-client behaviour can be observed.
func doFrom(t *testing.T, router http.Handler, remoteAddr, xff, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "Bearer "+testToken)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// do issues a request with the valid bearer token unless token is overridden.
func do(t *testing.T, router http.Handler, method, path string, body any, token *string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	authToken := testToken
	if token != nil {
		authToken = *token
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestServerRefusesToStartWithoutAToken(t *testing.T) {
	// The API can deploy contracts with a live signing key. Starting it
	// unauthenticated is the failure this guards against, so it must be
	// impossible rather than merely discouraged.
	_, err := api.NewRouter(api.Options{Service: &fakeService{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "SOROFORGE_API_TOKEN")
	assert.ErrorContains(t, err, "will not start unauthenticated")
}

func TestServerRefusesShortTokens(t *testing.T) {
	_, err := api.NewRouter(api.Options{Service: &fakeService{}, Token: "short"})
	assert.ErrorContains(t, err, "at least")
}

func TestServerRefusesDuplicateTokens(t *testing.T) {
	// Duplicates are almost certainly a copy-paste slip during rotation, and
	// accepting them would make "remove one token" ambiguous at audit time.
	_, err := api.NewRouter(api.Options{
		Service: &fakeService{},
		Tokens:  []string{testTokenA, testTokenA},
	})
	assert.ErrorContains(t, err, "duplicates")
	// The message must not print the token itself.
	assert.NotContains(t, err.Error(), testTokenA)
}

func TestServerRefusesAWeakTokenInTheList(t *testing.T) {
	// One strong token does not excuse a weak one: every accepted credential
	// guards the same signing key.
	_, err := api.NewRouter(api.Options{
		Service: &fakeService{},
		Tokens:  []string{testTokenA, "short"},
	})
	assert.ErrorContains(t, err, "at least")
}

func TestEveryConfiguredTokenAuthorises(t *testing.T) {
	// Zero-downtime rotation: during the overlap, both the old and the new
	// token must work.
	svc := &fakeService{contracts: []store.Contract{}}
	router := newTestRouterWithTokens(t, svc, testTokenA, testTokenB)

	for _, token := range []string{testTokenA, testTokenB} {
		token := token
		t.Run(token, func(t *testing.T) {
			rec := do(t, router, http.MethodGet, "/v1/contracts", nil, &token)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}

	// A token matching neither is rejected.
	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, ptr(testTokenC))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRotationRejectsRemovedToken(t *testing.T) {
	// The point of rotation: once the old token is removed from config, it
	// stops working immediately.
	svc := &fakeService{contracts: []store.Contract{}}

	beforeRemoval := newTestRouterWithTokens(t, svc, testTokenA, testTokenB)
	assert.Equal(t, http.StatusOK,
		do(t, beforeRemoval, http.MethodGet, "/v1/contracts", nil, ptr(testTokenB)).Code)

	afterRemoval := newTestRouterWithTokens(t, svc, testTokenB)
	assert.Equal(t, http.StatusOK,
		do(t, afterRemoval, http.MethodGet, "/v1/contracts", nil, ptr(testTokenB)).Code)
	assert.Equal(t, http.StatusUnauthorized,
		do(t, afterRemoval, http.MethodGet, "/v1/contracts", nil, ptr(testTokenA)).Code)
}

// ptr returns a pointer to v, for do's optional token argument.
func ptr(v string) *string { return &v }

func TestNewRouterRequiresService(t *testing.T) {
	_, err := api.NewRouter(api.Options{Token: testToken})
	assert.ErrorContains(t, err, "service is required")
}

func TestHealthNeedsNoToken(t *testing.T) {
	// A liveness probe should not require distributing the deploy token to
	// every monitoring system.
	router := newTestRouter(t, &fakeService{})

	empty := ""
	rec := do(t, router, http.MethodGet, "/health", nil, &empty)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ok")
}

func TestMutatingEndpointsRejectBadAuth(t *testing.T) {
	tests := map[string]struct {
		header string
	}{
		"no header":            {header: ""},
		"wrong token":          {header: "Bearer wrong-token-but-long-enough"},
		"not bearer":           {header: "Basic " + testToken},
		"bearer no value":      {header: "Bearer "},
		"token without scheme": {header: testToken},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{}
			router := newTestRouter(t, svc)

			req := httptest.NewRequest(http.MethodPost, "/v1/deploy",
				bytes.NewReader([]byte(`{"alias":"counter"}`)))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, 0, svc.calls, "a rejected request must never reach the service")
			assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
		})
	}
}

func TestReadEndpointsAlsoRequireAuth(t *testing.T) {
	// The deployment history reveals infrastructure detail; it is not public.
	router := newTestRouter(t, &fakeService{})
	empty := ""

	for _, path := range []string{
		"/v1/contracts",
		"/v1/contracts/testnet/counter/history",
		"/v1/contracts/testnet/counter/status",
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, router, http.MethodGet, path, nil, &empty)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	// RFC 7235 makes the auth scheme case-insensitive; clients do send "bearer".
	router := newTestRouter(t, &fakeService{deployResult: &deploy.DeployResult{}})

	req := httptest.NewRequest(http.MethodPost, "/v1/deploy",
		bytes.NewReader([]byte(`{"alias":"counter"}`)))
	req.Header.Set("Authorization", "bearer "+testToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

func TestDeployReturnsCreated(t *testing.T) {
	svc := &fakeService{deployResult: &deploy.DeployResult{
		Alias:      "counter",
		Network:    "testnet",
		ContractID: "CCOUNTER",
		WasmHash:   "abc123",
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter", "network": "testnet", "notes": "release 1"}, nil)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "counter", svc.lastDeploy.Alias)
	assert.Equal(t, "testnet", svc.lastDeploy.Network)
	assert.Equal(t, "release 1", svc.lastDeploy.Notes)

	var body deploy.DeployResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "CCOUNTER", body.ContractID)
}

func TestDeployDryRunReturnsOKNotCreated(t *testing.T) {
	// Nothing was created, so 201 would be a lie.
	svc := &fakeService{deployResult: &deploy.DeployResult{DryRun: true}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter", "dry_run": true}, nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, svc.lastDeploy.DryRun)
}

func TestDeployRequiresAlias(t *testing.T) {
	svc := &fakeService{}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy", map[string]any{}, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, svc.calls)
}

func TestDeployRejectsUnknownFields(t *testing.T) {
	// A misspelled "dry_run" that silently deploys for real is exactly the
	// mistake worth failing loudly on.
	svc := &fakeService{}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter", "dryrun": true}, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "dryrun")
	assert.Equal(t, 0, svc.calls)
}

func TestDeployRejectsMalformedJSON(t *testing.T) {
	router := newTestRouter(t, &fakeService{})

	req := httptest.NewRequest(http.MethodPost, "/v1/deploy",
		bytes.NewReader([]byte(`{"alias":`)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpgradeSucceeds(t *testing.T) {
	svc := &fakeService{upgradeResult: &deploy.UpgradeResult{
		Alias:            "counter",
		ContractID:       "CCOUNTER",
		PreviousWasmHash: "old",
		WasmHash:         "new",
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/upgrade",
		map[string]any{"alias": "counter"}, nil)

	require.Equal(t, http.StatusOK, rec.Code)

	var body deploy.UpgradeResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "old", body.PreviousWasmHash)
	assert.Equal(t, "new", body.WasmHash)
}

func TestErrorsMapToStatusCodes(t *testing.T) {
	tests := map[string]struct {
		err        error
		wantStatus int
	}{
		"unknown contract is a client error": {
			err:        errors.New(`unknown contract "ghost" (configured: counter)`),
			wantStatus: http.StatusBadRequest,
		},
		"unknown network is a client error": {
			err:        errors.New(`unknown network "staging" (configured: testnet)`),
			wantStatus: http.StatusBadRequest,
		},
		"untracked contract is a client error": {
			err:        errors.New("contract counter is not tracked on testnet"),
			wantStatus: http.StatusBadRequest,
		},
		"missing signer is a client error": {
			err:        errors.New("no signing key configured: set SOROFORGE_SECRET_KEY"),
			wantStatus: http.StatusBadRequest,
		},
		"not found maps to 404": {
			err:        fmt.Errorf("contract counter: %w", store.ErrNotFound),
			wantStatus: http.StatusNotFound,
		},
		"network failure is a server error": {
			err:        errors.New("simulateTransaction: connection refused"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			router := newTestRouter(t, &fakeService{err: tc.err})

			rec := do(t, router, http.MethodPost, "/v1/deploy",
				map[string]any{"alias": "counter"}, nil)

			assert.Equal(t, tc.wantStatus, rec.Code)

			// Every failure returns the same body shape.
			var body map[string]string
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.NotEmpty(t, body["error"])
		})
	}
}

func TestListContractsFiltersByNetwork(t *testing.T) {
	svc := &fakeService{contracts: []store.Contract{
		{Alias: "counter", Network: "testnet", ContractID: "C1", CurrentWasmHash: "a"},
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts?network=testnet", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "testnet", svc.lastNetwork)
	assert.Contains(t, rec.Body.String(), "counter")
}

func TestListContractsWithNoNetworkListsAll(t *testing.T) {
	svc := &fakeService{contracts: []store.Contract{}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, svc.lastNetwork)
	// An empty result must serialise as [], not null, so clients can iterate.
	assert.Contains(t, rec.Body.String(), `"contracts":[]`)
}

func TestHistoryUsesPathParameters(t *testing.T) {
	svc := &fakeService{deployments: []store.Deployment{
		{Alias: "counter", Network: "testnet", WasmHash: "v2", Action: store.ActionUpgrade},
		{Alias: "counter", Network: "testnet", WasmHash: "v1", Action: store.ActionDeploy},
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts/testnet/counter/history", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "testnet", svc.lastNetwork)
	assert.Equal(t, "counter", svc.lastAlias)

	var body struct {
		Alias       string             `json:"alias"`
		Network     string             `json:"network"`
		Deployments []store.Deployment `json:"deployments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "counter", body.Alias)
	require.Len(t, body.Deployments, 2)
	assert.Equal(t, store.ActionUpgrade, body.Deployments[0].Action)
}

func TestStatusReportsDriftAsASuccessfulCheck(t *testing.T) {
	// The check ran and produced an answer. Returning 5xx would make a working
	// drift detector look like a broken server to every monitoring tool.
	svc := &fakeService{statusResult: &deploy.StatusResult{
		Alias:            "counter",
		Network:          "testnet",
		State:            deploy.StateDrift,
		ExpectedWasmHash: "recorded",
		ActualWasmHash:   "live",
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts/testnet/counter/status", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)

	var body deploy.StatusResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, deploy.StateDrift, body.State)
	assert.False(t, body.InSync())
	assert.Equal(t, "counter", svc.lastStatus.Alias)
	assert.Equal(t, "testnet", svc.lastStatus.Network)
}

func TestResponsesAreJSON(t *testing.T) {
	router := newTestRouter(t, &fakeService{contracts: []store.Contract{}})

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestStatusAllReportsDriftAsASuccessfulCheck(t *testing.T) {
	svc := &fakeService{networkStatus: &deploy.NetworkStatus{
		Network: "testnet",
		InSync:  false,
		Contracts: []deploy.StatusResult{
			{Alias: "counter", Network: "testnet", State: deploy.StateInSync},
			{Alias: "token", Network: "testnet", State: deploy.StateDrift},
		},
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts/testnet/status", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body deploy.NetworkStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.InSync)
	assert.Len(t, body.Contracts, 2)
	assert.Equal(t, "testnet", svc.lastNetwork)
}

func TestStatusAllRequiresAuth(t *testing.T) {
	router := newTestRouter(t, &fakeService{networkStatus: &deploy.NetworkStatus{}})
	req := httptest.NewRequest(http.MethodGet, "/v1/contracts/testnet/status", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRateLimitTriggersAndResets(t *testing.T) {
	// 5 requests/second with a burst of 1: the second immediate request is
	// refused, and the bucket refills within ~200ms.
	router := newRateLimitedRouter(t, &fakeService{contracts: []store.Contract{}},
		api.RateLimitOptions{RequestsPerSecond: 5, Burst: 1})

	first := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusOK, first.Code)

	second := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusTooManyRequests, second.Code)

	// 429 with a Retry-After telling the client how long to wait.
	retryAfter := second.Header().Get("Retry-After")
	require.NotEmpty(t, retryAfter)
	seconds, err := strconv.Atoi(retryAfter)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, seconds, 1)

	// Every failure returns the same JSON shape as the rest of the API.
	var body map[string]string
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &body))
	assert.NotEmpty(t, body["error"])
	assert.Contains(t, second.Header().Get("Content-Type"), "application/json")

	// The limit resets: one token refills in 200ms.
	time.Sleep(250 * time.Millisecond)

	third := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	assert.Equal(t, http.StatusOK, third.Code)
}

func TestHealthIsNeverRateLimited(t *testing.T) {
	router := newRateLimitedRouter(t, &fakeService{},
		api.RateLimitOptions{RequestsPerSecond: 1, Burst: 1})

	// Exhaust the /v1 budget for this client.
	require.Equal(t, http.StatusOK,
		do(t, router, http.MethodGet, "/v1/contracts", nil, nil).Code)
	require.Equal(t, http.StatusTooManyRequests,
		do(t, router, http.MethodGet, "/v1/contracts", nil, nil).Code)

	// /health is a liveness probe: a caller over its limit must still be able
	// to learn the server is alive.
	for i := 0; i < 5; i++ {
		assert.Equal(t, http.StatusOK,
			do(t, router, http.MethodGet, "/health", nil, nil).Code)
	}
}

func TestRateLimitIsPerClient(t *testing.T) {
	router := newRateLimitedRouter(t, &fakeService{contracts: []store.Contract{}},
		api.RateLimitOptions{RequestsPerSecond: 1, Burst: 1})

	assert.Equal(t, http.StatusOK,
		doFrom(t, router, "198.51.100.7:1234", "", "/v1/contracts").Code)

	// Same address, different source port: still the same client.
	assert.Equal(t, http.StatusTooManyRequests,
		doFrom(t, router, "198.51.100.7:5678", "", "/v1/contracts").Code)

	// A different address has its own budget.
	assert.Equal(t, http.StatusOK,
		doFrom(t, router, "198.51.100.8:1234", "", "/v1/contracts").Code)
}

func TestRateLimitReadsForwardedForOnlyWhenTrusted(t *testing.T) {
	t.Run("untrusted header is ignored", func(t *testing.T) {
		// Without the opt-in, a client cannot mint itself a fresh bucket by
		// forging X-Forwarded-For.
		router := newRateLimitedRouter(t, &fakeService{contracts: []store.Contract{}},
			api.RateLimitOptions{RequestsPerSecond: 1, Burst: 1})

		assert.Equal(t, http.StatusOK,
			doFrom(t, router, "192.0.2.10:4321", "203.0.113.1", "/v1/contracts").Code)
		assert.Equal(t, http.StatusTooManyRequests,
			doFrom(t, router, "192.0.2.10:4321", "203.0.113.2", "/v1/contracts").Code)
	})

	t.Run("trusted header separates clients behind a proxy", func(t *testing.T) {
		router := newRateLimitedRouter(t, &fakeService{contracts: []store.Contract{}},
			api.RateLimitOptions{RequestsPerSecond: 1, Burst: 1, TrustProxyHeaders: true})

		assert.Equal(t, http.StatusOK,
			doFrom(t, router, "192.0.2.10:4321", "203.0.113.1", "/v1/contracts").Code)
		assert.Equal(t, http.StatusOK,
			doFrom(t, router, "192.0.2.10:4321", "203.0.113.2", "/v1/contracts").Code)
		assert.Equal(t, http.StatusTooManyRequests,
			doFrom(t, router, "192.0.2.10:4321", "203.0.113.1", "/v1/contracts").Code)
	})
}

func TestRateLimitDisabledByDefaultAtTheRouterLayer(t *testing.T) {
	// The zero value keeps NewRouter's previous behaviour; serve opts in from
	// configuration, where the documented defaults live.
	router := newTestRouter(t, &fakeService{contracts: []store.Contract{}})

	for i := 0; i < 30; i++ {
		require.Equal(t, http.StatusOK, do(t, router, http.MethodGet, "/v1/contracts", nil, nil).Code)
	}
}
