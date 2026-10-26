package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

func userIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

// race runs f concurrently n times, releasing all goroutines at once.
func race(n int, f func(i int) error) []error {
	errs := make([]error, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			errs[i] = f(i)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return errs
}

// assertEachKeyOnce checks that every delivery of the campaign reached the
// sender exactly once.
func assertEachKeyOnce(t *testing.T, s *memSender, campaignID int64, ids []int64, chs ...domain.Channel) {
	t.Helper()
	for _, id := range ids {
		for _, ch := range chs {
			if n := s.count(domain.IdempotencyKey(campaignID, id, ch)); n != 1 {
				t.Errorf("campaign %d user %d %s sent %d times, want 1", campaignID, id, ch, n)
			}
		}
	}
}

// B1. Twenty "send" clicks at the same instant, spread over two dispatchers
// (two server processes on one database): exactly one starts the campaign,
// and nobody receives anything twice.
func TestParallelStartsHaveExactlyOneWinner(t *testing.T) {
	store, campaigns, sender := newMem()
	sender.delay = 5 * time.Millisecond // keep the winning run alive during the race
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS, domain.ChannelEmail)
	users := memUsers(userIDs(20)...)
	ds := []*Dispatcher{newMemDispatcher(t, store, campaigns, sender), newMemDispatcher(t, store, campaigns, sender)}

	runs := make([]*Run, 20)
	errs := race(20, func(i int) error {
		r, err := ds[i%2].Start(context.Background(), 1, StaticAudience(users...))
		runs[i] = r
		return err
	})
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			awaitRun(t, runs[i])
		case errors.Is(err, ErrAlreadyRunning), errors.Is(err, ErrInvalidState):
		default:
			t.Errorf("start %d: unexpected error %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d starts succeeded, want exactly 1", won)
	}
	assertEachKeyOnce(t, sender, 1, userIDs(20), domain.ChannelSMS, domain.ChannelEmail)
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status = %s", st)
	}
}

// B2. A campaign was left in 'sending' (crash, restart). Two dispatcher
// instances are asked to resume it at the same moment. Without a lock both
// would run, send every pending delivery twice (the provider's idempotency
// keys would hide it, but stats and logs double and the second run's writes
// fail). Exactly one may own the run.
func TestConcurrentResumeHasSingleOwner(t *testing.T) {
	for iter := int64(1); iter <= 20; iter++ {
		store, campaigns, sender := newMem()
		sender.delay = 2 * time.Millisecond
		campaigns.Add(iter, domain.CampaignStatusSending, nil, domain.ChannelSMS)
		store.AddPending(iter, domain.ChannelSMS, userIDs(30)...)
		users := memUsers(userIDs(30)...)
		ds := []*Dispatcher{newMemDispatcher(t, store, campaigns, sender), newMemDispatcher(t, store, campaigns, sender)}

		runs := make([]*Run, 2)
		errs := race(2, func(i int) error {
			r, err := ds[i].Resume(context.Background(), iter, StaticAudience(users...))
			runs[i] = r
			return err
		})
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
				awaitRun(t, runs[i])
				if rerr := runs[i].Err(); rerr != nil {
					t.Errorf("iteration %d: winning run failed: %v", iter, rerr)
				}
			case errors.Is(err, ErrAlreadyRunning):
			default:
				t.Errorf("iteration %d: resume %d: unexpected error %v", iter, i, err)
			}
		}
		if won != 1 {
			t.Fatalf("iteration %d: %d resumes succeeded, want exactly 1", iter, won)
		}
		assertEachKeyOnce(t, sender, iter, userIDs(30), domain.ChannelSMS)
		if st := campaigns.Status(iter); st != domain.CampaignStatusCompleted {
			t.Fatalf("iteration %d: campaign status = %s", iter, st)
		}
		if owner, _ := campaigns.Lease(iter); owner != "" {
			t.Fatalf("iteration %d: a finished run must release its lease (owner %q)", iter, owner)
		}
	}
}

// A run that is alive somewhere else (its lease has not expired) must not be
// resumed; one whose owner crashed (lease expired) is taken over.
func TestResumeRespectsLiveLeaseAndTakesOverExpiredOne(t *testing.T) {
	store, campaigns, sender := newMem()
	campaigns.Add(1, domain.CampaignStatusSending, nil, domain.ChannelSMS)
	store.AddPending(1, domain.ChannelSMS, 1, 2, 3)
	users := memUsers(1, 2, 3)
	d := newMemDispatcher(t, store, campaigns, sender)

	campaigns.SetLease(1, "worker-on-another-host", time.Now().Add(time.Hour))
	if _, err := d.Resume(context.Background(), 1, StaticAudience(users...)); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("resume under a live lease: want ErrAlreadyRunning, got %v", err)
	}
	if n := sender.total(); n != 0 {
		t.Fatalf("%d messages were sent while another worker owned the run", n)
	}

	campaigns.SetLease(1, "worker-that-crashed", time.Now().Add(-time.Second))
	run, err := d.Resume(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatalf("resume after the lease expired: %v", err)
	}
	awaitRun(t, run)
	assertEachKeyOnce(t, sender, 1, []int64{1, 2, 3}, domain.ChannelSMS)
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status = %s", st)
	}
}

func newLeaseDispatcher(t *testing.T, store Store, campaigns CampaignStore, sender Sender, ttl time.Duration) *Dispatcher {
	t.Helper()
	cfg := memConfig()
	cfg.LeaseTTL = ttl
	cfg.WorkersPerChannel = 1
	d, err := New(store, campaigns, sender, NewComposerRenderer(nil), cfg,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A long run keeps renewing its lease, so a resume elsewhere keeps being
// refused even after several TTLs.
func TestHeartbeatKeepsTheLeaseAlive(t *testing.T) {
	store, campaigns, sender := newMem()
	sender.delay = 20 * time.Millisecond // 80 sends ≈ 1.6 s with one worker
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	users := memUsers(userIDs(80)...)
	ttl := 150 * time.Millisecond
	owner := newLeaseDispatcher(t, store, campaigns, sender, ttl)
	other := newLeaseDispatcher(t, store, campaigns, sender, ttl)

	run, err := owner.Start(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		time.Sleep(2 * ttl)
		if _, err := other.Resume(context.Background(), 1, StaticAudience(users...)); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("after %v: resume by another dispatcher = %v, want ErrAlreadyRunning", time.Duration(i+1)*2*ttl, err)
		}
	}
	awaitRun(t, run)
	assertEachKeyOnce(t, sender, 1, userIDs(80), domain.ChannelSMS)
}

// If the lease is taken over (this dispatcher stalled past its TTL and
// another one resumed), the old run must stop sending at once.
func TestRunStopsWhenItsLeaseIsTakenOver(t *testing.T) {
	store, campaigns, sender := newMem()
	sender.delay = 20 * time.Millisecond
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	users := memUsers(userIDs(100)...)
	d := newLeaseDispatcher(t, store, campaigns, sender, 90*time.Millisecond)

	run, err := d.Start(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	campaigns.SetLease(1, "new-owner", time.Now().Add(time.Hour))
	awaitRun(t, run)

	if !errors.Is(run.Err(), domain.ErrLeaseLost) {
		t.Fatalf("run error = %v, want ErrLeaseLost", run.Err())
	}
	if s, _ := d.Stats(context.Background(), 1); s.State != StateInterrupted {
		t.Fatalf("state = %s, want interrupted", s.State)
	}
	if n := sender.total(); n >= 100 {
		t.Fatalf("the run kept sending after losing its lease (%d sends)", n)
	}
	if owner, _ := campaigns.Lease(1); owner != "new-owner" {
		t.Fatalf("the old run must not touch the new owner's lease, owner = %q", owner)
	}
	if st := campaigns.Status(1); st != domain.CampaignStatusSending {
		t.Fatalf("campaign status = %s, want sending (the new owner finishes it)", st)
	}
}

// A shutdown releases the lease, so the campaign can be resumed right away
// instead of after the TTL.
func TestInterruptedRunReleasesItsLease(t *testing.T) {
	store, campaigns, sender := newMem()
	sender.delay = 10 * time.Millisecond
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	users := memUsers(userIDs(50)...)
	first := newLeaseDispatcher(t, store, campaigns, sender, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	run, err := first.Start(ctx, 1, StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	cancel()
	awaitRun(t, run)
	if owner, _ := campaigns.Lease(1); owner != "" {
		t.Fatalf("interrupted run kept its lease (owner %q)", owner)
	}
	if st := campaigns.Status(1); st != domain.CampaignStatusSending {
		t.Fatalf("status = %s, want sending", st)
	}

	second := newLeaseDispatcher(t, store, campaigns, sender, time.Hour)
	run, err = second.Resume(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatalf("immediate resume after shutdown: %v", err)
	}
	awaitRun(t, run)
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Fatalf("status = %s", st)
	}
	for _, id := range userIDs(50) {
		if st := store.Status(1, id, domain.ChannelSMS); st != domain.DispatchStatusSent {
			t.Fatalf("user %d: %s", id, st)
		}
	}
}
