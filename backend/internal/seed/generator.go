// Package seed generates realistic fake users for local development, demos
// and tests. Output is deterministic for a given (Seed, Now) pair.
package seed

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

const (
	// DefaultCount is the number of users the seed command creates.
	DefaultCount = 3000
	// MinCount keeps every cohort non-empty.
	MinCount = 100
	// DefaultSeed makes the default dataset reproducible.
	DefaultSeed uint64 = 42

	// CityIstanbul is the canonical spelling used in the users table.
	CityIstanbul = "İstanbul"

	day = 24 * time.Hour
	// BriefWindow is the "son 30 gün" window of the sample brief.
	BriefWindow = 30 * day
)

// Config controls dataset generation.
type Config struct {
	Count int
	Seed  uint64
	// Now anchors every relative date ("last 30 days"). Pass time.Now() in
	// production and a fixed instant in tests.
	Now time.Time
}

// MatchesSampleBrief is a reference oracle for the case's sample brief:
//
//	"Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılar"
//
// interpreted strictly as: lives in İstanbul, cart activity within the last
// 30 days, and no purchase within the last 30 days. It deliberately ignores
// opt-in flags; channel permission is a dispatch-time concern. Segment
// engine tests can compare their query results against this function.
func MatchesSampleBrief(u domain.User, now time.Time) bool {
	if u.City != CityIstanbul || u.CartLastActiveAt == nil {
		return false
	}
	cutoff := now.Add(-BriefWindow)
	if u.CartLastActiveAt.Before(cutoff) {
		return false
	}
	return u.LastPurchaseAt == nil || u.LastPurchaseAt.Before(cutoff)
}

// cohort is a slice of the population with a known behavioural shape. The
// shapes are chosen so a segment rule for the sample brief has true
// positives, near misses on every condition, and opt-outs to filter.
type cohort struct {
	name  string
	share float64 // fraction of Count; the last cohort takes the remainder
	build func(g *generator, u *domain.User)
}

var cohorts = []cohort{
	// True positives: İstanbul, cart in the last 30 days, no purchase in
	// the last 30 days (never purchased, or last purchase older than 30d).
	{"istanbul_abandoned_cart", 0.22, func(g *generator, u *domain.User) {
		u.City = CityIstanbul
		g.abandonedCartDates(u)
	}},
	// Near miss: added to cart recently but then purchased (converted).
	{"istanbul_converted", 0.06, func(g *generator, u *domain.User) {
		u.City = CityIstanbul
		cartAge := g.between(2*day, 25*day)
		u.CartLastActiveAt = g.ago(cartAge)
		u.LastPurchaseAt = g.ago(g.between(time.Hour, cartAge-time.Hour))
	}},
	// Near miss: abandoned a cart, but more than 30 days ago.
	{"istanbul_stale_cart", 0.06, func(g *generator, u *domain.User) {
		u.City = CityIstanbul
		cartAge := g.between(BriefWindow+time.Hour, 120*day)
		u.CartLastActiveAt = g.ago(cartAge)
		if g.chance(0.5) {
			u.LastPurchaseAt = g.ago(g.between(cartAge+day, 600*day))
		}
	}},
	// Ambiguous case: purchased within 30 days, then left a newer cart.
	// Excluded by the strict reading of the brief, included by a
	// "purchase before last cart activity" reading.
	{"istanbul_rebuy_new_cart", 0.04, func(g *generator, u *domain.User) {
		u.City = CityIstanbul
		purchaseAge := g.between(8*day, BriefWindow-day)
		u.LastPurchaseAt = g.ago(purchaseAge)
		u.CartLastActiveAt = g.ago(g.between(time.Hour, purchaseAge-day))
	}},
	// Near miss: İstanbul users who never used the cart.
	{"istanbul_no_cart", 0.07, func(g *generator, u *domain.User) {
		u.City = CityIstanbul
		if g.chance(0.6) {
			u.LastPurchaseAt = g.ago(g.between(day, 400*day))
		}
	}},
	// Near miss on city only: same behaviour as the true positives.
	{"other_city_abandoned_cart", 0.15, func(g *generator, u *domain.User) {
		u.City = g.weighted(otherCities)
		g.abandonedCartDates(u)
	}},
	// Background population with unconstrained behaviour.
	{"general", 0, func(g *generator, u *domain.User) {
		if g.chance(0.25) {
			u.City = CityIstanbul
		} else {
			u.City = g.weighted(otherCities)
		}
		if g.chance(0.55) {
			u.CartLastActiveAt = g.ago(g.between(time.Hour, 365*day))
		}
		if g.chance(0.7) {
			u.LastPurchaseAt = g.ago(g.between(time.Hour, 730*day))
		}
	}},
}

// Generate builds cfg.Count users. The result is shuffled so cohorts are
// not contiguous by id.
func Generate(cfg Config) ([]domain.User, error) {
	if cfg.Count < MinCount {
		return nil, fmt.Errorf("count must be at least %d, got %d", MinCount, cfg.Count)
	}
	if cfg.Now.IsZero() {
		return nil, errors.New("now must be set")
	}

	g := &generator{
		r:          rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		now:        cfg.Now.UTC().Truncate(time.Second),
		usedEmails: make(map[string]struct{}, cfg.Count),
		usedPhones: make(map[string]struct{}, cfg.Count),
	}

	users := make([]domain.User, 0, cfg.Count)
	remaining := cfg.Count
	for i, c := range cohorts {
		n := remaining
		if i < len(cohorts)-1 {
			n = int(c.share * float64(cfg.Count))
		}
		for j := 0; j < n; j++ {
			u := g.baseUser()
			c.build(g, &u)
			g.assignOptIns(&u, j)
			g.finalize(&u)
			users = append(users, u)
		}
		remaining -= n
	}

	g.r.Shuffle(len(users), func(i, j int) { users[i], users[j] = users[j], users[i] })
	return users, nil
}

type generator struct {
	r          *rand.Rand
	now        time.Time
	usedEmails map[string]struct{}
	usedPhones map[string]struct{}
}

func (g *generator) baseUser() domain.User {
	first := firstNames[g.r.IntN(len(firstNames))]
	last := lastNames[g.r.IntN(len(lastNames))]
	return domain.User{
		FirstName: first,
		LastName:  last,
		Email:     g.uniqueEmail(first, last),
		Phone:     g.uniquePhone(),
		Age:       g.age(),
	}
}

// abandonedCartDates: cart within the window, no purchase within it.
func (g *generator) abandonedCartDates(u *domain.User) {
	u.CartLastActiveAt = g.ago(g.between(time.Hour, BriefWindow-time.Hour))
	if g.chance(0.65) {
		u.LastPurchaseAt = g.ago(g.between(BriefWindow+day, 540*day))
	}
}

// assignOptIns guarantees every cohort contains SMS-only opt-outs,
// email-only opt-outs and users who opted out of both, so channel
// permission filtering is always exercised. idx is the index within the
// cohort; the final shuffle hides the pattern.
func (g *generator) assignOptIns(u *domain.User, idx int) {
	switch idx % 10 {
	case 0:
		u.OptInSMS, u.OptInEmail = false, true
	case 1:
		u.OptInSMS, u.OptInEmail = true, false
	case 2:
		u.OptInSMS, u.OptInEmail = false, false
	default:
		u.OptInSMS = g.chance(0.85)
		u.OptInEmail = g.chance(0.90)
	}
}

// finalize derives fields that depend on the behavioural dates so every
// user is internally consistent.
func (g *generator) finalize(u *domain.User) {
	if u.LastPurchaseAt == nil {
		u.TotalSpend = 0
	} else {
		u.TotalSpend = g.spend()
	}

	// Account creation precedes every recorded activity.
	oldest := time.Duration(0)
	for _, t := range []*time.Time{u.CartLastActiveAt, u.LastPurchaseAt} {
		if t != nil {
			if age := g.now.Sub(*t); age > oldest {
				oldest = age
			}
		}
	}
	u.CreatedAt = *g.ago(oldest + g.between(day, 720*day))
}

func (g *generator) ago(d time.Duration) *time.Time {
	t := g.now.Add(-d).Truncate(time.Second)
	return &t
}

// between returns a duration uniformly distributed in [lo, hi).
func (g *generator) between(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(g.r.Int64N(int64(hi-lo)))
}

func (g *generator) chance(p float64) bool { return g.r.Float64() < p }

type weightedString struct {
	value  string
	weight int
}

func (g *generator) weighted(items []weightedString) string {
	total := 0
	for _, it := range items {
		total += it.weight
	}
	n := g.r.IntN(total)
	for _, it := range items {
		if n < it.weight {
			return it.value
		}
		n -= it.weight
	}
	return items[len(items)-1].value
}

type ageBand struct{ min, max, weight int }

var ageBands = []ageBand{
	{18, 24, 18},
	{25, 34, 32},
	{35, 44, 24},
	{45, 54, 15},
	{55, 70, 11},
}

func (g *generator) age() int {
	total := 0
	for _, b := range ageBands {
		total += b.weight
	}
	n := g.r.IntN(total)
	for _, b := range ageBands {
		if n < b.weight {
			return b.min + g.r.IntN(b.max-b.min+1)
		}
		n -= b.weight
	}
	return 30
}

type spendBand struct {
	min, max float64
	weight   int
}

var spendBands = []spendBand{
	{50, 500, 45},
	{500, 2500, 35},
	{2500, 10000, 15},
	{10000, 50000, 5},
}

func (g *generator) spend() float64 {
	total := 0
	for _, b := range spendBands {
		total += b.weight
	}
	n := g.r.IntN(total)
	for _, b := range spendBands {
		if n < b.weight {
			v := b.min + g.r.Float64()*(b.max-b.min)
			return float64(int64(v*100)) / 100 // 2 decimals
		}
		n -= b.weight
	}
	return 0
}

var asciiFold = strings.NewReplacer(
	"ç", "c", "Ç", "c", "ğ", "g", "Ğ", "g", "ı", "i", "İ", "i",
	"ö", "o", "Ö", "o", "ş", "s", "Ş", "s", "ü", "u", "Ü", "u",
)

func slug(s string) string { return strings.ToLower(asciiFold.Replace(s)) }

// emailDomains are RFC 2606 reserved domains: they can never belong to a
// real mailbox, so a misconfigured provider cannot reach a real person.
var emailDomains = []string{"example.com", "example.net", "example.org"}

func (g *generator) uniqueEmail(first, last string) string {
	f, l := slug(first), slug(last)
	var local string
	switch g.r.IntN(4) {
	case 0:
		local = f + "." + l
	case 1:
		local = f + l
	case 2:
		local = f + "_" + l
	default:
		local = f[:1] + "." + l
	}
	domainName := emailDomains[g.r.IntN(len(emailDomains))]
	for n := 1; ; n++ {
		candidate := local
		if n > 1 {
			candidate = fmt.Sprintf("%s%d", local, n)
		}
		addr := candidate + "@" + domainName
		if _, taken := g.usedEmails[addr]; !taken {
			g.usedEmails[addr] = struct{}{}
			return addr
		}
	}
}

// mobilePrefixes are Turkish GSM operator prefixes (5XX).
var mobilePrefixes = []string{
	"530", "531", "532", "533", "534", "535", "536", "537", "538", "539",
	"541", "542", "543", "544", "545", "546", "547", "548", "549",
	"551", "552", "553", "554", "555", "559",
}

func (g *generator) uniquePhone() string {
	for {
		p := fmt.Sprintf("+90%s%07d", mobilePrefixes[g.r.IntN(len(mobilePrefixes))], g.r.IntN(10_000_000))
		if _, taken := g.usedPhones[p]; !taken {
			g.usedPhones[p] = struct{}{}
			return p
		}
	}
}

var otherCities = []weightedString{
	{"Ankara", 18}, {"İzmir", 15}, {"Bursa", 9}, {"Antalya", 8},
	{"Adana", 6}, {"Konya", 6}, {"Kocaeli", 6}, {"Gaziantep", 5},
	{"Mersin", 5}, {"Eskişehir", 4}, {"Kayseri", 4}, {"Trabzon", 4},
	{"Samsun", 4}, {"Diyarbakır", 4},
}

var firstNames = []string{
	"Ayşe", "Fatma", "Zeynep", "Elif", "Merve", "Büşra", "Esra", "Selin", "Deniz", "İrem",
	"Ceren", "Gizem", "Ebru", "Sena", "Derya", "Özge", "Melis", "Nazlı", "Pınar", "Şeyma",
	"Mehmet", "Mustafa", "Ahmet", "Ali", "Hüseyin", "Emre", "Burak", "Can", "Kerem", "Mert",
	"Oğuz", "Serkan", "Murat", "Hakan", "Barış", "Onur", "Tolga", "Uğur", "Çağrı", "Yusuf",
	"Ege", "Arda", "Efe", "Kaan", "Gökhan", "Ece", "Defne", "Ilgaz", "Su", "Umut",
}

var lastNames = []string{
	"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir",
	"Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek",
	"Polat", "Korkmaz", "Karaca", "Güneş", "Aksoy", "Bulut", "Tekin", "Ünal", "Avcı", "Erdem",
	"Acar", "Kaplan", "Turan", "Çakır", "Uçar", "Tan", "Ateş", "Sarı", "Güler", "Akın",
}
