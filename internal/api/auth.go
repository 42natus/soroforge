package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
)

// minTokenLength is the shortest API token the server will accept.
//
// This endpoint can deploy contracts with a live signing key, so a token short
// enough to guess is not meaningfully different from no token at all. The limit
// is a floor, not advice: use a long random value.
const minTokenLength = 16

// BearerAuth rejects requests without a matching bearer token.
//
// It is BearerAuthTokens for the single-token case, kept as the name most
// callers and tests know.
func BearerAuth(token string) func(http.Handler) http.Handler {
	return BearerAuthTokens([]string{token})
}

// BearerAuthTokens rejects requests without a bearer token matching one of
// tokens.
//
// More than one token is accepted so operators can rotate without downtime:
// add the new token, move clients over, then remove the old one.
//
// The comparison is constant-time, and every configured token is compared on
// every request with the results OR-ed rather than returned early, so response
// timing does not reveal which token matched or how far through the list the
// server got. Tokens are hashed once at startup and the fixed-width digests
// are compared, so token length cannot be inferred from timing either.
func BearerAuthTokens(tokens []string) func(http.Handler) http.Handler {
	expected := make([][sha256.Size]byte, len(tokens))
	for i, token := range tokens {
		expected[i] = sha256.Sum256([]byte(token))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r)
			if !ok {
				unauthorized(w, "missing bearer token")
				return
			}

			given := sha256.Sum256([]byte(presented))
			var matched int
			for i := range expected {
				matched |= subtle.ConstantTimeCompare(given[:], expected[i][:])
			}
			if matched != 1 {
				unauthorized(w, "invalid bearer token")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from an Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// validateTokens checks every configured token before the server starts
// listening.
//
// The server refuses to boot on failure rather than starting in an open state.
// An HTTP endpoint that deploys contracts using a signing key should never be
// reachable without authentication, and "we'll put it behind a proxy" is a
// promise that outlives the person who made it.
//
// Errors name positions, never values: a malformed token is still a secret,
// and startup errors end up in logs.
func validateTokens(tokens []string) error {
	if len(tokens) == 0 {
		return fmt.Errorf(
			"no API token configured: set %s (or %s for several) to a long random value. "+
				"The HTTP API can deploy and upgrade contracts with the configured signing key, "+
				"so it will not start unauthenticated",
			config.EnvAPIToken, config.EnvAPITokens)
	}

	seen := make(map[string]int, len(tokens))
	for i, token := range tokens {
		if len(token) < minTokenLength {
			return fmt.Errorf(
				"API token %d of %d is %d characters; use at least %d each "+
					"(e.g. `openssl rand -hex 32`)", i+1, len(tokens), len(token), minTokenLength)
		}
		if first, duplicate := seen[token]; duplicate {
			return fmt.Errorf(
				"API tokens %d and %d are duplicates; remove one", first+1, i+1)
		}
		seen[token] = i
	}
	return nil
}

func unauthorized(w http.ResponseWriter, detail string) {
	// WWW-Authenticate tells a client how to authenticate, per RFC 7235.
	w.Header().Set("WWW-Authenticate", `Bearer realm="soroforge"`)
	writeError(w, http.StatusUnauthorized, detail)
}
