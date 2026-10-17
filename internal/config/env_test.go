package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeKeystore writes data at an explicit mode, bypassing umask effects so
// the permission tests mean what they say.
func writeKeystore(t *testing.T, mode os.FileMode, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deployer.key")
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestLoadEnvReadsAllVariables(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://localhost/soroforge")
	t.Setenv(EnvSecretKey, "SEED")
	t.Setenv(EnvKeystorePath, "/keys/deployer")
	t.Setenv(EnvAPIToken, "token")
	t.Setenv(EnvAPIAddr, "0.0.0.0:9000")

	env := LoadEnv()

	assert.Equal(t, "postgres://localhost/soroforge", env.DatabaseURL)
	assert.Equal(t, "SEED", env.SecretKey)
	assert.Equal(t, "/keys/deployer", env.KeystorePath)
	assert.Equal(t, "token", env.APIToken)
	assert.Equal(t, "0.0.0.0:9000", env.ListenAddr())
}

func TestListenAddrDefaultsToLoopback(t *testing.T) {
	// The process holds a signing key; exposing it beyond loopback should be a
	// deliberate act, not a default.
	assert.Equal(t, DefaultAPIAddr, Env{}.ListenAddr())
	assert.Contains(t, DefaultAPIAddr, "127.0.0.1")
}

func TestRequireDatabaseURL(t *testing.T) {
	_, err := Env{}.RequireDatabaseURL()
	assert.ErrorContains(t, err, EnvDatabaseURL)

	url, err := Env{DatabaseURL: "postgres://x"}.RequireDatabaseURL()
	require.NoError(t, err)
	assert.Equal(t, "postgres://x", url)
}

func TestSigningSeedFromEnvVar(t *testing.T) {
	seed, source, err := Env{SecretKey: "SABC"}.SigningSeed()
	require.NoError(t, err)
	assert.Equal(t, "SABC", seed)
	assert.Equal(t, "env:"+EnvSecretKey, source)
}

func TestSigningSeedFromKeystoreFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployer.key")
	// A trailing newline is what `echo S... > file` produces, and it must not
	// corrupt the seed.
	require.NoError(t, os.WriteFile(path, []byte("SABC\n"), 0o600))

	seed, source, err := Env{KeystorePath: path}.SigningSeed()
	require.NoError(t, err)
	assert.Equal(t, "SABC", seed)
	assert.Contains(t, source, "keystore:")
}

func TestSigningSeedAcceptsOwnerOnlyKeystoreModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}

	for _, mode := range []os.FileMode{0o600, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeKeystore(t, mode, "SABC\n")

			seed, _, err := Env{KeystorePath: path}.SigningSeed()
			require.NoError(t, err)
			assert.Equal(t, "SABC", seed)
		})
	}
}

func TestSigningSeedRefusesReadableKeystore(t *testing.T) {
	// A seed that signs mainnet deploys must not be readable by every account
	// on a shared machine, exactly as SSH refuses private keys in this state.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			const seed = "SSECRETSEEDMUSTNOTAPPEAR"
			path := writeKeystore(t, mode, seed)

			_, _, err := Env{KeystorePath: path}.SigningSeed()
			require.Error(t, err)

			// The message has to be actionable: which file, which mode, and
			// the exact fix. A bare "permission denied" would leave an
			// operator guessing.
			assert.ErrorContains(t, err, path)
			assert.ErrorContains(t, err, fmt.Sprintf("%04o", mode))
			assert.ErrorContains(t, err, "chmod 600")
			assert.ErrorContains(t, err, EnvAllowInsecureKeystore)

			// Errors end up in CI logs; the file's contents must not.
			assert.NotContains(t, err.Error(), seed)
		})
	}
}

func TestSigningSeedEscapeHatchAllowsReadableKeystore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}

	// Some container secret mounts have modes the operator cannot change; the
	// escape hatch makes accepting that risk an explicit, greppable act.
	path := writeKeystore(t, 0o644, "SABC\n")

	seed, _, err := Env{KeystorePath: path, AllowInsecureKeystore: true}.SigningSeed()
	require.NoError(t, err)
	assert.Equal(t, "SABC", seed)
}

func TestLoadEnvParsesInsecureKeystoreOptIn(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes"} {
		t.Setenv(EnvAllowInsecureKeystore, value)
		assert.True(t, LoadEnv().AllowInsecureKeystore, "%q should opt in", value)
	}

	// Anything unrecognised leaves the permission check on: a typo must not
	// silently waive a security control.
	for _, value := range []string{"", "0", "false", "tru", "no"} {
		t.Setenv(EnvAllowInsecureKeystore, value)
		assert.False(t, LoadEnv().AllowInsecureKeystore, "%q must not opt in", value)
	}
}

func TestSigningSeedPrefersEnvVarOverKeystore(t *testing.T) {
	// So CI can override a developer's on-disk key without editing files.
	path := filepath.Join(t.TempDir(), "deployer.key")
	require.NoError(t, os.WriteFile(path, []byte("SFILE"), 0o600))

	seed, source, err := Env{SecretKey: "SENV", KeystorePath: path}.SigningSeed()
	require.NoError(t, err)
	assert.Equal(t, "SENV", seed)
	assert.Contains(t, source, "env:")
}

func TestSigningSeedErrors(t *testing.T) {
	t.Run("nothing configured", func(t *testing.T) {
		_, _, err := Env{}.SigningSeed()
		assert.ErrorContains(t, err, EnvSecretKey)
		assert.ErrorContains(t, err, EnvKeystorePath)
	})

	t.Run("keystore missing", func(t *testing.T) {
		_, _, err := Env{KeystorePath: filepath.Join(t.TempDir(), "absent")}.SigningSeed()
		assert.ErrorContains(t, err, "read keystore")
	})

	t.Run("keystore empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.key")
		require.NoError(t, os.WriteFile(path, []byte("   \n"), 0o600))

		_, _, err := Env{KeystorePath: path}.SigningSeed()
		assert.ErrorContains(t, err, "empty")
	})
}

func TestHasSigningKey(t *testing.T) {
	assert.False(t, Env{}.HasSigningKey())
	assert.True(t, Env{SecretKey: "S"}.HasSigningKey())
	assert.True(t, Env{KeystorePath: "/k"}.HasSigningKey())
}

func TestRedactedNeverExposesSecrets(t *testing.T) {
	// Redacted output is what gets printed by diagnostics, so it must not carry
	// key material or a database password under any circumstances.
	env := Env{
		DatabaseURL:  "postgres://user:hunter2@localhost/soroforge",
		SecretKey:    "SDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEAD",
		KeystorePath: "/keys/deployer",
		APIToken:     "super-secret-token",
	}

	redacted := env.Redacted()

	for key, value := range redacted {
		assert.NotContains(t, value, "hunter2", "%s leaked the database password", key)
		assert.NotContains(t, value, "SDEADBEEF", "%s leaked the secret key", key)
		assert.NotContains(t, value, "super-secret-token", "%s leaked the API token", key)
	}

	assert.Equal(t, "(set)", redacted[EnvSecretKey])
	assert.Equal(t, "(set)", redacted[EnvAPIToken])
	assert.Equal(t, "(unset)", Env{}.Redacted()[EnvSecretKey])

	// A path is not itself a secret and is useful for diagnosing which key
	// source was picked.
	assert.Equal(t, "/keys/deployer", redacted[EnvKeystorePath])
}
