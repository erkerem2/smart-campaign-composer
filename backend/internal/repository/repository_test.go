package repository

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func ptr[T any](v T) *T { return &v }

func sampleUser(email string) domain.User {
	return domain.User{
		FirstName:  "Ayşe",
		LastName:   "Yılmaz",
		Email:      email,
		Phone:      "+905321234567",
		City:       "İstanbul",
		Age:        29,
		TotalSpend: 1249.90,
		CreatedAt:  time.Date(2025, 1, 10, 9, 30, 0, 0, time.UTC),
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestPragmasAppliedOnEveryConnection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Hold several connections at once so the pool must open new ones.
	conns := make([]*sql.Conn, 4)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	for i, c := range conns {
		var fk, busy int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy != 5000 {
			t.Errorf("conn %d: foreign_keys=%d busy_timeout=%d", i, fk, busy)
		}
		_ = c.Close()
	}
}

func TestUserRoundTrip(t *testing.T) {
	db := openTestDB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	withDates := sampleUser("ayse.yilmaz@example.com")
	withDates.CartLastActiveAt = ptr(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	withDates.LastPurchaseAt = ptr(time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC))
	withDates.OptInSMS = true

	noDates := sampleUser("mehmet.kaya@example.com")
	noDates.OptInEmail = true

	users := []domain.User{withDates, noDates}
	if err := repo.InsertBatch(ctx, users); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if users[0].ID == 0 || users[1].ID == 0 {
		t.Fatal("ids were not written back")
	}

	for _, want := range users {
		got, err := repo.GetByID(ctx, want.ID)
		if err != nil {
			t.Fatalf("get %d: %v", want.ID, err)
		}
		if got.Email != want.Email || got.City != want.City || got.FirstName != want.FirstName ||
			got.TotalSpend != want.TotalSpend || got.OptInSMS != want.OptInSMS ||
			got.OptInEmail != want.OptInEmail || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		if !equalTimePtr(got.CartLastActiveAt, want.CartLastActiveAt) ||
			!equalTimePtr(got.LastPurchaseAt, want.LastPurchaseAt) {
			t.Errorf("nullable time mismatch for %s", want.Email)
		}
	}

	n, err := repo.Count(ctx)
	if err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestGetByIDNotFound(t *testing.T) {
	db := openTestDB(t)
	_, err := NewUserRepository(db).GetByID(context.Background(), 999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestInsertBatchIsAtomic(t *testing.T) {
	db := openTestDB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	// Duplicate email (case-insensitive) must fail the whole batch.
	users := []domain.User{sampleUser("dup@example.com"), sampleUser("DUP@example.com")}
	if err := repo.InsertBatch(ctx, users); err == nil {
		t.Fatal("expected unique violation on email")
	}
	if n, _ := repo.Count(ctx); n != 0 {
		t.Fatalf("partial batch persisted: %d rows", n)
	}
}

func TestTimestampsSortLexicographically(t *testing.T) {
	db := openTestDB(t)
	repo := NewUserRepository(db)
	ctx := context.Background()

	early := sampleUser("early@example.com")
	early.CartLastActiveAt = ptr(time.Date(2026, 9, 2, 3, 4, 5, 999_000_000, time.UTC))
	late := sampleUser("late@example.com")
	// Non-UTC input must be normalized before storage.
	late.CartLastActiveAt = ptr(time.Date(2026, 10, 20, 1, 0, 0, 0, time.FixedZone("TRT", 3*3600)))
	if err := repo.InsertBatch(ctx, []domain.User{early, late}); err != nil {
		t.Fatal(err)
	}

	cutoff := formatTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	var email string
	if err := db.QueryRowContext(ctx,
		"SELECT email FROM users WHERE cart_last_active_at >= ?", cutoff).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != "late@example.com" {
		t.Fatalf("range filter returned %q", email)
	}
}

// insertCampaign is a schema-level helper until a CampaignRepository exists.
func insertCampaign(t *testing.T, db *sql.DB, status string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO campaigns (brief, status, created_at) VALUES (?, ?, ?)`,
		"test brief", status, formatTime(time.Now()))
	if err != nil {
		t.Fatalf("insert campaign: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestDispatchLogUniquePerCampaignUserChannel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	users := []domain.User{sampleUser("u@example.com")}
	if err := NewUserRepository(db).InsertBatch(ctx, users); err != nil {
		t.Fatal(err)
	}
	campaignID := insertCampaign(t, db, "sending")

	insert := func(channel string) error {
		_, err := db.Exec(`INSERT INTO dispatch_logs (campaign_id, user_id, channel, status, retry_count)
			VALUES (?, ?, ?, 'sent', 0)`, campaignID, users[0].ID, channel)
		return err
	}
	if err := insert("sms"); err != nil {
		t.Fatalf("first sms: %v", err)
	}
	if err := insert("email"); err != nil {
		t.Fatalf("same user, other channel should be allowed: %v", err)
	}
	if err := insert("sms"); err == nil {
		t.Fatal("duplicate (campaign, user, channel) was accepted")
	}
}

func TestSchemaConstraints(t *testing.T) {
	db := openTestDB(t)
	campaignID := insertCampaign(t, db, "draft")

	cases := map[string]struct {
		query string
		args  []any
	}{
		"unknown campaign status": {
			`INSERT INTO campaigns (brief, status, created_at) VALUES ('b', 'archived', '2026-01-01T00:00:00Z')`, nil},
		"empty brief": {
			`INSERT INTO campaigns (brief, status, created_at) VALUES ('   ', 'draft', '2026-01-01T00:00:00Z')`, nil},
		"sms with subject": {
			`INSERT INTO campaign_messages (campaign_id, channel, content, subject) VALUES (?, 'sms', 'hi', 'subj')`,
			[]any{campaignID}},
		"email without subject": {
			`INSERT INTO campaign_messages (campaign_id, channel, content) VALUES (?, 'email', 'hi')`,
			[]any{campaignID}},
		"unknown channel": {
			`INSERT INTO campaign_messages (campaign_id, channel, content) VALUES (?, 'push', 'hi')`,
			[]any{campaignID}},
		"dispatch to missing user (FK)": {
			`INSERT INTO dispatch_logs (campaign_id, user_id, channel, status) VALUES (?, 424242, 'sms', 'sent')`,
			[]any{campaignID}},
		"dispatch for missing campaign (FK)": {
			`INSERT INTO dispatch_logs (campaign_id, user_id, channel, status) VALUES (424242, 1, 'sms', 'sent')`, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := db.Exec(c.query, c.args...); err == nil {
				t.Fatal("constraint not enforced")
			}
		})
	}

	// Valid messages are accepted.
	if _, err := db.Exec(`INSERT INTO campaign_messages (campaign_id, channel, content) VALUES (?, 'sms', 'hi')`, campaignID); err != nil {
		t.Fatalf("valid sms rejected: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO campaign_messages (campaign_id, channel, content, subject) VALUES (?, 'email', 'hi', 's')`, campaignID); err != nil {
		t.Fatalf("valid email rejected: %v", err)
	}
}

func TestResetData(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewUserRepository(db)

	first := []domain.User{sampleUser("a@example.com"), sampleUser("b@example.com")}
	if err := repo.InsertBatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	campaignID := insertCampaign(t, db, "sending")
	if _, err := db.Exec(`INSERT INTO dispatch_logs (campaign_id, user_id, channel, status) VALUES (?, ?, 'sms', 'sent')`,
		campaignID, first[0].ID); err != nil {
		t.Fatal(err)
	}

	if err := ResetData(ctx, db); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, table := range []string{"users", "campaigns", "campaign_messages", "dispatch_logs"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s not empty after reset: %d (%v)", table, n, err)
		}
	}

	again := []domain.User{sampleUser("c@example.com")}
	if err := repo.InsertBatch(ctx, again); err != nil {
		t.Fatal(err)
	}
	if again[0].ID != 1 {
		t.Errorf("ids should restart at 1 after reset, got %d", again[0].ID)
	}
}

func TestResetDataOnFreshDatabase(t *testing.T) {
	db := openTestDB(t)
	if err := ResetData(context.Background(), db); err != nil {
		t.Fatalf("reset on empty db: %v", err)
	}
}
