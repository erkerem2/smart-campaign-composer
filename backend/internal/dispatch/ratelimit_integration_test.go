package dispatch_test

// Rate limiting and retry behaviour against the real mock provider over
// loopback HTTP, with in-memory persistence (internal/memstore) so these
// timing-sensitive tests do not depend on SQLite. Assertions are made on what
// the PROVIDER observed.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/memstore"
	"smart-campaign-composer/backend/internal/provider"
)

func rlProvider(t *testing.T, cfg provider.Config) (*provider.MockServer, *provider.Client) {
	t.Helper()
	cfg.RecordAttempts = true
	m, err := provider.NewMockServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	t.Cleanup(tr.CloseIdleConnections)
	return m, provider.NewClient(srv.URL, &http.Client{Transport: tr, Timeout: 10 * time.Second})
}

func rlUsers(first, n int) []domain.User {
	out := make([]domain.User, n)
	for i := range out {
		id := int64(first + i)
		out[i] = domain.User{ID: id, FirstName: "Ayşe", Phone: fmt.Sprintf("+90532%07d", id),
			Email: fmt.Sprintf("u%d@example.com", id), OptInSMS: true, OptInEmail: true}
	}
	return out
}

func rlWait(t *testing.T, r *dispatch.Run) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("run did not finish")
	}
}

// rlMaxPerWindow: the largest number of attempts in any closed window.
func rlMaxPerWindow(ts []time.Time, window time.Duration) int {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	best, j := 0, 0
	for i := range ts {
		for j < len(ts) && ts[j].Sub(ts[i]) <= window {
			j++
		}
		best = max(best, j-i)
	}
	return best
}

func campaignOf(key string) string { return strings.SplitN(key, ":", 2)[0] }

// B5. Two campaigns send on the same channel at the same time. The limiter is
// shared, so together they stay within 100 requests per second as seen by
// the provider — and neither campaign is starved by the other.
func TestTwoCampaignsShareTheChannelLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("real 100 msg/s limit: a few seconds")
	}
	mock, client := rlProvider(t, provider.Config{MinLatency: 5 * time.Millisecond, MaxLatency: 20 * time.Millisecond})
	store, campaigns := memstore.NewStore(), memstore.NewCampaigns()
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	campaigns.Add(2, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	d, err := dispatch.New(store, campaigns, client, dispatch.NewComposerRenderer(nil), dispatch.DefaultConfig(),
		dispatch.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	r1, err := d.Start(context.Background(), 1, dispatch.StaticAudience(rlUsers(1, 150)...))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := d.Start(context.Background(), 2, dispatch.StaticAudience(rlUsers(1001, 150)...))
	if err != nil {
		t.Fatal(err)
	}
	rlWait(t, r1)
	rlWait(t, r2)
	elapsed := time.Since(began)

	var times []time.Time
	var accepted []provider.Attempt
	for _, a := range mock.Attempts() {
		if a.Channel == "sms" {
			times = append(times, a.At)
			if a.Status == http.StatusAccepted {
				accepted = append(accepted, a)
			}
		}
	}
	if got := rlMaxPerWindow(times, time.Second); got > 100 {
		t.Fatalf("provider saw %d SMS requests in one second from two campaigns, limit is 100", got)
	}
	// 300 requests at ≤ 100/s cannot be done much faster than 3 s: the
	// shared limiter was the bottleneck, not luck.
	if elapsed < 2500*time.Millisecond {
		t.Fatalf("300 messages took only %v: was the limiter shared?", elapsed)
	}
	if len(accepted) != 300 {
		t.Fatalf("provider accepted %d messages, want 300", len(accepted))
	}
	// Fairness: in the first half of the traffic both campaigns progressed.
	sort.Slice(accepted, func(i, j int) bool { return accepted[i].At.Before(accepted[j].At) })
	share := map[string]int{}
	for _, a := range accepted[:150] {
		share[campaignOf(a.IdempotencyKey)]++
	}
	if share["1"] < 40 || share["2"] < 40 {
		t.Fatalf("one campaign starved the other in the first 150 messages: %v", share)
	}
	for _, id := range []int64{1, 2} {
		if st := campaigns.Status(id); st != domain.CampaignStatusCompleted {
			t.Errorf("campaign %d: %s", id, st)
		}
	}
}

// B7. A provider storm: 90% of requests answered 429. Every message is tried
// at most 1 + MaxRetries times, every attempt (retries included) goes through
// the limiter, nothing is accepted twice and every delivery ends final.
func TestRateLimitStormKeepsRetryBound(t *testing.T) {
	if testing.Short() {
		t.Skip("real 100 msg/s limit: a few seconds")
	}
	mock, client := rlProvider(t, provider.Config{RateLimitedRatio: 0.9, MinLatency: time.Millisecond,
		MaxLatency: 5 * time.Millisecond, RetryAfter: 0, Seed: 20261026})
	store, campaigns := memstore.NewStore(), memstore.NewCampaigns()
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	cfg := dispatch.DefaultConfig()
	cfg.BaseBackoff, cfg.MaxBackoff = 10*time.Millisecond, 40*time.Millisecond
	d, err := dispatch.New(store, campaigns, client, dispatch.NewComposerRenderer(nil), cfg,
		dispatch.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	users := rlUsers(1, 60)
	run, err := d.Start(context.Background(), 1, dispatch.StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	rlWait(t, run)

	attempts := map[string]int{}
	acceptedKeys := map[string]int{}
	var times []time.Time
	for _, a := range mock.Attempts() {
		attempts[a.IdempotencyKey]++
		times = append(times, a.At)
		if a.Status == http.StatusAccepted {
			acceptedKeys[a.IdempotencyKey]++
		}
	}
	if got := rlMaxPerWindow(times, time.Second); got > 100 {
		t.Fatalf("%d requests in one second during the storm, limit is 100 (retries count)", got)
	}
	totalAttempts := 0
	for key, n := range attempts {
		totalAttempts += n
		if n > 1+cfg.MaxRetries {
			t.Errorf("%s attempted %d times, max %d", key, n, 1+cfg.MaxRetries)
		}
		if acceptedKeys[key] > 1 {
			t.Errorf("%s accepted %d times", key, acceptedKeys[key])
		}
	}
	sent, failed := 0, 0
	for _, u := range users {
		row, ok := store.Row(1, u.ID, domain.ChannelSMS)
		switch {
		case !ok:
			t.Fatalf("user %d has no dispatch log", u.ID)
		case row.Status == domain.DispatchStatusSent:
			sent++
		case row.Status == domain.DispatchStatusFailed:
			failed++
			if row.Retries != cfg.MaxRetries || !strings.Contains(row.LastError, fmt.Sprintf("gave up after %d retries", cfg.MaxRetries)) {
				t.Errorf("user %d failed with retries=%d error=%q", u.ID, row.Retries, row.LastError)
			}
		default:
			t.Errorf("user %d left %s", u.ID, row.Status)
		}
	}
	if sent+failed != len(users) || sent != len(acceptedKeys) {
		t.Fatalf("sent=%d failed=%d accepted=%d", sent, failed, len(acceptedKeys))
	}
	if sent == 0 || failed == 0 {
		t.Fatalf("a 90%% storm should both deliver and give up on some: sent=%d failed=%d", sent, failed)
	}
	s, _ := d.Stats(context.Background(), 1)
	if s.Retried != int64(totalAttempts-len(users)) {
		t.Errorf("stats retried=%d, provider saw %d retries", s.Retried, totalAttempts-len(users))
	}
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Errorf("campaign status = %s", st)
	}
	t.Logf("storm: %d attempts for %d messages, sent %d, gave up %d", totalAttempts, len(users), sent, failed)
}
