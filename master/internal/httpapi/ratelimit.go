package httpapi

import (
	"sync"
	"time"
)

// rateLimiter is an in-memory token bucket per key
// (docs/specs/protocol.md §3: 5 rps per player_id on matchmaking endpoints).
// Matchmaking keys are effectively public (baked into game clients), so this
// is abuse damping, not a security boundary.
type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
	sweepAt time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{
		rate: rate, burst: burst,
		buckets: map[string]*bucket{},
		sweepAt: time.Now().Add(time.Minute),
	}
}

// allow consumes one token for key, refilling at rate up to burst.
func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Occasional sweep of idle buckets keeps memory bounded.
	if now.After(rl.sweepAt) {
		for k, b := range rl.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(rl.buckets, k)
			}
		}
		rl.sweepAt = now.Add(time.Minute)
	}

	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	} else {
		b.tokens = min(rl.burst, b.tokens+rl.rate*now.Sub(b.last).Seconds())
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
