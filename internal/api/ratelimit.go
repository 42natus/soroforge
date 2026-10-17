package api

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultRateBurst is the burst applied when a positive rate is configured
// without one. It matches config.DefaultAPIRateBurst — the two packages cannot
// share the constant without an import cycle, since config must not depend on
// the HTTP layer.
const defaultRateBurst = 20

// Rate limiting exists because a bearer token bounds *who* may call the API,
// not *how often*. Every /v1 call can trigger RPC traffic and some deploy
// contracts, so a leaked token or a runaway CI loop could otherwise hammer the
// network (and the signing key) without bound.
const (
	// limiterTTL is how long an idle client's token bucket is remembered.
	// Past it, the client starts fresh; the map stays small either way.
	limiterTTL = 10 * time.Minute

	// limiterSweepInterval bounds how often the whole map is walked to evict
	// stale entries. Sweeping on every request would make the common path
	// O(clients).
	limiterSweepInterval = time.Minute
)

// RateLimitOptions configures the per-client limit applied to /v1.
//
// The zero value disables limiting, so callers of NewRouter that never opted
// in keep their previous behaviour. soroforge serve always sets a limit from
// its configuration, where the documented defaults live.
type RateLimitOptions struct {
	// RequestsPerSecond is the sustained per-client rate. Zero or negative
	// disables limiting.
	RequestsPerSecond float64

	// Burst is how many requests a client may make at once, before the
	// sustained rate applies. Non-positive means defaultRateBurst.
	Burst int

	// TrustProxyHeaders takes the client IP from X-Forwarded-For instead of
	// the connection's remote address. Enable it only when a reverse proxy you
	// control is the sole way in: the header is otherwise client-controlled,
	// so trusting it lets a client dodge the limit by forging it — or frame
	// another client by naming their address.
	TrustProxyHeaders bool
}

// RateLimit returns middleware that bounds how often one client may call.
//
// It is mounted on /v1 only. /health is a liveness probe and must stay
// answerable even while a client is over its limit, or an overloaded caller
// would look like a dead server.
func RateLimit(opts RateLimitOptions) func(http.Handler) http.Handler {
	if opts.RequestsPerSecond <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	burst := opts.Burst
	if burst < 1 {
		burst = defaultRateBurst
	}

	limiters := newClientLimiters(rate.Limit(opts.RequestsPerSecond), burst, time.Now)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			allowed, retryAfter := limiters.allow(clientKey(r, opts.TrustProxyHeaders))
			if !allowed {
				// Retry-After is whole seconds per RFC 9110; round up so a
				// sub-second wait is not reported as 0, which clients read as
				// "retry immediately".
				seconds := int(math.Ceil(retryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeError(w, http.StatusTooManyRequests,
					"rate limit exceeded; retry after the number of seconds in Retry-After")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientKey identifies the rate-limit bucket for a request.
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// The first entry is the original client; every proxy that
			// handled the request appended itself after it.
			if i := strings.IndexByte(forwarded, ','); i >= 0 {
				forwarded = forwarded[:i]
			}
			if ip := strings.TrimSpace(forwarded); ip != "" {
				return ip
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Test requests and some servers carry a bare address; it is still a
		// usable bucket key.
		return r.RemoteAddr
	}
	return host
}

// clientLimiters is a map of per-client token buckets with bounded memory:
// entries idle for longer than limiterTTL are swept away, so a stream of
// distinct clients cannot grow the map without limit.
//
// now is a field so tests can control time; the token buckets themselves use
// the real clock, so the tests that exercise refill wait real (short)
// intervals.
type clientLimiters struct {
	mu        sync.Mutex
	limiters  map[string]*clientEntry
	limit     rate.Limit
	burst     int
	now       func() time.Time
	lastSweep time.Time
}

type clientEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newClientLimiters(limit rate.Limit, burst int, now func() time.Time) *clientLimiters {
	return &clientLimiters{
		limiters:  make(map[string]*clientEntry),
		limit:     limit,
		burst:     burst,
		now:       now,
		lastSweep: now(),
	}
}

// allow reports whether key may make a request now and, if not, how long until
// it may.
func (c *clientLimiters) allow(key string) (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.sweep(now)

	entry, ok := c.limiters[key]
	if !ok {
		entry = &clientEntry{limiter: rate.NewLimiter(c.limit, c.burst)}
		c.limiters[key] = entry
	}
	entry.lastSeen = now

	if entry.limiter.Allow() {
		return true, 0
	}

	// Reserve and immediately cancel to learn the wait without consuming a
	// token: repeated over-limit requests must not push a client further into
	// debt than one token's worth.
	reservation := entry.limiter.Reserve()
	delay := reservation.Delay()
	reservation.Cancel()
	return false, delay
}

// sweep drops entries idle longer than limiterTTL. It runs at most once per
// limiterSweepInterval to keep the common path cheap.
func (c *clientLimiters) sweep(now time.Time) {
	if now.Sub(c.lastSweep) < limiterSweepInterval {
		return
	}
	c.lastSweep = now

	for key, entry := range c.limiters {
		if now.Sub(entry.lastSeen) > limiterTTL {
			delete(c.limiters, key)
		}
	}
}
