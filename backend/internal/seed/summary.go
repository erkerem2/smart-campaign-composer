package seed

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// Summary describes a generated dataset; the seed command prints it so the
// shape of the data is visible without opening the database.
type Summary struct {
	Total          int
	ByCity         map[string]int
	ByAgeGroup     map[string]int
	BySpendTier    map[string]int
	NeverPurchased int
	SMSOptOut      int // opted out of SMS (includes both-out)
	EmailOptOut    int // opted out of email (includes both-out)
	BothOptOut     int

	// Users matching the sample brief (strict reading), and how many of
	// them each channel may legally reach.
	BriefMatch      int
	BriefMatchSMS   int
	BriefMatchEmail int
}

// AgeGroup buckets an age for reporting and segment examples.
func AgeGroup(age int) string {
	switch {
	case age < 25:
		return "18-24"
	case age < 35:
		return "25-34"
	case age < 45:
		return "35-44"
	case age < 55:
		return "45-54"
	default:
		return "55+"
	}
}

// SpendTier buckets lifetime spend (TRY).
func SpendTier(spend float64) string {
	switch {
	case spend == 0:
		return "none"
	case spend < 500:
		return "<500"
	case spend < 2500:
		return "500-2.5K"
	case spend < 10000:
		return "2.5K-10K"
	default:
		return "10K+"
	}
}

var (
	ageGroupOrder  = []string{"18-24", "25-34", "35-44", "45-54", "55+"}
	spendTierOrder = []string{"none", "<500", "500-2.5K", "2.5K-10K", "10K+"}
)

// Summarize computes a Summary for users relative to now.
func Summarize(users []domain.User, now time.Time) Summary {
	s := Summary{
		Total:       len(users),
		ByCity:      map[string]int{},
		ByAgeGroup:  map[string]int{},
		BySpendTier: map[string]int{},
	}
	for _, u := range users {
		s.ByCity[u.City]++
		s.ByAgeGroup[AgeGroup(u.Age)]++
		s.BySpendTier[SpendTier(u.TotalSpend)]++
		if u.LastPurchaseAt == nil {
			s.NeverPurchased++
		}
		if !u.OptInSMS {
			s.SMSOptOut++
		}
		if !u.OptInEmail {
			s.EmailOptOut++
		}
		if !u.OptInSMS && !u.OptInEmail {
			s.BothOptOut++
		}
		if MatchesSampleBrief(u, now) {
			s.BriefMatch++
			if u.CanReceive(domain.ChannelSMS) {
				s.BriefMatchSMS++
			}
			if u.CanReceive(domain.ChannelEmail) {
				s.BriefMatchEmail++
			}
		}
	}
	return s
}

// String renders the summary as a human-readable report.
func (s Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Total users:            %d\n", s.Total)
	fmt.Fprintf(&b, "Never purchased:        %d\n", s.NeverPurchased)
	fmt.Fprintf(&b, "SMS opt-out:            %d\n", s.SMSOptOut)
	fmt.Fprintf(&b, "Email opt-out:          %d\n", s.EmailOptOut)
	fmt.Fprintf(&b, "Both opt-out:           %d\n", s.BothOptOut)
	fmt.Fprintf(&b, "Sample brief matches:   %d (SMS reachable %d, email reachable %d)\n",
		s.BriefMatch, s.BriefMatchSMS, s.BriefMatchEmail)
	writeCounts(&b, "By city", s.ByCity, nil)
	writeCounts(&b, "By age group", s.ByAgeGroup, ageGroupOrder)
	writeCounts(&b, "By spend tier (TRY)", s.BySpendTier, spendTierOrder)
	return b.String()
}

// writeCounts prints m in the given key order, or by descending count when
// order is nil.
func writeCounts(b *strings.Builder, title string, m map[string]int, order []string) {
	keys := order
	if keys == nil {
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if m[keys[i]] != m[keys[j]] {
				return m[keys[i]] > m[keys[j]]
			}
			return keys[i] < keys[j]
		})
	}
	fmt.Fprintf(b, "%s:\n", title)
	for _, k := range keys {
		fmt.Fprintf(b, "  %-12s %d\n", k, m[k])
	}
}
