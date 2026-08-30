package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// newTestLimiter returns a limiter whose clock is under the test's
// control, so refill and eviction behavior can be exercised at full speed
// instead of by sleeping through authRateRefill.
func newTestLimiter(burst int, refill, idleTTL time.Duration) (*rateLimiter, func(time.Duration)) {
	rl := newRateLimiter(burst, refill, idleTTL)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return now }
	return rl, func(d time.Duration) { now = now.Add(d) }
}

func TestRateLimiterAllowsBurstThenRefuses(t *testing.T) {
	rl, _ := newTestLimiter(3, time.Second, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}

	ok, retryAfter := rl.allow("1.2.3.4")
	if ok {
		t.Fatal("expected the request past the burst to be refused")
	}
	if retryAfter <= 0 || retryAfter > time.Second {
		t.Errorf("retryAfter = %v, want a positive duration no larger than the refill interval", retryAfter)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	rl, advance := newTestLimiter(2, time.Second, time.Minute)

	rl.allow("1.2.3.4")
	rl.allow("1.2.3.4")
	if ok, _ := rl.allow("1.2.3.4"); ok {
		t.Fatal("bucket should be empty after spending the whole burst")
	}

	// Half an interval isn't a whole token yet.
	advance(500 * time.Millisecond)
	if ok, _ := rl.allow("1.2.3.4"); ok {
		t.Error("half a refill interval should not yield a usable token")
	}

	advance(500 * time.Millisecond)
	if ok, _ := rl.allow("1.2.3.4"); !ok {
		t.Error("a full refill interval should yield exactly one token")
	}
	if ok, _ := rl.allow("1.2.3.4"); ok {
		t.Error("that one refilled token should have been the only one available")
	}
}

// Refill must not accumulate past the burst size, or an IP could sit idle
// for an hour and then get an hour's worth of attempts in one go — which
// is precisely the burst this limiter exists to prevent.
func TestRateLimiterCapsRefillAtBurst(t *testing.T) {
	rl, advance := newTestLimiter(3, time.Second, time.Hour)

	rl.allow("1.2.3.4")
	advance(10 * time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d should be allowed from a fully refilled bucket", i+1)
		}
	}
	if ok, _ := rl.allow("1.2.3.4"); ok {
		t.Error("a long idle period should refill to the burst cap, not beyond it")
	}
}

func TestRateLimiterIsolatesKeys(t *testing.T) {
	rl, _ := newTestLimiter(1, time.Second, time.Minute)

	if ok, _ := rl.allow("1.1.1.1"); !ok {
		t.Fatal("first caller should be allowed")
	}
	if ok, _ := rl.allow("1.1.1.1"); ok {
		t.Fatal("first caller should now be out of tokens")
	}
	if ok, _ := rl.allow("2.2.2.2"); !ok {
		t.Error("a different IP must have its own bucket, not inherit an exhausted one")
	}
}

func TestRateLimiterReapsIdleBuckets(t *testing.T) {
	rl, advance := newTestLimiter(2, time.Second, 5*time.Minute)

	rl.allow("1.1.1.1")
	rl.allow("2.2.2.2")
	if len(rl.buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(rl.buckets))
	}

	// Past the TTL, a touch from an unrelated key should sweep both of the
	// now-idle entries (and leave its own behind).
	advance(6 * time.Minute)
	rl.allow("3.3.3.3")

	if _, ok := rl.buckets["1.1.1.1"]; ok {
		t.Error("idle bucket 1.1.1.1 should have been reaped")
	}
	if _, ok := rl.buckets["2.2.2.2"]; ok {
		t.Error("idle bucket 2.2.2.2 should have been reaped")
	}
	if _, ok := rl.buckets["3.3.3.3"]; !ok {
		t.Error("the bucket that triggered the sweep should still be present")
	}
}

func TestClientIPStripsEphemeralPort(t *testing.T) {
	for _, tc := range []struct {
		remoteAddr string
		want       string
	}{
		{"203.0.113.7:54321", "203.0.113.7"},
		{"203.0.113.7:54322", "203.0.113.7"}, // a new connection must map to the same bucket
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"no-port-here", "no-port-here"}, // falls back rather than dropping the key
	} {
		got := clientIP(&http.Request{RemoteAddr: tc.remoteAddr})
		if got != tc.want {
			t.Errorf("clientIP(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
		}
	}
}

// A forged X-Forwarded-For must not create a fresh bucket: honoring it
// would let one caller bypass the limit outright just by varying a header.
func TestLimitIgnoresForwardedForHeader(t *testing.T) {
	rl, _ := newTestLimiter(1, time.Second, time.Minute)

	handler := rl.limit(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	first := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "198.51.100.5:1111"
	handler(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "198.51.100.5:2222" // same host, new connection
	req.Header.Set("X-Forwarded-For", "10.0.0.99")
	handler(second, req)

	if second.Code != http.StatusTooManyRequests {
		t.Errorf("got %d, want 429 — X-Forwarded-For must not earn a fresh bucket", second.Code)
	}
	retryAfter := second.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("a 429 should carry a Retry-After header")
	} else if n, err := strconv.Atoi(retryAfter); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive whole number of seconds", retryAfter)
	}
}

// A rate-limited /register must not just return 429 — it must not create
// the account either. Wires the real registerHandler (not a dummy) behind
// the real limit() middleware, so this fails if the 429 response is ever
// written without the handler actually stopping there.
func TestRateLimitedRegisterDoesNotCreateAccount(t *testing.T) {
	st := openTestStore(t)
	rl, _ := newTestLimiter(1, time.Hour, time.Hour)
	handler := rl.limit(registerHandler(st, ""))

	first := postJSON(t, handler, `{"username":"alice","password":"hunter2"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first request: got %d, want 201 (%s)", first.Code, first.Body.String())
	}

	second := postJSON(t, handler, `{"username":"bob","password":"hunter2"}`)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429 (%s)", second.Code, second.Body.String())
	}

	if _, err := st.GetUserByUsername("bob"); err == nil {
		t.Fatal("rate-limited registration created the account anyway")
	}
}

func TestLimitPassesThroughUnderTheLimit(t *testing.T) {
	rl, _ := newTestLimiter(5, time.Second, time.Minute)

	called := 0
	handler := rl.limit(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/register", nil)
		req.RemoteAddr = "198.51.100.6:1234"
		handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
	if called != 5 {
		t.Errorf("wrapped handler ran %d times, want 5", called)
	}
}
