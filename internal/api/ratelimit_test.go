package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestClientLimitersEvictIdleEntries(t *testing.T) {
	// A stream of distinct clients must not grow the map without bound.
	now := time.Unix(0, 0)
	limiters := newClientLimiters(rate.Limit(1), 1, func() time.Time { return now })

	for _, key := range []string{"a", "b", "c"} {
		allowed, _ := limiters.allow(key)
		require.True(t, allowed)
	}
	require.Len(t, limiters.limiters, 3)

	// Past the TTL, the next request triggers a sweep and every idle entry is
	// dropped before the map is consulted again.
	now = now.Add(limiterTTL + time.Minute)
	allowed, _ := limiters.allow("a")
	require.True(t, allowed)

	assert.Len(t, limiters.limiters, 1, "idle clients should have been evicted")
}

func TestClientLimitersDoesNotSweepOnEveryRequest(t *testing.T) {
	now := time.Unix(0, 0)
	limiters := newClientLimiters(rate.Limit(1), 1, func() time.Time { return now })

	allowed, _ := limiters.allow("a")
	require.True(t, allowed)

	// Just inside the sweep interval, an idle entry survives: walking the map
	// on every request would make the common path O(clients).
	now = now.Add(limiterSweepInterval - time.Second)
	limiters.allow("b")
	assert.Len(t, limiters.limiters, 2)
}

func TestClientLimitersDeniedRequestDoesNotConsumeFutureTokens(t *testing.T) {
	// Repeated over-limit requests must not push a client further into debt:
	// once the wait has passed, exactly one token is available again.
	limiters := newClientLimiters(rate.Limit(20), 1, time.Now)

	allowed, _ := limiters.allow("key")
	require.True(t, allowed)

	for i := 0; i < 5; i++ {
		denied, retryAfter := limiters.allow("key")
		require.False(t, denied)
		assert.Positive(t, retryAfter)
	}

	time.Sleep(75 * time.Millisecond)

	allowed, _ = limiters.allow("key")
	assert.True(t, allowed, "one refilled token should be enough after the bucket's debt was cancelled")
}

func TestClientKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:5555"

	assert.Equal(t, "192.0.2.1", clientKey(req, false))

	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	assert.Equal(t, "192.0.2.1", clientKey(req, false),
		"the header must be ignored unless it is explicitly trusted")
	assert.Equal(t, "203.0.113.9", clientKey(req, true))

	// The first entry is the original client; proxies append themselves.
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1, 10.0.0.2")
	assert.Equal(t, "203.0.113.9", clientKey(req, true))

	// Trusted but absent: fall back to the connection address rather than
	// lumping everyone under one empty key.
	req.Header.Del("X-Forwarded-For")
	assert.Equal(t, "192.0.2.1", clientKey(req, true))

	// A bare address with no port is still a usable key.
	req.RemoteAddr = "192.0.2.2"
	assert.Equal(t, "192.0.2.2", clientKey(req, false))

	// Ports are not part of the key: one client, one bucket.
	req.RemoteAddr = "192.0.2.2:1234"
	assert.Equal(t, clientKey(req, false), func() string {
		other := httptest.NewRequest("GET", "/", nil)
		other.RemoteAddr = "192.0.2.2:9876"
		return clientKey(other, false)
	}())
}
