package oauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginThrottle is a tiny per-IP failed-login rate limiter. It protects the
// single password gate against brute-force without needing an external
// dependency: any IP that fails N times within failureWindow is blocked for
// blockDuration, and every failure incurs a fixed delay to cap raw attempt
// rate regardless of the counter state.
type loginThrottle struct {
	failureWindow time.Duration
	failureMax    int
	blockDuration time.Duration
	failureDelay  time.Duration

	clock func() time.Time

	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count        int
	lastFail     time.Time
	blockedUntil time.Time
}

func newLoginThrottle(clock func() time.Time) *loginThrottle {
	return &loginThrottle{
		failureWindow: loginFailureWindow,
		failureMax:    loginFailureMax,
		blockDuration: loginBlockDuration,
		failureDelay:  loginFailureDelay,
		clock:         clock,
		fails:         map[string]*failRecord{},
	}
}

// check returns (allowed, retryAfter). When allowed is false the caller
// should refuse the attempt and emit a Retry-After hint to the client.
func (t *loginThrottle) check(key string) (bool, time.Duration) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.fails[key]
	if !ok {
		return true, 0
	}
	if now.Before(r.blockedUntil) {
		return false, r.blockedUntil.Sub(now)
	}
	// Block expired or never set, but the failure record is stale —
	// drop it so the next failure starts a fresh window.
	if now.Sub(r.lastFail) > t.failureWindow {
		delete(t.fails, key)
	}
	return true, 0
}

// recordFail bumps the failure counter for the given key. After the Nth
// failure inside the window the IP is blocked for blockDuration.
func (t *loginThrottle) recordFail(key string) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.fails[key]
	if !ok || now.Sub(r.lastFail) > t.failureWindow {
		r = &failRecord{}
		t.fails[key] = r
	}
	r.count++
	r.lastFail = now
	if r.count >= t.failureMax {
		r.blockedUntil = now.Add(t.blockDuration)
	}
}

// recordSuccess clears the failure counter for the given key. Called on a
// successful login so a flapping legitimate user doesn't get blocked by a
// streak of fat-finger attempts they then corrected.
func (t *loginThrottle) recordSuccess(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}

// gc is invoked from Server.gc(). Sweeps expired records so the map doesn't
// keep growing for one-off scanners that hit /authorize once and disappear.
func (t *loginThrottle) gc() {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, r := range t.fails {
		if now.After(r.blockedUntil) && now.Sub(r.lastFail) > t.failureWindow {
			delete(t.fails, k)
		}
	}
}

// clientIP returns a stable per-source identifier for rate-limiting under
// the deployment topology where this process sits behind a single reverse
// proxy on the loopback interface (Apache on the same box).
//
// XFF spoofing was a real bug in the first cut: many proxies, Apache
// included, *append* the real client IP to a client-supplied
// X-Forwarded-For value, so the leftmost element is whatever the attacker
// chose to send. Per-IP throttling that keys on a spoofable string is no
// throttling at all.
//
// We now:
//   - if the immediate connection is NOT loopback, trust RemoteAddr and
//     ignore XFF entirely — nothing we'd believe is upstream.
//   - if it IS loopback, take the *rightmost* XFF entry — the one our
//     trusted reverse proxy just appended — and ignore everything to the
//     left. An attacker can prepend whatever they like; they can't change
//     what the proxy adds.
//
// A wrong/missing header keys on the proxy's own IP, which means heavy
// abuse falls back to a single bucket instead of zero throttling — fine
// for a single-user prototype, and the error mode is loud (legitimate
// users notice immediately).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	// Use the rightmost element — that's the one our local reverse proxy
	// added. Anything to the left of it is attacker-controlled.
	if i := strings.LastIndex(xff, ","); i >= 0 {
		return strings.TrimSpace(xff[i+1:])
	}
	return strings.TrimSpace(xff)
}
