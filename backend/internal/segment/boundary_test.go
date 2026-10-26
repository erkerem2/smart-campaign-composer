package segment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// jsonEsc returns the JSON escape sequence backslash-u-XXXX for hex.
func jsonEsc(hex string) string { return "\x5cu" + hex }

func condRule(field, op, value string) string { return cond(field, op, value) }

func mustAccept(t *testing.T, name, raw string) *Node {
	t.Helper()
	n, err := NewParser(DefaultRegistry).Parse([]byte(raw))
	if err != nil {
		t.Fatalf("%s: must be accepted, got %v", name, err)
	}
	return n
}

func mustReject(t *testing.T, name, raw string, want error) {
	t.Helper()
	n, err := NewParser(DefaultRegistry).Parse([]byte(raw))
	if err == nil {
		out, _ := json.Marshal(n)
		t.Fatalf("%s: expected %v, parse succeeded: %s", name, want, out)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s: expected %v, got %v", name, want, err)
	}
}

// A3. Every limit is inclusive: a tree exactly at the limit is accepted, one
// step past it is rejected. Only the "past" side was tested before.
func TestParseAcceptsExactlyAtLimits(t *testing.T) {
	lim := DefaultRegistry.Limits
	c := `{"type":"condition","field":"age","operator":"GREATER_THAN","value":20}`

	nest := func(groups int) string {
		s := c
		for i := 0; i < groups; i++ {
			s = `{"type":"group","logic":"AND","rules":[` + s + `]}`
		}
		return s
	}
	mustAccept(t, "depth 4 (root = 1)", nest(lim.MaxDepth))
	mustReject(t, "depth 5", nest(lim.MaxDepth+1), ErrLimitExceeded)

	notDepth := `{"type":"group","logic":"NOT","rules":[{"type":"group","logic":"NOT","rules":[{"type":"group","logic":"NOT","rules":[{"type":"group","logic":"AND","rules":[` + c + `]}]}]}]}`
	mustAccept(t, "NOT groups count as levels: depth 4", notDepth)

	many := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = c
		}
		return `{"type":"group","logic":"OR","rules":[` + strings.Join(parts, ",") + `]}`
	}
	mustAccept(t, "20 conditions", many(lim.MaxConditions))
	mustReject(t, "21 conditions", many(lim.MaxConditions+1), ErrLimitExceeded)

	// The 81 provinces are distinct, so the IN list is not shrunk by
	// de-duplication.
	cities := func(n int) string {
		p := Provinces()[:n]
		for i := range p {
			p[i] = `"` + p[i] + `"`
		}
		return "[" + strings.Join(p, ",") + "]"
	}
	n := mustAccept(t, "IN with 50 items", condRule("city", "IN", cities(lim.MaxListSize)))
	if got := len(n.Rules[0].Values); got != lim.MaxListSize {
		t.Fatalf("IN kept %d values, want %d", got, lim.MaxListSize)
	}
	mustReject(t, "IN with 51 items", condRule("city", "IN", cities(lim.MaxListSize+1)), ErrLimitExceeded)
	// The size limit applies to the list as sent, before duplicates collapse.
	dup := "[" + strings.Repeat(`"İstanbul",`, lim.MaxListSize) + `"Istanbul"]`
	mustReject(t, "51 items that collapse to 1", condRule("city", "IN", dup), ErrLimitExceeded)

	// Exactly MaxRawBytes (JSON whitespace padding keeps the document valid).
	base := condRule("age", "EQUALS", "30")
	pad := func(size int) string { return base + strings.Repeat(" ", size-len(base)) }
	mustAccept(t, "document of exactly 16384 bytes", pad(lim.MaxRawBytes))
	mustReject(t, "document of 16385 bytes", pad(lim.MaxRawBytes+1), ErrLimitExceeded)
}

func TestParseNumericBoundaries(t *testing.T) {
	accept := []struct{ field, op, value string }{
		{"age", "EQUALS", "13"}, {"age", "EQUALS", "120"},
		{"age", "BETWEEN", "[13,120]"}, {"age", "BETWEEN", "[30,30]"},
		{"age", "EQUALS", "30.0"}, {"age", "EQUALS", "2e1"}, {"age", "EQUALS", "1.3e1"},
		{"total_spend", "GREATER_THAN_OR_EQUAL", "0"}, {"total_spend", "LESS_THAN_OR_EQUAL", "1e9"},
		{"total_spend", "GREATER_THAN", "-0"}, {"total_spend", "GREATER_THAN", "0.01"},
		{"cart_last_active_days", "LESS_THAN_OR_EQUAL", "0"}, {"cart_last_active_days", "LESS_THAN_OR_EQUAL", "3650"},
	}
	for _, a := range accept {
		mustAccept(t, a.field+" "+a.op+" "+a.value, condRule(a.field, a.op, a.value))
	}
	reject := []struct{ field, op, value string }{
		{"age", "EQUALS", "12"}, {"age", "EQUALS", "121"}, {"age", "EQUALS", "12.999"},
		{"age", "BETWEEN", "[12,30]"}, {"age", "BETWEEN", "[30,121]"}, {"age", "EQUALS", "1.35e1"},
		{"total_spend", "GREATER_THAN", "-0.01"}, {"total_spend", "GREATER_THAN", "1000000000.01"},
		{"cart_last_active_days", "EQUALS", "-1"}, {"cart_last_active_days", "EQUALS", "3651"},
		{"age", "EQUALS", "1e400"}, {"age", "EQUALS", "-1e400"},
	}
	for _, r := range reject {
		mustReject(t, r.field+" "+r.op+" "+r.value, condRule(r.field, r.op, r.value), ErrInvalidValue)
	}
	n := mustAccept(t, "2e1", condRule("age", "EQUALS", "2e1"))
	if v := n.Rules[0].Value; v != int64(20) {
		t.Fatalf("2e1 normalised to %#v, want int64(20)", v)
	}
}

// A4. A node carries only the keys of its own type. The value of a foreign
// key does not matter: "rules": null on a condition, "field": "" on a group
// are just as wrong as non-empty ones.
func TestParseRejectsForeignKeysWhateverTheirValue(t *testing.T) {
	c := `{"type":"condition","field":"age","operator":"EQUALS","value":30`
	g := `{"type":"group","logic":"AND","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":30}]`
	wrap := func(node string) string { return `{"type":"group","logic":"AND","rules":[` + node + `]}` }
	cases := map[string]string{
		"condition with rules null": wrap(c + `,"rules":null}`),
		"condition with rules []":   wrap(c + `,"rules":[]}`),
		"condition with logic ''":   wrap(c + `,"logic":""}`),
		"condition with logic null": wrap(c + `,"logic":null}`),
		"group with field ''":       g + `,"field":""}`,
		"group with operator ''":    g + `,"operator":""}`,
		"group with value null":     g + `,"value":null}`,
		"group with field null":     g + `,"field":null}`,
	}
	for name, raw := range cases {
		mustReject(t, name, raw, ErrUnknownKey)
	}
	mustReject(t, "group with rules null", `{"type":"group","logic":"AND","rules":null}`, ErrInvalidLogic)
}

// A2. City values that look like a province but are not one byte-for-byte
// in any accepted spelling: invisible or control characters, lone UTF-16
// surrogate escapes (they decode to U+FFFD and never reach utf8.Valid as
// bytes), homoglyphs from other scripts and full-width forms.
func TestParseRejectsDeceptiveCityValues(t *testing.T) {
	values := map[string]string{
		"lone high surrogate":         `"İstanbul` + jsonEsc("d800") + `"`,
		"lone low surrogate":          `"` + jsonEsc("dc00") + `İstanbul"`,
		"escaped NUL":                 `"İstanbul` + jsonEsc("0000") + `"`,
		"zero width space":            `"İstan` + jsonEsc("200b") + `bul"`,
		"BOM":                         `"` + jsonEsc("feff") + `İstanbul"`,
		"cyrillic capital I":          `"` + jsonEsc("0406") + `stanbul"`,
		"cyrillic a in Ankara":        `"Ank` + jsonEsc("0430") + `ra"`,
		"greek omicron in Konya":      `"K` + jsonEsc("03bf") + `nya"`,
		"full-width ISTANBUL":         `"` + jsonEsc("ff29") + jsonEsc("ff33") + jsonEsc("ff34") + jsonEsc("ff21") + jsonEsc("ff2e") + jsonEsc("ff22") + jsonEsc("ff35") + jsonEsc("ff2c") + `"`,
		"tab inside the name":         `"İstan\tbul"`,
		"right-to-left override":      `"` + jsonEsc("202e") + `İstanbul"`,
		"SQL wildcard":                `"İstanbul%"`,
		"64+ bytes of padding spaces": `"İstanbul` + strings.Repeat(" ", 60) + `"`,
	}
	for name, v := range values {
		mustReject(t, name, condRule("city", "EQUALS", v), ErrInvalidValue)
		mustReject(t, name+" (IN)", condRule("city", "IN", "["+v+"]"), ErrInvalidValue)
	}
	// Legitimate spellings, including JSON escapes and decomposed (NFD)
	// Unicode, all normalise to the canonical province.
	for name, v := range map[string]string{
		"escaped dotted I":   `"` + jsonEsc("0130") + `stanbul"`,
		"NFD dotted I":       `"I` + jsonEsc("0307") + `stanbul"`,
		"ASCII":              `"istanbul"`,
		"surrounding spaces": `"  İSTANBUL  "`,
	} {
		n := mustAccept(t, name, condRule("city", "EQUALS", v))
		if got := n.Rules[0].Value; got != "İstanbul" {
			t.Errorf("%s normalised to %q", name, got)
		}
	}
}
