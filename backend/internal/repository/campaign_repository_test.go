package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
)

// Composer output is stored in the existing schema without loss.
func TestCampaignMessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := NewCampaignRepository(openTestDB(t))
	now := time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)

	c, err := repo.Create(ctx, "  "+composer.ReferenceBrief+" ", now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, c.ID)
	if err != nil || got.Brief != composer.ReferenceBrief || got.Status != domain.CampaignStatusDraft || !got.CreatedAt.Equal(now) {
		t.Fatalf("campaign = %+v, %v", got, err)
	}

	msgs := composer.ReferenceMessage.CampaignMessages(c.ID)
	if err := repo.SaveDraftMessages(ctx, c.ID, msgs); err != nil {
		t.Fatalf("composer rows must satisfy the schema CHECKs: %v", err)
	}
	if msgs[0].ID == 0 || msgs[1].ID == 0 {
		t.Fatal("ids must be written back")
	}
	stored, err := repo.Messages(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Channel != domain.ChannelSMS || stored[0].Subject != "" {
		t.Fatalf("stored = %+v", stored)
	}
	if back := composer.MessageFromCampaign(stored); back != composer.ReferenceMessage {
		t.Fatalf("round trip = %+v", back)
	}

	// The SMS subject is NULL in the database, not an empty string.
	var nullSubjects int
	if err := repo.db.QueryRowContext(ctx,
		"SELECT count(*) FROM campaign_messages WHERE campaign_id = ? AND channel = 'sms' AND subject IS NULL", c.ID).Scan(&nullSubjects); err != nil || nullSubjects != 1 {
		t.Fatalf("sms subject must be NULL: %d %v", nullSubjects, err)
	}

	// Regenerating replaces the drafts instead of appending.
	edited := composer.ReferenceMessage
	edited.SMSContent = "{{first_name}}, %30 indirim seni bekliyor!"
	if err := repo.SaveDraftMessages(ctx, c.ID, edited.CampaignMessages(c.ID)); err != nil {
		t.Fatal(err)
	}
	stored, _ = repo.Messages(ctx, c.ID)
	if len(stored) != 2 || stored[0].Content != edited.SMSContent {
		t.Fatalf("after replace = %+v", stored)
	}
}

func TestSaveDraftMessagesGuards(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewCampaignRepository(db)
	c, err := repo.Create(ctx, "brief", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.SaveDraftMessages(ctx, 9999, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
	bad := [][]domain.CampaignMessage{
		{{Channel: domain.ChannelSMS, Content: "x", Subject: "SMS'te konu olmaz"}},
		{{Channel: "push", Content: "x"}},
		{{Channel: domain.ChannelSMS, Content: "a"}, {Channel: domain.ChannelSMS, Content: "b"}},
	}
	for i, msgs := range bad {
		if err := repo.SaveDraftMessages(ctx, c.ID, msgs); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	// A failed save is rolled back as a whole.
	if stored, _ := repo.Messages(ctx, c.ID); len(stored) != 0 {
		t.Fatalf("partial write leaked: %+v", stored)
	}

	if _, err := db.ExecContext(ctx, "UPDATE campaigns SET status = 'approved' WHERE id = ?", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveDraftMessages(ctx, c.ID, composer.ReferenceMessage.CampaignMessages(c.ID)); !errors.Is(err, ErrCampaignNotDraft) {
		t.Fatalf("approved content is immutable: %v", err)
	}
}
