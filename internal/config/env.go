package config

import (
	"fmt"
	"os"
	"runtime"
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

	// EnvAllowInsecureKeystore disables the keystore permission check. It
	// exists for environments that mount secrets with modes the operator
	// cannot change, such as some container secret mounts. Set it to 1 (or
	// true) deliberately; the default is to refuse.
	EnvAllowInsecureKeystore = "SOROFORGE_ALLOW_INSECURE_KEYSTORE"

	// EnvAPIToken is the bearer token required by the HTTP API. The server
	// refuses to start when it is unset.
	EnvAPIToken = "SOROFORGE_API_TOKEN"

	// EnvAPIAddr overrides the HTTP API listen address.
	EnvAPIAddr = "SOROFORGE_API_ADDR"
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
	APIAddr      string

	// AllowInsecureKeystore accepts a keystore readable by group or others.
	// See EnvAllowInsecureKeystore.
	AllowInsecureKeystore bool
}

// LoadEnv reads SoroForge's environment variables. It does not validate them:
// most commands need only a subset, so each entry point checks what it needs
// via RequireDatabaseURL or SigningSeed.
func LoadEnv() Env {
	return Env{
		DatabaseURL:           strings.TrimSpace(os.Getenv(EnvDatabaseURL)),
		SecretKey:             strings.TrimSpace(os.Getenv(EnvSecretKey)),
		KeystorePath:          strings.TrimSpace(os.Getenv(EnvKeystorePath)),
		AllowInsecureKeystore: envBool(os.Getenv(EnvAllowInsecureKeystore)),
		APIToken:              os.Getenv(EnvAPIToken),
		APIAddr:               strings.TrimSpace(os.Getenv(EnvAPIAddr)),
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
		// The flag is security-relevant, so diagnostics should show whether
		// the permission check was explicitly waived.
		EnvAllowInsecureKeystore: fmt.Sprintf("%t", e.AllowInsecureKeystore),
		EnvAPIToken:              redact(e.APIToken),
		EnvAPIAddr:               e.ListenAddr(),
	}
}

func redact(v string) string {
	if v == "" {
		return "(unset)"
	}
	return "(set)"
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
