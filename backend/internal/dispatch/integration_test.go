package dispatch_test

// End-to-end tests: real SQLite (dispatch_logs, claims, opt-ins), the real
// HTTP client and the in-repo mock provider over loopback HTTP with its
// default chaos (~10% 429 + Retry-After: 1, ~5% 500, 10-30 ms latency).
// Assertions are made on what the PROVIDER observed, not only on our own
// bookkeeping.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/repository"
)

type world struct {
	campaigns  *repository.CampaignRepository
	dispatches *repository.DispatchRepository
	users      *repository.UserRepository
	campaignID int64
	all        []domain.User
}

func newWorld(t *testing.T, n int, optIn func(i int) (sms, email bool)) *world {
	t.Helper()
	ctx := context.Background()
	db, err := repository.Open(ctx, filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	w := &world{
		campaigns:  repository.NewCampaignRepository(db),
		dispatches: repository.NewDispatchRepository(db),
		users:      repository.NewUserRepository(db),
	}
	w.all = make([]domain.User, n)
	for i := range w.all {
		sms, email := optIn(i)
		w.all[i] = domain.User{
			FirstName: fmt.Sprintf("Kullanıcı%d", i), LastName: "Test",
			Email: fmt.Sprintf("it%d@example.com", i), Phone: fmt.Sprintf("+90555%07d", i),
			City: "İstanbul", Age: 25, OptInSMS: sms, OptInEmail: email, CreatedAt: time.Now().UTC(),
		}
	}
	if err := w.users.InsertBatch(ctx, w.all); err != nil {
		t.Fatal(err)
	}
	c, err := w.campaigns.Create(ctx, composer.ReferenceBrief, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.campaigns.SaveDraftMessages(ctx, c.ID, composer.ReferenceMessage.CampaignMessages(c.ID)); err != nil {
		t.Fatal(err)
	}
	if err := w.campaigns.Approve(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	w.campaignID = c.ID
	return w
}

func (w *world) audience() dispatch.AudienceSource {
	ids := make([]int64, len(w.all))
	for i, u := range w.all {
		ids[i] = u.ID
	}
	return dispatch.IDAudience(w.users, ids)
}

// expected returns the idempotency keys that must be delivered and the
// recipients that must never be contacted, per channel.
func (w *world) expected() (keys map[string]bool, forbidden map[string]bool) {
	keys, forbidden = map[string]bool{}, map[string]bool{}
	for _, u := range w.all {
		for _, ch := range domain.Channels {
			to := u.Phone
			if ch == domain.ChannelEmail {
				to = u.Email
			}
			if u.CanReceive(ch) {
				keys[domain.IdempotencyKey(w.campaignID, u.ID, ch)] = true
			} else {
				forbidden[string(ch)+"|"+to] = true
			}
		}
	}
	return keys, forbidden
}

func (w *world) dispatcher(t *testing.T, s dispatch.Sender, cfg dispatch.Config) *dispatch.Dispatcher {
	t.Helper()
	d, err := dispatch.New(w.dispatches, w.campaigns, s, dispatch.NewComposerRenderer(nil), cfg,
		dispatch.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func startMock(t *testing.T, cfg provider.Config, wrap func(http.Handler) http.Handler) (*provider.MockServer, *provider.Client) {
	t.Helper()
	cfg.RecordAttempts = true
	m, err := provider.NewMockServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := m.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	t.Cleanup(tr.CloseIdleConnections)
	return m, provider.NewClient(srv.URL, &http.Client{Transport: tr, Timeout: 10 * time.Second})
}

func wait(t *testing.T, r *dispatch.Run) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("run did not finish")
	}
}

func maxPerWindow(ts []time.Time, window time.Duration) int {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	best, j := 0, 0
	for i := range ts {
		for j < len(ts) && ts[j].Sub(ts[i]) < window {
			j++
		}
		best = max(best, j-i)
	}
	return best
}

// The headline test: a few hundred recipients through the flaky provider.
func TestDispatchThroughFlakyProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a few seconds (real 100 msg/s limit)")
	}
	w := newWorld(t, 300, func(i int) (bool, bool) { return i%10 < 7, i%5 != 0 })
	cfg := provider.DefaultConfig()
	cfg.Seed = 20261022
	mock, client := startMock(t, cfg, nil)
	d := w.dispatcher(t, client, dispatch.DefaultConfig())

	run, err := d.Start(context.Background(), w.campaignID, w.audience())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, run)
	if err := run.Err(); err != nil {
		t.Fatal(err)
	}

	attempts := mock.Attempts()
	wantKeys, forbidden := w.expected()
	byKey := map[string][]provider.Attempt{}
	arrivals := map[string][]time.Time{}
	for _, a := range attempts {
		byKey[a.IdempotencyKey] = append(byKey[a.IdempotencyKey], a)
		arrivals[a.Channel] = append(arrivals[a.Channel], a.At)
		// 1. Permission: nobody who opted out of a channel was contacted on it.
		if forbidden[a.Channel+"|"+a.To] {
			t.Fatalf("opted-out recipient %s contacted on %s", a.To, a.Channel)
		}
		if !wantKeys[a.IdempotencyKey] {
			t.Fatalf("unexpected delivery %s", a.IdempotencyKey)
		}
	}

	// 2. Rate: the provider never saw more than 100 requests (retries
	// included) in any 1-second window of one channel — and the limiter was
	// actually the bottleneck, so this is not passing by accident.
	for ch, ts := range arrivals {
		peak := maxPerWindow(ts, time.Second)
		t.Logf("%s: %d requests, peak %d per second", ch, len(ts), peak)
		if peak > 100 {
			t.Fatalf("%s: %d requests in one second, limit is 100", ch, peak)
		}
		if peak < 80 {
			t.Fatalf("%s: peak %d/s — the test did not exercise the limiter", ch, peak)
		}
	}

	// 3. Backoff: after a 429 the next attempt of the same message came no
	// sooner than Retry-After (1 s); after a 500 no sooner than the first
	// backoff floor (Base/2 = 100 ms). At most 1 + 3 attempts per message.
	saw429Retry := false
	for key, as := range byKey {
		sort.Slice(as, func(i, j int) bool { return as[i].At.Before(as[j].At) })
		if len(as) > 4 {
			t.Fatalf("%s: %d attempts, max is 1 + 3 retries", key, len(as))
		}
		for i := 1; i < len(as); i++ {
			gap := as[i].At.Sub(as[i-1].At)
			switch as[i-1].Status {
			case http.StatusTooManyRequests:
				saw429Retry = true
				if gap < time.Second {
					t.Fatalf("%s: retried %v after a 429 with Retry-After: 1", key, gap)
				}
			case http.StatusInternalServerError:
				if gap < 100*time.Millisecond {
					t.Fatalf("%s: retried %v after a 500", key, gap)
				}
			default:
				t.Fatalf("%s: retried after status %d", key, as[i-1].Status)
			}
		}
	}
	if !saw429Retry {
		t.Fatal("no 429 was retried; chaos not exercised")
	}

	// 4. Exactly-once: every expected key was attempted, the provider
	// accepted each at most once, and our sent rows are exactly the keys the
	// provider accepted.
	if len(byKey) != len(wantKeys) {
		t.Fatalf("attempted %d distinct deliveries, want %d", len(byKey), len(wantKeys))
	}
	accepted := map[string]bool{}
	for key, as := range byKey {
		n := 0
		for _, a := range as {
			if a.Status == http.StatusAccepted {
				n++
			}
		}
		if n > 1 {
			t.Fatalf("%s accepted %d times", key, n)
		}
		accepted[key] = n == 1
	}
	ctx := context.Background()
	for _, u := range w.all {
		for _, ch := range domain.Channels {
			key := domain.IdempotencyKey(w.campaignID, u.ID, ch)
			log, err := w.dispatches.Get(ctx, w.campaignID, u.ID, ch)
			if !wantKeys[key] {
				if err == nil {
					t.Fatalf("dispatch log for opted-out %s", key)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			if (log.Status == domain.DispatchStatusSent) != accepted[key] {
				t.Fatalf("%s: db status %s, provider accepted=%v", key, log.Status, accepted[key])
			}
			if log.RetryCount != len(byKey[key])-1 {
				t.Fatalf("%s: retry_count %d, provider saw %d attempts", key, log.RetryCount, len(byKey[key]))
			}
		}
	}

	// 5. Metrics agree with the provider's view.
	s, err := d.Stats(ctx, w.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("stats: %+v", s)
	if s.State != dispatch.StateCompleted || s.Total != int64(len(wantKeys)) || s.Processed != s.Total ||
		s.Sent+s.Failed != s.Total || s.Pending != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if s.Retried != int64(len(attempts)-len(byKey)) {
		t.Fatalf("retried = %d, provider saw %d retry attempts", s.Retried, len(attempts)-len(byKey))
	}
	var totalAccepted int64
	for _, st := range mock.Stats() {
		totalAccepted += st.Accepted
	}
	if s.Sent != totalAccepted {
		t.Fatalf("sent = %d, provider accepted %d", s.Sent, totalAccepted)
	}
	if c, _ := w.campaigns.GetByID(ctx, w.campaignID); c.Status != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status %s", c.Status)
	}
}

// The provider accepts a message but the response is lost (client timeout).
// The retry carries the same idempotency key and is answered with the
// original message id: the recipient gets one message, not two.
func TestLostResponsesAreNotDeliveredTwice(t *testing.T) {
	w := newWorld(t, 30, func(int) (bool, bool) { return true, true })
	var (
		mu    sync.Mutex
		seen  = map[string]int{}
		drops atomic.Int64
	)
	dropFirstOfEveryThird := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			mu.Lock()
			seen[key]++
			first := seen[key] == 1
			mu.Unlock()
			var userID int64
			_, _ = fmt.Sscanf(strings.Split(key, ":")[1], "%d", &userID)
			if first && userID%3 == 0 {
				next.ServeHTTP(httptest.NewRecorder(), r) // provider accepts...
				drops.Add(1)
				<-r.Context().Done() // ...but the response never arrives
				return
			}
			next.ServeHTTP(rw, r)
		})
	}
	mock, client := startMock(t, provider.Config{}, dropFirstOfEveryThird)
	cfg := dispatch.DefaultConfig()
	cfg.SendTimeout = 150 * time.Millisecond
	d := w.dispatcher(t, client, cfg)

	run, err := d.Start(context.Background(), w.campaignID, w.audience())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, run)

	st := mock.Stats()
	accepted := st["sms"].Accepted + st["email"].Accepted
	replayed := st["sms"].Replayed + st["email"].Replayed
	if drops.Load() == 0 || replayed != drops.Load() {
		t.Fatalf("drops = %d, replays = %d", drops.Load(), replayed)
	}
	if accepted != 60 {
		t.Fatalf("provider accepted %d distinct messages, want 60 (one per recipient and channel)", accepted)
	}
	s, _ := d.Stats(context.Background(), w.campaignID)
	if s.Sent != 60 || s.Failed != 0 || s.Retried != drops.Load() {
		t.Fatalf("stats = %+v", s)
	}
}

// The process stops mid-campaign (context cancelled). A new dispatcher
// resumes it: unfinished deliveries are re-sent with the same key, finished
// ones are skipped, and nobody receives anything twice.
func TestInterruptedRunResumesWithoutDuplicates(t *testing.T) {
	w := newWorld(t, 120, func(int) (bool, bool) { return true, true })
	mockCfg := provider.Config{MinLatency: 10 * time.Millisecond, MaxLatency: 30 * time.Millisecond}
	mock, client := startMock(t, mockCfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	var ok atomic.Int64
	cancelAfter60 := dispatch.SenderFunc(func(c context.Context, m dispatch.Message) (dispatch.Receipt, error) {
		r, err := client.Send(c, m)
		if err == nil && ok.Add(1) == 60 {
			cancel() // "kill -TERM" in the middle of the campaign
		}
		return r, err
	})
	first := w.dispatcher(t, cancelAfter60, dispatch.DefaultConfig())
	run, err := first.Start(ctx, w.campaignID, w.audience())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, run)

	bg := context.Background()
	s, _ := first.Stats(bg, w.campaignID)
	if s.State != dispatch.StateInterrupted || s.Pending == 0 {
		t.Fatalf("after interruption: %+v", s)
	}
	if c, _ := w.campaigns.GetByID(bg, w.campaignID); c.Status != domain.CampaignStatusSending {
		t.Fatalf("interrupted campaign must stay 'sending', got %s", c.Status)
	}

	// "Restart": a fresh dispatcher resumes from the database.
	second := w.dispatcher(t, client, dispatch.DefaultConfig())
	if _, err := second.Start(bg, w.campaignID, w.audience()); err == nil {
		t.Fatal("Start of a sending campaign must be refused; Resume is explicit")
	}
	run, err = second.Resume(bg, w.campaignID, w.audience())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, run)

	st := mock.Stats()
	if got := st["sms"].Accepted + st["email"].Accepted; got != 240 {
		t.Fatalf("provider accepted %d distinct messages, want 240", got)
	}
	counts, _ := w.dispatches.Counts(bg, w.campaignID)
	if counts[domain.ChannelSMS].Sent != 120 || counts[domain.ChannelEmail].Sent != 120 {
		t.Fatalf("db counts = %+v", counts)
	}
	byKey := map[string]int{}
	for _, a := range mock.Attempts() {
		if a.Status == http.StatusAccepted {
			byKey[a.IdempotencyKey]++
		}
	}
	for key, n := range byKey {
		if n != 1 {
			t.Fatalf("%s accepted %d times", key, n)
		}
	}
	resumed, _ := second.Stats(bg, w.campaignID)
	if resumed.State != dispatch.StateCompleted || resumed.Skipped != s.Sent+s.Failed {
		t.Fatalf("resume stats = %+v (first run: %+v)", resumed, s)
	}
	if c, _ := w.campaigns.GetByID(bg, w.campaignID); c.Status != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status %s", c.Status)
	}
}
