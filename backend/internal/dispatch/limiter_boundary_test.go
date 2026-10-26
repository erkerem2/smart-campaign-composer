package dispatch

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/memstore"
)

// maxInClosedWindow is maxInWindow for closed intervals [t, t+window]: how a
// provider that counts "the last second" inclusively would see the traffic.
func maxInClosedWindow(ts []time.Time, window time.Duration) int {
	sorted := append([]time.Time(nil), ts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Before(sorted[j]) })
	best, j := 0, 0
	for i := range sorted {
		for j < len(sorted) && sorted[j].Sub(sorted[i]) <= window {
			j++
		}
		best = max(best, j-i)
	}
	return best
}

func waitN(t *testing.T, l *Limiter, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// B6. The limiter's guarantee is for half-open windows: the (limit+1)-th grant
// comes no earlier than exactly one window after the first. At that instant
// a closed window [t0, t0+window] holds limit+1 grants — which is why the
// dispatcher widens the window by RateSafetyMargin.
func TestLimiterExactWindowBoundary(t *testing.T) {
	clock := newFakeClock()
	l, err := NewLimiter(3, time.Second, 3, clock)
	if err != nil {
		t.Fatal(err)
	}
	grants := recordGrants(l)
	waitN(t, l, 7)
	g := *grants
	t0 := g[0]
	for i := 0; i < 3; i++ {
		if !g[i].Equal(t0) {
			t.Fatalf("grant %d at +%v, the burst should be immediate", i, g[i].Sub(t0))
		}
	}
	if d := g[3].Sub(t0); d < time.Second || d > time.Second+time.Millisecond {
		t.Fatalf("4th grant at +%v, want exactly one window (+1s)", d)
	}
	if got := maxInWindow(g, time.Second); got > 3 {
		t.Fatalf("%d grants in a half-open second, limit 3", got)
	}
	// The bucket refills to its full burst by t0+1s and the ring allows all
	// three again at that very instant: a closed window [t0, t0+1s] holds
	// 2 × limit grants. This is the case RateSafetyMargin exists for.
	if got := maxInClosedWindow(g, time.Second); got != 6 {
		t.Fatalf("closed window holds %d grants; this test documents the 2×limit boundary case", got)
	}
}

// A Config that leaves RateSafetyMargin at zero gets the default margin, like
// every other zero field; only a negative value disables it.
func TestZeroSafetyMarginMeansDefault(t *testing.T) {
	if got := (Config{}).withDefaults().RateSafetyMargin; got != DefaultConfig().RateSafetyMargin {
		t.Fatalf("zero margin became %v, want the default %v", got, DefaultConfig().RateSafetyMargin)
	}
	if got := (Config{RateSafetyMargin: -1}).withDefaults().RateSafetyMargin; got != 0 {
		t.Fatalf("negative margin became %v, want 0 (disabled)", got)
	}
}

// With the production margin (window 1 s + 50 ms) even a provider that
// counts closed one-second windows never sees more than the limit.
func TestLimiterSafetyMarginHoldsForClosedWindows(t *testing.T) {
	cfg := DefaultConfig()
	for _, burst := range []int{1, cfg.RateBurst, cfg.RateLimit} {
		clock := newFakeClock()
		l, err := NewLimiter(cfg.RateLimit, cfg.RateWindow+cfg.RateSafetyMargin, burst, clock)
		if err != nil {
			t.Fatal(err)
		}
		grants := recordGrants(l)
		waitN(t, l, 450)
		if got := maxInClosedWindow(*grants, time.Second); got > cfg.RateLimit {
			t.Fatalf("burst %d: %d grants in a closed second, limit %d", burst, got, cfg.RateLimit)
		}
	}
}

// RateLimit 1: one grant per window, never two in the same window.
func TestLimiterLimitOfOne(t *testing.T) {
	clock := newFakeClock()
	l, err := NewLimiter(1, time.Second, 1, clock)
	if err != nil {
		t.Fatal(err)
	}
	grants := recordGrants(l)
	waitN(t, l, 5)
	g := *grants
	for i := 1; i < len(g); i++ {
		if d := g[i].Sub(g[i-1]); d < time.Second {
			t.Fatalf("grants %d and %d only %v apart", i-1, i, d)
		}
	}
}

// A burst larger than the limit is clamped by the dispatcher's config, so a
// misconfiguration cannot open a 50-request burst on a 5/s channel.
func TestDispatcherClampsBurstToLimit(t *testing.T) {
	clock := newFakeClock()
	d, err := New(memstore.NewStore(), memstore.NewCampaigns(), &memSender{}, NewComposerRenderer(nil),
		Config{RateLimit: 5, RateBurst: 50, RateWindow: time.Second}, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	l := d.Limiter(domain.ChannelSMS)
	grants := recordGrants(l)
	waitN(t, l, 12)
	if got := maxInWindow(*grants, time.Second+DefaultConfig().RateSafetyMargin); got > 5 {
		t.Fatalf("%d grants in one window with RateLimit 5", got)
	}
	if !(*grants)[4].Equal((*grants)[0]) || (*grants)[5].Equal((*grants)[0]) {
		t.Fatal("burst should be clamped to exactly 5")
	}
}

// Many goroutines hitting the limiter at the same moment with the real
// clock: the closed-window cap holds on the timestamps it actually granted.
func TestLimiterConcurrentClosedWindowRealClock(t *testing.T) {
	if testing.Short() {
		t.Skip("real clock")
	}
	cfg := DefaultConfig()
	l, err := NewLimiter(cfg.RateLimit, cfg.RateWindow+cfg.RateSafetyMargin, cfg.RateBurst, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var grants []time.Time
	l.onGrant = func(ts time.Time) {
		mu.Lock()
		grants = append(grants, ts)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 7; i++ {
				_ = l.Wait(context.Background())
			}
		}()
	}
	wg.Wait()
	if got := maxInClosedWindow(grants, time.Second); got > cfg.RateLimit {
		t.Fatalf("%d grants in a closed second", got)
	}
}
