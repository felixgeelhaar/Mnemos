package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.klarlabs.de/bolt"
)

// withTrustProxy flips the process-wide X-Forwarded-For trust switch for the
// duration of one test and restores it. Not parallel-safe by construction —
// the switch is startup configuration, not per-request state.
func withTrustProxy(t *testing.T, v bool) {
	t.Helper()
	prev := trustProxyHeaders
	trustProxyHeaders = v
	t.Cleanup(func() { trustProxyHeaders = prev })
}

func leadPost(t *testing.T, h http.Handler, xff string) int {
	t.Helper()
	body, _ := json.Marshal(leadRequest{Email: "spam@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/leads", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.9:44321" // one attacker, one socket
	req.Header.Set("Content-Type", "application/json")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestClientIP_IgnoresForwardedForByDefault is the exploit in miniature.
// POST /v1/leads is the single route jwtAuthMiddleware exempts from auth, so
// the per-IP token bucket is its only defence. clientIP used to believe
// X-Forwarded-For unconditionally, and the header is client-supplied — so a
// spammer varying it got a brand-new bucket on every request and the limiter
// never fired. The socket peer is now authoritative unless the operator
// declares a fronting proxy.
func TestClientIP_IgnoresForwardedForByDefault(t *testing.T) {
	withTrustProxy(t, false)
	h := leadsRateLimitMiddleware(makeLeadsHandler(newDiscardLogger()))

	// Burst budget is leadsBurst; the next request from the same socket must
	// be throttled even though every request claims a different origin.
	throttled := false
	for i := 0; i < leadsBurst+5; i++ {
		if leadPost(t, h, forgedIP(i)) == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Errorf("rotating X-Forwarded-For defeated the /v1/leads rate limiter (%d requests all accepted)", leadsBurst+5)
	}
}

// TestClientIP_TrustsForwardedForWhenOptedIn keeps the CDN deployment
// working: behind a proxy that OVERWRITES the header, per-visitor buckets are
// exactly what we want, and the operator says so with --trust-proxy.
func TestClientIP_TrustsForwardedForWhenOptedIn(t *testing.T) {
	withTrustProxy(t, true)
	h := leadsRateLimitMiddleware(makeLeadsHandler(newDiscardLogger()))

	for i := 0; i < leadsBurst+5; i++ {
		if code := leadPost(t, h, forgedIP(i)); code != http.StatusOK {
			t.Fatalf("request %d from a distinct forwarded client: status = %d, want 200", i, code)
		}
	}
	// ...and a single forwarded client is still bucketed.
	throttled := false
	for i := 0; i < leadsBurst+5; i++ {
		if leadPost(t, h, "198.51.100.7") == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Error("with --trust-proxy a single forwarded client was never throttled")
	}
}

// TestClientIP_ResolutionSource documents the resolution rule directly.
func TestClientIP_ResolutionSource(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	withTrustProxy(t, false)
	if got := clientIP(req); got != "192.0.2.10" {
		t.Errorf("default clientIP = %q, want the socket peer 192.0.2.10", got)
	}
	withTrustProxy(t, true)
	if got := clientIP(req); got != "1.2.3.4" {
		t.Errorf("trusted-proxy clientIP = %q, want the leftmost forwarded entry 1.2.3.4", got)
	}
}

// forgedIP yields a distinct spoofed origin per attempt.
func forgedIP(i int) string {
	return fmt.Sprintf("10.0.%d.%d", i/256, i%256)
}

// newDiscardLogger keeps the lead-capture audit line out of the test output.
func newDiscardLogger() *bolt.Logger {
	return bolt.New(bolt.NewJSONHandler(io.Discard))
}
