package repository

import (
	"context"
	"testing"

	"smart-campaign-composer/backend/internal/domain"
)

func TestMetricsTotals(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	metrics := NewMetricsRepository(db)

	// Empty database: all zeros, no NULL from SUM over zero rows.
	got, err := metrics.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != (domain.SystemTotals{}) {
		t.Fatalf("empty totals = %+v, want zeros", got)
	}

	campaignID, users := seedDispatchFixture(t, db, "sending",
		[2]bool{true, true}, [2]bool{true, true}, [2]bool{true, false})
	insertCampaign(t, db, "draft") // a campaign without deliveries still counts

	repo := NewDispatchRepository(db)
	ids := []int64{users[0].ID, users[1].ID, users[2].ID}
	if _, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelSMS, ids, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimBatch(ctx, campaignID, domain.ChannelEmail, ids, testNow); err != nil {
		t.Fatal(err)
	}
	// 3 SMS + 2 e-mail claimed. Finish 3 as sent (2 + 1 retries), 1 as
	// failed (3 retries); one e-mail stays pending.
	mustOK := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mustOK(repo.MarkSent(ctx, campaignID, users[0].ID, domain.ChannelSMS, "p-1", 2, testNow))
	mustOK(repo.MarkSent(ctx, campaignID, users[1].ID, domain.ChannelSMS, "p-2", 0, testNow))
	mustOK(repo.MarkSent(ctx, campaignID, users[0].ID, domain.ChannelEmail, "p-3", 1, testNow))
	mustOK(repo.MarkFailed(ctx, campaignID, users[2].ID, domain.ChannelSMS, 3, "gave up", testNow))

	got, err = metrics.Totals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.SystemTotals{Campaigns: 2, Sent: 3, Failed: 1, Retries: 6}
	if got != want {
		t.Fatalf("totals = %+v, want %+v", got, want)
	}
}
