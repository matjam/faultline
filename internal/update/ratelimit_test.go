package update

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRateLimitDelay_NotRateLimitErr(t *testing.T) {
	if d, ok := rateLimitDelay(errors.New("ordinary network error")); ok {
		t.Errorf("non-rate-limit error returned ok=true (%v)", d)
	}
	if d, ok := rateLimitDelay(nil); ok {
		t.Errorf("nil error returned ok=true (%v)", d)
	}
}

func TestRateLimitDelay_UsesResetAt(t *testing.T) {
	resetAt := time.Now().Add(45 * time.Minute)
	err := &RateLimitError{ResetAt: resetAt}
	d, ok := rateLimitDelay(err)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Should be ~45m + jitter (0-30s); allow generous slack for the
	// time we spent in the call itself.
	min := 44 * time.Minute
	max := 46*time.Minute + time.Second
	if d < min || d > max {
		t.Errorf("delay = %v; want roughly 45m", d)
	}
}

func TestRateLimitDelay_PastResetUsesFloor(t *testing.T) {
	// ResetAt in the past (clock skew, late wakeup) should not produce
	// a negative or zero delay.
	err := &RateLimitError{ResetAt: time.Now().Add(-1 * time.Hour)}
	d, ok := rateLimitDelay(err)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if d < rateLimitMinBackoff {
		t.Errorf("delay = %v; want at least %v", d, rateLimitMinBackoff)
	}
}

func TestRateLimitDelay_FutureResetCappedAtMax(t *testing.T) {
	// ResetAt absurdly far in the future (server clock skew, bug)
	// should be capped so the updater doesn't lock up.
	err := &RateLimitError{ResetAt: time.Now().Add(48 * time.Hour)}
	d, ok := rateLimitDelay(err)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if d > rateLimitMaxBackoff {
		t.Errorf("delay = %v; want at most %v", d, rateLimitMaxBackoff)
	}
}

func TestRateLimitDelay_MissingResetUsesDefault(t *testing.T) {
	err := &RateLimitError{}
	d, ok := rateLimitDelay(err)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// Default + jitter: between rateLimitDefaultBackoff and
	// rateLimitDefaultBackoff + 30s.
	if d < rateLimitDefaultBackoff || d > rateLimitDefaultBackoff+30*time.Second {
		t.Errorf("delay = %v; want default ~%v", d, rateLimitDefaultBackoff)
	}
}

func TestRateLimitDelay_UnwrapsWrappedError(t *testing.T) {
	inner := &RateLimitError{ResetAt: time.Now().Add(10 * time.Minute)}
	wrapped := fmt.Errorf("github releases: %w", inner)
	d, ok := rateLimitDelay(wrapped)
	if !ok {
		t.Fatal("expected ok=true through error wrap")
	}
	if d < 9*time.Minute {
		t.Errorf("delay = %v; want roughly 10m", d)
	}
}

func TestRateLimitError_MessageWithReset(t *testing.T) {
	resetAt := time.Now().Add(30 * time.Minute)
	err := &RateLimitError{ResetAt: resetAt}
	msg := err.Error()
	if msg == "" {
		t.Error("empty error message")
	}
	if !contains(msg, "rate limit exceeded") {
		t.Errorf("message missing 'rate limit exceeded': %q", msg)
	}
}

func TestRateLimitError_MessageWithoutReset(t *testing.T) {
	err := &RateLimitError{}
	msg := err.Error()
	if !contains(msg, "reset time unknown") {
		t.Errorf("message should note unknown reset; got %q", msg)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
