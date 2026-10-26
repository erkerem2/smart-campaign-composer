package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
)

func approvedCampaign(t *testing.T, repo *CampaignRepository) domain.Campaign {
	t.Helper()
	c, err := repo.CreateApproved(context.Background(), "brief", json.RawMessage(`{"type":"group","logic":"AND","rules":[]}`),
		composer.ReferenceMessage.CampaignMessages(0), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The dispatch lease is the cross-process lock of a campaign's run (B2).
func TestDispatchLeaseLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewCampaignRepository(db)
	c := approvedCampaign(t, repo)
	now := time.Now()

	// Start: approved → sending, owned by A.
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusApproved, "A", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetByID(ctx, c.ID); got.Status != domain.CampaignStatusSending {
		t.Fatalf("status = %s", got.Status)
	}
	if owner, until, _ := repo.DispatchLease(ctx, c.ID); owner != "A" || until.UnixMilli() != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("lease = %q until %v", owner, until)
	}
	// A second Start loses the status CAS.
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusApproved, "B", now, now.Add(time.Minute)); !errors.Is(err, ErrStatusConflict) {
		t.Fatalf("second start: %v", err)
	}
	// B cannot resume while A's lease is live, nor renew or release it.
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, "B", now, now.Add(time.Minute)); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("resume under a live lease: %v", err)
	}
	if err := repo.RenewDispatch(ctx, c.ID, "B", now.Add(time.Hour)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew by a non-owner: %v", err)
	}
	if err := repo.ReleaseDispatch(ctx, c.ID, "B", domain.CampaignStatusCompleted); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release by a non-owner: %v", err)
	}
	// A renews; the owner may re-acquire its own lease.
	if err := repo.RenewDispatch(ctx, c.ID, "A", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, "A", now, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("owner re-acquire: %v", err)
	}
	// A crashes; once its lease expires, B takes over and A is locked out.
	later := now.Add(3 * time.Minute)
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, "B", later, later.Add(time.Minute)); err != nil {
		t.Fatalf("take-over after expiry: %v", err)
	}
	if err := repo.RenewDispatch(ctx, c.ID, "A", later.Add(time.Hour)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old owner renew: %v", err)
	}
	// B pauses (shutdown): status stays sending, lease cleared, anyone can
	// resume at once.
	if err := repo.ReleaseDispatch(ctx, c.ID, "B", domain.CampaignStatusSending); err != nil {
		t.Fatal(err)
	}
	if owner, _, _ := repo.DispatchLease(ctx, c.ID); owner != "" {
		t.Fatalf("released lease still owned by %q", owner)
	}
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, "C", now, now.Add(time.Minute)); err != nil {
		t.Fatalf("resume after release: %v", err)
	}
	// C finishes.
	if err := repo.ReleaseDispatch(ctx, c.ID, "C", domain.CampaignStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetByID(ctx, c.ID); got.Status != domain.CampaignStatusCompleted {
		t.Fatalf("status = %s", got.Status)
	}
	if err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, "D", later, later); !errors.Is(err, ErrStatusConflict) {
		t.Fatalf("resume of a completed campaign: %v", err)
	}
	if err := repo.ReleaseDispatch(ctx, c.ID, "C", domain.CampaignStatusApproved); err == nil {
		t.Fatal("sending -> approved accepted")
	}
	if err := repo.AcquireDispatch(ctx, 4242, domain.CampaignStatusSending, "A", now, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

// Concurrent resumes of a campaign whose owner is gone, through separate
// connections: exactly one wins the SQL compare-and-set.
func TestDispatchLeaseConcurrentAcquireHasOneWinner(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewCampaignRepository(db)
	c := approvedCampaign(t, repo)
	if err := repo.TransitionStatus(ctx, c.ID, domain.CampaignStatusApproved, domain.CampaignStatusSending); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		wins, held  int
		start       = make(chan struct{})
		concurrency = 12
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := repo.AcquireDispatch(ctx, c.ID, domain.CampaignStatusSending, fmt.Sprintf("worker-%d", i), now, now.Add(time.Minute))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrLeaseHeld):
				held++
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 || held != concurrency-1 {
		t.Fatalf("wins=%d held=%d", wins, held)
	}
}

func TestMigrateV3ToV4AddsDispatchLease(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "v3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v3 := `
CREATE TABLE campaigns (id INTEGER PRIMARY KEY AUTOINCREMENT, brief TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'draft', created_at TEXT NOT NULL,
  segment_rule TEXT CHECK (segment_rule IS NULL OR json_valid(segment_rule)));
INSERT INTO campaigns (brief, status, created_at) VALUES ('yarım kalan', 'sending', '2026-01-01T00:00:00Z');
PRAGMA user_version = 3;`
	if _, err := db.ExecContext(ctx, v3); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion || SchemaVersion != 4 {
		t.Fatalf("user_version = %d (SchemaVersion %d), %v", version, SchemaVersion, err)
	}
	// A campaign left 'sending' by a v3 binary has no owner: it can be
	// resumed at once.
	repo := NewCampaignRepository(db)
	now := time.Now()
	if err := repo.AcquireDispatch(ctx, 1, domain.CampaignStatusSending, "A", now, now.Add(time.Minute)); err != nil {
		t.Fatalf("resume of a pre-v4 campaign: %v", err)
	}
}
