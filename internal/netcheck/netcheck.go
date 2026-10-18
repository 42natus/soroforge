// Package netcheck answers whether the networks configured in soroforge.yaml
// are the networks their endpoints actually serve.
//
// `soroforge deploy` trusts that a network name maps to the right RPC URL and
// passphrase; a mistyped URL or a provider that moved can point "testnet" at
// a different chain, and the failure surfaces only as a rejected signature or
// a deploy on the wrong network. This package probes every configured network
// up front — does it answer, how fast, what ledger is it on, and does the
// passphrase it reports match what the config claims — so that mismatch is a
// command you can run before you deploy, and a gate you can put in CI.
//
// The probes are read-only (getLatestLedger and getNetwork) and run
// concurrently, each under its own timeout, mirroring the readiness package's
// dependency checks. Unlike readiness there is no caching or database check:
// this is an operator-facing report, not a health endpoint.
package netcheck

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"net/url"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
)

// ClientFactory creates a Stellar client for a named network. It matches
// deploy.ClientFactory, so callers pass the same factory the deploy service
// uses and tests substitute a fake.
type ClientFactory func(networkName string, network config.Network) (stellar.Client, error)

// DefaultTimeout bounds a single network probe. Testnet RPC typically answers
// getLatestLedger in well under a second; several seconds of silence is
// indistinguishable from a dead endpoint for reporting purposes.
const DefaultTimeout = 5 * time.Second

// Result is the outcome of checking one configured network.
type Result struct {
	// Network is the name from soroforge.yaml.
	Network string `json:"network"`

	// RedactedURL is the RPC URL with any userinfo and query string removed,
	// so the report can be pasted into a ticket or CI log without leaking
	// provider credentials that live in the URL.
	RedactedURL string `json:"rpc_url"`

	// Responded reports that the endpoint answered getLatestLedger within
	// the timeout. It is false when the URL is unreachable or too slow.
	Responded bool `json:"responded"`

	// Latency is how long the latest-ledger probe took. Zero when the
	// endpoint never answered.
	Latency time.Duration `json:"latency_ns"`

	// LatestLedger is the sequence the endpoint reported, 0 when unknown.
	LatestLedger uint32 `json:"latest_ledger,omitempty"`

	// ReportedPassphrase is the passphrase the endpoint says it serves, when
	// it answered getNetwork.
	ReportedPassphrase string `json:"reported_passphrase,omitempty"`

	// PassphraseVerified is true when the endpoint reported a passphrase at
	// all; false means the match below is unknown, not disagreed.
	PassphraseVerified bool `json:"passphrase_verified"`

	// PassphraseMatch is true only when the reported passphrase equals the
	// configured one.
	PassphraseMatch bool `json:"passphrase_match"`

	// Error explains why the check failed. Empty when OK.
	Error string `json:"error,omitempty"`

	// OK is the verdict: the endpoint answered, reported a passphrase, and
	// that passphrase matches the configuration.
	OK bool `json:"ok"`
}

// Check probes every configured network concurrently and returns one Result
// per network, sorted by name. Each probe is bounded by timeout, and the
// whole sweep stops with ctx.
func Check(ctx context.Context, networks map[string]config.Network, clients ClientFactory, timeout time.Duration) []Result {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	// Sorted so the report — and any CI diff of it — is stable across runs.
	sort.Strings(names)

	results := make([]Result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = checkOne(ctx, name, networks[name], clients, timeout)
		}()
	}
	wg.Wait()
	return results
}

// checkOne runs the probe cycle for a single network. Every failure mode
// produces a Result with Error set rather than an error return, because one
// bad network must not suppress the reports for the others.
//
// Failure text always passes through redactURLsIn: RPC drivers interpolate the
// request URL into their error messages, so a dial or HTTP failure would
// otherwise print the very userinfo and query-string credentials the report
// exists to hide — including in the --json payload.
func checkOne(ctx context.Context, name string, network config.Network, clients ClientFactory, timeout time.Duration) Result {
	res := Result{Network: name, RedactedURL: RedactURL(network.RPCURL)}
	fail := func(msg string) Result {
		res.Error = redactURLsIn(msg)
		return res
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := clients(name, network)
	if err != nil {
		return fail(err.Error())
	}
	if closer, ok := client.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	start := time.Now()
	ledger, err := client.LatestLedger(probeCtx)
	res.Latency = time.Since(start)
	if err != nil {
		return fail(fmt.Sprintf("latest ledger: %v", err))
	}
	res.Responded = true
	if ledger == 0 {
		return fail("RPC returned ledger 0")
	}
	res.LatestLedger = ledger

	reporter, ok := client.(stellar.NetworkReporter)
	if !ok {
		return fail("endpoint does not report its network passphrase")
	}
	reported, err := reporter.ReportNetworkPassphrase(probeCtx)
	if err != nil {
		return fail(fmt.Sprintf("passphrase check: %v", err))
	}
	if reported == "" {
		return fail("endpoint reported an empty passphrase")
	}
	res.ReportedPassphrase = reported
	res.PassphraseVerified = true
	res.PassphraseMatch = PassphrasesMatch(network.Passphrase, reported)
	if !res.PassphraseMatch {
		return fail(fmt.Sprintf(
			"configured passphrase %q does not match the passphrase the endpoint serves, %q",
			strings.TrimSpace(network.Passphrase), strings.TrimSpace(reported)))
	}

	res.OK = true
	return res
}

// AnyFailure reports whether any result failed. The CLI uses it to choose a
// non-zero exit code so `soroforge networks` can gate a pipeline.
func AnyFailure(results []Result) bool {
	for _, r := range results {
		if !r.OK {
			return true
		}
	}
	return false
}

// RedactURL removes userinfo and the query string from a URL so a network
// report can be printed or logged without exposing provider credentials that
// users commonly place in the rpc_url (https://user:token@host, ?api_key=...).
//
// A URL that net/url cannot parse is redacted by hand rather than returned
// unchanged: an unparsable string is exactly where credentials would hide from
// a parser, so printing it verbatim would defeat the point. The hand fallback
// drops the query first, then the userinfo, because '@' is legal inside a
// query value and would otherwise swallow the wrong span.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}

	// Manual fallback: drop everything from the query '?' on, then everything
	// up to and including the userinfo '@'.
	out := raw
	if q := strings.IndexByte(out, '?'); q >= 0 {
		out = out[:q]
	}
	if at := strings.LastIndex(out, "@"); at >= 0 && strings.Contains(out[:at], "://") {
		out = out[at+1:]
	}
	return out
}

// schemeURLRE matches a URL-shaped span inside free text: a scheme ("https://",
// "grpc://", ...) up to the first character that can never appear in a URL.
// Quote characters end the match so a message like Post "https://...": dial
// keeps its punctuation and only the URL itself is rewritten.
var schemeURLRE = regexp.MustCompile("[a-zA-Z][a-zA-Z0-9+.\\-]*://[^\\s\"'`<>]+")

// redactURLsIn scrubs every embedded URL from error text produced by the RPC
// stack. Drivers interpolate the request URL into their failures, and some
// mask only the password (user:***@host?api_key=secret), so the whole span is
// replaced with its credential-free form rather than trusted to be clean.
func redactURLsIn(msg string) string {
	return schemeURLRE.ReplaceAllStringFunc(msg, RedactURL)
}

// PassphrasesMatch compares a configured passphrase with one an endpoint
// reports. Surrounding whitespace is ignored: YAML quoting habits and older
// RPC responses are notorious for stray spaces, and "Test SDF Network ;
// September 2015 " does serve testnet.
func PassphrasesMatch(configured, reported string) bool {
	configured = strings.TrimSpace(configured)
	reported = strings.TrimSpace(reported)
	return configured != "" && configured == reported
}
