package repository

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
)

var testNow = time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)

// seedDispatchFixture inserts users with the given (sms, email) opt-ins and a
// campaign in the given status.
func seedDispatchFixture(t *testing.T, db *sql.DB, status string, optIns ...[2]bool) (int64, []domain.User) {
	t.Helper()
	users := make([]domain.User, len(optIns))
	for i, o := range optIns {
		u := sampleUser("")
		u.Email = "user" + string(rune('a'+i)) + "@example.com"
		u.OptInSMS, u.OptInEmail = o[0], o[1]
		users[i] = u
	}
	if err := NewUserRepository(db).InsertBatch(context.Background(), users); err != nil {
		t.Fatal(err)
	}
	return insertCampaign(t, db, status), users
}

func TestClaimBatchEnforcesOptInAndUniqueness(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewDispatchRepository(db)
	campaignID, users := seedDispatchFixture(t, db, "sending",
		[2]bool{true, true}, [2]bool{false, true}, [2]bool{true, false})
	ids := []int64{users[0].ID, users[1].ID, users[2].ID, 999999 /* unknown user */}

	sms, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, ids, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{users[0].ID, users[2].ID}; !equalIDs(sms, want) {
		t.Fatalf("sms claims = %v, want %v (opted-out and unknown users must not be claimed)", sms, want)
	}
	email, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelEmail, ids, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{users[0].ID, users[1].ID}; !equalIDs(email, want) {
		t.Fatalf("email claims = %v, want %v", email, want)
	}

	// A second claim of the same slots acquires nothing.
	again, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, ids, testNow)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-claim = %v, %v; want none", again, err)
	}
	if _, err := repo.ClaimBatch(ctx, campaignID, domain.Channel("push"), ids, testNow); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

// Many goroutines racing for the same slots: every slot is won exactly once.
func TestClaimBatchConcurrentClaimsWinOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewDispatchRepository(db)
	optIns := make([][2]bool, 20)
	for i := range optIns {
		optIns[i] = [2]bool{true, true}
	}
	campaignID, users := seedDispatchFixture(t, db, "sending", optIns...)
	ids := make([]int64, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total = map[int64]int{}
	)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, ids, testNow)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, id := range got {
				total[id]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(total) != len(ids) {
		t.Fatalf("%d slots claimed, want %d", len(total), len(ids))
	}
	for id, n := range total {
		if n != 1 {
			t.Fatalf("user %d claimed %d times", id, n)
		}
	}
}

func TestMarkOnlyFromPendingAndCounts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewDispatchRepository(db)
	campaignID, users := seedDispatchFixture(t, db, "sending", [2]bool{true, true}, [2]bool{true, true}, [2]bool{true, false})
	ids := []int64{users[0].ID, users[1].ID, users[2].ID}
	if _, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, ids, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelEmail, ids, testNow); err != nil {
		t.Fatal(err)
	}

	if err := repo.MarkSent(ctx, campaignID, users[0].ID, domain.ChannelSMS, "prov-1", 2, testNow); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkFailed(ctx, campaignID, users[1].ID, domain.ChannelSMS, 3, "http 500", testNow); err != nil {
		t.Fatal(err)
	}
	// Terminal states are never overwritten.
	if err := repo.MarkFailed(ctx, campaignID, users[0].ID, domain.ChannelSMS, 0, "late", testNow); !errors.Is(err, ErrDispatchNotPending) {
		t.Fatalf("overwrite of sent row: %v", err)
	}
	if err := repo.MarkSent(ctx, campaignID, users[1].ID, domain.ChannelSMS, "x", 0, testNow); !errors.Is(err, ErrDispatchNotPending) {
		t.Fatalf("overwrite of failed row: %v", err)
	}
	// Never claimed (email opt-out) -> cannot be finalised.
	if err := repo.MarkSent(ctx, campaignID, users[2].ID, domain.ChannelEmail, "x", 0, testNow); !errors.Is(err, ErrDispatchNotPending) {
		t.Fatalf("finalising an unclaimed slot: %v", err)
	}

	got, err := repo.Get(ctx, campaignID, users[0].ID, domain.ChannelSMS)
	if err != nil || got.Status != domain.DispatchStatusSent || got.ProviderMessageID != "prov-1" ||
		got.RetryCount != 2 || got.SentAt == nil || !got.SentAt.Equal(testNow) {
		t.Fatalf("sent row = %+v, %v", got, err)
	}
	if got.IdempotencyKey() != domain.IdempotencyKey(campaignID, users[0].ID, domain.ChannelSMS) {
		t.Fatal("idempotency key mismatch")
	}
	failed, _ := repo.Get(ctx, campaignID, users[1].ID, domain.ChannelSMS)
	if failed.Status != domain.DispatchStatusFailed || failed.LastError != "http 500" || failed.SentAt != nil {
		t.Fatalf("failed row = %+v", failed)
	}

	pending, err := repo.PendingUserIDs(ctx, campaignID, domain.ChannelSMS)
	if err != nil || !equalIDs(pending, []int64{users[2].ID}) {
		t.Fatalf("pending sms = %v, %v", pending, err)
	}

	counts, err := repo.Counts(ctx, campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if c := counts[domain.ChannelSMS]; c != (domain.DispatchCounts{Total: 3, Pending: 1, Sent: 1, Failed: 1, Retries: 5}) {
		t.Fatalf("sms counts = %+v", c)
	}
	if c := counts[domain.ChannelEmail]; c != (domain.DispatchCounts{Total: 2, Pending: 2}) {
		t.Fatalf("email counts = %+v", c)
	}
}

func TestApproveAndTransitionStatus(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewCampaignRepository(db)

	empty, _ := repo.Create(ctx, "brief", testNow)
	if err := repo.Approve(ctx, empty.ID); !errors.Is(err, ErrNoMessages) {
		t.Fatalf("approving a campaign without content: %v", err)
	}

	c, _ := repo.Create(ctx, "brief", testNow)
	if err := repo.SaveDraftMessages(ctx, c.ID, composer.ReferenceMessage.CampaignMessages(c.ID)); err != nil {
		t.Fatal(err)
	}
	// Approval cannot be skipped.
	if err := repo.TransitionStatus(ctx, c.ID, domain.CampaignStatusDraft, domain.CampaignStatusSending); err == nil {
		t.Fatal("draft -> sending accepted")
	}
	if err := repo.Approve(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	msgs, _ := repo.Messages(ctx, c.ID)
	for _, m := range msgs {
		if m.Status != domain.MessageStatusApproved {
			t.Fatalf("message %+v not approved", m)
		}
	}
	if err := repo.Approve(ctx, c.ID); !errors.Is(err, ErrCampaignNotDraft) {
		t.Fatalf("double approve: %v", err)
	}
	if err := repo.SaveDraftMessages(ctx, c.ID, nil); !errors.Is(err, ErrCampaignNotDraft) {
		t.Fatalf("approved content must be immutable: %v", err)
	}

	// Two concurrent "send" clicks: exactly one wins the CAS.
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		wins      int
		conflicts int
	)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.TransitionStatus(ctx, c.ID, domain.CampaignStatusApproved, domain.CampaignStatusSending)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrStatusConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 5 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	if err := repo.TransitionStatus(ctx, 4242, domain.CampaignStatusApproved, domain.CampaignStatusSending); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

func TestForEachByIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, users := seedDispatchFixture(t, db, "draft", [2]bool{true, true}, [2]bool{false, false}, [2]bool{true, false})
	var got []int64
	err := NewUserRepository(db).ForEachByIDs(ctx, []int64{users[2].ID, 777, users[0].ID}, func(u domain.User) error {
		got = append(got, u.ID)
		return nil
	})
	if err != nil || !equalIDs(got, []int64{users[0].ID, users[2].ID}) {
		t.Fatalf("got %v, %v", got, err)
	}
	stop := errors.New("stop")
	if err := NewUserRepository(db).ForEachByIDs(ctx, []int64{users[0].ID}, func(domain.User) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error not propagated: %v", err)
	}
}

// A database created by the version-1 schema (dispatch_logs without the
// 'pending' state) is upgraded in place, keeping its rows.
func TestMigrateUpgradesV1DispatchLogs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	v1 := `
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, first_name TEXT NOT NULL, last_name TEXT NOT NULL,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE, phone TEXT NOT NULL, city TEXT NOT NULL,
  age INTEGER NOT NULL CHECK (age BETWEEN 13 AND 120), total_spend REAL NOT NULL DEFAULT 0 CHECK (total_spend >= 0),
  cart_last_active_at TEXT, last_purchase_at TEXT,
  opt_in_sms INTEGER NOT NULL DEFAULT 0 CHECK (opt_in_sms IN (0, 1)),
  opt_in_email INTEGER NOT NULL DEFAULT 0 CHECK (opt_in_email IN (0, 1)), created_at TEXT NOT NULL);
CREATE TABLE campaigns (id INTEGER PRIMARY KEY AUTOINCREMENT, brief TEXT NOT NULL CHECK (length(trim(brief)) > 0),
  status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'sending', 'completed', 'failed')),
  created_at TEXT NOT NULL);
CREATE TABLE dispatch_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  campaign_id INTEGER NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  channel TEXT NOT NULL CHECK (channel IN ('sms', 'email')),
  status TEXT NOT NULL CHECK (status IN ('sent', 'failed')),
  provider_message_id TEXT, retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0), sent_at TEXT,
  UNIQUE (campaign_id, user_id, channel));
CREATE INDEX idx_dispatch_logs_campaign_status ON dispatch_logs (campaign_id, status);
INSERT INTO users (first_name, last_name, email, phone, city, age, opt_in_sms, created_at)
  VALUES ('A', 'B', 'a@example.com', '+905320000000', 'İstanbul', 30, 1, '2026-01-01T00:00:00Z');
INSERT INTO campaigns (brief, status, created_at) VALUES ('b', 'completed', '2026-01-01T00:00:00Z');
INSERT INTO dispatch_logs (campaign_id, user_id, channel, status, provider_message_id, retry_count, sent_at)
  VALUES (1, 1, 'sms', 'sent', 'p-1', 1, '2026-01-02T00:00:00Z');
PRAGMA user_version = 1;`
	if _, err := db.ExecContext(ctx, v1); err != nil {
		t.Fatalf("create v1: %v", err)
	}

	for i := 0; i < 2; i++ { // second run must be a no-op
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	repo := NewDispatchRepository(db)
	old, err := repo.Get(ctx, 1, 1, domain.ChannelSMS)
	if err != nil || old.Status != domain.DispatchStatusSent || old.ProviderMessageID != "p-1" || old.RetryCount != 1 {
		t.Fatalf("legacy row not preserved: %+v, %v", old, err)
	}
	var idx int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_dispatch_logs_campaign_status' AND tbl_name = 'dispatch_logs'").Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index not recreated on new table: %d, %v", idx, err)
	}
	var leftovers int
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name = 'dispatch_logs_v1'").Scan(&leftovers)
	if leftovers != 0 {
		t.Fatal("legacy table left behind")
	}
	// The new 'pending' state is now accepted and uniqueness still holds.
	campaignID := insertCampaign(t, db, "sending")
	if got, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, []int64{1}, testNow); err != nil || len(got) != 1 {
		t.Fatalf("claim after upgrade = %v, %v", got, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO dispatch_logs (campaign_id, user_id, channel, status) VALUES (1, 1, 'sms', 'pending')`); err == nil {
		t.Fatal("UNIQUE constraint lost during upgrade")
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
