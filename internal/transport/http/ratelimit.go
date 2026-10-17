package http

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket held in memory. Limits are per server instance:
// with N instances a client gets up to N× the budget, which still bounds brute force and
// hashing load per instance without a shared store.
type rateLimiter struct {
	perSecond float64
	burst     float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	nextSweep time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		perSecond: float64(perMinute) / 60,
		burst:     float64(burst),
		buckets:   make(map[string]*bucket),
	}
}

// allow takes a token for key. When none is left it returns how long until one is.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.nextSweep) { // drop buckets that have refilled and gone idle
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst}
		l.buckets[key] = b
	} else {
		b.tokens = min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.perSecond)
	}
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
}

// writeRateLimited answers 429 with Retry-After.
func writeRateLimited(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	writeJSON(w, http.StatusTooManyRequests, apiError{Code: "RATE_LIMITED", Message: "too many requests"})
}

// limitByIP rate-limits a route per client IP.
func limitByIP(l *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retry := l.allow(clientIP(r), time.Now()); !ok {
				writeRateLimited(w, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP is the peer address. Behind a reverse proxy, enable Options.TrustProxyHeaders so
// chi's RealIP middleware rewrites RemoteAddr from X-Forwarded-For / X-Real-IP first.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
