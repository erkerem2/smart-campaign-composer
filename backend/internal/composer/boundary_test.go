package composer_test

import (
	"errors"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/composer"
)

const (
	zwj = "\xe2\x80\x8d" // U+200D zero-width joiner (emoji sequences)
	// name18 / name19 are real-looking Turkish names of 18 and 19 code
	// points; every non-ASCII letter is 2 bytes in UTF-8.
	name18 = "Şükrü Çağlayanoğlu"
	name19 = "Gülşen Çağlayanoğlu"
)

func runes(s string) int { return utf8.RuneCountInString(s) }

// A9. The hard limits hold on the rendered text, counted in code points, with
// Turkish names and Turkish / emoji template text: exactly 160 (SMS) and 80
// (subject) are delivered, one more is refused.
func TestRenderedLimitsWithTurkishNames(t *testing.T) {
	if runes(name18) != 18 || runes(name19) != 19 {
		t.Fatalf("fixture names: %d / %d runes", runes(name18), runes(name19))
	}
	for _, pad := range []string{"ş", "ğ", "🎉"} {
		sms := "{{first_name}}" + strings.Repeat(pad, 142) // raw 156, budget 160
		if _, err := validate(withSMS(sms)); err != nil {
			t.Fatalf("pad %q: template must pass: %v", pad, err)
		}
		got, err := composer.DefaultRenderer.RenderSMS(sms, user(name18))
		if err != nil || runes(got) != 160 {
			t.Fatalf("pad %q: 18-letter name must render exactly 160: %d %v", pad, runes(got), err)
		}
		if len(got) <= 160 {
			t.Fatalf("pad %q: test must exercise multi-byte text (%d bytes)", pad, len(got))
		}
		if _, err := composer.DefaultRenderer.RenderSMS(sms, user(name19)); !errors.Is(err, composer.ErrRenderedTooLong) {
			t.Fatalf("pad %q: 161 must be refused, got %v", pad, err)
		}

		subject := "{{first_name}}" + strings.Repeat(pad, 62) // raw 76, budget 80
		if _, err := validate(withSubject(subject)); err != nil {
			t.Fatalf("pad %q: subject must pass: %v", pad, err)
		}
		s, _, _, err := composer.DefaultRenderer.RenderEmail(subject, "Gövde", user(name18))
		if err != nil || runes(s) != 80 {
			t.Fatalf("pad %q: subject must render exactly 80: %d %v", pad, runes(s), err)
		}
		if _, _, _, err := composer.DefaultRenderer.RenderEmail(subject, "Gövde", user(name19)); !errors.Is(err, composer.ErrRenderedTooLong) {
			t.Fatalf("pad %q: subject of 81 must be refused, got %v", pad, err)
		}
	}
}

// Decomposed (NFD) letters are two code points each. Providers bill UCS-2
// code units, so counting code points is the honest measure: the NFD form of
// an 18-letter name is longer than 18 and pushes the SMS over the limit.
func TestRenderedLengthCountsCodePointsNotGraphemes(t *testing.T) {
	nfd := "S\xcc\xa7u\xcc\x88kru\xcc\x88 C\xcc\xa7ag\xcc\x86layanog\xcc\x86lu" // "Şükrü Çağlayanoğlu" decomposed
	if runes(nfd) != 24 {
		t.Fatalf("fixture: %d runes", runes(nfd))
	}
	sms := "{{first_name}}" + strings.Repeat("a", 142)
	if _, err := composer.DefaultRenderer.RenderSMS(sms, user(nfd)); !errors.Is(err, composer.ErrRenderedTooLong) {
		t.Fatalf("NFD name renders 166 code points and must be refused, got %v", err)
	}
}

// A10. Emoji are counted in code points too: a ZWJ family is 5, a flag 2.
func TestEmojiCountsAtTheLimit(t *testing.T) {
	family := "👨" + zwj + "👩" + zwj + "👧"
	flag := "🇹🇷"
	if runes(family) != 5 || runes(flag) != 2 {
		t.Fatalf("fixtures: family %d, flag %d", runes(family), runes(flag))
	}
	ok := map[string]string{
		"160 party poppers":   strings.Repeat("🎉", 160),
		"32 families":         strings.Repeat(family, 32),
		"80 flags":            strings.Repeat(flag, 80),
		"mixed 160":           strings.Repeat(family, 20) + strings.Repeat(flag, 25) + strings.Repeat("ş", 10),
		"160 with heart+VS16": strings.Repeat("❤️", 80),
	}
	for name, sms := range ok {
		if rep, err := validate(withSMS(sms)); err != nil || rep.SMS.Runes != 160 {
			t.Errorf("%s: want valid at 160, got runes=%d err=%v", name, rep.SMS.Runes, err)
		}
	}
	tooLong := map[string]string{
		"161 party poppers": strings.Repeat("🎉", 161),
		"32 families + a":   strings.Repeat(family, 32) + "a",
		"81 flags":          strings.Repeat(flag, 81),
	}
	for name, sms := range tooLong {
		_, err := validate(withSMS(sms))
		if !errors.Is(err, composer.ErrTooLong) {
			t.Errorf("%s: want too_long, got %v", name, err)
		}
	}
	if _, err := validate(withSubject(strings.Repeat(flag, 40))); err != nil {
		t.Errorf("40 flags = 80: %v", err)
	}
	if _, err := validate(withSubject(strings.Repeat(flag, 40) + "a")); !errors.Is(err, composer.ErrTooLong) {
		t.Errorf("40 flags + a = 81: want too_long, got %v", err)
	}
}

// A13. Every spelling of a non-http(s) scheme a browser would accept is
// rejected, in HTML and in Markdown.
func TestObfuscatedLinkSchemesRejected(t *testing.T) {
	bodies := map[string]string{
		"named entity colon":           `<a href="javascript&colon;alert(1)">x</a>`,
		"zero-padded decimal entity":   `<a href="&#0000106;avascript:alert(1)">x</a>`,
		"decimal entity no semicolon":  `<a href="&#106avascript:alert(1)">x</a>`,
		"hex entity":                   `<a href="&#x6A;avascript:alert(1)">x</a>`,
		"Tab entity":                   `<a href="java&Tab;script:alert(1)">x</a>`,
		"NewLine entity":               `<a href="java&NewLine;script:alert(1)">x</a>`,
		"literal newline":              "<a href=\"java\nscript:alert(1)\">x</a>",
		"upper-case scheme":            `<a href="JAVASCRIPT:alert(1)">x</a>`,
		"leading spaces":               `<a href="   javascript:alert(1)">x</a>`,
		"upper-case attribute":         `<A HREF="javascript:alert(1)">x</A>`,
		"vbscript":                     `<a href="vbscript:msgbox(1)">x</a>`,
		"protocol-relative":            `<a href="//evil.example/x">x</a>`,
		"https without slashes":        `<a href="https:evil.example">x</a>`,
		"duplicate href by case":       `<a href="https://example.com" HREF="javascript:alert(1)">x</a>`,
		"md named entity colon":        "[x](javascript&colon;alert(1))",
		"md decimal entity":            "[x](&#106;avascript:alert(1))",
		"md upper-case":                "[x](JAVASCRIPT:alert(1))",
		"md angle brackets":            "[x](<javascript:alert(1)>)",
		"md newline before scheme":     "[x](\njavascript:alert(1))",
		"md image":                     "![x](javascript:alert(1))",
		"md reference in angles":       "[x]: <javascript:alert(1)>",
		"autolink":                     "<javascript:alert(1)>",
		"variable as the host":         `<a href="https://{{first_name}}.example.com/">x</a>`,
		"variable in userinfo/host":    `<a href="https://example.com@{{first_name}}">x</a>`,
		"variable in mailto":           `<a href="mailto:{{first_name}}@example.com">x</a>`,
		"md variable as the host":      "[x](https://{{first_name}}.example.com/sepet)",
		"md variable after the scheme": "[x](https://{{first_name}})",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, err := validate(withBody(body))
			wantIssue(t, err, composer.FieldEmailBody, composer.ErrUnsafeMarkup)
		})
	}
	for _, ok := range []string{
		`<a href="https://example.com/sepet/{{first_name}}">x</a>`,
		`<a href="https://example.com/sepet?ad={{first_name}}&amp;k=1">x</a>`,
		"[x](https://example.com/?ad={{first_name}})",
		`<a href="HTTPS://EXAMPLE.COM/">x</a>`,
	} {
		if _, err := validate(withBody(ok)); err != nil {
			t.Errorf("%q must stay valid: %v", ok, err)
		}
	}
}

var hostileNames = []string{
	`" onmouseover="alert(1)`,
	`' onfocus='alert(1)' x='`,
	`Ali&utm_source=evil#frag`,
	`Ali Veli`,
	`<script>alert(1)</script>`,
	`javascript:alert(1)`,
	`a)](https://evil.example`,
	`%3Cb%3E`,
	`Ayşe Gül`,
}

// A14. A variable inside an attribute is data in two contexts at once: HTML
// (quotes must not end the attribute) and, in href, a URL (characters such as
// & # ? and space must not add parameters, start a fragment or end the link).
func TestRenderEscapesValuesInsideAttributes(t *testing.T) {
	tmpl := `<p><a href="https://example.com/sepet?ad={{first_name}}&amp;k=1" title="{{first_name}}">Sepet</a></p>` +
		`<p><a href='https://example.com/u/{{first_name}}' title='{{first_name}}'>Profil</a></p>`
	hrefRe := regexp.MustCompile(`href=["']([^"']*)["']`)
	titleRe := regexp.MustCompile(`title=["']([^"']*)["']`)

	for _, name := range hostileNames {
		t.Run(name, func(t *testing.T) {
			_, body, format, err := composer.DefaultRenderer.RenderEmail("Konu", tmpl, user(name))
			if err != nil || format != composer.BodyHTML {
				t.Fatalf("render: %v (%s)", err, format)
			}
			// The rendered body is still whitelisted HTML: no new tag or
			// attribute was injected.
			m := composer.ReferenceMessage
			m.EmailBody = body
			if _, err := validate(m); err != nil {
				t.Fatalf("rendered body is no longer valid HTML: %v\n%s", err, body)
			}
			hrefs := hrefRe.FindAllStringSubmatch(body, -1)
			titles := titleRe.FindAllStringSubmatch(body, -1)
			if len(hrefs) != 2 || len(titles) != 2 {
				t.Fatalf("attributes changed: %s", body)
			}
			for _, tt := range titles {
				if got := html.UnescapeString(tt[1]); got != name {
					t.Errorf("title = %q, want the name as text", got)
				}
			}
			q, err := url.Parse(html.UnescapeString(hrefs[0][1]))
			if err != nil {
				t.Fatalf("href: %v", err)
			}
			vals := q.Query()
			if q.Host != "example.com" || q.Fragment != "" || len(vals) != 2 || vals.Get("ad") != name || vals.Get("k") != "1" {
				t.Errorf("query link changed: host=%q fragment=%q query=%v", q.Host, q.Fragment, vals)
			}
			p, err := url.Parse(html.UnescapeString(hrefs[1][1]))
			if err != nil || p.Host != "example.com" || p.RawQuery != "" || p.Fragment != "" || p.Path != "/u/"+name {
				t.Errorf("path link changed: %+v (%v)", p, err)
			}
		})
	}
}

func TestRenderEscapesValuesInsideMarkdownLinks(t *testing.T) {
	tmpl := "Merhaba {{first_name}},\n\n[Sepetime git](https://example.com/sepet?ad={{first_name}}&k=1)\n\n[profil]: https://example.com/u/{{first_name}}"
	destRe := regexp.MustCompile(`\]\(([^)\s]*)\)`)
	refRe := regexp.MustCompile(`(?m)^\[profil\]: (\S*)$`)
	for _, name := range hostileNames {
		t.Run(name, func(t *testing.T) {
			_, body, format, err := composer.DefaultRenderer.RenderEmail("Konu", tmpl, user(name))
			if err != nil || format != composer.BodyMarkdown {
				t.Fatalf("render: %v (%s)", err, format)
			}
			d := destRe.FindStringSubmatch(body)
			if d == nil {
				t.Fatalf("link destination broken: %s", body)
			}
			q, err := url.Parse(d[1])
			if err != nil || q.Host != "example.com" || q.Fragment != "" || q.Query().Get("ad") != name || len(q.Query()) != 2 {
				t.Errorf("markdown link changed: %q", d[1])
			}
			r := refRe.FindStringSubmatch(body)
			if r == nil {
				t.Fatalf("reference definition broken: %s", body)
			}
			p, err := url.Parse(r[1])
			if err != nil || p.Host != "example.com" || p.Path != "/u/"+name || p.RawQuery != "" || p.Fragment != "" {
				t.Errorf("reference link changed: %q", r[1])
			}
			if strings.Contains(body, "](https://evil") {
				t.Errorf("a value opened a new link: %s", body)
			}
		})
	}
}
