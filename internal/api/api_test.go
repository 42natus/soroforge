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
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/readiness"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testToken         = "test-token-long-enough-to-pass"
	testReadOnlyToken = "read-only-token-long-enough"
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
	lastLimit   int
	lastOffset  int

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

func (f *fakeService) ListPage(_ context.Context, network string, limit, offset int) ([]store.Contract, error) {
	f.lastNetwork = network
	f.lastLimit = limit
	f.lastOffset = offset
	if f.err != nil {
		return nil, f.err
	}
	if offset >= len(f.contracts) {
		return []store.Contract{}, nil
	}
	end := offset + limit
	if end > len(f.contracts) {
		end = len(f.contracts)
	}
	return f.contracts[offset:end], nil
}

func (f *fakeService) History(_ context.Context, network, alias string) ([]store.Deployment, error) {
	f.lastNetwork = network
	f.lastAlias = alias
	if f.err != nil {
		return nil, f.err
	}
	return f.deployments, nil
}

func (f *fakeService) GetDeployment(_ context.Context, id int64) (store.Deployment, error) {
	if f.err != nil {
		return store.Deployment{}, f.err
	}
	for _, d := range f.deployments {
		if d.ID == id {
			return d, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
}

func (f *fakeService) HistoryPage(_ context.Context, network, alias string, limit, offset int) ([]store.Deployment, error) {
	f.lastNetwork = network
	f.lastAlias = alias
	f.lastLimit = limit
	f.lastOffset = offset
	if f.err != nil {
		return nil, f.err
	}
	if offset >= len(f.deployments) {
		return []store.Deployment{}, nil
	}
	end := offset + limit
	if end > len(f.deployments) {
		end = len(f.deployments)
	}
	return f.deployments[offset:end], nil
}

// testTokens matches what the server builds from SOROFORGE_API_TOKEN plus a
// scoped read-only entry: testToken is read-write, testReadOnlyToken is not.
func testTokens() []api.Token {
	return []api.Token{
		api.NewToken("ci", testToken, false),
		api.NewToken("dashboard", testReadOnlyToken, true),
	}
}

// healthyReadiness is a Readiness that reports every dependency OK.
type healthyReadiness struct{}

func (healthyReadiness) Check(context.Context) readiness.Status {
	return readiness.Status{
		Ready:        true,
		Dependencies: []readiness.Dependency{{Name: readiness.DatabaseName, OK: true}},
	}
}

// fakeReadiness returns a canned verdict, for testing how the handler renders
// the not-ready case.
type fakeReadiness struct {
	status readiness.Status
}

func (f *fakeReadiness) Check(context.Context) readiness.Status { return f.status }

func newRouter(t *testing.T, svc api.Service, rd api.Readiness, logger *slog.Logger) http.Handler {
	t.Helper()
	router, err := api.NewRouter(api.Options{
		Service:   svc,
		Tokens:    testTokens(),
		Readiness: rd,
		Log:       logger,
	})
	require.NoError(t, err)
	return router
}

func newTestRouter(t *testing.T, svc api.Service) http.Handler {
	t.Helper()
	return newRouter(t, svc, healthyReadiness{},
		slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// newTestRouterWith captures the access log in w.
func newTestRouterWith(t *testing.T, svc api.Service, rd api.Readiness, w io.Writer) http.Handler {
	t.Helper()
	return newRouter(t, svc, rd, slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
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
	_, err := api.NewRouter(api.Options{Service: &fakeService{}, Readiness: healthyReadiness{}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "SOROFORGE_API_TOKEN")
	assert.ErrorContains(t, err, "SOROFORGE_API_TOKENS")
	assert.ErrorContains(t, err, "will not start unauthenticated")
}

func TestServerRefusesShortTokens(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Service:   &fakeService{},
		Tokens:    []api.Token{api.NewToken("dashboard", "short", true)},
		Readiness: healthyReadiness{},
	})
	assert.ErrorContains(t, err, "at least")
}

func TestServerRefusesDuplicateTokenNames(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Service: &fakeService{},
		Tokens: []api.Token{
			api.NewToken("ci", testToken, false),
			api.NewToken("ci", testReadOnlyToken, true),
		},
		Readiness: healthyReadiness{},
	})
	assert.ErrorContains(t, err, "more than once")
}

func TestNewRouterRequiresService(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Tokens:    []api.Token{api.NewToken("ci", testToken, false)},
		Readiness: healthyReadiness{},
	})
	assert.ErrorContains(t, err, "service is required")
}

func TestNewRouterRequiresReadiness(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Service: &fakeService{},
		Tokens:  []api.Token{api.NewToken("ci", testToken, false)},
	})
	assert.ErrorContains(t, err, "readiness is required")
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

func TestOpenAPIDocumentIsServedWithoutAuthentication(t *testing.T) {
	router := newTestRouter(t, &fakeService{})
	empty := ""
	rec := do(t, router, http.MethodGet, "/openapi.json", nil, &empty)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var document map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &document))
	assert.Equal(t, "3.1.0", document["openapi"])
}

func TestOpenAPIDocumentMatchesRouter(t *testing.T) {
	router := newTestRouter(t, &fakeService{})
	empty := ""
	response := do(t, router, http.MethodGet, "/openapi.json", nil, &empty)
	require.Equal(t, http.StatusOK, response.Code)
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))

	documented := map[string]map[string]bool{}
	for path, operations := range document.Paths {
		documented[path] = map[string]bool{}
		for method := range operations {
			documented[path][method] = true
		}
	}

	routes := router.(chi.Routes)
	seen := map[string]map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		method = strings.ToLower(method)
		if seen[route] == nil {
			seen[route] = map[string]bool{}
		}
		seen[route][method] = true
		assert.True(t, documented[route][method], "router route %s %s is undocumented", method, route)
		return nil
	})
	require.NoError(t, err)
	for path, operations := range documented {
		for method := range operations {
			assert.True(t, seen[path][method], "documented operation %s %s is not served", method, path)
		}
	}
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
		"/v1/deployments/1",
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, router, http.MethodGet, path, nil, &empty)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestReadOnlyTokenCanReadButNotDeploy(t *testing.T) {
	// A dashboard or monitoring job holds a read-only token. It must be able
	// to watch, and must be stopped with 403 — not 401 — when it tries to
	// mutate: the token is valid, the operation simply is not permitted.
	svc := &fakeService{
		contracts:     []store.Contract{},
		deployResult:  &deploy.DeployResult{Alias: "counter"},
		upgradeResult: &deploy.UpgradeResult{Alias: "counter"},
	}
	router := newTestRouter(t, svc)
	readOnly := testReadOnlyToken

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, &readOnly)
	require.Equal(t, http.StatusOK, rec.Code)

	for _, path := range []string{"/v1/deploy", "/v1/upgrade"} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, router, http.MethodPost, path,
				map[string]any{"alias": "counter"}, &readOnly)

			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"a valid read-only token is authenticated; 401 would send the client hunting for a credential")
			assert.Contains(t, rec.Body.String(), "read-only")
			assert.Equal(t, 0, svc.calls, "a forbidden request must never reach the service")
		})
	}
}

func TestReadOnlyTokenReadsEveryReadRoute(t *testing.T) {
	svc := &fakeService{
		contracts:     []store.Contract{},
		deployments:   []store.Deployment{{ID: 1, Alias: "counter", Network: "testnet"}},
		statusResult:  &deploy.StatusResult{},
		networkStatus: &deploy.NetworkStatus{},
	}
	router := newTestRouter(t, svc)
	readOnly := testReadOnlyToken

	for _, path := range []string{
		"/v1/contracts",
		"/v1/contracts/testnet/counter/history",
		"/v1/contracts/testnet/counter/status",
		"/v1/contracts/testnet/status",
		"/v1/deployments/1",
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, router, http.MethodGet, path, nil, &readOnly)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestGetDeploymentByID(t *testing.T) {
	svc := &fakeService{
		deployments: []store.Deployment{
			{
				ID:             42,
				Alias:          "counter",
				Network:        "testnet",
				ContractID:     "CC",
				WasmHash:       "hash42",
				Action:         store.ActionDeploy,
				DeployerPubkey: "GABC",
			},
		},
	}
	router := newTestRouter(t, svc)
	tok := testToken

	t.Run("found returns deployment", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, "/v1/deployments/42", nil, &tok)
		require.Equal(t, http.StatusOK, rec.Code)
		var got store.Deployment
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, int64(42), got.ID)
		assert.Equal(t, "counter", got.Alias)
		assert.Equal(t, "testnet", got.Network)
	})

	t.Run("not found returns 404", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, "/v1/deployments/999", nil, &tok)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Contains(t, rec.Body.String(), "not found")
	})

	t.Run("invalid id returns 400", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, "/v1/deployments/not-a-number", nil, &tok)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestPaginationReturnsNextOffset(t *testing.T) {
	svc := &fakeService{
		contracts:   []store.Contract{{Alias: "a"}, {Alias: "b"}, {Alias: "c"}},
		deployments: []store.Deployment{{WasmHash: "v3"}, {WasmHash: "v2"}, {WasmHash: "v1"}},
	}
	router := newTestRouter(t, svc)

	for _, path := range []string{
		"/v1/contracts?limit=2&offset=0",
		"/v1/contracts/testnet/counter/history?limit=2&offset=0",
	} {
		rec := do(t, router, http.MethodGet, path, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		var hasMore bool
		require.NoError(t, json.Unmarshal(body["has_more"], &hasMore))
		assert.True(t, hasMore)
		var nextOffset int
		require.NoError(t, json.Unmarshal(body["next_offset"], &nextOffset))
		assert.Equal(t, 2, nextOffset)
	}
	assert.Equal(t, 3, svc.lastLimit, "the service receives one extra row to detect another page")
	assert.Equal(t, 0, svc.lastOffset)
}

func TestPaginationDefaultsAndRejectsInvalidLimits(t *testing.T) {
	svc := &fakeService{}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 101, svc.lastLimit)

	for _, path := range []string{
		"/v1/contracts?limit=0",
		"/v1/contracts?limit=501",
		"/v1/contracts?limit=nope",
		"/v1/contracts?offset=-1",
	} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, router, http.MethodGet, path, nil, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestReadWriteTokenCanReadAndWrite(t *testing.T) {
	svc := &fakeService{
		contracts:    []store.Contract{},
		deployResult: &deploy.DeployResult{Alias: "counter"},
	}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestReadOnlyTokenIsReportedAsForbiddenNotUnauthorized(t *testing.T) {
	// The distinction is the point of the feature: dashboards retry a 401,
	// but a 403 is final and tells the caller to change its token.
	router := newTestRouter(t, &fakeService{})
	readOnly := testReadOnlyToken

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter"}, &readOnly)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"),
		"WWW-Authenticate belongs on 401 only; the caller already authenticated")
}

func TestParseTokens(t *testing.T) {
	t.Run("legacy token becomes a read-write default", func(t *testing.T) {
		tokens, err := api.ParseTokens(testToken, "")
		require.NoError(t, err)
		require.Len(t, tokens, 1)
		assert.True(t, tokens[0].Allows(api.ScopeWrite))
	})

	t.Run("scoped entries carry their scope", func(t *testing.T) {
		tokens, err := api.ParseTokens("", "dashboard:read:"+testReadOnlyToken+",ci:write:"+testToken)
		require.NoError(t, err)
		require.Len(t, tokens, 2)

		assert.True(t, tokens[0].Allows(api.ScopeRead))
		assert.False(t, tokens[0].Allows(api.ScopeWrite))
		assert.True(t, tokens[1].Allows(api.ScopeWrite))
	})

	t.Run("legacy and scoped tokens combine", func(t *testing.T) {
		tokens, err := api.ParseTokens(testToken, "dashboard:read:"+testReadOnlyToken)
		require.NoError(t, err)
		assert.Len(t, tokens, 2)
	})

	t.Run("a secret may contain colons", func(t *testing.T) {
		tokens, err := api.ParseTokens("", "dashboard:read:"+testReadOnlyToken+":extra")
		require.NoError(t, err)
		require.Len(t, tokens, 1)
		assert.True(t, tokens[0].Allows(api.ScopeRead))
	})

	t.Run("malformed entry is an error naming the variable", func(t *testing.T) {
		_, err := api.ParseTokens("", testToken)
		require.Error(t, err)
		assert.ErrorContains(t, err, "SOROFORGE_API_TOKENS")
		assert.NotContains(t, err.Error(), testToken, "the error must not echo a value that might be a secret")
	})

	t.Run("unknown scope lists the accepted values", func(t *testing.T) {
		_, err := api.ParseTokens("", "dashboard:admin:"+testToken)
		require.Error(t, err)
		assert.ErrorContains(t, err, "read")
		assert.ErrorContains(t, err, "write")
	})
}

func TestTokenStringRedactsSecret(t *testing.T) {
	// A Token can end up in a log or an error via %v; its secret must not.
	token := api.NewToken("dashboard", testReadOnlyToken, true)
	assert.NotContains(t, token.String(), testReadOnlyToken)
	assert.Contains(t, token.String(), "dashboard")
	assert.NotContains(t, fmt.Sprintf("%v", token), testReadOnlyToken)
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

func TestMutatingEndpointsRejectOversizedBodies(t *testing.T) {
	// Both payloads are tiny by design, so a body beyond the cap is refused
	// with 413 before the service can see it — the alias field carries the
	// oversized value so the decoder trips on the size limit rather than on
	// an unknown field.
	oversized := `{"alias":"` + strings.Repeat("a", 70<<10) + `"}`

	for _, path := range []string{"/v1/deploy", "/v1/upgrade"} {
		t.Run(path, func(t *testing.T) {
			svc := &fakeService{}
			router := newTestRouter(t, svc)

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(oversized))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
			assert.Equal(t, 0, svc.calls, "an oversized request must never reach the service")

			var body map[string]string
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.NotEmpty(t, body["error"])
		})
	}
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

func TestReadyReportsHealthyDependenciesWithoutAuth(t *testing.T) {
	// A readiness probe must not require the token that can deploy.
	router := newTestRouter(t, &fakeService{})
	empty := ""

	rec := do(t, router, http.MethodGet, "/ready", nil, &empty)

	require.Equal(t, http.StatusOK, rec.Code)

	var body readiness.Status
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Ready)
	assert.Contains(t, body.Dependencies, readiness.Dependency{Name: readiness.DatabaseName, OK: true})
}

func TestReadyReturns503AndNamesFailingDependencies(t *testing.T) {
	rd := &fakeReadiness{status: readiness.Status{
		Ready: false,
		Dependencies: []readiness.Dependency{
			{Name: readiness.DatabaseName, OK: true},
			{Name: readiness.RPCName("mainnet"), Error: "connection refused"},
		},
	}}
	router := newTestRouterWith(t, &fakeService{}, rd, io.Discard)
	empty := ""

	rec := do(t, router, http.MethodGet, "/ready", nil, &empty)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), readiness.RPCName("mainnet"))
	assert.Contains(t, rec.Body.String(), "connection refused")

	var body readiness.Status
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.Ready)
}

func TestReadyReturns503WhenDatabaseIsDown(t *testing.T) {
	rd := &fakeReadiness{status: readiness.Status{
		Ready: false,
		Dependencies: []readiness.Dependency{
			{Name: readiness.DatabaseName, Error: "dial tcp: connection refused"},
		},
	}}
	router := newTestRouterWith(t, &fakeService{}, rd, io.Discard)
	empty := ""

	rec := do(t, router, http.MethodGet, "/ready", nil, &empty)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), readiness.DatabaseName)
}

func TestAccessLogRecordsOneStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	svc := &fakeService{contracts: []store.Contract{}}
	router := newTestRouterWith(t, svc, healthyReadiness{}, &buf)

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	entry := lastLogEntry(t, &buf)
	assert.Equal(t, "http request", entry["msg"])
	assert.Equal(t, http.MethodGet, entry["method"])
	assert.Equal(t, "/v1/contracts", entry["route"])
	assert.Equal(t, float64(http.StatusOK), entry["status"])
	assert.Contains(t, entry, "duration")
	assert.NotEmpty(t, entry["request_id"])
	assert.NotEmpty(t, entry["remote_addr"])
	// The token is attributed by name; its value is never logged.
	assert.Equal(t, "ci", entry["token"])
	assert.NotContains(t, buf.String(), testToken)
}

func TestAccessLogUsesTheRoutePatternNotThePath(t *testing.T) {
	// Log aggregators group by route pattern, so a thousand contract IDs do
	// not become a thousand log streams.
	var buf bytes.Buffer
	svc := &fakeService{statusResult: &deploy.StatusResult{}}
	router := newTestRouterWith(t, svc, healthyReadiness{}, &buf)

	rec := do(t, router, http.MethodGet, "/v1/contracts/testnet/counter/status", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	entry := lastLogEntry(t, &buf)
	assert.Equal(t, "/v1/contracts/{network}/{alias}/status", entry["route"])
	assert.NotContains(t, entry["route"], "counter")
}

func TestAccessLogNeverLogsTheTokenOrBody(t *testing.T) {
	var buf bytes.Buffer
	svc := &fakeService{deployResult: &deploy.DeployResult{Alias: "counter"}}
	router := newTestRouterWith(t, svc, healthyReadiness{}, &buf)

	rec := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter", "notes": "ticket-SECRET-1234"}, nil)
	require.Equal(t, http.StatusCreated, rec.Code)

	logs := buf.String()
	assert.NotContains(t, logs, testToken, "the bearer token must never be logged")
	assert.NotContains(t, logs, "ticket-SECRET-1234", "the request body must never be logged")
	assert.Contains(t, logs, `"token":"ci"`, "the token name is the safe attribution")
}

func TestHealthRequestsAreLoggedAtDebugOnly(t *testing.T) {
	empty := ""

	infoBuf := &bytes.Buffer{}
	router := newRouter(t, &fakeService{}, healthyReadiness{},
		slog.New(slog.NewJSONHandler(infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	rec := do(t, router, http.MethodGet, "/health", nil, &empty)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, strings.TrimSpace(infoBuf.String()),
		"a liveness probe polling every second would flood the log")

	debugBuf := &bytes.Buffer{}
	router = newRouter(t, &fakeService{}, healthyReadiness{},
		slog.New(slog.NewJSONHandler(debugBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec = do(t, router, http.MethodGet, "/health", nil, &empty)
	require.Equal(t, http.StatusOK, rec.Code)

	entry := lastLogEntry(t, debugBuf)
	assert.Equal(t, "DEBUG", entry["level"])
	assert.Equal(t, "/health", entry["route"])
}

// lastLogEntry decodes the final JSON line written to buf.
func lastLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.NotEmpty(t, lines)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &entry),
		"log line is not JSON: %s", lines[len(lines)-1])
	return entry
}
