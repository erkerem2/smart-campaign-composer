package segment

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestBuildReferenceSQLite(t *testing.T) {
	w, err := NewBuilder(DefaultRegistry, SQLite{}).Build(mustParse(t, ReferenceRule))
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := "(city = ? AND cart_last_active_days <= ? AND has_purchased_since_cart = ?)"
	if w.SQL != wantSQL {
		t.Errorf("SQL\n got: %s\nwant: %s", w.SQL, wantSQL)
	}
	if want := []any{"İstanbul", int64(30), int64(0)}; !reflect.DeepEqual(w.Args, want) {
		t.Errorf("args got %#v want %#v", w.Args, want)
	}
}

func TestBuildNestedPostgres(t *testing.T) {
	n := mustParse(t, `{"type":"group","logic":"AND","rules":[
		{"type":"condition","field":"city","operator":"IN","value":["istanbul","ankara"]},
		{"type":"group","logic":"OR","rules":[
			{"type":"condition","field":"age","operator":"BETWEEN","value":[18,25]},
			{"type":"condition","field":"total_spend","operator":"GREATER_THAN","value":5000.5}]},
		{"type":"group","logic":"NOT","rules":[
			{"type":"condition","field":"has_purchased_since_cart","operator":"EQUALS","value":true}]}]}`)
	w, err := NewBuilder(DefaultRegistry, Postgres{}).Build(n)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := "(city IN ($1, $2) AND (age BETWEEN $3 AND $4 OR total_spend > $5) AND NOT (has_purchased_since_cart = $6))"
	if w.SQL != wantSQL {
		t.Errorf("SQL\n got: %s\nwant: %s", w.SQL, wantSQL)
	}
	want := []any{"İstanbul", "Ankara", int64(18), int64(25), 5000.5, true}
	if !reflect.DeepEqual(w.Args, want) {
		t.Errorf("args got %#v want %#v", w.Args, want)
	}
}

// Defence in depth: even a hand-built *Node that bypassed Parse cannot
// smuggle a field or operator into the SQL text.
func TestBuildRejectsHandBuiltNodes(t *testing.T) {
	b := NewBuilder(DefaultRegistry, SQLite{})
	bad := []*Node{
		{Type: NodeGroup, Logic: LogicAnd, Rules: []*Node{{Type: NodeCondition, Field: "password", Operator: OpEquals, Value: "x"}}},
		{Type: NodeGroup, Logic: LogicAnd, Rules: []*Node{{Type: NodeCondition, Field: "age", Operator: "LIKE", Value: "%"}}},
		{Type: NodeGroup, Logic: "AND 1=1 --", Rules: []*Node{{Type: NodeCondition, Field: "age", Operator: OpEquals, Value: int64(1)}}},
		{Type: NodeGroup, Logic: LogicAnd},
		{Type: "raw"},
	}
	for i, n := range bad {
		if w, err := b.Build(n); err == nil {
			t.Errorf("case %d: expected error, got %q", i, w.SQL)
		}
	}
}

// safeSQL matches the only characters the builder can ever emit: whitelisted
// identifiers, keywords, comparison symbols, parentheses and placeholders.
// Quotes, semicolons and comment markers can never appear.
var safeSQL = regexp.MustCompile(`^[a-z_ ()?$0-9,<>=A-Z]*$`)

func assertSafeWhere(t *testing.T, w Where) {
	t.Helper()
	if !safeSQL.MatchString(w.SQL) {
		t.Fatalf("SQL contains disallowed characters: %q", w.SQL)
	}
	for _, bad := range []string{"'", `"`, ";", "--", "/*"} {
		if strings.Contains(w.SQL, bad) {
			t.Fatalf("SQL contains %q: %s", bad, w.SQL)
		}
	}
	for _, a := range w.Args {
		if s, ok := a.(string); ok && strings.Contains(w.SQL, s) {
			t.Fatalf("bind value %q leaked into SQL text: %s", s, w.SQL)
		}
	}
	if got := strings.Count(w.SQL, "?"); got != len(w.Args) {
		t.Fatalf("%d placeholders but %d args", got, len(w.Args))
	}
}

// FuzzParseBuild: for ANY input, Parse either rejects it, or the compiled SQL
// contains only whitelisted tokens and one placeholder per argument.
// Run longer with: go test ./internal/segment -fuzz=FuzzParseBuild -fuzztime=30s
func FuzzParseBuild(f *testing.F) {
	for _, s := range []string{
		ReferenceRule,
		cond("city", "IN", `["Istanbul","izmir","ANKARA"]`),
		cond("age", "BETWEEN", `[18,65]`),
		cond("password", "EQUALS", `"x"`),
		cond("city", "EQUALS", `"x' OR 1=1--"`),
		`{"type":"group","logic":"NOT","rules":[{"type":"group","logic":"OR","rules":[` +
			`{"type":"condition","field":"total_spend","operator":"LESS_THAN","value":99.9}]}]}`,
		`{}`, `[]`, `null`, `"`,
	} {
		f.Add([]byte(s))
	}
	p := NewParser(DefaultRegistry)
	b := NewBuilder(DefaultRegistry, SQLite{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		n, err := p.Parse(raw)
		if err != nil {
			return
		}
		w, err := b.Build(n)
		if err != nil {
			t.Fatalf("parsed tree failed to build: %v", err)
		}
		assertSafeWhere(t, w)
	})
}
