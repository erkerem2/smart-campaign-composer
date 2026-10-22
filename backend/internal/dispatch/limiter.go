package dispatch

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Clock abstracts time so the limiter and the retry loop can be tested
// deterministically.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is done (then it returns ctx.Err()).
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Limiter caps request starts at Limit per Window, as a hard guarantee.
//
// It combines two mechanisms:
//
//   - a token bucket (rate Limit/Window, small burst) that spreads requests
//     evenly instead of firing a whole second's budget at once;
//   - a sliding-window log holding the timestamps of the last Limit grants.
//     A grant is only issued when the Limit-th previous grant is at least
//     Window old, so ANY half-open interval of length Window contains at most
//     Limit grants, whatever the scheduling jitter or burst setting.
//
// A token bucket alone only bounds the average: with burst B it admits up to
// Limit+B requests in some 1-second windows. The log closes that gap.
//
// Every attempt, including retries, takes a slot: the provider counts
// requests, not unique messages. One Limiter is shared by all campaigns that
// send on the same channel.
type Limiter struct {
	clock  Clock
	limit  int
	window time.Duration

	mu sync.Mutex
	// token bucket
	rate   float64 // tokens per nanosecond
	burst  float64
	tokens float64
	last   time.Time
	// sliding-window log: ring of the last `limit` grant times
	ring []time.Time
	head int // oldest entry once the ring is full
	size int

	onGrant func(time.Time) // test hook, called under mu
}

// NewLimiter returns a limiter admitting at most limit requests per window.
// burst (1..limit) is the token bucket depth; 1 gives perfectly even spacing.
func NewLimiter(limit int, window time.Duration, burst int, clock Clock) (*Limiter, error) {
	if limit <= 0 || window <= 0 {
		return nil, errors.New("dispatch: limiter needs limit > 0 and window > 0")
	}
	if burst <= 0 || burst > limit {
		return nil, errors.New("dispatch: limiter burst must be in 1..limit")
	}
	if clock == nil {
		clock = realClock{}
	}
	return &Limiter{
		clock:  clock,
		limit:  limit,
		window: window,
		rate:   float64(limit) / float64(window),
		burst:  float64(burst),
		tokens: float64(burst),
		ring:   make([]time.Time, limit),
	}, nil
}

// Wait blocks until a request may start or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		now := l.clock.Now()
		wait := l.delay(now)
		if wait <= 0 {
			l.grant(now)
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		// Sleep outside the lock, then re-check: another waiter may have
		// taken the slot in the meantime.
		if err := l.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// delay returns how long the caller must wait at time now. Called under mu.
func (l *Limiter) delay(now time.Time) time.Duration {
	if l.last.IsZero() {
		l.last = now
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens = min(l.burst, l.tokens+float64(elapsed)*l.rate)
		l.last = now
	}
	var wait time.Duration
	if l.tokens < 1 {
		// Round up so we never wake a nanosecond too early and spin.
		wait = time.Duration((1-l.tokens)/l.rate) + 1
	}
	if l.size == l.limit {
		if w := l.ring[l.head].Add(l.window).Sub(now); w > wait {
			wait = w
		}
	}
	return wait
}

// grant records a request start at now. Called under mu.
func (l *Limiter) grant(now time.Time) {
	l.tokens--
	if l.size < l.limit {
		l.ring[(l.head+l.size)%l.limit] = now
		l.size++
	} else {
		l.ring[l.head] = now
		l.head = (l.head + 1) % l.limit
	}
	if l.onGrant != nil {
		l.onGrant(now)
	}
}
