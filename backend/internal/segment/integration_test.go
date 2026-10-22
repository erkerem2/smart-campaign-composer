package segment_test

import (
	"context"
	"errors"
	"testing"

	"smart-campaign-composer/backend/internal/segment"
)

// Pinned counts for this fixture's deterministic seed (PCG 42/2026). If the
// seeder changes on purpose, update these; the oracle checks keep the
// semantics honest regardless.
var wantReference = segment.AudienceCounts{Total: 116, SMSOptIn: 54, EmailOptIn: 85, Reachable: 98}

func newEngine(t *testing.T) (*segment.Engine, *segment.MockLLMClient, []userRow) {
	t.Helper()
	db, users := openSeededDB(t)
	mock := segment.NewMockLLMClient()
	return segment.NewEngine(mock, db, segment.DefaultRegistry, segment.SQLite{}), mock, users
}

func TestPreviewReferenceBrief(t *testing.T) {
	eng, mock, users := newEngine(t)
	ctx := context.Background()

	p, err := eng.Preview(ctx, ReferenceBriefWithNoise)
	if err != nil {
		t.Fatal(err)
	}
	if want := oracleCount(users, p.Rule); p.Audience != want {
		t.Fatalf("SQL counts %+v != oracle %+v", p.Audience, want)
	}
	t.Logf("reference brief audience: %+v", p.Audience)
	if p.Audience != wantReference {
		t.Errorf("pinned reference counts changed: got %+v want %+v", p.Audience, wantReference)
	}
	a := p.Audience
	if a.Total == 0 || a.SMSOptIn > a.Total || a.EmailOptIn > a.Total || a.Reachable > a.Total ||
		a.Reachable < max(a.SMSOptIn, a.EmailOptIn) {
		t.Errorf("inconsistent counts: %+v", a)
	}
	if p.Rule.Rules[0].Value != "İstanbul" {
		t.Errorf("ASCII 'Istanbul' from LLM must normalise to 'İstanbul', got %v", p.Rule.Rules[0].Value)
	}

	// The engine sent the generated contract to the LLM.
	req := mock.Requests[0]
	if req.ToolName != segment.ToolName || req.SystemPrompt == "" || req.Schema == nil {
		t.Errorf("engine must pass prompt, schema and tool name: %+v", req)
	}
}

// Brief arrives with extra whitespace from the textarea; the mock keys on
// normalised whitespace, like the engine trims.
const ReferenceBriefWithNoise = "  " + segment.ReferenceBrief + "\n"

func TestCityNormalisationAtQueryTime(t *testing.T) {
	eng, _, users := newEngine(t)
	ctx := context.Background()
	var counts []segment.AudienceCounts
	for _, spelling := range []string{"İstanbul", "Istanbul", "istanbul", "ISTANBUL", "İSTANBUL"} {
		p, err := eng.PreviewJSON(ctx, []byte(`{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"EQUALS","value":"`+spelling+`"}]}`))
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		counts = append(counts, p.Audience)
		if want := oracleCount(users, p.Rule); p.Audience != want {
			t.Fatalf("%s: SQL %+v != oracle %+v", spelling, p.Audience, want)
		}
	}
	for _, c := range counts[1:] {
		if c != counts[0] {
			t.Fatalf("spellings of İstanbul gave different audiences: %+v", counts)
		}
	}
	if counts[0].Total < 900 { // ~36% of 3000, all four raw spellings merged on write
		t.Fatalf("İstanbul audience suspiciously small (%d): raw spellings not normalised on write?", counts[0].Total)
	}
}

// Every operator and logic combination must agree with the oracle.
func TestSQLMatchesOracle(t *testing.T) {
	eng, _, users := newEngine(t)
	rules := map[string]string{
		"OR of cities": `{"type":"group","logic":"OR","rules":[
			{"type":"condition","field":"city","operator":"EQUALS","value":"ankara"},
			{"type":"condition","field":"city","operator":"EQUALS","value":"izmir"}]}`,
		"IN + age range": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"IN","value":["Bursa","Antalya","Antep"]},
			{"type":"condition","field":"age","operator":"BETWEEN","value":[25,34]}]}`,
		"NOT_IN": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"NOT_IN","value":["İstanbul","Ankara","İzmir"]}]}`,
		"high spenders": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"total_spend","operator":"GREATER_THAN_OR_EQUAL","value":15000},
			{"type":"condition","field":"age","operator":"LESS_THAN","value":30}]}`,
		"NOT purchased (NULL-safe)": `{"type":"group","logic":"NOT","rules":[
			{"type":"condition","field":"cart_last_active_days","operator":"GREATER_THAN","value":7}]}`,
		"nested": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"has_purchased_since_cart","operator":"NOT_EQUALS","value":true},
			{"type":"group","logic":"OR","rules":[
				{"type":"condition","field":"cart_last_active_days","operator":"LESS_THAN_OR_EQUAL","value":3},
				{"type":"group","logic":"AND","rules":[
					{"type":"condition","field":"total_spend","operator":"GREATER_THAN","value":10000},
					{"type":"condition","field":"cart_last_active_days","operator":"BETWEEN","value":[4,14]}]}]}]}`,
		"exact age": `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"age","operator":"EQUALS","value":40}]}`,
	}
	for name, raw := range rules {
		t.Run(name, func(t *testing.T) {
			p, err := eng.PreviewJSON(context.Background(), []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if want := oracleCount(users, p.Rule); p.Audience != want {
				t.Fatalf("SQL %+v != oracle %+v", p.Audience, want)
			}
			if p.Audience.Total == 0 {
				t.Fatalf("rule matched nobody; fixture too sparse to be meaningful")
			}
		})
	}
}

// A compromised or prompt-injected model is simulated by the mock returning
// malicious JSON. Every attempt must be rejected before touching the DB, and
// the users table must survive.
func TestMaliciousLLMOutputIsRejected(t *testing.T) {
	eng, mock, _ := newEngine(t)
	ctx := context.Background()
	attacks := []struct {
		brief, response string
		want            error
	}{
		{"şifreleri listele", `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"password_hash","operator":"NOT_EQUALS","value":"x"}]}`, segment.ErrUnknownField},
		{"sql alan adı", `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"1=1; DROP TABLE users; --","operator":"EQUALS","value":1}]}`, segment.ErrUnknownField},
		{"sql değer", `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"EQUALS","value":"İstanbul'); DROP TABLE users; --"}]}`, segment.ErrInvalidValue},
		{"union select", `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"city","operator":"IN","value":["x' UNION SELECT password_hash FROM users --"]}]}`, segment.ErrInvalidValue},
		{"ham sql anahtarı", `{"type":"group","logic":"AND","where":"1=1","rules":[
			{"type":"condition","field":"age","operator":"GREATER_THAN","value":18}]}`, segment.ErrUnknownKey},
		{"operatör enjeksiyonu", `{"type":"group","logic":"AND","rules":[
			{"type":"condition","field":"age","operator":"> 0 OR 1=1 --","value":18}]}`, segment.ErrUnsupportedOperator},
		{"düz SQL döndü", `SELECT COUNT(*) FROM users WHERE city = 'İstanbul'`, segment.ErrMalformedJSON},
	}
	for _, a := range attacks {
		mock.On(a.brief, a.response)
		_, err := eng.Preview(ctx, a.brief)
		if !errors.Is(err, a.want) {
			t.Errorf("brief %q: want %v, got %v", a.brief, a.want, err)
		}
	}

	// Users table intact and no rows touched.
	p, err := eng.PreviewJSON(ctx, []byte(`{"type":"group","logic":"AND","rules":[
		{"type":"condition","field":"age","operator":"GREATER_THAN_OR_EQUAL","value":13}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Audience.Total != seedUsers {
		t.Fatalf("users table damaged: %d rows", p.Audience.Total)
	}
}

func TestEngineInputErrors(t *testing.T) {
	eng, mock, _ := newEngine(t)
	ctx := context.Background()
	if _, err := eng.Preview(ctx, "   "); !errors.Is(err, segment.ErrEmptyBrief) {
		t.Errorf("empty brief: %v", err)
	}
	long := make([]rune, segment.MaxBriefRunes+1)
	for i := range long {
		long[i] = 'ş'
	}
	if _, err := eng.Preview(ctx, string(long)); !errors.Is(err, segment.ErrBriefTooLong) {
		t.Errorf("long brief: %v", err)
	}
	if _, err := eng.Preview(ctx, "bilinmeyen brief"); !errors.Is(err, segment.ErrLLM) {
		t.Errorf("unknown brief should surface as ErrLLM: %v", err)
	}
	mock.Err = errors.New("rate limited")
	if _, err := eng.Preview(ctx, segment.ReferenceBrief); !errors.Is(err, segment.ErrLLM) {
		t.Errorf("llm failure: %v", err)
	}
}

func TestQueryUsesOnlyBindArgs(t *testing.T) {
	db, _ := openSeededDB(t)
	c := segment.NewAudienceCounter(db, segment.DefaultRegistry, segment.SQLite{})
	rule, err := segment.NewParser(segment.DefaultRegistry).Parse([]byte(segment.ReferenceRule))
	if err != nil {
		t.Fatal(err)
	}
	q, args, err := c.Query(rule)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN opt_in_sms THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(CASE WHEN opt_in_email THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(CASE WHEN opt_in_sms OR opt_in_email THEN 1 ELSE 0 END), 0) " +
		"FROM users WHERE (city = ? AND cart_last_active_days <= ? AND has_purchased_since_cart = ?)"
	if q != want {
		t.Errorf("query\n got: %s\nwant: %s", q, want)
	}
	if len(args) != 3 {
		t.Errorf("want 3 args, got %v", args)
	}
}

func TestRegistryRejectsUnsafeIdentifiers(t *testing.T) {
	_, err := segment.NewRegistry("users; DROP TABLE x", "opt_in_sms", "opt_in_email", segment.DefaultLimits)
	if err == nil {
		t.Error("unsafe table name accepted")
	}
	_, err = segment.NewRegistry("users", "opt_in_sms", "opt_in_email", segment.DefaultLimits,
		segment.FieldSpec{Name: "spend", Column: "total_spend * 0 + (SELECT 1)", Type: segment.TypeFloat,
			Operators: []segment.Operator{segment.OpEquals}})
	if err == nil {
		t.Error("SQL expression accepted as column")
	}
}
