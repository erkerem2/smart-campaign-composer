package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/segment"
)

type viewRow struct {
	cartDays     sql.NullInt64
	sinceCart    int64
	optSMS, optE int64
}

func readViewRow(t *testing.T, db *sql.DB, id int64) viewRow {
	t.Helper()
	var r viewRow
	err := db.QueryRowContext(context.Background(),
		"SELECT cart_last_active_days, has_purchased_since_cart, opt_in_sms, opt_in_email FROM "+SegmentView+" WHERE id = ?", id).
		Scan(&r.cartDays, &r.sinceCart, &r.optSMS, &r.optE)
	if err != nil {
		t.Fatalf("read view row %d: %v", id, err)
	}
	return r
}

// The view derives the two time-relative segment attributes from the stored
// timestamps at query time.
func TestSegmentViewDerivesAttributes(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	const day = 24 * time.Hour

	cases := []struct {
		name          string
		cart, bought  *time.Time
		wantDays      any // nil = NULL
		wantSinceCart int64
	}{
		{"never used the cart", nil, ago(3 * day), nil, 0},
		{"cart an hour ago, never bought", ago(time.Hour), nil, int64(1), 0},
		{"cart 30d minus a minute ago is still day 30", ago(30*day - time.Minute), nil, int64(30), 0},
		{"cart 30d plus an hour ago is day 31", ago(30*day + time.Hour), nil, int64(31), 0},
		{"cart in the future (clock skew) clamps to 0", ago(-time.Hour), nil, int64(0), 0},
		{"bought after the cart", ago(5 * day), ago(2 * day), int64(5), 1},
		{"bought at the same second as the cart", ago(5 * day), ago(5 * day), int64(5), 1},
		{"bought before the cart (abandoned again)", ago(2 * day), ago(5 * day), int64(2), 0},
	}
	users := make([]domain.User, len(cases))
	for i, c := range cases {
		u := sampleUser("view" + string(rune('a'+i)) + "@example.com")
		u.CartLastActiveAt, u.LastPurchaseAt = c.cart, c.bought
		u.OptInSMS, u.OptInEmail = i%2 == 0, i%3 == 0
		users[i] = u
	}
	if err := NewUserRepository(db).InsertBatch(ctx, users); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		got := readViewRow(t, db, users[i].ID)
		switch want := c.wantDays.(type) {
		case nil:
			if got.cartDays.Valid {
				t.Errorf("%s: cart_last_active_days = %d, want NULL", c.name, got.cartDays.Int64)
			}
		case int64:
			if !got.cartDays.Valid || got.cartDays.Int64 != want {
				t.Errorf("%s: cart_last_active_days = %+v, want %d", c.name, got.cartDays, want)
			}
		}
		if got.sinceCart != c.wantSinceCart {
			t.Errorf("%s: has_purchased_since_cart = %d, want %d", c.name, got.sinceCart, c.wantSinceCart)
		}
		if (got.optSMS == 1) != users[i].OptInSMS || (got.optE == 1) != users[i].OptInEmail {
			t.Errorf("%s: opt-in flags not passed through", c.name)
		}
	}
}

// The view is the contract between storage and the segment whitelist: it
// must expose every whitelisted column and nothing personally identifying.
func TestSegmentViewMatchesRegistryAndHidesPII(t *testing.T) {
	db := openTestDB(t)
	reg := segment.DefaultRegistry
	if reg.Table != SegmentView {
		t.Fatalf("segment.DefaultRegistry.Table = %q, want %q", reg.Table, SegmentView)
	}
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", SegmentView)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	want := []string{"id", reg.SMSColumn, reg.EmailCol}
	for _, f := range reg.Fields() {
		want = append(want, f.Column)
	}
	for _, c := range want {
		if !slices.Contains(cols, c) {
			t.Errorf("view lacks whitelisted column %q (has %v)", c, cols)
		}
	}
	for _, pii := range []string{"first_name", "last_name", "email", "phone"} {
		if slices.Contains(cols, pii) {
			t.Errorf("view must not expose %q", pii)
		}
	}
}

// A database at schema version 2 gets campaigns.segment_rule and the view;
// existing campaigns keep their data with a NULL rule.
func TestMigrateV2ToV3(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v2 := `
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, first_name TEXT NOT NULL, last_name TEXT NOT NULL,
  email TEXT NOT NULL UNIQUE COLLATE NOCASE, phone TEXT NOT NULL, city TEXT NOT NULL,
  age INTEGER NOT NULL, total_spend REAL NOT NULL DEFAULT 0, cart_last_active_at TEXT, last_purchase_at TEXT,
  opt_in_sms INTEGER NOT NULL DEFAULT 0, opt_in_email INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE TABLE campaigns (id INTEGER PRIMARY KEY AUTOINCREMENT, brief TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'draft', created_at TEXT NOT NULL);
INSERT INTO campaigns (brief, status, created_at) VALUES ('eski', 'completed', '2026-01-01T00:00:00Z');
CREATE VIEW v_users_segment AS SELECT id FROM users; -- stale definition
PRAGMA user_version = 2;`
	if _, err := db.ExecContext(ctx, v2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	repo := NewCampaignRepository(db)
	old, err := repo.GetByID(ctx, 1)
	if err != nil || old.Brief != "eski" || old.SegmentRule != nil {
		t.Fatalf("legacy campaign = %+v, %v", old, err)
	}
	// The stale view was replaced by the current definition.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name = 'cart_last_active_days'", SegmentView).Scan(&n); err != nil || n != 1 {
		t.Fatalf("view not recreated: %d, %v", n, err)
	}
	// json_valid CHECK guards the new column.
	if _, err := db.Exec("UPDATE campaigns SET segment_rule = 'SELECT 1' WHERE id = 1"); err == nil {
		t.Fatal("non-JSON segment_rule accepted")
	}
}

func TestCreateApproved(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewCampaignRepository(db)
	now := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	rule := json.RawMessage(`{"type":"group","logic":"AND","rules":[{"type":"condition","field":"city","operator":"EQUALS","value":"İstanbul"}]}`)

	c, err := repo.CreateApproved(ctx, " "+composer.ReferenceBrief+" ", rule, composer.ReferenceMessage.CampaignMessages(0), now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, c.ID)
	if err != nil || got.Status != domain.CampaignStatusApproved || got.Brief != composer.ReferenceBrief ||
		string(got.SegmentRule) != string(rule) || !got.CreatedAt.Equal(now) {
		t.Fatalf("campaign = %+v, %v", got, err)
	}
	msgs, err := repo.Messages(ctx, c.ID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	for _, m := range msgs {
		if m.Status != domain.MessageStatusApproved || m.CampaignID != c.ID {
			t.Errorf("message not approved: %+v", m)
		}
	}
	if back := composer.MessageFromCampaign(msgs); back != composer.ReferenceMessage {
		t.Fatalf("content changed: %+v", back)
	}
	// Approved content is immutable.
	if err := repo.SaveDraftMessages(ctx, c.ID, composer.ReferenceMessage.CampaignMessages(c.ID)); !errors.Is(err, ErrCampaignNotDraft) {
		t.Fatalf("approved content must be immutable: %v", err)
	}

	// Failures roll back completely: no campaign row without messages.
	bad := []struct {
		name string
		rule json.RawMessage
		msgs []domain.CampaignMessage
	}{
		{"no rule", nil, composer.ReferenceMessage.CampaignMessages(0)},
		{"rule is not JSON", json.RawMessage(`SELECT * FROM users`), composer.ReferenceMessage.CampaignMessages(0)},
		{"no messages", rule, nil},
		{"sms with subject", rule, []domain.CampaignMessage{{Channel: domain.ChannelSMS, Content: "x", Subject: "y"}}},
		{"duplicate channel", rule, []domain.CampaignMessage{{Channel: domain.ChannelSMS, Content: "a"}, {Channel: domain.ChannelSMS, Content: "b"}}},
	}
	for _, b := range bad {
		if _, err := repo.CreateApproved(ctx, "brief", b.rule, b.msgs, now); err == nil {
			t.Errorf("%s: want error", b.name)
		}
	}
	var campaigns, messages int
	_ = db.QueryRow("SELECT count(*) FROM campaigns").Scan(&campaigns)
	_ = db.QueryRow("SELECT count(*) FROM campaign_messages").Scan(&messages)
	if campaigns != 1 || messages != 2 {
		t.Fatalf("failed creates leaked rows: %d campaigns, %d messages", campaigns, messages)
	}
}
