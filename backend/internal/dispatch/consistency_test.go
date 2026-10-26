package dispatch

import (
	"context"
	"errors"
	"testing"

	"smart-campaign-composer/backend/internal/domain"
)

// B3. A run was interrupted with five SMS deliveries claimed. By the time it
// is resumed the audience is re-evaluated (time-relative fields such as
// cart_last_active_days change every day) and only three users still match.
// The other two are closed as failed — and must be counted in the total, or
// the dashboard shows Processed > Total and a negative Pending.
func TestResumeWithShrunkAudienceKeepsStatsConsistent(t *testing.T) {
	store, campaigns, sender := newMem()
	campaigns.Add(1, domain.CampaignStatusSending, nil, domain.ChannelSMS)
	store.AddPending(1, domain.ChannelSMS, 1, 2, 3, 4, 5)

	d := newMemDispatcher(t, store, campaigns, sender)
	run, err := d.Resume(context.Background(), 1, StaticAudience(memUsers(1, 2, 3)...))
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, run)

	s, err := d.Stats(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	assertConsistent(t, s)
	sms := s.Channels[domain.ChannelSMS]
	if sms.Total != 5 || sms.Sent != 3 || sms.Failed != 2 || sms.Pending != 0 {
		t.Fatalf("sms stats = %+v, want total 5, sent 3, failed 2, pending 0", sms)
	}
	for _, id := range []int64{4, 5} {
		if st := store.Status(1, id, domain.ChannelSMS); st != domain.DispatchStatusFailed {
			t.Errorf("user %d: dispatch log is %q, want failed", id, st)
		}
	}
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Errorf("campaign status = %s, want completed", st)
	}
}

// B4. The provider accepted a message but recording it failed for good
// (disk full, database locked beyond busy_timeout). The row is still pending,
// so the campaign must NOT be marked completed: it stays 'sending', the run
// reports the problem, and a resume re-sends with the same idempotency key
// (the provider answers with the original message id) and records it.
func TestUnrecordedDeliveryKeepsCampaignResumable(t *testing.T) {
	store, campaigns, sender := newMem()
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	store.SetMarkSentErr(func(userID int64, _ int) error {
		if userID == 2 {
			return errors.New("database is locked")
		}
		return nil
	})
	users := memUsers(1, 2, 3)

	d := newMemDispatcher(t, store, campaigns, sender)
	run, err := d.Start(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, run)

	if st := campaigns.Status(1); st != domain.CampaignStatusSending {
		t.Fatalf("campaign with an unrecorded delivery is %s, want it to stay sending (resumable)", st)
	}
	if st := store.Status(1, 2, domain.ChannelSMS); st != domain.DispatchStatusPending {
		t.Fatalf("user 2 log = %q, want pending", st)
	}
	s, _ := d.Stats(context.Background(), 1)
	if s.State != StateInterrupted || s.Error == "" {
		t.Fatalf("run must end interrupted with an error, got state=%q error=%q", s.State, s.Error)
	}
	if run.Err() == nil {
		t.Fatal("run.Err() must report the unrecorded delivery")
	}
	assertConsistent(t, s)

	// The database recovers; a resume reconciles the pending row.
	store.SetMarkSentErr(nil)
	d2 := newMemDispatcher(t, store, campaigns, sender)
	run, err = d2.Resume(context.Background(), 1, StaticAudience(users...))
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, run)
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Fatalf("after resume campaign is %s, want completed", st)
	}
	if st := store.Status(1, 2, domain.ChannelSMS); st != domain.DispatchStatusSent {
		t.Fatalf("after resume user 2 log = %q, want sent", st)
	}
	key := domain.IdempotencyKey(1, 2, domain.ChannelSMS)
	if n := sender.count(key); n != 2 {
		t.Fatalf("user 2 was sent %d times; want the original send plus one replay with the same key", n)
	}
	for _, id := range []int64{1, 3} {
		if n := sender.count(domain.IdempotencyKey(1, id, domain.ChannelSMS)); n != 1 {
			t.Errorf("user %d sent %d times, want 1", id, n)
		}
	}
}

// B4. A transient write failure (SQLITE_BUSY) is retried, so a short lock
// does not leave a delivered message pending in a "completed" campaign.
func TestTransientMarkSentFailureIsRetried(t *testing.T) {
	store, campaigns, sender := newMem()
	campaigns.Add(1, domain.CampaignStatusApproved, nil, domain.ChannelSMS)
	store.SetMarkSentErr(func(userID int64, call int) error {
		if userID == 2 && call <= 2 {
			return errors.New("database is locked")
		}
		return nil
	})

	d := newMemDispatcher(t, store, campaigns, sender)
	run, err := d.Start(context.Background(), 1, StaticAudience(memUsers(1, 2, 3)...))
	if err != nil {
		t.Fatal(err)
	}
	awaitRun(t, run)
	if err := run.Err(); err != nil {
		t.Fatalf("run error: %v", err)
	}
	for _, id := range []int64{1, 2, 3} {
		if st := store.Status(1, id, domain.ChannelSMS); st != domain.DispatchStatusSent {
			t.Errorf("user %d log = %q, want sent", id, st)
		}
	}
	if st := campaigns.Status(1); st != domain.CampaignStatusCompleted {
		t.Fatalf("campaign status = %s, want completed", st)
	}
	if n := sender.count(domain.IdempotencyKey(1, 2, domain.ChannelSMS)); n != 1 {
		t.Fatalf("a retried database write must not re-send the message (sent %d times)", n)
	}
}
