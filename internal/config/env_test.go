package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadEnvReadsAllVariables(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "postgres://localhost/soroforge")
	t.Setenv(EnvSecretKey, "SEED")
	t.Setenv(EnvKeystorePath, "/keys/deployer")
	t.Setenv(EnvSignerCommand, "/usr/local/bin/sign")
	t.Setenv(EnvSignerPublicKey, "GABC")
	t.Setenv(EnvMaxContractSize, "65536")
	t.Setenv(EnvAPIToken, "token")
	t.Setenv(EnvAPITokens, "dashboard:read:readonly-token")
	t.Setenv(EnvAPIAddr, "0.0.0.0:9000")
	t.Setenv(EnvLogFormat, "JSON")

	env := LoadEnv()

	assert.Equal(t, "postgres://localhost/soroforge", env.DatabaseURL)
	assert.Equal(t, "SEED", env.SecretKey)
	assert.Equal(t, "/keys/deployer", env.KeystorePath)
	assert.Equal(t, "/usr/local/bin/sign", env.SignerCommand)
	assert.Equal(t, "GABC", env.SignerPublicKey)
	assert.Equal(t, "65536", env.MaxContractSize)
	assert.Equal(t, "token", env.APIToken)
	assert.Equal(t, "dashboard:read:readonly-token", env.APITokens)
	assert.Equal(t, "0.0.0.0:9000", env.ListenAddr())
	// The format is normalised so callers can compare it directly.
	assert.Equal(t, "json", env.LogFormat)
}

func TestLoadEnvReadsCORSOrigins(t *testing.T) {
	// Only the whole value is trimmed here; per-entry trimming and validation
	// belong to ParseCORSOrigins, which the serve entry point calls.
	t.Setenv(EnvAPICORSOrigins, "  https://dash.example.com,https://ops.example.com  ")

	env := LoadEnv()

	assert.Equal(t, "https://dash.example.com,https://ops.example.com", env.APICORSOrigins)
}

func TestParseCORSOrigins(t *testing.T) {
	t.Run("unset means no origins and no CORS", func(t *testing.T) {
		origins, err := ParseCORSOrigins("")
		require.NoError(t, err)
		assert.Empty(t, origins, "an unset variable must leave CORS exactly as it is today")
	})

	t.Run("exact origins are kept in order", func(t *testing.T) {
		origins, err := ParseCORSOrigins("https://dash.example.com,https://ops.example.com")
		require.NoError(t, err)
		assert.Equal(t, []string{"https://dash.example.com", "https://ops.example.com"}, origins)
	})

	t.Run("whitespace is tolerated and empty entries are skipped", func(t *testing.T) {
		origins, err := ParseCORSOrigins(" https://dash.example.com , ,https://ops.example.com,")
		require.NoError(t, err)
		assert.Equal(t, []string{"https://dash.example.com", "https://ops.example.com"}, origins)
	})

	t.Run("http origins with a port are allowed", func(t *testing.T) {
		origins, err := ParseCORSOrigins("http://localhost:3000")
		require.NoError(t, err)
		assert.Equal(t, []string{"http://localhost:3000"}, origins)
	})

	t.Run("wildcard is rejected with an explanation", func(t *testing.T) {
		_, err := ParseCORSOrigins("*")
		require.Error(t, err)
		assert.ErrorContains(t, err, EnvAPICORSOrigins)
		assert.ErrorContains(t, err, `must not contain "*"`)
		assert.ErrorContains(t, err, "exact origins")
	})

	t.Run("a wildcard anywhere in an entry is rejected", func(t *testing.T) {
		_, err := ParseCORSOrigins("https://*.example.com")
		require.Error(t, err)
		assert.ErrorContains(t, err, `must not contain "*"`)
	})

	t.Run("malformed entries are rejected", func(t *testing.T) {
		tests := map[string]string{
			"no scheme":       "dash.example.com",
			"unparseable":     "://dash.example.com",
			"non-http scheme": "ftp://dash.example.com",
			"no host":         "https://",
			"trailing slash":  "https://dash.example.com/",
			"has a path":      "https://dash.example.com/app",
			"has a query":     "https://dash.example.com?x=1",
		}

		for name, raw := range tests {
			t.Run(name, func(t *testing.T) {
				_, err := ParseCORSOrigins(raw)
				require.Error(t, err)
				assert.ErrorContains(t, err, EnvAPICORSOrigins)
				assert.ErrorContains(t, err, "not a valid origin")
			})
		}
	})
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
	assert.True(t, Env{SignerCommand: "/bin/sign", SignerPublicKey: "GABC"}.HasSigningKey())
}

func TestValidateSigningConfig(t *testing.T) {
	tests := map[string]struct {
		env     Env
		wantErr string
	}{
		"no signer at all is allowed": {
			env: Env{},
		},
		"seed only": {
			env: Env{SecretKey: "SABC"},
		},
		"keystore only": {
			env: Env{KeystorePath: "/keys/deployer"},
		},
		"external signer with a public key": {
			env: Env{SignerCommand: "/usr/local/bin/sign", SignerPublicKey: "GABC"},
		},
		"external signer without a public key": {
			env:     Env{SignerCommand: "/usr/local/bin/sign"},
			wantErr: EnvSignerPublicKey,
		},
		"public key without a command": {
			env:     Env{SignerPublicKey: "GABC"},
			wantErr: EnvSignerCommand,
		},
		"external signer and a seed": {
			env:     Env{SignerCommand: "/usr/local/bin/sign", SignerPublicKey: "GABC", SecretKey: "SABC"},
			wantErr: "cannot be combined",
		},
		"external signer and a keystore": {
			env:     Env{SignerCommand: "/usr/local/bin/sign", SignerPublicKey: "GABC", KeystorePath: "/k"},
			wantErr: "cannot be combined",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.env.ValidateSigningConfig()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestMaxContractSizeBytes(t *testing.T) {
	const def = 128 * 1024

	t.Run("defaults when unset", func(t *testing.T) {
		got, err := Env{}.MaxContractSizeBytes(def)
		require.NoError(t, err)
		assert.Equal(t, def, got)
	})

	t.Run("override", func(t *testing.T) {
		got, err := Env{MaxContractSize: "65536"}.MaxContractSizeBytes(def)
		require.NoError(t, err)
		assert.Equal(t, 65536, got)
	})

	t.Run("not a number", func(t *testing.T) {
		_, err := Env{MaxContractSize: "128KiB"}.MaxContractSizeBytes(def)
		assert.ErrorContains(t, err, EnvMaxContractSize)
		assert.ErrorContains(t, err, "whole number")
	})

	t.Run("zero", func(t *testing.T) {
		_, err := Env{MaxContractSize: "0"}.MaxContractSizeBytes(def)
		assert.ErrorContains(t, err, "positive")
	})

	t.Run("negative", func(t *testing.T) {
		_, err := Env{MaxContractSize: "-1"}.MaxContractSizeBytes(def)
		assert.ErrorContains(t, err, "positive")
	})
}

func TestRedactedNeverExposesSecrets(t *testing.T) {
	// Redacted output is what gets printed by diagnostics, so it must not carry
	// key material or a database password under any circumstances.
	env := Env{
		DatabaseURL:     "postgres://user:hunter2@localhost/soroforge",
		SecretKey:       "SDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEAD",
		KeystorePath:    "/keys/deployer",
		SignerCommand:   "/usr/local/bin/sign --token=signer-secret",
		SignerPublicKey: "GPUBLICKEY",
		APIToken:        "super-secret-token",
		APITokens:       "dashboard:read:another-secret-token",
	}

	redacted := env.Redacted()

	for key, value := range redacted {
		assert.NotContains(t, value, "hunter2", "%s leaked the database password", key)
		assert.NotContains(t, value, "SDEADBEEF", "%s leaked the secret key", key)
		assert.NotContains(t, value, "signer-secret", "%s leaked the signer command arguments", key)
		assert.NotContains(t, value, "super-secret-token", "%s leaked the API token", key)
		assert.NotContains(t, value, "another-secret-token", "%s leaked a scoped API token", key)
	}

	assert.Equal(t, "(set)", redacted[EnvSecretKey])
	assert.Equal(t, "(set)", redacted[EnvSignerCommand])
	assert.Equal(t, "(set)", redacted[EnvAPIToken])
	assert.Equal(t, "(set)", redacted[EnvAPITokens])
	assert.Equal(t, "(unset)", Env{}.Redacted()[EnvSecretKey])

	// A path is not itself a secret and is useful for diagnosing which key
	// source was picked.
	assert.Equal(t, "/keys/deployer", redacted[EnvKeystorePath])

	// A public key is public and helps confirm which address will deploy.
	assert.Equal(t, "GPUBLICKEY", redacted[EnvSignerPublicKey])
}
