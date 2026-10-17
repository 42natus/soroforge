package config

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Environment variables recognised by SoroForge.
//
// Secrets live here and only here. Nothing in this list is ever written to
// soroforge.yaml, echoed by --json output, or included in a log line; see
// Env.Redacted and the security notes in the README.
const (
	// EnvConfigPath overrides the location of soroforge.yaml.
	EnvConfigPath = "SOROFORGE_CONFIG"

	// EnvDatabaseURL is the Postgres connection string for deployment history.
	EnvDatabaseURL = "DATABASE_URL"

	// EnvSecretKey holds the deployer's Stellar secret seed (S...). Convenient
	// for CI, where the value comes from the runner's secret store.
	EnvSecretKey = "SOROFORGE_SECRET_KEY"

	// EnvKeystorePath points at a file containing nothing but the secret seed.
	// Preferred for workstations: a file can be mode 0600, whereas an
	// environment variable is readable by every child process and tends to end
	// up in shell history.
	EnvKeystorePath = "SOROFORGE_KEYSTORE_PATH"

	// EnvAPIToken is the original bearer token for the HTTP API. It grants
	// read and write, so existing single-token deployments keep working. The
	// server refuses to start when it and EnvAPITokens are both unset.
	EnvAPIToken = "SOROFORGE_API_TOKEN"

	// EnvAPITokens configures scoped bearer tokens as a comma-separated list
	// of name:scope:token entries, where scope is "read" or "write" (write
	// implies read). Read-only tokens can call GET routes and receive 403 on
	// the mutating ones. Example:
	//
	//	SOROFORGE_API_TOKENS="dashboard:read:$DASHBOARD_TOKEN,ci:write:$CI_TOKEN"
	EnvAPITokens = "SOROFORGE_API_TOKENS"

	// EnvAPIAddr overrides the HTTP API listen address.
	EnvAPIAddr = "SOROFORGE_API_ADDR"

	// EnvLogFormat selects the logging handler: "text" (default) or "json".
	// JSON is for CI and log aggregators that index structured fields.
	EnvLogFormat = "SOROFORGE_LOG_FORMAT"
)

// DefaultAPIAddr is the listen address used when EnvAPIAddr is unset.
//
// It binds loopback rather than 0.0.0.0 deliberately: this process holds a
// signing key, and exposing it to the network should be a decision someone
// makes on purpose.
const DefaultAPIAddr = "127.0.0.1:8080"

// Env holds configuration sourced from the process environment.
//
// The struct is deliberately not tagged for YAML or JSON. SecretKey must not be
// serialised anywhere, and leaving the tags off means an accidental
// json.Marshal of a Config cannot leak it.
type Env struct {
	DatabaseURL  string
	SecretKey    string
	KeystorePath string
	APIToken     string
	APITokens    string
	APIAddr      string
	LogFormat    string
}

// LoadEnv reads SoroForge's environment variables. It does not validate them:
// most commands need only a subset, so each entry point checks what it needs
// via RequireDatabaseURL or SigningSeed.
func LoadEnv() Env {
	return Env{
		DatabaseURL:  strings.TrimSpace(os.Getenv(EnvDatabaseURL)),
		SecretKey:    strings.TrimSpace(os.Getenv(EnvSecretKey)),
		KeystorePath: strings.TrimSpace(os.Getenv(EnvKeystorePath)),
		APIToken:     os.Getenv(EnvAPIToken),
		APITokens:    strings.TrimSpace(os.Getenv(EnvAPITokens)),
		APIAddr:      strings.TrimSpace(os.Getenv(EnvAPIAddr)),
		LogFormat:    strings.ToLower(strings.TrimSpace(os.Getenv(EnvLogFormat))),
	}
}

// RequireDatabaseURL returns the configured Postgres URL, or an error naming
// the variable to set.
func (e Env) RequireDatabaseURL() (string, error) {
	if e.DatabaseURL == "" {
		return "", fmt.Errorf("%s is not set; SoroForge needs Postgres to record deployment history", EnvDatabaseURL)
	}
	return e.DatabaseURL, nil
}

// ListenAddr returns the HTTP API listen address, defaulting to DefaultAPIAddr.
func (e Env) ListenAddr() string {
	if e.APIAddr == "" {
		return DefaultAPIAddr
	}
	return e.APIAddr
}

// TLSFiles returns the configured certificate and private key paths, if TLS
// is configured.
//
// Setting exactly one is an error rather than a silent fallback to plaintext:
// an operator who asked for TLS must not get an unencrypted listener because
// they forgot the other half of the pair.
func (e Env) TLSFiles() (certFile, keyFile string, err error) {
	if (e.TLSCert == "") != (e.TLSKey == "") {
		return "", "", fmt.Errorf(
			"TLS needs both a certificate and a key: set both %s and %s "+
				"(or pass both --tls-cert and --tls-key)", EnvTLSCert, EnvTLSKey)
	}
	return e.TLSCert, e.TLSKey, nil
}

// RateLimitConfig is the resolved per-client rate limit for the HTTP API.
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained per-client rate. Zero disables
	// limiting.
	RequestsPerSecond float64

	// Burst is how many requests a client may make at once. Zero when
	// limiting is disabled.
	Burst int

	// TrustProxyHeaders makes the limiter read the client IP from
	// X-Forwarded-For instead of the connection's remote address.
	TrustProxyHeaders bool
}

// RateLimit returns the resolved rate limit configuration.
//
// Malformed values are errors rather than silent fallbacks: a typo in
// SOROFORGE_API_RATE_LIMIT that quietly disabled limiting would be the exact
// failure this feature exists to prevent.
func (e Env) RateLimit() (RateLimitConfig, error) {
	cfg := RateLimitConfig{
		RequestsPerSecond: DefaultAPIRateLimit,
		Burst:             DefaultAPIRateBurst,
	}

	if raw := e.APIRateLimit; raw != "" {
		rps, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return RateLimitConfig{}, fmt.Errorf("%s=%q is not a number", EnvAPIRateLimit, raw)
		}
		if rps < 0 {
			return RateLimitConfig{}, fmt.Errorf(
				"%s must be 0 (disabled) or a positive number of requests per second, got %g",
				EnvAPIRateLimit, rps)
		}
		cfg.RequestsPerSecond = rps
	}

	if raw := e.APIRateBurst; raw != "" {
		burst, err := strconv.Atoi(raw)
		if err != nil {
			return RateLimitConfig{}, fmt.Errorf("%s=%q is not a whole number", EnvAPIRateBurst, raw)
		}
		if burst < 1 {
			return RateLimitConfig{}, fmt.Errorf("%s must be at least 1, got %d", EnvAPIRateBurst, burst)
		}
		cfg.Burst = burst
	}

	if raw := e.APITrustProxy; raw != "" {
		trust, err := strconv.ParseBool(raw)
		if err != nil {
			return RateLimitConfig{}, fmt.Errorf(
				"%s=%q is not a boolean (true or false)", EnvAPITrustProxy, raw)
		}
		cfg.TrustProxyHeaders = trust
	}

	// A disabled limit ignores burst; report that rather than a value the
	// consumer might mistakenly apply.
	if cfg.RequestsPerSecond == 0 {
		cfg.Burst = 0
	}

	return cfg, nil
}

// APITokenList returns every configured API token, in the order they appear.
//
// EnvAPITokens wins when both variables are set: an operator who writes the
// list means the list. EnvAPIToken remains the one-token shorthand.
func (e Env) APITokenList() []string {
	if len(e.APITokens) > 0 {
		return e.APITokens
	}
	if e.APIToken != "" {
		return []string{e.APIToken}
	}
	return nil
}

// SigningSeed returns the deployer's secret seed from whichever source is
// configured, along with a label describing that source for diagnostics.
//
// The returned seed is a secret: callers must hand it straight to a signer and
// must not log it, store it, or include it in command output. The label exists
// so operators can confirm *which* key source was used without the value ever
// being printed.
//
// EnvSecretKey wins over EnvKeystorePath when both are set, so that CI can
// override a developer's on-disk key without editing files.
func (e Env) SigningSeed() (seed string, source string, err error) {
	if e.SecretKey != "" {
		return e.SecretKey, "env:" + EnvSecretKey, nil
	}

	if e.KeystorePath != "" {
		if err := checkKeystorePermissions(e.KeystorePath, e.AllowInsecureKeystore); err != nil {
			return "", "", err
		}

		raw, err := os.ReadFile(e.KeystorePath)
		if err != nil {
			return "", "", fmt.Errorf("read keystore %s: %w", e.KeystorePath, err)
		}
		// Trim so a trailing newline from `echo S... > keystore` is tolerated;
		// a stray newline in a seed is a confusing failure to debug.
		seed := strings.TrimSpace(string(raw))
		if seed == "" {
			return "", "", fmt.Errorf("keystore %s is empty", e.KeystorePath)
		}
		return seed, "keystore:" + e.KeystorePath, nil
	}

	return "", "", fmt.Errorf(
		"no signing key configured: set %s or %s (see README security notes)",
		EnvSecretKey, EnvKeystorePath,
	)
}

// checkKeystorePermissions refuses a keystore that group or other can read.
//
// A keystore holds a seed that may sign mainnet deploys, and the README asks
// for mode 0600 for exactly that reason; reading the file regardless would let
// an accidental `chmod 644` quietly leak the key to every account on the
// machine. SSH refuses private keys in this state for the same reason. The
// error names the file, its mode, and the fix — never the file's contents.
//
// Windows has no POSIX permission bits, so the check is skipped there.
// allowInsecure is the documented escape hatch for environments that mount
// secrets with modes the operator cannot change (see EnvAllowInsecureKeystore).
func checkKeystorePermissions(path string, allowInsecure bool) error {
	if runtime.GOOS == "windows" || allowInsecure {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("read keystore %s: %w", path, err)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf(
			"keystore %s is mode %04o: readable by group or others, so anyone on "+
				"this machine can read the deployer seed that signs deploys. Run "+
				"`chmod 600 %s` to fix it, or set %s=1 to accept the risk deliberately",
			path, perm, path, EnvAllowInsecureKeystore)
	}
	return nil
}

// HasSigningKey reports whether a signing key source is configured. Read-only
// commands use it to give a clear error instead of failing deeper in the stack.
func (e Env) HasSigningKey() bool {
	return e.SecretKey != "" || e.KeystorePath != ""
}

// Redacted returns a copy safe to print or log: every secret is replaced with a
// fixed placeholder, and the database URL keeps only its shape.
func (e Env) Redacted() map[string]string {
	return map[string]string{
		EnvDatabaseURL:  redact(e.DatabaseURL),
		EnvSecretKey:    redact(e.SecretKey),
		EnvKeystorePath: e.KeystorePath, // a path is not itself a secret
		EnvAPIToken:     redact(e.APIToken),
		EnvAPITokens:    redact(e.APITokens),
		EnvAPIAddr:      e.ListenAddr(),
		EnvLogFormat:    e.LogFormat,
	}
	return v
}

func redact(v string) string {
	if v == "" {
		return "(unset)"
	}
	return "(set)"
}

func redactList(v []string) string {
	if len(v) == 0 {
		return "(unset)"
	}
	return "(set)"
}

// splitTokens parses a comma-separated token list, trimming surrounding space
// and dropping empty entries so a trailing comma is not a startup failure.
func splitTokens(v string) []string {
	var tokens []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			tokens = append(tokens, part)
		}
	}
	return tokens
}

// envBool reports whether an environment variable is set to a truthy value.
//
// It is deliberately strict about what counts as true: anything unrecognised
// leaves the safe default (the permission check) in place, so a typo cannot
// silently disable a security control.
func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
