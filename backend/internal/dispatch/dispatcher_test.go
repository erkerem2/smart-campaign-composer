package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/repository"
)

// testEnv is a real SQLite database with users and an approved campaign.
type testEnv struct {
	campaigns  *repository.CampaignRepository
	dispatches *repository.DispatchRepository
	users      *repository.UserRepository
	campaignID int64
	all        []domain.User
}

// makeUsers builds n users; optIn(i) decides (sms, email).
func makeUsers(n int, optIn func(i int) (bool, bool)) []domain.User {
	users := make([]domain.User, n)
	for i := range users {
		sms, email := optIn(i)
		users[i] = domain.User{
			FirstName: fmt.Sprintf("Ad%d", i), LastName: "Soyad",
			Email: fmt.Sprintf("user%d@example.com", i), Phone: fmt.Sprintf("+90532%07d", i),
			City: "İstanbul", Age: 30, OptInSMS: sms, OptInEmail: email,
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	return users
}

func newTestEnv(t *testing.T, users []domain.User) *testEnv {
	t.Helper()
	ctx := context.Background()
	db, err := repository.Open(ctx, filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	env := &testEnv{
		campaigns:  repository.NewCampaignRepository(db),
		dispatches: repository.NewDispatchRepository(db),
		users:      repository.NewUserRepository(db),
	}
	if err := env.users.InsertBatch(ctx, users); err != nil {
		t.Fatal(err)
	}
	env.all = users
	env.campaignID = env.newApprovedCampaign(t)
	return env
}

func (e *testEnv) newApprovedCampaign(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	c, err := e.campaigns.Create(ctx, composer.ReferenceBrief, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.campaigns.SaveDraftMessages(ctx, c.ID, composer.ReferenceMessage.CampaignMessages(c.ID)); err != nil {
		t.Fatal(err)
	}
	if err := e.campaigns.Approve(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (e *testEnv) audience() AudienceSource {
	ids := make([]int64, len(e.all))
	for i, u := range e.all {
		ids[i] = u.ID
	}
	return IDAudience(e.users, ids)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// unitConfig removes rate limiting from the picture (it has its own tests).
func unitConfig() Config {
	c := DefaultConfig()
	c.RateLimit, c.RateBurst = 1_000_000, 1_000_000
	c.WorkersPerChannel = 4
	return c
}

func (e *testEnv) dispatcher(t *testing.T, s Sender, cfg Config, opts ...Option) *Dispatcher {
	t.Helper()
	opts = append([]Option{WithLogger(quietLogger())}, opts...)
	d, err := New(e.dispatches, e.campaigns, s, NewComposerRenderer(nil), cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func waitRun(t *testing.T, r *Run) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
}

// recordingSender returns scripted errors per idempotency key and records
// every call.
type recordingSender struct {
	mu     sync.Mutex
	calls  map[string]int
	sentTo map[domain.Channel][]string
	script func(key string, attempt int) error
}

func newRecordingSender(script func(key string, attempt int) error) *recordingSender {
	return &recordingSender{calls: map[string]int{}, sentTo: map[domain.Channel][]string{}, script: script}
}

func (s *recordingSender) Send(_ context.Context, m Message) (Receipt, error) {
	s.mu.Lock()
	s.calls[m.IdempotencyKey]++
	attempt := s.calls[m.IdempotencyKey]
	s.sentTo[m.Channel] = append(s.sentTo[m.Channel], m.To)
	s.mu.Unlock()
	if s.script != nil {
		if err := s.script(m.IdempotencyKey, attempt); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{ProviderMessageID: "p-" + m.IdempotencyKey}, nil
}

func (s *recordingSender) attempts(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

var err429 = &SendError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Second, Temporary: true, Err: errors.New("rate_limited")}

func TestRetriesOn429HonouringRetryAfterThenSucceeds(t *testing.T) {
	env := newTestEnv(t, makeUsers(1, func(int) (bool, bool) { return true, false }))
	clock := newFakeClock()
	sender := newRecordingSender(func(_ string, attempt int) error {
		if attempt <= 2 {
			return err429
		}
		return nil
	})
	d := env.dispatcher(t, sender, unitConfig(), WithClock(clock))
	run, err := d.Start(context.Background(), env.campaignID, env.audience())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)

	key := domain.IdempotencyKey(env.campaignID, env.all[0].ID, domain.ChannelSMS)
	if n := sender.attempts(key); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
	var backoffs []time.Duration
	for _, s := range clock.Sleeps() {
		if s >= 500*time.Millisecond { // ignore limiter waits
			backoffs = append(backoffs, s)
		}
	}
	if len(backoffs) != 2 {
		t.Fatalf("backoff sleeps = %v, want 2", clock.Sleeps())
	}
	for _, b := range backoffs {
		if b < time.Second {
			t.Fatalf("backoff %v shorter than Retry-After 1s", b)
		}
	}
	log, _ := env.dispatches.Get(context.Background(), env.campaignID, env.all[0].ID, domain.ChannelSMS)
	if log.Status != domain.DispatchStatusSent || log.RetryCount != 2 || log.ProviderMessageID != "p-"+key {
		t.Fatalf("log = %+v", log)
	}
	s, _ := d.Stats(context.Background(), env.campaignID)
	if s.State != StateCompleted || s.Total != 1 || s.Sent != 1 || s.Retried != 2 || s.Processed != 1 {
		t.Fatalf("stats = %+v", s)
	}
	c, _ := env.campaigns.GetByID(context.Background(), env.campaignID)
	if c.Status != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status = %s", c.Status)
	}
}

func TestGivesUpAfterThreeRetries(t *testing.T) {
	env := newTestEnv(t, makeUsers(1, func(int) (bool, bool) { return false, true }))
	clock := newFakeClock()
	sender := newRecordingSender(func(string, int) error {
		return &SendError{StatusCode: 500, Temporary: true, Err: errors.New("internal_error")}
	})
	d := env.dispatcher(t, sender, unitConfig(), WithClock(clock), WithRand(func() float64 { return 0 }))
	run, err := d.Start(context.Background(), env.campaignID, env.audience())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)

	key := domain.IdempotencyKey(env.campaignID, env.all[0].ID, domain.ChannelEmail)
	if n := sender.attempts(key); n != 4 {
		t.Fatalf("attempts = %d, want 1 + 3 retries", n)
	}
	// Exponential: 100ms, 200ms, 400ms with Rand=0 (equal jitter lower bound).
	var backoffs []time.Duration
	for _, s := range clock.Sleeps() {
		if s >= 50*time.Millisecond {
			backoffs = append(backoffs, s)
		}
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if fmt.Sprint(backoffs) != fmt.Sprint(want) {
		t.Fatalf("backoffs = %v, want %v", backoffs, want)
	}
	log, _ := env.dispatches.Get(context.Background(), env.campaignID, env.all[0].ID, domain.ChannelEmail)
	if log.Status != domain.DispatchStatusFailed || log.RetryCount != 3 || !strings.Contains(log.LastError, "gave up after 3 retries") {
		t.Fatalf("log = %+v", log)
	}
	// Nothing was delivered at all -> the campaign failed.
	c, _ := env.campaigns.GetByID(context.Background(), env.campaignID)
	if s, _ := d.Stats(context.Background(), env.campaignID); c.Status != domain.CampaignStatusFailed || s.State != StateFailed || s.Failed != 1 {
		t.Fatalf("campaign = %s, stats = %+v", c.Status, s)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	env := newTestEnv(t, makeUsers(1, func(int) (bool, bool) { return true, false }))
	sender := newRecordingSender(func(string, int) error {
		return &SendError{StatusCode: 422, Err: errors.New("invalid_message")}
	})
	d := env.dispatcher(t, sender, unitConfig(), WithClock(newFakeClock()))
	run, _ := d.Start(context.Background(), env.campaignID, env.audience())
	waitRun(t, run)
	key := domain.IdempotencyKey(env.campaignID, env.all[0].ID, domain.ChannelSMS)
	if n := sender.attempts(key); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// Opt-outs never reach the sender, and an audience that lists a user twice
// still produces one delivery per channel.
func TestOptOutAndDuplicateRecipients(t *testing.T) {
	users := makeUsers(4, func(i int) (bool, bool) {
		return []bool{true, false, true, false}[i], []bool{true, true, false, false}[i]
	})
	env := newTestEnv(t, users)
	sender := newRecordingSender(nil)
	d := env.dispatcher(t, sender, unitConfig())

	loaded := env.all
	dup := StaticAudience(append(append([]domain.User{}, loaded...), loaded[0], loaded[2])...)
	run, err := d.Start(context.Background(), env.campaignID, dup)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)

	want := map[domain.Channel][]string{
		domain.ChannelSMS:   {users[0].Phone, users[2].Phone},
		domain.ChannelEmail: {users[0].Email, users[1].Email},
	}
	for ch, to := range want {
		got := sender.sentTo[ch]
		if len(got) != len(to) {
			t.Fatalf("%s sent to %v, want %v", ch, got, to)
		}
		set := map[string]bool{}
		for _, x := range got {
			if set[x] {
				t.Fatalf("%s: %s received twice", ch, x)
			}
			set[x] = true
		}
		for _, x := range to {
			if !set[x] {
				t.Fatalf("%s: %s missing from %v", ch, x, got)
			}
		}
	}
	s, _ := d.Stats(context.Background(), env.campaignID)
	if s.Total != 4 || s.Sent != 4 || s.OptedOut != 5 || s.Skipped != 3 {
		t.Fatalf("stats = %+v", s)
	}
}

// The audience was loaded while the user was opted in; they opted out before
// the claim. The SQL claim re-checks permission, so nothing is sent.
func TestStaleOptInIsRejectedByClaim(t *testing.T) {
	env := newTestEnv(t, makeUsers(1, func(int) (bool, bool) { return true, false }))
	stale := env.all[0] // OptInSMS = true in memory
	sender := newRecordingSender(nil)
	d := env.dispatcher(t, sender, unitConfig())

	optOut := AudienceFunc(func(ctx context.Context, _ int64, yield func(domain.User) error) error {
		// The user opts out after we read them.
		if err := setOptIn(ctx, env, stale.ID, false); err != nil {
			return err
		}
		return yield(stale)
	})
	run, err := d.Start(context.Background(), env.campaignID, optOut)
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)
	if len(sender.sentTo[domain.ChannelSMS]) != 0 {
		t.Fatalf("message sent to a user who opted out: %v", sender.sentTo)
	}
	if s, _ := d.Stats(context.Background(), env.campaignID); s.Skipped != 1 || s.Total != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func setOptIn(ctx context.Context, env *testEnv, userID int64, v bool) error {
	return env.users.SetOptIn(ctx, userID, domain.ChannelSMS, v)
}

func TestStartRequiresApprovedCampaignAndIsExclusive(t *testing.T) {
	env := newTestEnv(t, makeUsers(3, func(int) (bool, bool) { return true, true }))
	ctx := context.Background()

	release := make(chan struct{})
	blocking := SenderFunc(func(ctx context.Context, m Message) (Receipt, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Receipt{ProviderMessageID: "x"}, nil
	})
	d := env.dispatcher(t, blocking, unitConfig())

	draft, _ := env.campaigns.Create(ctx, "brief", time.Now())
	if _, err := d.Start(ctx, draft.ID, env.audience()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("draft campaign: %v", err)
	}
	if _, err := d.Start(ctx, 9999, env.audience()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}

	run, err := d.Start(ctx, env.campaignID, env.audience())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, env.campaignID, env.audience()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second start: %v", err)
	}
	// A second dispatcher (another process) loses the status CAS.
	other := env.dispatcher(t, blocking, unitConfig())
	if _, err := other.Start(ctx, env.campaignID, env.audience()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("start from another dispatcher: %v", err)
	}
	close(release)
	waitRun(t, run)
	if _, err := d.Start(ctx, env.campaignID, env.audience()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("restart of a completed campaign: %v", err)
	}
	if _, err := d.Resume(ctx, env.campaignID, env.audience()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resume of a completed campaign: %v", err)
	}
}

// A message whose personalised text breaks a channel limit (a long real name
// pushes the reference SMS over 160 and the subject over 80) is failed and
// never sent.
func TestRenderFailureIsNeverSent(t *testing.T) {
	users := makeUsers(1, func(int) (bool, bool) { return true, true })
	users[0].FirstName = strings.Repeat("Ü", 60)
	env := newTestEnv(t, users)
	sender := newRecordingSender(nil)
	d := env.dispatcher(t, sender, unitConfig())
	run, _ := d.Start(context.Background(), env.campaignID, env.audience())
	waitRun(t, run)

	for _, ch := range domain.Channels {
		log, _ := env.dispatches.Get(context.Background(), env.campaignID, users[0].ID, ch)
		if log.Status != domain.DispatchStatusFailed || !strings.HasPrefix(log.LastError, "render:") {
			t.Fatalf("%s log = %+v", ch, log)
		}
	}
	if len(sender.calls) != 0 {
		t.Fatalf("over-limit content reached the provider: %v", sender.sentTo)
	}
	if s, _ := d.Stats(context.Background(), env.campaignID); s.Total != 2 || s.Failed != 2 || s.State != StateFailed {
		t.Fatalf("stats = %+v", s)
	}
}

// Stats after a restart come from dispatch_logs.
func TestStatsFallBackToDatabase(t *testing.T) {
	env := newTestEnv(t, makeUsers(5, func(i int) (bool, bool) { return true, i%2 == 0 }))
	d := env.dispatcher(t, newRecordingSender(nil), unitConfig())
	run, _ := d.Start(context.Background(), env.campaignID, env.audience())
	waitRun(t, run)
	live, _ := d.Stats(context.Background(), env.campaignID)

	fresh := env.dispatcher(t, newRecordingSender(nil), unitConfig()) // "after restart"
	db, err := fresh.Stats(context.Background(), env.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if db.Live || db.State != StateCompleted || db.Total != live.Total || db.Sent != live.Sent || db.Total != 8 {
		t.Fatalf("db stats = %+v, live = %+v", db, live)
	}
	if _, err := fresh.Stats(context.Background(), 4242); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown campaign: %v", err)
	}
}
