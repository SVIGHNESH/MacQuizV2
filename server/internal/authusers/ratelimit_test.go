package authusers

import (
	"testing"
	"time"
)

func TestRateLimiterBlocksOverLimitThenRecovers(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	now := time.Now()

	for i := range 3 {
		if ok, _ := rl.allow("k", now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("hit %d refused, want allowed", i+1)
		}
	}
	ok, retry := rl.allow("k", now.Add(3*time.Second))
	if ok {
		t.Fatal("4th hit inside the window allowed, want refused")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter = %v, want positive", retry)
	}

	// Other keys are independent.
	if ok, _ := rl.allow("other", now); !ok {
		t.Fatal("independent key refused")
	}

	// After the window slides past the oldest hits, the key recovers.
	if ok, _ := rl.allow("k", now.Add(2*time.Minute)); !ok {
		t.Fatal("hit after window elapsed refused, want allowed")
	}
}
