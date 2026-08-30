package main

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// /register and /login are the only unauthenticated endpoints, and
// bcrypt's deliberate slowness makes an unthrottled /login both a
// guessing oracle and a CPU-burn vector.
const (
	// authRateBurst: back-to-back attempts allowed before throttling.
	authRateBurst = 5

	// authRateRefill: sustained rate is 1 attempt per this interval.
	authRateRefill = 12 * time.Second

	// authRateIdleTTL: idle buckets are fully refilled anyway, so
	// dropping them after this long loses no state.
	authRateIdleTTL = 10 * time.Minute
)

// bucket is one IP's token allowance, refilled lazily from lastSeen
// rather than by a per-IP ticker.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// rateLimiter is a per-IP token bucket, safe for concurrent use — every
// http.Handler goroutine shares the one instance built in main.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	burst    float64
	refill   time.Duration
	idleTTL  time.Duration
	lastReap time.Time

	// now is swapped in tests to exercise refill/eviction without sleeping.
	now func() time.Time
}

func newRateLimiter(burst int, refill, idleTTL time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*bucket{},
		burst:   float64(burst),
		refill:  refill,
		idleTTL: idleTTL,
		now:     time.Now,
	}
}

// allow spends one token for key, reporting whether there was one to spend
// and, when there wasn't, how long until there will be.
func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	rl.reap(now)

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, lastSeen: now}
		rl.buckets[key] = b
	} else {
		b.tokens += float64(now.Sub(b.lastSeen)) / float64(rl.refill)
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.lastSeen = now
	}

	if b.tokens < 1 {
		// Time until one whole token — for the caller's Retry-After.
		return false, time.Duration((1 - b.tokens) * float64(rl.refill))
	}
	b.tokens--
	return true, 0
}

// reap drops buckets idle long enough to have fully refilled, so the
// map doesn't grow unbounded. Throttled to once per idleTTL.
func (rl *rateLimiter) reap(now time.Time) {
	if now.Sub(rl.lastReap) < rl.idleTTL {
		return
	}
	rl.lastReap = now
	for key, b := range rl.buckets {
		if now.Sub(b.lastSeen) >= rl.idleTTL {
			delete(rl.buckets, key)
		}
	}
}

// limit wraps next behind the token bucket, keyed on the TCP peer
// address — never X-Forwarded-For, which is attacker-controlled unless
// a trusted proxy sets it.
func (rl *rateLimiter) limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r)
		if ok, retryAfter := rl.allow(key); !ok {
			secs := int(retryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			log.Printf("rate limit: refused %s %s from %s", r.Method, r.URL.Path, key)
			http.Error(w, "too many requests, slow down", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// clientIP strips the ephemeral port so every connection from one host
// shares a bucket. Falls back to RemoteAddr if unparseable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
