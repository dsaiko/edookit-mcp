//! Per-subject token bucket + per-IP login/DCR window throttles + client-IP
//! extraction. Port of Go's `internal/oauth/ratelimit.go`. All time comes from
//! an injectable clock (unix seconds) so tests are deterministic.

use std::collections::HashMap;
use std::net::IpAddr;

use parking_lot::Mutex;

use super::Clock;

/// Per-subject token bucket for the Bearer-authenticated `/mcp` endpoint. Caps
/// the amplification a leaked JWT can drive against every downstream surface.
pub struct SubLimiter {
    rate: f64,
    burst: f64,
    ttl: i64,
    clock: Clock,
    buckets: Mutex<HashMap<String, Bucket>>,
}

struct Bucket {
    tokens: f64,
    last_refill: i64,
    last_use: i64,
}

impl SubLimiter {
    pub fn new(per_sec: f64, burst: u32, ttl_secs: i64, clock: Clock) -> Self {
        Self {
            rate: per_sec,
            burst: burst as f64,
            ttl: ttl_secs,
            clock,
            buckets: Mutex::new(HashMap::new()),
        }
    }

    /// True if the bucket for `key` has spare capacity. A new key starts with
    /// its full burst, so the first call is never denied.
    pub fn allow(&self, key: &str) -> bool {
        let now = (self.clock)();
        let mut buckets = self.buckets.lock();
        let b = buckets.entry(key.to_string()).or_insert(Bucket {
            tokens: self.burst,
            last_refill: now,
            last_use: now,
        });
        let elapsed = (now - b.last_refill).max(0) as f64;
        b.tokens = (b.tokens + elapsed * self.rate).min(self.burst);
        b.last_refill = now;
        b.last_use = now;
        if b.tokens >= 1.0 {
            b.tokens -= 1.0;
            true
        } else {
            false
        }
    }

    /// Drops buckets idle past the TTL so the map stays bounded under churn.
    pub fn gc(&self) {
        let now = (self.clock)();
        self.buckets
            .lock()
            .retain(|_, b| now - b.last_use <= self.ttl);
    }
}

/// A per-IP failed-attempt rate limiter: N events within `window` per key
/// triggers a block for `block`, and every event optionally incurs a fixed
/// delay. Reused for both the login form and `/oauth/register` (DCR), with
/// different tuning.
pub struct Throttle {
    window: i64,
    max: i32,
    block: i64,
    pub failure_delay_ms: u64,
    clock: Clock,
    fails: Mutex<HashMap<String, FailRecord>>,
}

#[derive(Default)]
struct FailRecord {
    count: i32,
    last_fail: i64,
    blocked_until: i64,
}

impl Throttle {
    pub fn new(window: i64, max: i32, block: i64, failure_delay_ms: u64, clock: Clock) -> Self {
        Self {
            window,
            max,
            block,
            failure_delay_ms,
            clock,
            fails: Mutex::new(HashMap::new()),
        }
    }

    /// Atomic check + count. If not blocked, admits AND increments (arming the
    /// block if the threshold trips); returns `(true, 0)`. If blocked, returns
    /// `(false, retry_secs)` without changing state. Counting on admission is
    /// what closes the parallel-burst race.
    pub fn try_acquire(&self, key: &str) -> (bool, i64) {
        let now = (self.clock)();
        let mut fails = self.fails.lock();
        if let Some(r) = fails.get(key)
            && now < r.blocked_until
        {
            return (false, r.blocked_until - now);
        }
        let r = fails.entry(key.to_string()).or_default();
        if now - r.last_fail > self.window {
            // Stale window — start fresh.
            *r = FailRecord::default();
        }
        r.count += 1;
        r.last_fail = now;
        if r.count >= self.max {
            r.blocked_until = now + self.block;
        }
        (true, 0)
    }

    /// Clears the counter for `key` (a successful login) so a fat-fingering
    /// legitimate user isn't penalized later.
    pub fn record_success(&self, key: &str) {
        self.fails.lock().remove(key);
    }

    pub fn gc(&self) {
        let now = (self.clock)();
        self.fails
            .lock()
            .retain(|_, r| !(now > r.blocked_until && now - r.last_fail > self.window));
    }
}

/// A stable per-source identifier for rate-limiting under the deployment
/// topology where this process sits behind a single reverse proxy on loopback.
///
/// XFF spoofing is real: proxies append the client IP, so the LEFTMOST element
/// is attacker-chosen. We therefore: trust RemoteAddr and ignore XFF when the
/// peer isn't loopback; when it IS loopback, take the RIGHTMOST XFF entry (the
/// one our trusted proxy just appended) and ignore everything left of it.
pub fn client_ip(remote: IpAddr, xff: Option<&str>) -> String {
    if !remote.is_loopback() {
        return remote.to_string();
    }
    match xff {
        None => remote.to_string(),
        Some(xff) => match xff.rsplit_once(',') {
            Some((_, last)) => last.trim().to_string(),
            None => xff.trim().to_string(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicI64, Ordering};

    fn fixed_clock(t: i64) -> (Clock, Arc<AtomicI64>) {
        let now = Arc::new(AtomicI64::new(t));
        let n = now.clone();
        (Arc::new(move || n.load(Ordering::SeqCst)), now)
    }

    #[test]
    fn sub_limiter_burst_then_refill() {
        let (clock, now) = fixed_clock(0);
        let lim = SubLimiter::new(5.0, 3, 1800, clock);
        // burst 3 in the same instant
        assert!(lim.allow("a"));
        assert!(lim.allow("a"));
        assert!(lim.allow("a"));
        assert!(!lim.allow("a"), "burst exhausted");
        // advance 1s → +5 tokens (capped at burst 3)
        now.store(1, Ordering::SeqCst);
        assert!(lim.allow("a"));
        // a different subject has its own full bucket
        assert!(lim.allow("b"));
    }

    #[test]
    fn throttle_blocks_after_max_then_success_resets() {
        let (clock, _now) = fixed_clock(100);
        let t = Throttle::new(60, 3, 900, 0, clock);
        assert!(t.try_acquire("ip").0);
        assert!(t.try_acquire("ip").0);
        let (ok, _) = t.try_acquire("ip"); // 3rd trips the block
        assert!(ok);
        let (ok4, retry) = t.try_acquire("ip");
        assert!(!ok4, "blocked after max");
        assert!(retry > 0);
        // success clears it
        t.record_success("ip");
        assert!(t.try_acquire("ip").0);
    }

    #[test]
    fn client_ip_xff_logic() {
        let loopback: IpAddr = "127.0.0.1".parse().unwrap();
        let public: IpAddr = "203.0.113.7".parse().unwrap();
        // non-loopback peer → ignore XFF entirely
        assert_eq!(client_ip(public, Some("1.2.3.4")), "203.0.113.7");
        // loopback peer → rightmost XFF entry (proxy-appended)
        assert_eq!(client_ip(loopback, Some("1.2.3.4, 5.6.7.8")), "5.6.7.8");
        assert_eq!(client_ip(loopback, Some("9.9.9.9")), "9.9.9.9");
        // loopback peer, no XFF → peer
        assert_eq!(client_ip(loopback, None), "127.0.0.1");
    }
}
