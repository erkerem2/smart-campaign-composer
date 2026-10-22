package segment_test

// Test fixture: a deterministic 3,000-user SQLite database plus a pure-Go
// "oracle" that evaluates a rule tree row by row WITHOUT SQL. Integration
// tests compare the SQL counts against the oracle, so they verify the
// compiler's semantics rather than just a magic number.
//
// To run against the project's real seed instead, replace openSeededDB with
// the seeder from session 1; the oracle comparison keeps working unchanged.

import (
	"database/sql"
	"math/rand/v2"
	"testing"

	"smart-campaign-composer/backend/internal/segment"
)

const seedUsers = 3000

type userRow struct {
	ID                    int64
	City                  string
	Age                   int64
	TotalSpend            float64
	CartLastActiveDays    *int64 // nil = no cart activity
	HasPurchasedSinceCart bool
	OptInSMS, OptInEmail  bool
}

const schemaSQL = `
CREATE TABLE users (
	id                        INTEGER PRIMARY KEY,
	email                     TEXT    NOT NULL UNIQUE,
	password_hash             TEXT    NOT NULL,
	city                      TEXT    NOT NULL,
	age                       INTEGER NOT NULL,
	total_spend               REAL    NOT NULL DEFAULT 0,
	cart_last_active_days     INTEGER,
	has_purchased_since_cart  INTEGER NOT NULL DEFAULT 0,
	opt_in_sms                INTEGER NOT NULL DEFAULT 0,
	opt_in_email              INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_users_city ON users(city);
CREATE INDEX idx_users_cart ON users(cart_last_active_days);`

// Raw city spellings as they might arrive from different sources. The seeder
// normalises them on write with segment.NormalizeCity.
var rawCities = []struct {
	spellings []string
	weight    int
}{
	{[]string{"İstanbul", "Istanbul", "istanbul", "ISTANBUL"}, 36},
	{[]string{"Ankara", "ankara"}, 14},
	{[]string{"İzmir", "Izmir", "izmir"}, 11},
	{[]string{"Bursa"}, 7},
	{[]string{"Antalya"}, 6},
	{[]string{"Kocaeli", "Izmit"}, 5},
	{[]string{"Konya"}, 5},
	{[]string{"Adana"}, 4},
	{[]string{"Gaziantep", "Antep"}, 4},
	{[]string{"Eskişehir", "Eskisehir"}, 4},
	{[]string{"Trabzon"}, 4},
}

func generateUsers(t testing.TB) []userRow {
	t.Helper()
	rng := rand.New(rand.NewPCG(42, 2026))
	total := 0
	for _, c := range rawCities {
		total += c.weight
	}
	users := make([]userRow, 0, seedUsers)
	for i := 1; i <= seedUsers; i++ {
		pick, raw := rng.IntN(total), ""
		for _, c := range rawCities {
			if pick < c.weight {
				raw = c.spellings[rng.IntN(len(c.spellings))]
				break
			}
			pick -= c.weight
		}
		city, err := segment.NormalizeCity(raw)
		if err != nil {
			t.Fatalf("seed city %q: %v", raw, err)
		}
		u := userRow{
			ID:         int64(i),
			City:       city,
			Age:        int64(18 + rng.IntN(53)),
			TotalSpend: float64(rng.IntN(2_000_000)) / 100,
			OptInSMS:   rng.Float64() < 0.55,
			OptInEmail: rng.Float64() < 0.72,
		}
		if rng.Float64() < 0.65 { // has some cart activity
			d := int64(rng.IntN(91))
			u.CartLastActiveDays = &d
			u.HasPurchasedSinceCart = rng.Float64() < 0.40
		}
		users = append(users, u)
	}
	return users
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func openSeededDB(t testing.TB) (*sql.DB, []userRow) {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one shared in-memory database
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	users := generateUsers(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO users (id, email, password_hash, city, age, total_spend,
		cart_last_active_days, has_purchased_since_cart, opt_in_sms, opt_in_email)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		var cart any
		if u.CartLastActiveDays != nil {
			cart = *u.CartLastActiveDays
		}
		if _, err := stmt.Exec(u.ID, "user"+itoa(u.ID)+"@example.com", "argon2id$fake", u.City, u.Age, u.TotalSpend,
			cart, b2i(u.HasPurchasedSinceCart), b2i(u.OptInSMS), b2i(u.OptInEmail)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db, users
}

func itoa(i int64) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

// ---------------- oracle ----------------

func oracleCount(users []userRow, rule *segment.Node) segment.AudienceCounts {
	var c segment.AudienceCounts
	for i := range users {
		u := &users[i]
		if !oracleMatch(u, rule) {
			continue
		}
		c.Total++
		if u.OptInSMS {
			c.SMSOptIn++
		}
		if u.OptInEmail {
			c.EmailOptIn++
		}
		if u.OptInSMS || u.OptInEmail {
			c.Reachable++
		}
	}
	return c
}

// oracleMatch mirrors SQL three-valued logic for the one nullable column:
// a comparison against NULL is UNKNOWN, and NOT UNKNOWN is still UNKNOWN,
// so the row is excluded either way.
func oracleMatch(u *userRow, n *segment.Node) bool { return eval(u, n) == tTrue }

type tri int

const (
	tFalse tri = iota
	tTrue
	tUnknown
)

func eval(u *userRow, n *segment.Node) tri {
	if n.Type == segment.NodeGroup {
		switch n.Logic {
		case segment.LogicNot:
			switch eval(u, n.Rules[0]) {
			case tTrue:
				return tFalse
			case tFalse:
				return tTrue
			}
			return tUnknown
		case segment.LogicAnd:
			res := tTrue
			for _, r := range n.Rules {
				switch eval(u, r) {
				case tFalse:
					return tFalse
				case tUnknown:
					res = tUnknown
				}
			}
			return res
		default: // OR
			res := tFalse
			for _, r := range n.Rules {
				switch eval(u, r) {
				case tTrue:
					return tTrue
				case tUnknown:
					res = tUnknown
				}
			}
			return res
		}
	}

	var actual any
	switch n.Field {
	case "city":
		actual = u.City
	case "age":
		actual = float64(u.Age)
	case "total_spend":
		actual = u.TotalSpend
	case "cart_last_active_days":
		if u.CartLastActiveDays == nil {
			return tUnknown
		}
		actual = float64(*u.CartLastActiveDays)
	case "has_purchased_since_cart":
		actual = u.HasPurchasedSinceCart
	}
	num := func(v any) float64 {
		switch x := v.(type) {
		case int64:
			return float64(x)
		case float64:
			return x
		}
		return 0
	}
	b := func(ok bool) tri {
		if ok {
			return tTrue
		}
		return tFalse
	}
	switch n.Operator {
	case segment.OpEquals, segment.OpNotEquals:
		var eq bool
		switch a := actual.(type) {
		case float64:
			eq = a == num(n.Value)
		default:
			eq = a == n.Value
		}
		return b(eq == (n.Operator == segment.OpEquals))
	case segment.OpIn, segment.OpNotIn:
		in := false
		for _, v := range n.Values {
			if f, ok := actual.(float64); ok {
				in = in || f == num(v)
			} else {
				in = in || actual == v
			}
		}
		return b(in == (n.Operator == segment.OpIn))
	case segment.OpBetween:
		f := actual.(float64)
		return b(f >= num(n.Values[0]) && f <= num(n.Values[1]))
	}
	f, v := actual.(float64), num(n.Value)
	switch n.Operator {
	case segment.OpGreaterThan:
		return b(f > v)
	case segment.OpGreaterThanOrEqual:
		return b(f >= v)
	case segment.OpLessThan:
		return b(f < v)
	case segment.OpLessThanOrEqual:
		return b(f <= v)
	}
	panic("oracle: unhandled operator " + n.Operator)
}
