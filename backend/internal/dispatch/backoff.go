package dispatch

import "time"

// Backoff computes retry delays: exponential growth with "equal jitter",
// never shorter than what the provider asked for in Retry-After.
//
//	exp(n)   = min(Max, Base * 2^(n-1))          n = 1, 2, 3 ...
//	delay(n) = exp(n)/2 + rand[0, exp(n)/2)      in [exp/2, exp)
//	if Retry-After > delay: delay = Retry-After + rand[0, Base)
//
// Equal jitter keeps a guaranteed minimum wait (unlike full jitter, which can
// retry immediately) while still de-synchronising workers that failed at the
// same moment. The extra jitter on top of Retry-After stops every worker that
// got a 429 in the same second from returning in the same millisecond.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
	// MaxRetryAfter caps the provider's Retry-After so a misbehaving
	// provider cannot park a worker indefinitely.
	MaxRetryAfter time.Duration
	// Rand returns a value in [0, 1). It is injectable for tests.
	Rand func() float64
}

// Delay returns the wait before retry number n (1-based).
func (b Backoff) Delay(n int, retryAfter time.Duration) time.Duration {
	if n < 1 {
		n = 1
	}
	exp := b.Base
	for i := 1; i < n && exp < b.Max; i++ {
		exp *= 2
	}
	if exp > b.Max {
		exp = b.Max
	}
	d := exp/2 + time.Duration(b.Rand()*float64(exp/2))
	if retryAfter > 0 {
		if b.MaxRetryAfter > 0 && retryAfter > b.MaxRetryAfter {
			retryAfter = b.MaxRetryAfter
		}
		if d < retryAfter {
			d = retryAfter + time.Duration(b.Rand()*float64(b.Base))
		}
	}
	return d
}
