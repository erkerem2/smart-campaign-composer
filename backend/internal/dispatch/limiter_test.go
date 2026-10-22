package dispatch

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a virtual clock: Sleep advances time instead of blocking.
// It is safe for concurrent use (time only moves forward).
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if d > 0 {
		c.now = c.now.Add(d)
	}
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// maxInWindow returns the largest number of timestamps inside any half-open
// interval [t, t+window).
func maxInWindow(ts []time.Time, window time.Duration) int {
	sorted := append([]time.Time(nil), ts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	best, j := 0, 0
	for i := range sorted {
		for j < len(sorted) && sorted[j].Sub(sorted[i]) < window {
			j++
		}
		best = max(best, j-i)
	}
	return best
}

func recordGrants(l *Limiter) *[]time.Time {
	var grants []time.Time
	l.onGrant = func(t time.Time) { grants = append(grants, t) } // called under l.mu
	return &grants
}

func TestLimiterNeverExceedsLimitPerWindow(t *testing.T) {
	for _, burst := range []int{1, 10, 100} {
		clock := newFakeClock()
		l, err := NewLimiter(100, time.Second, burst, clock)
		if err != nil {
			t.Fatal(err)
		}
		grants := recordGrants(l)
		start := clock.Now()
		for i := 0; i < 350; i++ {
			if err := l.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if got := maxInWindow(*grants, time.Second); got > 100 {
			t.Fatalf("burst %d: %d grants in one second, limit is 100", burst, got)
		}
		// 350 requests at 100/s cannot finish before ~2.5 s of (virtual) time.
		if elapsed := clock.Now().Sub(start); elapsed < 2490*time.Millisecond {
			t.Fatalf("burst %d: 350 grants in %v", burst, elapsed)
		}
	}
}

// With burst == limit a token bucket alone would admit 100 requests at t=0
// and ~100 more during the next second (≈200 per window). The sliding-window
// log is what keeps the hard cap.
func TestLimiterSlidingWindowCapsFullBurst(t *testing.T) {
	clock := newFakeClock()
	l, _ := NewLimiter(100, time.Second, 100, clock)
	grants := recordGrants(l)
	for i := 0; i < 200; i++ {
		_ = l.Wait(context.Background())
	}
	first, g := (*grants)[0], *grants
	if !g[99].Equal(first) {
		t.Fatal("the full burst should be admitted immediately")
	}
	if d := g[100].Sub(first); d < time.Second {
		t.Fatalf("101st request admitted after %v, must wait a full window", d)
	}
}

// Real clock, many goroutines: the invariant holds on the timestamps the
// limiter actually used.
func TestLimiterConcurrentRealClock(t *testing.T) {
	l, err := NewLimiter(20, 100*time.Millisecond, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var grants []time.Time
	l.onGrant = func(t time.Time) { mu.Lock(); grants = append(grants, t); mu.Unlock() }

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				if err := l.Wait(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if len(grants) != 96 {
		t.Fatalf("%d grants", len(grants))
	}
	if got := maxInWindow(grants, 100*time.Millisecond); got > 20 {
		t.Fatalf("%d grants within 100ms, limit 20", got)
	}
}

func TestLimiterHonoursContext(t *testing.T) {
	l, _ := NewLimiter(1, time.Hour, 1, nil)
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewLimiterValidates(t *testing.T) {
	for _, c := range []struct{ limit, burst int }{{0, 1}, {10, 0}, {10, 11}} {
		if _, err := NewLimiter(c.limit, time.Second, c.burst, nil); err == nil {
			t.Errorf("limit=%d burst=%d accepted", c.limit, c.burst)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Base: 200 * time.Millisecond, Max: 5 * time.Second, MaxRetryAfter: 30 * time.Second}
	low, high := b, b
	low.Rand = func() float64 { return 0 }
	high.Rand = func() float64 { return 0.999999 }

	// Exponential growth with equal jitter: delay(n) in [exp/2, exp).
	for n, exp := range map[int]time.Duration{1: 200 * time.Millisecond, 2: 400 * time.Millisecond, 3: 800 * time.Millisecond, 10: 5 * time.Second} {
		if got := low.Delay(n, 0); got != exp/2 {
			t.Errorf("retry %d min = %v, want %v", n, got, exp/2)
		}
		if got := high.Delay(n, 0); got < exp*99/100 || got >= exp {
			t.Errorf("retry %d max = %v, want just below %v", n, got, exp)
		}
	}
	// Retry-After is a floor (plus jitter so workers do not return together).
	if got := low.Delay(1, time.Second); got != time.Second {
		t.Errorf("Retry-After floor: %v", got)
	}
	if got := high.Delay(1, time.Second); got < time.Second || got >= time.Second+b.Base {
		t.Errorf("Retry-After jitter: %v", got)
	}
	// A longer computed backoff wins over a short Retry-After.
	if got := low.Delay(5, time.Second); got != 1600*time.Millisecond {
		t.Errorf("retry 5 with Retry-After 1s = %v", got)
	}
	// Absurd Retry-After values are capped.
	if got := low.Delay(1, time.Hour); got != 30*time.Second {
		t.Errorf("capped Retry-After = %v", got)
	}
}
