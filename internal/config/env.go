package config

import (
	"fmt"
	"os"
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

	// EnvSignerCommand names an executable that signs a transaction hash for
	// keys SoroForge cannot read, such as HSM- or KMS-backed keys. It is
	// mutually exclusive with the seed sources above.
	EnvSignerCommand = "SOROFORGE_SIGNER_COMMAND"

	// EnvSignerPublicKey is the G... address the command signs for. It cannot
	// be derived without the private key, so it must be configured explicitly.
	EnvSignerPublicKey = "SOROFORGE_SIGNER_PUBLIC_KEY"

	// EnvMaxContractSize overrides the maximum contract WASM size, in bytes.
	// Defaults to the network's current limit; see stellar.DefaultMaxContractSize.
	EnvMaxContractSize = "SOROFORGE_MAX_CONTRACT_SIZE"

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
	DatabaseURL     string
	SecretKey       string
	KeystorePath    string
	SignerCommand   string
	SignerPublicKey string
	MaxContractSize string
	APIToken        string
	APITokens       string
	APIAddr         string
	LogFormat       string
}

// LoadEnv reads SoroForge's environment variables. It does not validate them:
// most commands need only a subset, so each entry point checks what it needs
// via RequireDatabaseURL or SigningSeed.
func LoadEnv() Env {
	return Env{
		DatabaseURL:     strings.TrimSpace(os.Getenv(EnvDatabaseURL)),
		SecretKey:       strings.TrimSpace(os.Getenv(EnvSecretKey)),
		KeystorePath:    strings.TrimSpace(os.Getenv(EnvKeystorePath)),
		SignerCommand:   strings.TrimSpace(os.Getenv(EnvSignerCommand)),
		SignerPublicKey: strings.TrimSpace(os.Getenv(EnvSignerPublicKey)),
		MaxContractSize: strings.TrimSpace(os.Getenv(EnvMaxContractSize)),
		APIToken:        os.Getenv(EnvAPIToken),
		APITokens:       strings.TrimSpace(os.Getenv(EnvAPITokens)),
		APIAddr:         strings.TrimSpace(os.Getenv(EnvAPIAddr)),
		LogFormat:       strings.ToLower(strings.TrimSpace(os.Getenv(EnvLogFormat))),
	}
}

// RequireDatabaseURL returns the configured database URL, or an error naming
// the variable to set. Both backends are accepted: a postgres:// connection
// string or a sqlite:///path/to/file.db URL.
func (e Env) RequireDatabaseURL() (string, error) {
	if e.DatabaseURL == "" {
		return "", fmt.Errorf("%s is not set; SoroForge needs a database (postgres:// or sqlite://) to record deployment history", EnvDatabaseURL)
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
		"no signing key configured: set %s or %s, or point %s at a signer command with %s (see README security notes)",
		EnvSecretKey, EnvKeystorePath, EnvSignerCommand, EnvSignerPublicKey,
	)
}

// ValidateSigningConfig rejects configurations that would otherwise fail
// halfway through a deploy: an external signer without the address it signs
// for, or two key sources racing to sign the same transaction.
func (e Env) ValidateSigningConfig() error {
	hasSeed := e.SecretKey != "" || e.KeystorePath != ""

	if e.SignerCommand == "" {
		if e.SignerPublicKey != "" {
			return fmt.Errorf(
				"%s is set but %s is not; a public key alone cannot sign",
				EnvSignerPublicKey, EnvSignerCommand)
		}
		return nil
	}

	if hasSeed {
		return fmt.Errorf(
			"%s cannot be combined with %s or %s; choose one key source",
			EnvSignerCommand, EnvSecretKey, EnvKeystorePath)
	}
	if e.SignerPublicKey == "" {
		return fmt.Errorf(
			"%s requires %s, the G... address the command signs for",
			EnvSignerCommand, EnvSignerPublicKey)
	}
	return nil
}

// HasSigningKey reports whether a signing key source is configured. Read-only
// commands use it to give a clear error instead of failing deeper in the stack.
func (e Env) HasSigningKey() bool {
	return e.SecretKey != "" || e.KeystorePath != "" || e.SignerCommand != ""
}

// MaxContractSizeBytes returns the configured contract WASM size limit, or def
// when no override is set.
func (e Env) MaxContractSizeBytes(def int) (int, error) {
	if e.MaxContractSize == "" {
		return def, nil
	}

	n, err := strconv.Atoi(e.MaxContractSize)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number of bytes, got %q", EnvMaxContractSize, e.MaxContractSize)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", EnvMaxContractSize, n)
	}
	return n, nil
}

// Redacted returns a copy safe to print or log: every secret is replaced with a
// fixed placeholder, and the database URL keeps only its shape.
func (e Env) Redacted() map[string]string {
	return map[string]string{
		EnvDatabaseURL:     redact(e.DatabaseURL),
		EnvSecretKey:       redact(e.SecretKey),
		EnvKeystorePath:    e.KeystorePath,          // a path is not itself a secret
		EnvSignerCommand:   redact(e.SignerCommand), // arguments may embed credentials
		EnvSignerPublicKey: e.SignerPublicKey,       // a public key is not a secret
		EnvMaxContractSize: e.MaxContractSize,
		EnvAPIToken:        redact(e.APIToken),
		EnvAPITokens:       redact(e.APITokens),
		EnvAPIAddr:         e.ListenAddr(),
		EnvLogFormat:       e.LogFormat,
	}
}

func redact(v string) string {
	if v == "" {
		return "(unset)"
	}
	return "(set)"
}
