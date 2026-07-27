package main

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// leadsRateBudget is the burst budget per source IP for POST /v1/leads.
// 5 requests/min smoothed over a 10-request burst — ample for a human
// re-trying a typo, hostile to automated form-spam.
const (
	leadsRequestsPerMinute = 5
	leadsBurst             = 10
	// leadsLimiterTTL bounds the number of cached limiters; entries
	// older than this expire on the next sweep so a one-off submit
	// doesn't keep a limiter pinned forever.
	leadsLimiterTTL = 30 * time.Minute
)

type leadsLimiter struct {
	limiter *rate.Limiter
	last    time.Time
}

type leadsLimiterMap struct {
	mu  sync.Mutex
	m   map[string]*leadsLimiter
	now func() time.Time // injectable clock for tests
}

func newLeadsLimiterMap() *leadsLimiterMap {
	return &leadsLimiterMap{
		m:   make(map[string]*leadsLimiter),
		now: time.Now,
	}
}

func (l *leadsLimiterMap) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// Lazy sweep — ok for the scale this protects (low-volume
	// public endpoint). Avoids a goroutine that would have to be
	// shut down on server stop.
	for k, v := range l.m {
		if now.Sub(v.last) > leadsLimiterTTL {
			delete(l.m, k)
		}
	}
	entry, ok := l.m[ip]
	if !ok {
		entry = &leadsLimiter{
			limiter: rate.NewLimiter(rate.Limit(float64(leadsRequestsPerMinute)/60.0), leadsBurst),
		}
		l.m[ip] = entry
	}
	entry.last = now
	return entry.limiter.Allow()
}

// leadsRateLimitMiddleware wraps the public /v1/leads handler with a
// per-IP token-bucket limiter. Anonymous public endpoints without a
// rate limit are a DoS amplifier and a free email-validation oracle
// for harvesters; this is the cheapest defence that closes both.
//
// The bucket is sized so a single legitimate visitor cannot trip it
// (5 req/min, 10 burst), but a botnet of one IP cannot exceed it.
//
// The identity it buckets on is clientIP, which by default is the socket
// peer. It deliberately does NOT trust X-Forwarded-For unless the operator
// declared a fronting proxy with `serve --trust-proxy` / MNEMOS_TRUST_PROXY:
// on a directly-exposed listener that header is attacker-controlled, so
// believing it would hand every request its own bucket and reduce this
// limiter — the only defence on the one auth-exempt route — to a no-op.
func leadsRateLimitMiddleware(next http.Handler) http.Handler {
	limiters := newLeadsLimiterMap()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Methods other than POST drop straight through — let the
		// downstream handler emit the 405 (so browser preflight or
		// curl HEAD probes don't consume budget).
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if !limiters.allow(ip) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}
