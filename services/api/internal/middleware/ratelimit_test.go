package middleware

import (
	"fmt"
	"testing"
)

// TestIPLimiterPerIPBucket verifies the per-IP token bucket still
// throttles a single source after `burst` attempts.
func TestIPLimiterPerIPBucket(t *testing.T) {
	l := NewIPLimiter(0, 5) // refill disabled: exactly 5 tokens
	defer l.Stop()

	for i := 1; i <= 5; i++ {
		if !l.Allow("1.1.1.1") {
			t.Fatalf("attempt %d should have been allowed", i)
		}
	}
	if l.Allow("1.1.1.1") {
		t.Fatal("6th attempt from the same IP should have been throttled")
	}
}

// TestIPLimiterGlobalBackstop is the regression test for detail.txt
// #59: an attacker who can vary the client-IP key (header spoofing
// through the H3C relay, or a botnet) must not be able to get an
// unlimited number of attempts by rotating that key.
func TestIPLimiterGlobalBackstop(t *testing.T) {
	// burst=2 => global burst = max(2*6, 20) = 20.
	l := NewIPLimiter(0, 2)
	defer l.Stop()

	allowed := 0
	for i := 0; i < 200; i++ {
		if l.Allow(fmt.Sprintf("10.0.0.%d", i)) {
			allowed++
		}
	}

	// Before the fix every one of the 200 rotating keys got its own
	// full bucket, so this would have been 200.
	if allowed > 20 {
		t.Fatalf("global backstop allowed %d attempts, want at most 20", allowed)
	}
	if allowed < 20 {
		t.Fatalf("global backstop rejected too early: only %d allowed", allowed)
	}
}

// TestIPLimiterSurvivesStop documents that Stop only stops the GC
// goroutine — Allow keeps working (used by tests and by shutdown paths).
func TestIPLimiterSurvivesStop(t *testing.T) {
	l := NewIPLimiter(0, 3)
	l.Stop()
	l.Stop() // idempotent

	if !l.Allow("2.2.2.2") {
		t.Fatal("Allow should keep working after Stop")
	}
}
