package segment_test

// The segment engine against the REAL storage: the project's seed generator
// (3,000 users, cohorts from ADR 004), the repository schema and the
// v_users_segment VIEW that derives cart_last_active_days and
// has_purchased_since_cart from timestamps at query time. Counts are checked
// against the pure-Go oracle from seed_helpers_test.go, fed with the same
// derivation computed in Go.

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/repository"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/segment"
)

func openRealSeed(t *testing.T) (*sql.DB, []domain.User) {
	t.Helper()
	ctx := context.Background()
	db, err := repository.Open(ctx, filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	users, err := seed.Generate(seed.Config{Count: seed.DefaultCount, Seed: seed.DefaultSeed, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.NewUserRepository(db).InsertBatch(ctx, users); err != nil {
		t.Fatal(err)
	}
	return db, users
}

// viewRows derives the segment attributes in Go exactly like the view:
// whole days rounded up (in integer seconds, clamped at 0) and "purchased at
// or after the last cart activity", 0 when there is no cart.
func viewRows(users []domain.User, now time.Time) []userRow {
	nowSec := now.Unix()
	rows := make([]userRow, len(users))
	for i, u := range users {
		r := userRow{ID: u.ID, City: u.City, Age: int64(u.Age), TotalSpend: u.TotalSpend,
			OptInSMS: u.OptInSMS, OptInEmail: u.OptInEmail}
		if u.CartLastActiveAt != nil {
			d := max(0, (nowSec-u.CartLastActiveAt.Unix()+86399)/86400)
			r.CartLastActiveDays = &d
			r.HasPurchasedSinceCart = u.LastPurchaseAt != nil && !u.LastPurchaseAt.Before(*u.CartLastActiveAt)
		}
		rows[i] = r
	}
	return rows
}

func oracleIDs(rows []userRow, rule *segment.Node) []int64 {
	var ids []int64
	for i := range rows {
		if oracleMatch(&rows[i], rule) {
			ids = append(ids, rows[i].ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestReferenceBriefOnRealSeedThroughView(t *testing.T) {
	db, users := openRealSeed(t)
	ctx := context.Background()
	eng := segment.NewEngine(segment.NewMockLLMClient(), db, segment.DefaultRegistry, segment.SQLite{})

	p, err := eng.Preview(ctx, segment.ReferenceBrief)
	if err != nil {
		t.Fatalf("preview over the real view must not fail: %v", err)
	}
	now := time.Now()
	rows := viewRows(users, now)
	if want := oracleCount(rows, p.Rule); p.Audience != want {
		t.Fatalf("view counts %+v != oracle %+v", p.Audience, want)
	}
	a := p.Audience
	t.Logf("reference brief on the 3,000-user seed: %+v", a)
	if a.Total < 600 || a.SMSOptIn == 0 || a.EmailOptIn == 0 || a.SMSOptIn == a.Total || a.EmailOptIn == a.Total {
		t.Fatalf("implausible audience for the seed cohorts: %+v", a)
	}

	ids, err := eng.MatchingIDs(ctx, p.Rule)
	if err != nil {
		t.Fatal(err)
	}
	if want := oracleIDs(rows, p.Rule); !slices.Equal(ids, want) {
		t.Fatalf("MatchingIDs returned %d ids, oracle %d", len(ids), len(want))
	}

	// Relation to the seed's strict oracle (ADR 004): every strict match is
	// in the audience, and the only extra users are those who bought within
	// 30 days and then left a NEWER cart ("purchase before last cart
	// activity" reading, ADR 008).
	byID := make(map[int64]domain.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	in := make(map[int64]bool, len(ids))
	for _, id := range ids {
		in[id] = true
	}
	strict, extra := 0, 0
	for _, u := range users {
		if seed.MatchesSampleBrief(u, now) {
			strict++
			if !in[u.ID] {
				t.Errorf("user %d matches the strict brief but not the rule", u.ID)
			}
		}
	}
	for _, id := range ids {
		u := byID[id]
		if seed.MatchesSampleBrief(u, now) {
			continue
		}
		extra++
		cutoff := now.Add(-seed.BriefWindow)
		if u.LastPurchaseAt == nil || u.LastPurchaseAt.Before(cutoff) || !u.LastPurchaseAt.Before(*u.CartLastActiveAt) {
			t.Errorf("user %d is in the audience for an unexpected reason: %+v", id, u)
		}
	}
	if extra == 0 || int64(strict+extra) != a.Total {
		t.Fatalf("strict %d + rebuy-then-new-cart %d != total %d", strict, extra, a.Total)
	}
}

func TestRulesOnRealSeedMatchOracle(t *testing.T) {
	db, users := openRealSeed(t)
	ctx := context.Background()
	eng := segment.NewEngine(segment.NewMockLLMClient(), db, segment.DefaultRegistry, segment.SQLite{})
	rules := map[string]string{
		"young İzmir or Ankara": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"IN","value":["izmir","Ankara"]},
			{"type":"condition","field":"age","operator":"BETWEEN","value":[18,30]}]}`,
		"big spenders who never carted recently": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"total_spend","operator":"GREATER_THAN","value":2500},
			{"type":"group","logic":"NOT","rules":[
				{"type":"condition","field":"cart_last_active_days","operator":"LESS_THAN_OR_EQUAL","value":30}]}]}`,
		"converted carts": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"cart_last_active_days","operator":"LESS_THAN","value":60},
			{"type":"condition","field":"has_purchased_since_cart","operator":"NOT_EQUALS","value":false}]}`,
		"cart today or week-old cart outside İstanbul": `{"type":"group","logic":"OR","rules":[
			{"type":"condition","field":"cart_last_active_days","operator":"EQUALS","value":1},
			{"type":"group","logic":"AND","rules":[
				{"type":"condition","field":"city","operator":"NOT_EQUALS","value":"İstanbul"},
				{"type":"condition","field":"cart_last_active_days","operator":"BETWEEN","value":[7,8]}]}]}`,
	}
	for name, raw := range rules {
		t.Run(name, func(t *testing.T) {
			p, err := eng.PreviewJSON(ctx, []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			rows := viewRows(users, time.Now())
			if want := oracleCount(rows, p.Rule); p.Audience != want {
				t.Fatalf("view %+v != oracle %+v", p.Audience, want)
			}
			if p.Audience.Total == 0 {
				t.Fatalf("rule selects nobody; the case does not test anything")
			}
		})
	}
}

func TestMockDefaultResponse(t *testing.T) {
	ctx := context.Background()
	m := segment.NewMockLLMClient()
	if _, err := m.GenerateRule(ctx, segment.RuleRequest{Brief: "bilinmeyen brief"}); err == nil {
		t.Fatal("unknown brief must fail without a default")
	}
	m.Default(segment.ReferenceRule)
	raw, err := m.GenerateRule(ctx, segment.RuleRequest{Brief: "bilinmeyen brief"})
	if err != nil || string(raw) != segment.ReferenceRule {
		t.Fatalf("default not used: %s, %v", raw, err)
	}
}
