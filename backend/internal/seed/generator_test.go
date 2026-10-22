package seed

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func generate(t *testing.T) []domain.User {
	t.Helper()
	users, err := Generate(Config{Count: DefaultCount, Seed: DefaultSeed, Now: testNow})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return users
}

func TestGenerateCountAndValidation(t *testing.T) {
	if got := len(generate(t)); got != DefaultCount {
		t.Fatalf("got %d users, want %d", got, DefaultCount)
	}
	if _, err := Generate(Config{Count: MinCount - 1, Now: testNow}); err == nil {
		t.Error("count below minimum accepted")
	}
	if _, err := Generate(Config{Count: DefaultCount}); err == nil {
		t.Error("zero Now accepted")
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a := generate(t)
	b := generate(t)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed and now produced different datasets")
	}
	c, _ := Generate(Config{Count: DefaultCount, Seed: DefaultSeed + 1, Now: testNow})
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical datasets")
	}
}

func TestUsersAreInternallyConsistent(t *testing.T) {
	emails := map[string]bool{}
	phones := map[string]bool{}
	for _, u := range generate(t) {
		if u.FirstName == "" || u.LastName == "" || u.City == "" {
			t.Fatalf("missing identity fields: %+v", u)
		}
		key := strings.ToLower(u.Email)
		if emails[key] {
			t.Fatalf("duplicate email %s", u.Email)
		}
		emails[key] = true
		if !strings.HasSuffix(u.Email, "@example.com") && !strings.HasSuffix(u.Email, "@example.net") &&
			!strings.HasSuffix(u.Email, "@example.org") {
			t.Fatalf("email outside reserved domains: %s", u.Email)
		}
		if strings.ContainsAny(u.Email, "çğıöşüÇĞİÖŞÜ") {
			t.Fatalf("non-ASCII email: %s", u.Email)
		}
		if phones[u.Phone] {
			t.Fatalf("duplicate phone %s", u.Phone)
		}
		phones[u.Phone] = true
		if len(u.Phone) != 13 || !strings.HasPrefix(u.Phone, "+905") {
			t.Fatalf("phone not in +905XXXXXXXXX form: %s", u.Phone)
		}
		if u.Age < 18 || u.Age > 70 {
			t.Fatalf("age out of range: %d", u.Age)
		}
		if (u.LastPurchaseAt == nil) != (u.TotalSpend == 0) {
			t.Fatalf("spend %.2f inconsistent with purchase %v", u.TotalSpend, u.LastPurchaseAt)
		}
		for _, ts := range []*time.Time{u.CartLastActiveAt, u.LastPurchaseAt} {
			if ts == nil {
				continue
			}
			if ts.After(testNow) {
				t.Fatalf("activity in the future: %v", ts)
			}
			if u.CreatedAt.After(*ts) {
				t.Fatalf("created_at %v after activity %v", u.CreatedAt, ts)
			}
		}
	}
}

func TestDatasetCoversSampleBrief(t *testing.T) {
	users := generate(t)
	s := Summarize(users, testNow)

	// At least ~20% of users are true positives for the sample brief.
	if s.BriefMatch < DefaultCount/5 {
		t.Errorf("too few brief matches: %d", s.BriefMatch)
	}
	// Opt-outs exist inside the target audience on both channels, so the
	// permission check changes the reachable count.
	if s.BriefMatchSMS >= s.BriefMatch || s.BriefMatchEmail >= s.BriefMatch {
		t.Errorf("opt-outs missing from target audience: %+v", s)
	}
	if s.BothOptOut == 0 {
		t.Error("no users opted out of both channels")
	}

	var (
		otherCityAbandoned, converted, staleCart, rebuy, noCart int
		cutoff                                                  = testNow.Add(-BriefWindow)
	)
	for _, u := range users {
		recentCart := u.CartLastActiveAt != nil && !u.CartLastActiveAt.Before(cutoff)
		recentPurchase := u.LastPurchaseAt != nil && !u.LastPurchaseAt.Before(cutoff)
		switch {
		case u.City != CityIstanbul && recentCart && !recentPurchase:
			otherCityAbandoned++
		case u.City != CityIstanbul:
		case u.CartLastActiveAt == nil:
			noCart++
		case !recentCart:
			staleCart++
		case recentPurchase && u.LastPurchaseAt.After(*u.CartLastActiveAt):
			converted++
		case recentPurchase:
			rebuy++
		}
	}
	for name, n := range map[string]int{
		"other-city abandoned carts": otherCityAbandoned,
		"istanbul converted":         converted,
		"istanbul stale carts":       staleCart,
		"istanbul rebuy + new cart":  rebuy,
		"istanbul no cart":           noCart,
	} {
		if n < 50 {
			t.Errorf("near-miss cohort %q too small: %d", name, n)
		}
	}
}

func TestDatasetDiversity(t *testing.T) {
	s := Summarize(generate(t), testNow)
	if len(s.ByCity) < 10 {
		t.Errorf("only %d cities", len(s.ByCity))
	}
	if share := float64(s.ByCity[CityIstanbul]) / float64(s.Total); share < 0.4 || share > 0.65 {
		t.Errorf("İstanbul share %.2f outside expected range", share)
	}
	for _, g := range ageGroupOrder {
		if s.ByAgeGroup[g] == 0 {
			t.Errorf("age group %s empty", g)
		}
	}
	for _, tier := range spendTierOrder {
		if s.BySpendTier[tier] == 0 {
			t.Errorf("spend tier %s empty", tier)
		}
	}
}

func TestMatchesSampleBrief(t *testing.T) {
	at := func(d time.Duration) *time.Time { t := testNow.Add(-d); return &t }
	tests := []struct {
		name string
		u    domain.User
		want bool
	}{
		{"never purchased, recent cart", domain.User{City: CityIstanbul, CartLastActiveAt: at(3 * day)}, true},
		{"old purchase, recent cart", domain.User{City: CityIstanbul, CartLastActiveAt: at(3 * day), LastPurchaseAt: at(90 * day)}, true},
		{"cart exactly at window edge", domain.User{City: CityIstanbul, CartLastActiveAt: at(BriefWindow)}, true},
		{"cart just outside window", domain.User{City: CityIstanbul, CartLastActiveAt: at(BriefWindow + time.Second)}, false},
		{"purchased after cart", domain.User{City: CityIstanbul, CartLastActiveAt: at(10 * day), LastPurchaseAt: at(2 * day)}, false},
		{"purchased recently before cart", domain.User{City: CityIstanbul, CartLastActiveAt: at(2 * day), LastPurchaseAt: at(10 * day)}, false},
		{"no cart", domain.User{City: CityIstanbul}, false},
		{"other city", domain.User{City: "Ankara", CartLastActiveAt: at(3 * day)}, false},
		{"ascii spelling is not canonical", domain.User{City: "Istanbul", CartLastActiveAt: at(3 * day)}, false},
	}
	for _, tt := range tests {
		if got := MatchesSampleBrief(tt.u, testNow); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSummaryString(t *testing.T) {
	out := Summarize(generate(t), testNow).String()
	for _, want := range []string{"Total users:", "Sample brief matches:", "İstanbul", "55+", "10K+"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}
