package abuse

import "testing"

func TestIPRateLimiterBouncesAfterPerHourCalls(t *testing.T) {
	const perHour = 10
	l := NewIPRateLimiter(perHour, nil)
	ip := "203.0.113.42"

	// First perHour calls should all be allowed (full bucket).
	for i := 0; i < perHour; i++ {
		ok, _ := l.Allow(ip)
		if !ok {
			t.Fatalf("call %d unexpectedly rate-limited", i+1)
		}
	}

	// The (perHour+1)th call should be rejected.
	ok, retry := l.Allow(ip)
	if ok {
		t.Fatalf("call %d unexpectedly allowed; expected rate-limit", perHour+1)
	}
	if retry < 1 {
		t.Errorf("retryAfter = %d, expected at least 1 second", retry)
	}
}

func TestIPRateLimiterDifferentIPsHaveSeparateBuckets(t *testing.T) {
	l := NewIPRateLimiter(2, nil)

	ok1, _ := l.Allow("203.0.113.1")
	ok2, _ := l.Allow("203.0.113.2")
	if !ok1 || !ok2 {
		t.Fatalf("first call from each IP should succeed: %v %v", ok1, ok2)
	}
	ok1b, _ := l.Allow("203.0.113.1")
	ok2b, _ := l.Allow("203.0.113.2")
	if !ok1b || !ok2b {
		t.Fatalf("second call from each IP should succeed: %v %v", ok1b, ok2b)
	}
	// Now both buckets should be empty.
	ok1c, _ := l.Allow("203.0.113.1")
	ok2c, _ := l.Allow("203.0.113.2")
	if ok1c || ok2c {
		t.Fatalf("third call from each IP should be limited: %v %v", ok1c, ok2c)
	}
}

func TestIPRateLimiterDisabledWhenPerHourZero(t *testing.T) {
	l := NewIPRateLimiter(0, nil)
	for i := 0; i < 1000; i++ {
		ok, _ := l.Allow("203.0.113.99")
		if !ok {
			t.Fatalf("disabled limiter rejected call %d", i)
		}
	}
}
