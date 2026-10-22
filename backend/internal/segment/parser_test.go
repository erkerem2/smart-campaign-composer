package segment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *Node {
	t.Helper()
	n, err := NewParser(DefaultRegistry).Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, raw)
	}
	return n
}

func TestParseReferenceRule(t *testing.T) {
	n := mustParse(t, ReferenceRule)
	if n.Type != NodeGroup || n.Logic != LogicAnd || len(n.Rules) != 3 {
		t.Fatalf("unexpected root: %+v", n)
	}
	if v := n.Rules[0].Value; v != "İstanbul" {
		t.Errorf("city not normalised: %v", v)
	}
	if v := n.Rules[1].Value; v != int64(30) {
		t.Errorf("days should be int64(30), got %#v", v)
	}
	if v := n.Rules[2].Value; v != false {
		t.Errorf("bool should be false, got %#v", v)
	}
}

func TestParseCanonicalRoundTrip(t *testing.T) {
	n := mustParse(t, `{"type":"group","logic":"OR","rules":[
		{"type":"condition","field":"city","operator":"IN","value":["istanbul","Istanbul","IZMIR"]},
		{"type":"group","logic":"NOT","rules":[{"type":"condition","field":"age","operator":"BETWEEN","value":[18,25.0]}]}]}`)
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"group","logic":"OR","rules":[{"type":"condition","field":"city","operator":"IN","value":["İstanbul","İzmir"]},{"type":"group","logic":"NOT","rules":[{"type":"condition","field":"age","operator":"BETWEEN","value":[18,25]}]}]}`
	if string(out) != want {
		t.Fatalf("canonical JSON\n got: %s\nwant: %s", out, want)
	}
	if _, err := NewParser(DefaultRegistry).Parse(out); err != nil {
		t.Fatalf("canonical output must re-parse: %v", err)
	}
}

func cond(field, op, value string) string {
	return `{"type":"group","logic":"AND","rules":[{"type":"condition","field":"` + field + `","operator":"` + op + `","value":` + value + `}]}`
}

func TestParseRejects(t *testing.T) {
	deep := `{"type":"condition","field":"age","operator":"EQUALS","value":30}`
	for i := 0; i < 5; i++ {
		deep = `{"type":"group","logic":"AND","rules":[` + deep + `]}`
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = `{"type":"condition","field":"age","operator":"GREATER_THAN","value":20}`
	}
	bigList := make([]string, 51)
	for i := range bigList {
		bigList[i] = `"Adana"`
	}

	tests := []struct {
		name string
		raw  string
		want error
	}{
		// --- whitelist: fields ---
		{"password field", cond("password", "EQUALS", `"x"`), ErrUnknownField},
		{"email field", cond("email", "EQUALS", `"a@b.c"`), ErrUnknownField},
		{"sql in field name", cond("city = 'x' OR 1=1 --", "EQUALS", `"İstanbul"`), ErrUnknownField},
		{"drop table via field", cond("age; DROP TABLE users;--", "EQUALS", `1`), ErrUnknownField},
		{"subquery field", cond("(SELECT password FROM admins)", "EQUALS", `1`), ErrUnknownField},
		{"case-variant field", cond("City", "EQUALS", `"İstanbul"`), ErrUnknownField},
		{"empty field", cond("", "EQUALS", `"İstanbul"`), ErrUnknownField},

		// --- whitelist: operators ---
		{"LIKE operator", cond("city", "LIKE", `"%"`), ErrUnsupportedOperator},
		{"raw sql operator", cond("age", "= 1 OR 1=1 --", `1`), ErrUnsupportedOperator},
		{"lowercase operator", cond("age", "equals", `1`), ErrUnsupportedOperator},
		{"GT on city", cond("city", "GREATER_THAN", `"İstanbul"`), ErrUnsupportedOperator},
		{"IN on bool", cond("has_purchased_since_cart", "IN", `[true]`), ErrUnsupportedOperator},
		{"BETWEEN on city", cond("city", "BETWEEN", `["Adana","Zonguldak"]`), ErrUnsupportedOperator},

		// --- values ---
		{"sql injection in city value", cond("city", "EQUALS", `"İstanbul' OR '1'='1"`), ErrInvalidValue},
		{"unknown city", cond("city", "EQUALS", `"Gotham"`), ErrInvalidValue},
		{"number as string", cond("age", "GREATER_THAN", `"30"`), ErrInvalidValue},
		{"fractional int", cond("cart_last_active_days", "LESS_THAN_OR_EQUAL", `30.5`), ErrInvalidValue},
		{"negative age", cond("age", "GREATER_THAN", `-1`), ErrInvalidValue},
		{"huge spend", cond("total_spend", "GREATER_THAN", `1e300`), ErrInvalidValue},
		{"bool as int", cond("has_purchased_since_cart", "EQUALS", `0`), ErrInvalidValue},
		{"bool as string", cond("has_purchased_since_cart", "EQUALS", `"false"`), ErrInvalidValue},
		{"null value", cond("age", "EQUALS", `null`), ErrInvalidValue},
		{"object value", cond("age", "EQUALS", `{"$gt":1}`), ErrInvalidValue},
		{"array for EQUALS", cond("age", "EQUALS", `[1,2]`), ErrInvalidValue},
		{"scalar for IN", cond("city", "IN", `"İstanbul"`), ErrInvalidValue},
		{"empty IN", cond("city", "IN", `[]`), ErrInvalidValue},
		{"injection inside IN", cond("city", "IN", `["İstanbul","x') OR 1=1--"]`), ErrInvalidValue},
		{"BETWEEN arity", cond("age", "BETWEEN", `[1,2,3]`), ErrInvalidValue},
		{"BETWEEN reversed", cond("age", "BETWEEN", `[40,20]`), ErrInvalidValue},
		{"missing value", `{"type":"group","logic":"AND","rules":[{"type":"condition","field":"age","operator":"EQUALS"}]}`, ErrInvalidValue},

		// --- structure ---
		{"extra sql key", `{"type":"group","logic":"AND","sql":"SELECT * FROM users","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30}]}`, ErrUnknownKey},
		{"extra key in condition", `{"type":"group","logic":"AND","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30,"raw_where":"1=1"}]}`, ErrUnknownKey},
		{"condition with rules", `{"type":"group","logic":"AND","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30,"rules":[]}]}`, ErrUnknownKey},
		{"group with field", `{"type":"group","logic":"AND","field":"age","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30}]}`, ErrUnknownKey},
		{"root is condition", `{"type":"condition","field":"age","operator":"EQUALS","value":30}`, ErrInvalidNodeType},
		{"bad node type", `{"type":"raw_sql","logic":"AND","rules":[]}`, ErrInvalidNodeType},
		{"bad logic", `{"type":"group","logic":"XOR","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30}]}`, ErrInvalidLogic},
		{"sql in logic", `{"type":"group","logic":"AND 1=1) --","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30}]}`, ErrInvalidLogic},
		{"empty group", `{"type":"group","logic":"AND","rules":[]}`, ErrInvalidLogic},
		{"NOT with two", `{"type":"group","logic":"NOT","rules":[` + many[0] + `,` + many[1] + `]}`, ErrInvalidLogic},
		{"trailing data", ReferenceRule + `{"type":"group"}`, ErrMalformedJSON},
		{"trailing garbage", ReferenceRule + `; DROP TABLE users;`, ErrMalformedJSON},
		{"not json", `SELECT * FROM users WHERE 1=1`, ErrMalformedJSON},
		{"empty", ``, ErrMalformedJSON},
		{"invalid utf8", "{\"type\":\"group\xff\"}", ErrMalformedJSON},

		// --- limits ---
		{"too deep", `{"type":"group","logic":"AND","rules":[` + deep + `]}`, ErrLimitExceeded},
		{"too many conditions", `{"type":"group","logic":"OR","rules":[` + strings.Join(many, ",") + `]}`, ErrLimitExceeded},
		{"IN list too long", cond("city", "IN", `[`+strings.Join(bigList, ",")+`]`), ErrLimitExceeded},
		{"oversized document", `{"type":"group","logic":"AND","rules":[],"pad":"` + strings.Repeat("A", 20<<10) + `"}`, ErrLimitExceeded},
	}
	p := NewParser(DefaultRegistry)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := p.Parse([]byte(tc.raw))
			if err == nil {
				out, _ := json.Marshal(n)
				t.Fatalf("expected %v, parse succeeded: %s", tc.want, out)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Path == "" {
				t.Fatalf("error should be a *ValidationError with a path: %v", err)
			}
		})
	}
}

func TestValidationErrorPath(t *testing.T) {
	_, err := NewParser(DefaultRegistry).Parse([]byte(`{"type":"group","logic":"AND","rules":[
		{"type":"condition","field":"age","operator":"EQUALS","value":30},
		{"type":"condition","field":"password","operator":"EQUALS","value":"x"}]}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "$.rules[1].field" {
		t.Fatalf("want path $.rules[1].field, got %v", err)
	}
}

func TestErrorMessagesTruncateAttackerInput(t *testing.T) {
	long := strings.Repeat("x", 500)
	_, err := NewParser(DefaultRegistry).Parse([]byte(cond(long, "EQUALS", "1")))
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("error should be short, got %d bytes", len(err.Error()))
	}
}
