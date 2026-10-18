package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two origins stand in for a dashboard the operator listed and a page
// nobody listed.
const (
	dashboardOrigin = "https://dash.example.com"
	rogueOrigin     = "https://evil.example.net"
)

// newCORSRouter builds the router the serve command builds for a given origin
// list, against the same fakes the rest of this file's neighbours use.
func newCORSRouter(t *testing.T, origins []string) (http.Handler, *fakeService) {
	t.Helper()
	svc := &fakeService{contracts: []store.Contract{}}
	router, err := api.NewRouter(api.Options{
		Service:     svc,
		Tokens:      testTokens(),
		Readiness:   healthyReadiness{},
		CORSOrigins: origins,
	})
	require.NoError(t, err)
	return router, svc
}

// corsRequest issues a request from origin (unless empty) with a bearer token
// (unless empty).
func corsRequest(t *testing.T, router http.Handler, method, path, origin, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestCORSAllowedOriginIsEchoed is the feature itself: a listed origin gets
// its own origin back in Access-Control-Allow-Origin, which is what makes the
// browser accept the response.
func TestCORSAllowedOriginIsEchoed(t *testing.T) {
	router, _ := newCORSRouter(t, []string{dashboardOrigin})

	rec := corsRequest(t, router, http.MethodGet, "/v1/contracts", dashboardOrigin, testToken)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, dashboardOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
}

// TestCORSDisallowedOriginGetsNoHeader proves the list is a list: an origin
// outside it gets no CORS headers, so its page cannot read the response even
// though the request itself was authenticated.
func TestCORSDisallowedOriginGetsNoHeader(t *testing.T) {
	router, _ := newCORSRouter(t, []string{dashboardOrigin})

	rec := corsRequest(t, router, http.MethodGet, "/v1/contracts", rogueOrigin, testToken)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

// TestCORSPreflightBypassesAuth is the reason the middleware wraps the router
// rather than joining its stack: a preflight carries no Authorization header
// by design, so it must be answered before the auth-wrapped routes — while
// still never reaching a handler.
func TestCORSPreflightBypassesAuth(t *testing.T) {
	router, svc := newCORSRouter(t, []string{dashboardOrigin})

	req := httptest.NewRequest(http.MethodOptions, "/v1/deploy", nil)
	req.Header.Set("Origin", dashboardOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "preflight must succeed without an Authorization header")
	assert.Equal(t, dashboardOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), http.MethodPost)
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	assert.NotEmpty(t, rec.Header().Get("Access-Control-Max-Age"))
	assert.Equal(t, 0, svc.calls, "a preflight must never reach a handler")
}

// TestCORSActualRequestStillRequiresToken keeps the two halves independent:
// CORS lets the browser send the request, authentication decides whether it is
// answered. An allowed origin without a token is still 401.
func TestCORSActualRequestStillRequiresToken(t *testing.T) {
	router, svc := newCORSRouter(t, []string{dashboardOrigin})

	rec := corsRequest(t, router, http.MethodGet, "/v1/contracts", dashboardOrigin, "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, 0, svc.calls)
}

// TestCORSDisabledSendsNoHeaders covers the default: an operator who never
// heard of CORS must see exactly the behaviour the API had before it existed.
func TestCORSDisabledSendsNoHeaders(t *testing.T) {
	router, svc := newCORSRouter(t, nil)

	rec := corsRequest(t, router, http.MethodGet, "/v1/contracts", dashboardOrigin, testToken)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))

	// Without CORS there is nothing to answer a preflight, so it falls
	// through to the router — and must not pick up CORS headers on the way.
	req := httptest.NewRequest(http.MethodOptions, "/v1/deploy", nil)
	req.Header.Set("Origin", dashboardOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight := httptest.NewRecorder()
	router.ServeHTTP(preflight, req)

	assert.Empty(t, preflight.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, 0, svc.calls)
}

// TestNewRouterRejectsWildcardCORSOrigin is the startup guarantee: "*" would
// let any page drive an API that deploys contracts, so the server refuses to
// start rather than logging a warning about it.
func TestNewRouterRejectsWildcardCORSOrigin(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Service:     &fakeService{},
		Tokens:      testTokens(),
		Readiness:   healthyReadiness{},
		CORSOrigins: []string{"*"},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, `must not contain "*"`)
	assert.ErrorContains(t, err, "exact origins")
}

// TestNewRouterRejectsNonExactCORSOrigin holds library callers to the same
// exact-origin rules the environment variable gets: a path or trailing slash
// would never match a browser's Origin header.
func TestNewRouterRejectsNonExactCORSOrigin(t *testing.T) {
	_, err := api.NewRouter(api.Options{
		Service:     &fakeService{},
		Tokens:      testTokens(),
		Readiness:   healthyReadiness{},
		CORSOrigins: []string{"https://dash.example.com/"},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "not a valid origin")
}
