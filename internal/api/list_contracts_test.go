package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The contracts listing is the endpoint a dashboard or CI script hits first, so
// its payload shape is a promise to those clients: which filter reaches the
// service, what a bad one produces, and that "no contracts" is `[]` rather than
// `null`. These tests decode the body instead of matching raw substrings,
// because that distinction is exactly what a substring check cannot prove.

func TestListContractsResultIsDecodedWithTheNetworkFilter(t *testing.T) {
	svc := &fakeService{contracts: []store.Contract{
		{Alias: "counter", Network: "testnet", ContractID: "CCOUNTER", CurrentWasmHash: "aaaa"},
	}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts?network=testnet", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, "testnet", svc.lastNetwork, "the filter must reach the service")

	var body struct {
		Contracts []store.Contract `json:"contracts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Contracts, 1)
	assert.Equal(t, "counter", body.Contracts[0].Alias)
	assert.Equal(t, "testnet", body.Contracts[0].Network)
	assert.Equal(t, "CCOUNTER", body.Contracts[0].ContractID)
	assert.Equal(t, "aaaa", body.Contracts[0].CurrentWasmHash)
}

func TestListContractsUnknownNetworkFailsLikeOtherRoutes(t *testing.T) {
	// An unknown network is a client error wherever it is given. A caller that
	// switches between routes must not have to special-case the status the
	// listing returns for the same mistake.
	svc := &fakeService{err: errors.New(`unknown network "staging" (configured: testnet)`)}
	router := newTestRouter(t, svc)

	list := do(t, router, http.MethodGet, "/v1/contracts?network=staging", nil, nil)
	deploy := do(t, router, http.MethodPost, "/v1/deploy",
		map[string]any{"alias": "counter", "network": "staging"}, nil)

	assert.Equal(t, http.StatusBadRequest, list.Code)
	assert.Equal(t, deploy.Code, list.Code, "the same error must map to the same status on every route")

	var body map[string]string
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "unknown network")
}

func TestListContractsEmptyResultIsAnArrayNotNull(t *testing.T) {
	// A stored empty slice must serialise as [], not null: JSON clients
	// (including `soroforge list --json | jq`) iterate the field directly, and
	// null would force every one of them to nil-check first.
	svc := &fakeService{contracts: []store.Contract{}}
	router := newTestRouter(t, svc)

	rec := do(t, router, http.MethodGet, "/v1/contracts", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Contracts []store.Contract `json:"contracts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotNil(t, body.Contracts, "an empty result must decode as [], not null")
	assert.Empty(t, body.Contracts)
}
