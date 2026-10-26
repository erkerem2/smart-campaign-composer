package dispatch

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

var (
	// ErrAlreadyRunning is returned when a campaign already has an active
	// run in this process.
	ErrAlreadyRunning = errors.New("dispatch: campaign is already being dispatched")
	// ErrInvalidState is returned when a campaign is not in the status the
	// operation needs (Start: approved, Resume: sending).
	ErrInvalidState = errors.New("dispatch: campaign is not in a dispatchable state")
	// ErrNoApprovedMessages is returned when an approved campaign has no
	// approved content.
	ErrNoApprovedMessages = errors.New("dispatch: campaign has no approved messages")
	// ErrUnrecorded is a run's error when the outcome of some deliveries
	// could not be written to the store. Those rows stay pending, so the
	// campaign stays 'sending' and a Resume reconciles them.
	ErrUnrecorded = errors.New("dispatch: delivery outcome could not be recorded")
)

// Final writes (MarkSent / MarkFailed) are retried with a short doubling
// delay before a run gives up on recording an outcome: SQLite answers
// SQLITE_BUSY when another writer holds the lock longer than busy_timeout.
const (
	persistAttempts = 4
	persistBackoff  = 20 * time.Millisecond
)

// Config tunes the dispatcher. Zero values are replaced by DefaultConfig's.
type Config struct {
	WorkersPerChannel int           // goroutines per channel and run
	RateLimit         int           // max request starts per RateWindow per channel (CASE_SPEC: 100/s)
	RateWindow        time.Duration // 1s
	// RateSafetyMargin widens the window the limiter enforces (1s + 50ms by
	// default, ≈95 msg/s) so the cap also holds as observed by the provider,
	// after network and scheduling jitter between our send and their arrival.
	// Without it a burst can land exactly on the window boundary and a
	// provider counting closed windows sees up to 2 × the limit. Zero means
	// the default; a negative value disables the margin.
	RateSafetyMargin time.Duration
	RateBurst        int // token bucket depth; small to keep the flow smooth
	MaxRetries       int // retries after the first attempt (CASE_SPEC: 3)
	BaseBackoff      time.Duration
	MaxBackoff       time.Duration
	MaxRetryAfter    time.Duration // cap on the provider's Retry-After
	SendTimeout      time.Duration // per attempt
	ClaimBatchSize   int           // users per claim transaction
	// LeaseTTL is how long a run's ownership of its campaign lasts without
	// renewal; it is renewed every LeaseTTL/3. After a crash, another
	// dispatcher can resume the campaign once the lease has expired.
	LeaseTTL time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		WorkersPerChannel: 16,
		RateLimit:         100,
		RateWindow:        time.Second,
		RateSafetyMargin:  50 * time.Millisecond,
		RateBurst:         10,
		MaxRetries:        3,
		BaseBackoff:       200 * time.Millisecond,
		MaxBackoff:        5 * time.Second,
		MaxRetryAfter:     30 * time.Second,
		SendTimeout:       5 * time.Second,
		ClaimBatchSize:    500,
		LeaseTTL:          30 * time.Second,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.WorkersPerChannel <= 0 {
		c.WorkersPerChannel = d.WorkersPerChannel
	}
	if c.RateLimit <= 0 {
		c.RateLimit = d.RateLimit
	}
	if c.RateWindow <= 0 {
		c.RateWindow = d.RateWindow
	}
	// Zero means "default" like every other field (a partially filled
	// Config must not silently lose the margin); a negative value turns the
	// margin off explicitly.
	switch {
	case c.RateSafetyMargin == 0:
		c.RateSafetyMargin = d.RateSafetyMargin
	case c.RateSafetyMargin < 0:
		c.RateSafetyMargin = 0
	}
	if c.RateBurst <= 0 {
		c.RateBurst = d.RateBurst
	}
	c.RateBurst = min(c.RateBurst, c.RateLimit)
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = d.BaseBackoff
	}
	if c.MaxBackoff < c.BaseBackoff {
		c.MaxBackoff = max(d.MaxBackoff, c.BaseBackoff)
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = d.MaxRetryAfter
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = d.SendTimeout
	}
	if c.ClaimBatchSize <= 0 {
		c.ClaimBatchSize = d.ClaimBatchSize
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = d.LeaseTTL
	}
	return c
}

// Option customises a Dispatcher.
type Option func(*Dispatcher)

// WithClock replaces the wall clock (tests).
func WithClock(c Clock) Option { return func(d *Dispatcher) { d.clock = c } }

// WithRand replaces the jitter source; f must return values in [0, 1).
func WithRand(f func() float64) Option { return func(d *Dispatcher) { d.rand = f } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(d *Dispatcher) { d.log = l } }

// WithOwner sets the identity this dispatcher uses for campaign leases
// (default: host name, process id and a random suffix, unique per instance).
func WithOwner(id string) Option { return func(d *Dispatcher) { d.owner = id } }

func newOwnerID() string {
	host, _ := os.Hostname()
	b := make([]byte, 6)
	_, _ = crand.Read(b)
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Dispatcher delivers approved campaigns through a rate-limited worker pool.
//
// Pipeline of one run:
//
//	Start ─► CAS approved→sending ─► plan: audience ─► opt-in filter ─► ClaimBatch (SQL: opt-in + UNIQUE)
//	      ─► render per user ─► per-channel queue ─► N workers ─► Limiter.Wait ─► Sender.Send
//	      ─► MarkSent | retry (backoff + jitter, ≥ Retry-After) | MarkFailed ─► completed | failed
type Dispatcher struct {
	cfg       Config
	store     Store
	campaigns CampaignStore
	sender    Sender
	renderer  Renderer
	clock     Clock
	rand      func() float64
	log       *slog.Logger
	owner     string // lease identity, unique per Dispatcher
	backoff   Backoff
	limiters  map[domain.Channel]*Limiter // shared by every run: the provider limit is global

	mu   sync.Mutex
	runs map[int64]*Run
	wg   sync.WaitGroup
}

// New wires a dispatcher.
func New(store Store, campaigns CampaignStore, sender Sender, renderer Renderer, cfg Config, opts ...Option) (*Dispatcher, error) {
	if store == nil || campaigns == nil || sender == nil || renderer == nil {
		return nil, errors.New("dispatch: store, campaigns, sender and renderer are required")
	}
	d := &Dispatcher{
		cfg: cfg.withDefaults(), store: store, campaigns: campaigns, sender: sender, renderer: renderer,
		clock: realClock{}, rand: rand.Float64, log: slog.Default(), runs: map[int64]*Run{},
		owner: newOwnerID(),
	}
	for _, o := range opts {
		o(d)
	}
	d.backoff = Backoff{Base: d.cfg.BaseBackoff, Max: d.cfg.MaxBackoff, MaxRetryAfter: d.cfg.MaxRetryAfter, Rand: d.rand}
	d.limiters = map[domain.Channel]*Limiter{}
	for _, ch := range domain.Channels {
		l, err := NewLimiter(d.cfg.RateLimit, d.cfg.RateWindow+d.cfg.RateSafetyMargin, d.cfg.RateBurst, d.clock)
		if err != nil {
			return nil, err
		}
		d.limiters[ch] = l
	}
	return d, nil
}

// Limiter returns the shared limiter of a channel (observability, tests).
func (d *Dispatcher) Limiter(ch domain.Channel) *Limiter { return d.limiters[ch] }

// Run is one execution of a campaign's dispatch.
type Run struct {
	CampaignID int64
	progress   *progress
	done       chan struct{}
	err        error
	// unrecorded counts deliveries whose final state could not be
	// persisted; any makes the run end interrupted instead of completed.
	unrecorded atomic.Int64
	// leaseLost is set when another dispatcher took the campaign over; the
	// run stops without touching the campaign any further.
	leaseLost atomic.Bool
}

// Done is closed when the run has finished.
func (r *Run) Done() <-chan struct{} { return r.done }

// Err is the run's fatal error (nil on success). Valid after Done.
func (r *Run) Err() error {
	<-r.done
	return r.err
}

// Start dispatches an approved campaign to audience. It validates and moves
// the campaign to 'sending' synchronously (so the caller gets a clear error),
// then works in the background until ctx is cancelled or every delivery is
// final. ctx should be the server's lifetime context, not a request context.
func (d *Dispatcher) Start(ctx context.Context, campaignID int64, audience AudienceSource) (*Run, error) {
	return d.begin(ctx, campaignID, audience, false)
}

// Resume continues a campaign left in 'sending' by an interrupted run.
// Deliveries already sent or failed are skipped; claimed-but-unfinished ones
// are sent again with the SAME idempotency key, so a message that did reach
// the provider before the interruption is not delivered twice.
func (d *Dispatcher) Resume(ctx context.Context, campaignID int64, audience AudienceSource) (*Run, error) {
	return d.begin(ctx, campaignID, audience, true)
}

func (d *Dispatcher) begin(ctx context.Context, campaignID int64, audience AudienceSource, resume bool) (*Run, error) {
	if audience == nil {
		return nil, errors.New("dispatch: audience is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.runs[campaignID]; ok && !isClosed(r.done) {
		return nil, ErrAlreadyRunning
	}

	c, err := d.campaigns.GetByID(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	want := domain.CampaignStatusApproved
	if resume {
		want = domain.CampaignStatusSending
	}
	if c.Status != want {
		return nil, fmt.Errorf("%w: campaign %d is %s, want %s", ErrInvalidState, campaignID, c.Status, want)
	}
	msgs, err := d.approvedMessages(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	// Compare-and-set on the campaign row: of concurrent Start or Resume
	// calls — in this process or any other sharing the database — exactly
	// one becomes the owner of the run. A Start also moves the campaign
	// approved → sending; a Resume only takes over a run whose owner is gone
	// (no owner, or its lease expired).
	now := d.clock.Now()
	if err := d.campaigns.AcquireDispatch(ctx, campaignID, want, d.owner, now, now.Add(d.cfg.LeaseTTL)); err != nil {
		switch {
		case errors.Is(err, domain.ErrLeaseHeld):
			return nil, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
		case errors.Is(err, domain.ErrStatusConflict):
			return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
		}
		return nil, err
	}

	run := &Run{CampaignID: campaignID, progress: newProgress(campaignID, d.clock.Now()), done: make(chan struct{})}
	d.runs[campaignID] = run
	d.wg.Add(1)
	go d.execute(ctx, run, audience, msgs, resume)
	return run, nil
}

func (d *Dispatcher) approvedMessages(ctx context.Context, campaignID int64) (map[domain.Channel]domain.CampaignMessage, error) {
	all, err := d.campaigns.Messages(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	out := map[domain.Channel]domain.CampaignMessage{}
	for _, m := range all {
		if m.Status == domain.MessageStatusApproved && m.Channel.Valid() {
			out[m.Channel] = m
		}
	}
	if len(out) == 0 {
		return nil, ErrNoApprovedMessages
	}
	return out, nil
}

// ActiveRuns returns the number of runs currently in progress (health).
func (d *Dispatcher) ActiveRuns() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.runs {
		if !isClosed(r.done) {
			n++
		}
	}
	return n
}

// Wait blocks until every run has finished (after cancelling their context
// on shutdown).
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Stats returns live progress while a run is active (or the last run of this
// process), otherwise aggregates the persisted dispatch logs.
func (d *Dispatcher) Stats(ctx context.Context, campaignID int64) (Stats, error) {
	d.mu.Lock()
	run := d.runs[campaignID]
	d.mu.Unlock()
	if run != nil {
		return run.progress.snapshot(!isClosed(run.done), d.clock.Now()), nil
	}
	c, err := d.campaigns.GetByID(ctx, campaignID)
	if err != nil {
		return Stats{}, err
	}
	counts, err := d.store.Counts(ctx, campaignID)
	if err != nil {
		return Stats{}, err
	}
	return statsFromCounts(campaignID, c.Status, counts), nil
}

// ---------------------------------------------------------------------------
// Run execution
// ---------------------------------------------------------------------------

type job struct {
	userID int64
	msg    Message
}

func (d *Dispatcher) execute(ctx context.Context, run *Run, audience AudienceSource, msgs map[domain.Channel]domain.CampaignMessage, resume bool) {
	defer d.wg.Done()
	defer close(run.done)
	p := run.progress
	log := d.log.With("campaign_id", run.CampaignID, "resume", resume)

	// runCtx is also cancelled when the lease is lost, which stops the
	// workers: another dispatcher now owns the campaign.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	stopHeartbeat := d.heartbeat(runCtx, run, cancelRun)

	jobs, err := d.plan(runCtx, run, audience, msgs, resume)
	if err == nil {
		p.setState(StateSending, time.Time{}, nil)
		d.deliver(runCtx, run, jobs)
	}
	stopHeartbeat()

	switch {
	case run.leaseLost.Load():
		// Not ours any more: the new owner finishes the campaign.
		run.err = fmt.Errorf("%w: another dispatcher took over campaign %d", domain.ErrLeaseLost, run.CampaignID)
		p.setState(StateInterrupted, d.clock.Now(), run.err)
		log.Warn("dispatch stopped: lease lost to another dispatcher")
		return
	case ctx.Err() != nil:
		// Shutdown. Deliveries not finished stay pending; the lease is
		// released so a resume (here or elsewhere) does not have to wait for
		// it to expire.
		if err == nil {
			err = ctx.Err()
		}
		run.err = err
		p.setState(StateInterrupted, d.clock.Now(), err)
		d.release(ctx, run, domain.CampaignStatusSending)
		log.Warn("dispatch interrupted; resume to finish pending deliveries", "err", err)
		return
	case err != nil:
		// Claims written so far stay pending; the campaign stays 'sending'
		// and can be resumed once the cause is fixed.
		run.err = err
		p.setState(StateFailed, d.clock.Now(), err)
		d.release(ctx, run, domain.CampaignStatusSending)
		log.Error("dispatch planning failed", "err", err)
		return
	}
	if n := run.unrecorded.Load(); n > 0 {
		// Some outcomes exist only in memory: their rows are still pending.
		// Completing the campaign would orphan them (a completed campaign
		// cannot be resumed), so it stays 'sending' and the dashboard
		// offers a resume, which re-sends with the same idempotency keys.
		run.err = fmt.Errorf("%w: %d deliveries; resume the campaign to reconcile them", ErrUnrecorded, n)
		p.setState(StateInterrupted, d.clock.Now(), run.err)
		d.release(ctx, run, domain.CampaignStatusSending)
		log.Error("dispatch finished with unrecorded deliveries", "unrecorded", n)
		return
	}

	final, state := domain.CampaignStatusCompleted, StateCompleted
	snap := p.snapshot(true, d.clock.Now())
	if snap.Total > 0 && snap.Sent == 0 {
		final, state = domain.CampaignStatusFailed, StateFailed
	}
	if err := d.release(ctx, run, final); err != nil {
		run.err = err
	}
	p.setState(state, d.clock.Now(), nil)
	log.Info("dispatch finished", "state", state, "sent", snap.Sent, "failed", snap.Failed,
		"retried", snap.Retried, "opted_out", snap.OptedOut, "skipped", snap.Skipped)
}

// heartbeat renews the run's lease every LeaseTTL/3 until stop is called. If
// the lease turns out to be lost, it flags the run and calls lost.
//
// It ticks on real time even when the dispatcher runs on a test clock: a
// virtual clock's Sleep returns at once, which would turn the loop into a
// busy spin. Lease timestamps still come from the dispatcher's clock.
func (d *Dispatcher) heartbeat(ctx context.Context, run *Run, lost func()) (stop func()) {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(max(d.cfg.LeaseTTL/3, time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := d.campaigns.RenewDispatch(ctx, run.CampaignID, d.owner, d.clock.Now().Add(d.cfg.LeaseTTL))
			switch {
			case err == nil:
			case errors.Is(err, domain.ErrLeaseLost):
				run.leaseLost.Store(true)
				lost()
				return
			case ctx.Err() == nil:
				// A transient database error: keep the run going and try
				// again; the TTL leaves room for two more attempts.
				d.log.Warn("could not renew dispatch lease", "campaign_id", run.CampaignID, "err", err)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// release ends this dispatcher's ownership of the run, moving the campaign
// from sending to `to` (sending itself when the run can be resumed).
func (d *Dispatcher) release(ctx context.Context, run *Run, to domain.CampaignStatus) error {
	rctx, cancel := finalCtx(ctx)
	defer cancel()
	err := d.campaigns.ReleaseDispatch(rctx, run.CampaignID, d.owner, to)
	if err != nil {
		d.log.Error("could not release dispatch lease", "campaign_id", run.CampaignID, "to", to, "err", err)
	}
	return err
}

// plan resolves the audience and claims every delivery before the first
// message is sent, so the dashboard knows the total up front and a crash at
// any point leaves a resumable, duplicate-free state.
func (d *Dispatcher) plan(ctx context.Context, run *Run, audience AudienceSource, msgs map[domain.Channel]domain.CampaignMessage, resume bool) (map[domain.Channel][]job, error) {
	pending := map[domain.Channel]map[int64]bool{}
	if resume {
		for ch := range msgs {
			ids, err := d.store.PendingUserIDs(ctx, run.CampaignID, ch)
			if err != nil {
				return nil, fmt.Errorf("load pending deliveries: %w", err)
			}
			pending[ch] = make(map[int64]bool, len(ids))
			for _, id := range ids {
				pending[ch][id] = true
			}
		}
	}

	jobs := map[domain.Channel][]job{}
	batch := make([]domain.User, 0, d.cfg.ClaimBatchSize)
	flush := func() error {
		for _, ch := range domain.Channels {
			m, ok := msgs[ch]
			if !ok {
				continue
			}
			if err := d.planChannel(ctx, run, ch, m, batch, pending[ch], &jobs); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	err := audience.Recipients(ctx, run.CampaignID, func(u domain.User) error {
		batch = append(batch, u)
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err == nil && len(batch) > 0 {
		err = flush()
	}
	if err != nil {
		return nil, err
	}

	// Pending deliveries whose user is no longer in the audience: whether
	// they reached the provider is unknown, and re-sending to someone no
	// longer targeted is wrong, so they are closed as failed.
	for ch, ids := range pending {
		for id := range ids {
			// It is still one of this run's deliveries: count it in the
			// total, or Processed would exceed Total on the dashboard.
			run.progress.channels[ch].total.Add(1)
			d.closeFailed(ctx, run, ch, id, 0, "recipient no longer in audience on resume")
		}
	}
	return jobs, nil
}

func (d *Dispatcher) planChannel(ctx context.Context, run *Run, ch domain.Channel, m domain.CampaignMessage,
	users []domain.User, pending map[int64]bool, jobs *map[domain.Channel][]job) error {
	c := run.progress.channels[ch]

	// Gate 1: permission as loaded by the audience.
	eligible := make([]int64, 0, len(users))
	byID := make(map[int64]domain.User, len(users))
	var resumed []int64
	for _, u := range users {
		if !u.CanReceive(ch) {
			c.optedOut.Add(1)
			if pending[u.ID] {
				// Claimed earlier, but the user opted out since: never send.
				delete(pending, u.ID)
				c.total.Add(1)
				d.closeFailed(ctx, run, ch, u.ID, 0, "recipient opted out before resume")
			}
			continue
		}
		byID[u.ID] = u
		if pending[u.ID] {
			delete(pending, u.ID)
			resumed = append(resumed, u.ID)
			continue
		}
		eligible = append(eligible, u.ID)
	}

	// Gate 2 + uniqueness: the claim itself re-checks opt-in in SQL and
	// relies on UNIQUE(campaign_id, user_id, channel).
	acquired, err := d.store.ClaimBatch(ctx, run.CampaignID, ch, eligible, d.clock.Now())
	if err != nil {
		return fmt.Errorf("claim %s deliveries: %w", ch, err)
	}
	c.skipped.Add(int64(len(eligible) - len(acquired)))

	for _, id := range append(acquired, resumed...) {
		c.total.Add(1)
		msg, err := d.renderer.Render(m, byID[id])
		if err != nil {
			// e.g. a real name pushes the SMS over 160 characters: never
			// send a message that breaks channel rules.
			d.closeFailed(ctx, run, ch, id, 0, "render: "+err.Error())
			continue
		}
		msg.IdempotencyKey = domain.IdempotencyKey(run.CampaignID, id, ch)
		msg.CampaignID, msg.UserID, msg.Channel = run.CampaignID, id, ch
		(*jobs)[ch] = append((*jobs)[ch], job{userID: id, msg: msg})
	}
	return nil
}

// deliver runs one worker pool per channel; channels proceed in parallel and
// each is throttled by its shared limiter.
func (d *Dispatcher) deliver(ctx context.Context, run *Run, jobs map[domain.Channel][]job) {
	var wg sync.WaitGroup
	for ch, list := range jobs {
		if len(list) == 0 {
			continue
		}
		queue := make(chan job)
		workers := min(d.cfg.WorkersPerChannel, len(list))
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range queue {
					d.process(ctx, run, d.limiters[ch], j)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(queue)
			for _, j := range list {
				select {
				case queue <- j:
				case <-ctx.Done():
					return // remaining deliveries stay pending for Resume
				}
			}
		}()
	}
	wg.Wait()
}

// process delivers one message: at most 1 + MaxRetries attempts, each one
// through the rate limiter.
func (d *Dispatcher) process(ctx context.Context, run *Run, limiter *Limiter, j job) {
	c := run.progress.channels[j.msg.Channel]
	retries := 0
	for {
		if err := limiter.Wait(ctx); err != nil {
			return // shutting down: the delivery stays pending
		}
		attemptCtx, cancel := context.WithTimeout(ctx, d.cfg.SendTimeout)
		receipt, err := d.sender.Send(attemptCtx, j.msg)
		cancel()
		if err == nil {
			d.closeSent(ctx, run, j, receipt, retries)
			return
		}
		if ctx.Err() != nil {
			// Outcome unknown and we are stopping. The row stays pending;
			// Resume re-sends with the same idempotency key.
			return
		}
		retryable, retryAfter := classify(err)
		if !retryable || retries >= d.cfg.MaxRetries {
			reason := err.Error()
			if retryable {
				reason = fmt.Sprintf("gave up after %d retries: %s", retries, reason)
			}
			d.closeFailed(ctx, run, j.msg.Channel, j.userID, retries, reason)
			return
		}
		retries++
		c.retried.Add(1)
		if err := d.clock.Sleep(ctx, d.backoff.Delay(retries, retryAfter)); err != nil {
			return
		}
	}
}

// finalCtx keeps final writes alive during shutdown: a message the provider
// accepted must be recorded as sent even if the run is being cancelled.
func finalCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// persist runs a final write, retrying transient failures. If it still
// fails, the delivery is counted as unrecorded: its row stays pending and
// the run will not complete the campaign.
func (d *Dispatcher) persist(ctx context.Context, run *Run, what string, ch domain.Channel, userID int64, write func(context.Context) error) {
	wctx, cancel := finalCtx(ctx)
	defer cancel()
	var err error
	delay := persistBackoff
	for attempt := 1; attempt <= persistAttempts; attempt++ {
		if err = write(wctx); err == nil {
			return
		}
		if errors.Is(err, domain.ErrDispatchNotPending) {
			// Already final: recorded by an earlier attempt whose answer was
			// lost, or by another run. Final states are never overwritten,
			// and there is nothing left to reconcile.
			d.log.Warn(what+" was already recorded", "campaign_id", run.CampaignID, "user_id", userID, "channel", ch)
			return
		}
		if attempt == persistAttempts || d.clock.Sleep(wctx, delay) != nil {
			break
		}
		delay *= 2
	}
	run.unrecorded.Add(1)
	d.log.Error(what+" could not be recorded; the delivery stays pending until the campaign is resumed",
		"campaign_id", run.CampaignID, "user_id", userID, "channel", ch, "err", err)
}

func (d *Dispatcher) closeSent(ctx context.Context, run *Run, j job, r Receipt, retries int) {
	// The provider accepted the message: it counts as sent on the live
	// dashboard even if recording it fails (it was delivered). The pending
	// row is reconciled by a resume, which the provider answers with the
	// original message id.
	run.progress.channels[j.msg.Channel].sent.Add(1)
	d.persist(ctx, run, "sent delivery", j.msg.Channel, j.userID, func(wctx context.Context) error {
		return d.store.MarkSent(wctx, run.CampaignID, j.userID, j.msg.Channel, r.ProviderMessageID, retries, d.clock.Now())
	})
}

func (d *Dispatcher) closeFailed(ctx context.Context, run *Run, ch domain.Channel, userID int64, retries int, reason string) {
	run.progress.channels[ch].failed.Add(1)
	d.persist(ctx, run, "failed delivery", ch, userID, func(wctx context.Context) error {
		return d.store.MarkFailed(wctx, run.CampaignID, userID, ch, retries, reason, d.clock.Now())
	})
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
