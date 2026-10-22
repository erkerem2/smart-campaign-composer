package composer

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// collect feeds parts and concatenates the deltas per field.
func collect(t *testing.T, parts []string) map[Field]string {
	t.Helper()
	x := newFieldExtractor()
	got := map[Field]string{}
	var order []Field
	for _, p := range parts {
		for _, d := range x.Feed(p) {
			if d.Text == "" {
				t.Fatalf("empty delta for %s", d.Field)
			}
			if len(order) == 0 || order[len(order)-1] != d.Field {
				order = append(order, d.Field)
			}
			got[d.Field] += d.Text
		}
	}
	// Each field streams as one contiguous run.
	seen := map[Field]bool{}
	for _, f := range order {
		if seen[f] {
			t.Fatalf("field %s interleaved: %v", f, order)
		}
		seen[f] = true
	}
	return got
}

func want(t *testing.T, raw string) map[Field]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	out := map[Field]string{}
	for k, v := range m {
		if Field(k).valid() && v != "" {
			out[Field(k)] = v
		}
	}
	return out
}

func equal(a, b map[Field]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// splitBytes cuts at arbitrary byte offsets — harsher than real deltas, it
// also splits UTF-8 sequences.
func splitBytes(s string, r *rand.Rand, maxLen int) []string {
	var parts []string
	for len(s) > 0 {
		n := 1 + r.IntN(maxLen)
		n = min(n, len(s))
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return parts
}

var extractorInputs = map[string]string{
	"reference": ReferenceMessageJSON,
	"escapes":   `{"sms_content":"Çok \"güzel\" 🎉\n\\ son \/ \t\b\f\r","email_subject":"Konu: %30 – İndirim","email_body":"<p>A &amp; B</p><br>"}`,
	"pretty": `{
  "sms_content" : "Merhaba {{first_name}}" ,
  "email_subject":
     "Konu",
  "email_body": "Satır 1\nSatır 2"
}`,
	"lone surrogates": `{"sms_content":"a\ud83c b \udf89 c \ud83cA d \ud83c🎉 e \ud83c","email_subject":"x","email_body":"y"}`,
	"unknown key":     `{"note":"gizli \"metin\"","sms_content":"a","email_subject":"b","email_body":"c"}`,
	"empty values":    `{"sms_content":"","email_subject":"s","email_body":""}`,
}

func TestExtractorMatchesStrictDecode(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for name, raw := range extractorInputs {
		t.Run(name, func(t *testing.T) {
			exp := want(t, raw)
			// whole, byte by byte, and many random splittings
			if got := collect(t, []string{raw}); !equal(got, exp) {
				t.Fatalf("whole: got %q want %q", got, exp)
			}
			if got := collect(t, splitBytes(raw, r, 1)); !equal(got, exp) {
				t.Fatalf("byte-wise: got %q want %q", got, exp)
			}
			for i := 0; i < 200; i++ {
				parts := splitBytes(raw, r, 1+i%9)
				if got := collect(t, parts); !equal(got, exp) {
					t.Fatalf("split %q: got %q want %q", parts, got, exp)
				}
			}
			for _, n := range []int{1, 2, 3, 7} {
				if got := collect(t, splitUTF8(raw, n)); !equal(got, exp) {
					t.Fatalf("mock split %d: got %q want %q", n, got, exp)
				}
			}
		})
	}
}

func TestExtractorDeltasAreValidUTF8(t *testing.T) {
	raw := `{"sms_content":"ğüşıöç İĞÜŞÖÇ 🎉🎉","email_subject":"ş","email_body":"ı"}`
	x := newFieldExtractor()
	for _, p := range splitBytes(raw, rand.New(rand.NewPCG(3, 4)), 1) {
		for _, d := range x.Feed(p) {
			if !utf8.ValidString(d.Text) {
				t.Fatalf("invalid UTF-8 delta %q", d.Text)
			}
		}
	}
}

func TestExtractorStopsOnUnexpectedInput(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want map[Field]string
	}{
		"non-string value":    {`{"sms_content":123,"email_subject":"x"}`, map[Field]string{}},
		"after non-string":    {`{"email_subject":"ok","sms_content":["x"],"email_body":"y"}`, map[Field]string{FieldEmailSubject: "ok"}},
		"duplicate key":       {`{"sms_content":"first","sms_content":"second"}`, map[Field]string{FieldSMS: "first"}},
		"not an object":       {`"sms_content"`, map[Field]string{}},
		"bad escape":          {`{"sms_content":"a\qb"}`, map[Field]string{FieldSMS: "a"}},
		"trailing ignored":    {`{"sms_content":"a"} {"sms_content":"b"}`, map[Field]string{FieldSMS: "a"}},
		"sql is just text":    {`{"sms_content":"'; DROP TABLE users; --"}`, map[Field]string{FieldSMS: "'; DROP TABLE users; --"}},
		"escaped key matches": {`{"sms_content":"a"}`, map[Field]string{FieldSMS: "a"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := collect(t, splitBytes(tc.raw, rand.New(rand.NewPCG(5, 6)), 3))
			if !equal(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeOutputStrict(t *testing.T) {
	if m, err := decodeOutput([]byte(ReferenceMessageJSON)); err != nil || m != ReferenceMessage {
		t.Fatalf("reference: %v %+v", err, m)
	}
	bad := map[string]string{
		"unknown key":   `{"sms_content":"a","email_subject":"b","email_body":"c","sql":"DROP TABLE users"}`,
		"duplicate key": `{"sms_content":"a","sms_content":"x","email_subject":"b","email_body":"c"}`,
		"missing key":   `{"sms_content":"a","email_subject":"b"}`,
		"number":        `{"sms_content":1,"email_subject":"b","email_body":"c"}`,
		"null":          `{"sms_content":null,"email_subject":"b","email_body":"c"}`,
		"object":        `{"sms_content":{"text":"a"},"email_subject":"b","email_body":"c"}`,
		"trailing":      `{"sms_content":"a","email_subject":"b","email_body":"c"} {}`,
		"array":         `[{"sms_content":"a"}]`,
		"text":          `SELECT * FROM users`,
		"truncated":     `{"sms_content":"a","email_subject":"b","email_body":"c`,
		"empty":         ``,
		"case variant":  `{"SMS_content":"a","email_subject":"b","email_body":"c"}`,
	}
	for name, raw := range bad {
		if _, err := decodeOutput([]byte(raw)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestScanPlaceholders(t *testing.T) {
	found, bad := scanPlaceholders("A {{first_name}} B {{ last_name }} C {x} D")
	if len(bad) != 0 || len(found) != 2 || found[0].Name != "first_name" || found[1].Name != "last_name" {
		t.Fatalf("found=%+v bad=%v", found, bad)
	}
	if s := "A {{first_name}} B"; s[found[0].Start:found[0].End] != "{{first_name}}" {
		t.Fatalf("offsets %+v", found[0])
	}
	if _, bad := scanPlaceholders("{{a}} }} {{b"); len(bad) != 2 {
		t.Fatalf("want 2 malformed, got %v", bad)
	}
	if _, bad := scanPlaceholders("{{" + strings.Repeat("a", 33) + "}}"); len(bad) != 1 {
		t.Fatalf("over-long names are malformed: %v", bad)
	}
}
