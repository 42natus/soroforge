// Package api exposes SoroForge's operations over HTTP so that CI pipelines can
// drive deploys without a shell.
//
// The handlers are a thin layer over the same deploy.Service the CLI uses —
// there is no lifecycle logic here, only request decoding, authentication, and
// JSON encoding. Anything that changes contract behaviour belongs in the deploy
// package, where it is exercised by both surfaces.
//
// Mutating endpoints require a bearer token and the server refuses to start
// without one; see auth.go.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/store"
)

// Service is the subset of deploy.Service the API needs.
//
// Declaring it here rather than taking the concrete type keeps the handlers
// testable against a fake and documents exactly which operations are reachable
// over HTTP.
type Service interface {
	Deploy(ctx context.Context, req deploy.DeployRequest) (*deploy.DeployResult, error)
	Upgrade(ctx context.Context, req deploy.UpgradeRequest) (*deploy.UpgradeResult, error)
	Status(ctx context.Context, req deploy.StatusRequest) (*deploy.StatusResult, error)
	StatusAll(ctx context.Context, network string) (*deploy.NetworkStatus, error)
	List(ctx context.Context, network string) ([]store.Contract, error)
	History(ctx context.Context, network, alias string) ([]store.Deployment, error)
}

// requestTimeout bounds a single API request.
//
// A deploy waits for two transactions to be confirmed, so this has to be
// generous — but it must exist, or a stuck network holds a connection open
// indefinitely.
const requestTimeout = 5 * time.Minute

// Options configures the HTTP server.
type Options struct {
	// Service performs the work. Required.
	Service Service

	// Token is a single bearer token accepted by /v1 endpoints. It is kept
	// for callers that predate rotation; Tokens supersedes it when both are
	// set.
	Token string

	// Tokens lists every bearer token accepted by /v1 endpoints. More than
	// one lets operators rotate without downtime: add the new token, move
	// clients over, then remove the old one. The server refuses to start if
	// the resolved list is empty, any token is too short, or two are equal.
	Tokens []string

	// RateLimit bounds how often one client may call /v1. The zero value
	// disables limiting; soroforge serve sets it from configuration, where
	// the documented defaults live.
	RateLimit RateLimitOptions

	// Log defaults to slog.Default().
	Log *slog.Logger
}

// NewRouter builds the HTTP handler.
//
// It returns an error rather than a router when the tokens are unusable, so
// that a misconfigured server fails at startup instead of serving
// unauthenticated deploy endpoints.
func NewRouter(opts Options) (http.Handler, error) {
	if opts.Service == nil {
		return nil, fmt.Errorf("api: service is required")
	}
	tokens := opts.Tokens
	if len(tokens) == 0 && opts.Token != "" {
		tokens = []string{opts.Token}
	}
	if err := validateTokens(tokens); err != nil {
		return nil, err
	}

	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	h := &handlers{svc: opts.Service, log: log}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(requestTimeout))

	// Unauthenticated: a liveness probe must work without distributing the
	// deploy token to every monitoring system.
	r.Get("/health", h.health)

	r.Route("/v1", func(r chi.Router) {
		// Before authentication on purpose: unauthenticated floods count
		// against the same per-client budget, so the limiter also bounds
		// token-guessing against the auth middleware.
		r.Use(RateLimit(opts.RateLimit))
		r.Use(BearerAuthTokens(tokens))

		r.Post("/deploy", h.deploy)
		r.Post("/upgrade", h.upgrade)

		r.Get("/contracts", h.listContracts)
		r.Get("/contracts/{network}/{alias}/history", h.history)
		r.Get("/contracts/{network}/{alias}/status", h.status)
		r.Get("/contracts/{network}/status", h.statusAll)
	})

	return r, nil
}

// NewServer builds an http.Server with sensible timeouts.
func NewServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		// ReadHeaderTimeout bounds how long a client may take to send headers,
		// which is the cheap defence against a Slowloris-style connection hold.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout must outlast requestTimeout so a slow deploy still gets
		// to write its response rather than having the connection cut.
		WriteTimeout: requestTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

type handlers struct {
	svc Service
	log *slog.Logger
}

// errorResponse is the body returned for every failure, so clients can parse
// one shape.
type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	// The status line is already sent, so an encoding failure cannot be turned
	// into an error response; log it at the call site's discretion instead.
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON reads a request body, rejecting unknown fields so that a
// misspelled key is a clear 400 rather than a silently ignored option — the
// same reasoning as KnownFields in the config loader.
func decodeJSON(r *http.Request, dest any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// statusForError maps an operation error to an HTTP status.
//
// Configuration mistakes are the client's fault (400) and a missing record is a
// 404; anything else is reported as a server error, since SoroForge cannot tell
// a network failure from a bug.
func statusForError(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case isClientError(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// clientErrorMarkers are substrings of the errors produced by bad request
// input, as opposed to network or database failures.
//
// Matching on message text is unlovely. It is here because these errors come
// from config lookups that predate any need for HTTP status codes, and adding
// sentinel error types across three packages to serve one status decision is a
// worse trade at this size. If this list grows much, that is the signal to
// introduce typed errors in the config and deploy packages.
var clientErrorMarkers = []string{
	"unknown contract",
	"unknown network",
	"not tracked",
	"no signing key configured",
	"invalid request body",
	"is required",
}

// isClientError recognises the errors produced by bad request input.
func isClientError(err error) bool {
	msg := err.Error()
	for _, marker := range clientErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
