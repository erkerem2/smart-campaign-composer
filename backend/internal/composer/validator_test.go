package composer_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
)

func validMsg() composer.Message { return composer.ReferenceMessage }

func withSMS(s string) composer.Message     { m := validMsg(); m.SMSContent = s; return m }
func withSubject(s string) composer.Message { m := validMsg(); m.EmailSubject = s; return m }
func withBody(s string) composer.Message    { m := validMsg(); m.EmailBody = s; return m }

func validate(m composer.Message) (composer.Report, error) {
	return composer.DefaultValidator.Validate(m)
}

// wantIssue asserts err is a validation error with an issue of kind on field.
func wantIssue(t *testing.T, err error, field composer.Field, kind error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v on %s, got nil", kind, field)
	}
	if !errors.Is(err, composer.ErrInvalidContent) || !errors.Is(err, kind) {
		t.Fatalf("want errors.Is(%v) and ErrInvalidContent, got %v", kind, err)
	}
	for _, is := range composer.IssuesOf(err) {
		if is.Field == field && errors.Is(is, kind) {
			if is.Code == "" || is.Message == "" {
				t.Errorf("issue must carry a code and a message: %+v", is)
			}
			return
		}
	}
	t.Fatalf("no %v issue on %s in %v", kind, field, err)
}

func TestLimitsMirrorDomain(t *testing.T) {
	if composer.DefaultLimits.SMSMaxRunes != domain.SMSMaxLength || domain.SMSMaxLength != 160 {
		t.Fatalf("SMS limit must be 160 and come from domain")
	}
	if composer.DefaultLimits.EmailSubjectMaxRunes != domain.EmailSubjectMaxLength || domain.EmailSubjectMaxLength != 80 {
		t.Fatalf("subject limit must be 80 and come from domain")
	}
}

func TestReferenceMessageIsValid(t *testing.T) {
	rep, err := validate(validMsg())
	if err != nil {
		t.Fatalf("reference message must validate: %v", err)
	}
	if rep.EmailBodyFormat != composer.BodyMarkdown {
		t.Errorf("format = %s, want markdown", rep.EmailBodyFormat)
	}
	if got := rep.SMS.Variables; len(got) != 1 || got[0] != "first_name" {
		t.Errorf("SMS variables = %v", got)
	}
	if rep.SMS.Limit != 160 || rep.EmailSubject.Limit != 80 || rep.EmailBody.Limit != 0 {
		t.Errorf("limits in report: %+v", rep)
	}
}

// {{first_name}} is 14 runes as written and budgeted at 18 when rendered: the
// wider of the average name (10) and the fallback "Değerli müşterimiz" (18).
func TestFirstNameEstimate(t *testing.T) {
	rep, err := validate(withSMS("Merhaba {{first_name}}!"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.SMS.Runes != 23 || rep.SMS.EstimatedRunes != 23-14+18 {
		t.Fatalf("runes=%d estimated=%d", rep.SMS.Runes, rep.SMS.EstimatedRunes)
	}
	rep, _ = validate(withSMS("{{first_name}} {{first_name}}"))
	if rep.SMS.EstimatedRunes != 37 || len(rep.SMS.Variables) != 1 {
		t.Fatalf("two occurrences: estimated=%d vars=%v", rep.SMS.EstimatedRunes, rep.SMS.Variables)
	}
}

func TestSMSLimit(t *testing.T) {
	name := "{{first_name}}"
	cases := []struct {
		label string
		sms   string
		want  error
	}{
		{"160 ascii", strings.Repeat("a", 160), nil},
		{"161 ascii", strings.Repeat("a", 161), composer.ErrTooLong},
		// Runes, not bytes: 160 × "ş" is 320 bytes but 160 characters.
		{"160 turkish", strings.Repeat("ş", 160), nil},
		{"161 turkish", strings.Repeat("ğ", 161), composer.ErrTooLong},
		{"160 emoji", strings.Repeat("🎉", 160), nil},
		// Raw template 156, estimated 160: fits both.
		{"template estimate 160", name + strings.Repeat("a", 142), nil},
		// Raw 157 fits, but a user without a name would get 161 characters.
		{"template estimate 161", name + strings.Repeat("a", 143), composer.ErrEstimatedTooLong},
		// Raw template 161 is rejected as too long as written (the raw
		// length is checked before the estimate).
		{"template 161", name + strings.Repeat("a", 147), composer.ErrTooLong},
		{"newline allowed", "Satır 1\nSatır 2", nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			rep, err := validate(withSMS(tc.sms))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				if rep.SMS.Runes > 160 || rep.SMS.EstimatedRunes > 160 {
					t.Fatalf("report over limit: %+v", rep.SMS)
				}
				return
			}
			wantIssue(t, err, composer.FieldSMS, tc.want)
		})
	}
}

func TestSMSTooLongMessageExplainsCount(t *testing.T) {
	_, err := validate(withSMS(strings.Repeat("a", 161)))
	is := composer.IssuesOf(err)
	if len(is) != 1 || is[0].Code != "too_long" || !strings.Contains(is[0].Message, "161") || !strings.Contains(is[0].Message, "160") {
		t.Fatalf("issue = %+v", is)
	}
}

func TestEmailSubjectLimit(t *testing.T) {
	if _, err := validate(withSubject(strings.Repeat("a", 80))); err != nil {
		t.Fatalf("80 chars must pass: %v", err)
	}
	if _, err := validate(withSubject(strings.Repeat("ü", 80))); err != nil {
		t.Fatalf("80 Turkish chars must pass: %v", err)
	}
	_, err := validate(withSubject(strings.Repeat("a", 81)))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrTooLong)

	if _, err := validate(withSubject("{{first_name}}" + strings.Repeat("a", 62))); err != nil {
		t.Fatalf("estimated 80 with variable must pass: %v", err)
	}
	_, err = validate(withSubject("{{first_name}}" + strings.Repeat("a", 63)))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrEstimatedTooLong)
	_, err = validate(withSubject("{{first_name}}" + strings.Repeat("a", 67)))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrTooLong)
}

// With a variable wider than its placeholder, the estimate is what fails.
func TestEstimatedLengthRejects(t *testing.T) {
	vars := composer.MustVariables(composer.Variable{
		Name: "store_name", EstimatedRunes: 30,
		Value: func(domain.User) string { return "Example Mağazacılık A.Ş. İstanbul" },
	})
	v := composer.NewValidator(composer.DefaultLimits, vars)
	m := withSMS("{{store_name}}" + strings.Repeat("a", 140)) // raw 154, estimated 170
	m.EmailSubject, m.EmailBody = "Konu", "Gövde"
	rep, err := v.Validate(m)
	wantIssue(t, err, composer.FieldSMS, composer.ErrEstimatedTooLong)
	if rep.SMS.Runes != 154 || rep.SMS.EstimatedRunes != 170 {
		t.Fatalf("report = %+v", rep.SMS)
	}
	if errors.Is(err, composer.ErrTooLong) {
		t.Fatal("raw length fits; only the estimate should fail")
	}
}

func TestUnknownVariablesRejected(t *testing.T) {
	for _, v := range []string{"{{credit_card}}", "{{email}}", "{{phone}}", "{{password}}", "{{ iban }}"} {
		for _, f := range composer.Fields {
			m := validMsg()
			switch f {
			case composer.FieldSMS:
				m.SMSContent = "Merhaba " + v
			case composer.FieldEmailSubject:
				m.EmailSubject = "Konu " + v
			case composer.FieldEmailBody:
				m.EmailBody = "Gövde " + v
			}
			_, err := validate(m)
			wantIssue(t, err, f, composer.ErrUnknownVariable)
			if !strings.Contains(err.Error(), "{{first_name}}") {
				t.Errorf("error should list allowed variables: %v", err)
			}
		}
	}
}

func TestMalformedPlaceholders(t *testing.T) {
	for _, s := range []string{
		"Merhaba {{first_name",
		"Merhaba first_name}}",
		"Merhaba {{{first_name}}}",
		"Merhaba {{first_name}}}",
		"Merhaba {{}}",
		"Merhaba {{ }}",
		"Merhaba {{first name}}",
		"Merhaba {{First_Name}}",
		"Merhaba {{first_name|upper}}",
		"Merhaba {{.FirstName}}",
		"Merhaba {{ {{first_name}} }}",
	} {
		t.Run(s, func(t *testing.T) {
			_, err := validate(withSMS(s))
			wantIssue(t, err, composer.FieldSMS, composer.ErrMalformedPlaceholder)
		})
	}
}

func TestPlaceholderVariantsAccepted(t *testing.T) {
	for _, s := range []string{
		"Merhaba {{ first_name }}",
		"{{first_name}}{{last_name}}",
		"Tek parantez {serbest} metindir",
		"%30 indirim! {{first_name}}",
	} {
		if _, err := validate(withSMS(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestForbiddenCharacters(t *testing.T) {
	_, err := validate(withSubject("Konu\nBcc: kurban@example.com"))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrForbiddenCharacter)
	_, err = validate(withSubject("Konu\r"))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrForbiddenCharacter)
	_, err = validate(withSubject("Konu\tsekme"))
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrForbiddenCharacter)
	_, err = validate(withSMS("Merhaba\x00"))
	wantIssue(t, err, composer.FieldSMS, composer.ErrForbiddenCharacter)
	// Right-to-left override makes text display differently from what was approved.
	_, err = validate(withSMS("İndirim ‮niz%03‬"))
	wantIssue(t, err, composer.FieldSMS, composer.ErrForbiddenCharacter)
	_, err = validate(withBody("Gövde ⁦gizli⁩"))
	wantIssue(t, err, composer.FieldEmailBody, composer.ErrForbiddenCharacter)
	_, err = validate(withSMS("bozuk \xff utf8"))
	wantIssue(t, err, composer.FieldSMS, composer.ErrForbiddenCharacter)
	if _, err := validate(withBody("Satır\n\tgirintili")); err != nil {
		t.Fatalf("newline and tab are fine in the body: %v", err)
	}
}

func TestEmptyFieldsAllReported(t *testing.T) {
	rep, err := validate(composer.Message{SMSContent: " ", EmailSubject: "", EmailBody: "\n"})
	for _, f := range composer.Fields {
		wantIssue(t, err, f, composer.ErrEmptyField)
	}
	if n := len(composer.IssuesOf(err)); n != 3 {
		t.Fatalf("want 3 issues, got %d", n)
	}
	if rep.SMS.Limit != 160 {
		t.Error("report is filled even on failure")
	}
}

func TestAllIssuesReportedAtOnce(t *testing.T) {
	m := composer.Message{
		SMSContent:   strings.Repeat("a", 170) + "{{credit_card}}",
		EmailSubject: strings.Repeat("b", 90),
		EmailBody:    "<script>alert(1)</script>",
	}
	_, err := validate(m)
	wantIssue(t, err, composer.FieldSMS, composer.ErrTooLong)
	wantIssue(t, err, composer.FieldSMS, composer.ErrUnknownVariable)
	wantIssue(t, err, composer.FieldEmailSubject, composer.ErrTooLong)
	wantIssue(t, err, composer.FieldEmailBody, composer.ErrUnsafeMarkup)
}

func TestBodyHasNoBusinessLimitButASafetyCap(t *testing.T) {
	if _, err := validate(withBody(strings.Repeat("Uzun bir paragraf. ", 2000))); err != nil {
		t.Fatalf("38 KB body must pass: %v", err)
	}
	_, err := validate(withBody(strings.Repeat("a", 64<<10+1)))
	wantIssue(t, err, composer.FieldEmailBody, composer.ErrTooLarge)
}

func TestHTMLBodyAccepted(t *testing.T) {
	body := `<h2>Merhaba {{first_name}},</h2>
<p>Sepetindeki ürünler <strong>%30 indirimle</strong> seni bekliyor.</p>
<ul><li>Hızlı teslimat</li><li>Kolay iade</li></ul>
<p><a href="https://example.com/sepet?utm_source=email&amp;utm_campaign=cart" title='Sepet'>Sepetime git</a><br/>
<em>Sevgiler</em> &amp; <b>teşekkürler</b></p><hr>
<blockquote><span>a &lt; b ve 3 < 5</span></blockquote>`
	rep, err := validate(withBody(body))
	if err != nil {
		t.Fatalf("clean HTML must pass: %v", err)
	}
	if rep.EmailBodyFormat != composer.BodyHTML {
		t.Fatalf("format = %s", rep.EmailBodyFormat)
	}
}

func TestHTMLBodyRejected(t *testing.T) {
	cases := map[string]string{
		"script":            `<p>Merhaba</p><script>alert(1)</script>`,
		"uppercase script":  `<SCRIPT>alert(1)</SCRIPT>`,
		"iframe":            `<iframe src="https://evil.example"></iframe>`,
		"img":               `<img src="https://example.com/x.png">`,
		"style tag":         `<style>body{}</style>`,
		"event handler":     `<p onclick="alert(1)">x</p>`,
		"style attribute":   `<p style="color:red">x</p>`,
		"javascript href":   `<a href="javascript:alert(1)">x</a>`,
		"obfuscated scheme": `<a href="jav&#x61;script:alert(1)">x</a>`,
		"tab in scheme":     "<a href=\"java\tscript:alert(1)\">x</a>",
		"data href":         `<a href="data:text/html;base64,PHNjcmlwdD4=">x</a>`,
		"relative href":     `<a href="/sepet">x</a>`,
		"unquoted attr":     `<a href=https://example.com>x</a>`,
		"duplicate attr":    `<a href="https://a.example" href="javascript:x">x</a>`,
		"comment":           `<p>x</p><!-- gizli -->`,
		"doctype":           `<!DOCTYPE html><p>x</p>`,
		"unclosed":          `<p>Merhaba`,
		"mismatched":        `<p><strong>x</p></strong>`,
		"stray end":         `x</p>`,
		"self-closing p":    `<p/>`,
		"closing br":        `<br></br>`,
		"angle in attr":     `<a href="https://x.example" title="<b>">x</a>`,
		"unterminated":      `<p class`,
		"svg":               `<svg onload="alert(1)">`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := validate(withBody(body))
			wantIssue(t, err, composer.FieldEmailBody, composer.ErrUnsafeMarkup)
		})
	}
}

func TestHTMLDepthLimit(t *testing.T) {
	body := strings.Repeat("<div>", 40) + "x" + strings.Repeat("</div>", 40)
	_, err := validate(withBody(body))
	wantIssue(t, err, composer.FieldEmailBody, composer.ErrUnsafeMarkup)
}

func TestMarkdownLinks(t *testing.T) {
	ok := "Merhaba {{first_name}},\n\n**%30** indirim: [Sepet](https://example.com/sepet \"başlık\")\n\n[destek]: mailto:destek@example.com\n\n- madde\n- 3 < 5"
	if _, err := validate(withBody(ok)); err != nil {
		t.Fatalf("markdown must pass: %v", err)
	}
	for _, body := range []string{
		"[tıkla](javascript:alert(1))",
		"[tıkla]( JavaScript:alert(1))",
		"[tıkla](data:text/html,x)",
		"[tıkla](/relative)",
		"[ref]: javascript:alert(1)",
	} {
		_, err := validate(withBody(body))
		wantIssue(t, err, composer.FieldEmailBody, composer.ErrUnsafeMarkup)
	}
}

func TestDetectBodyFormat(t *testing.T) {
	cases := map[string]composer.BodyFormat{
		"**kalın** metin":       composer.BodyMarkdown,
		"3 < 5 ve 7 > 2":        composer.BodyMarkdown,
		"<p>x</p>":              composer.BodyHTML,
		"metin <br> metin":      composer.BodyHTML,
		"<!-- yorum -->":        composer.BodyHTML,
		"<https://example.com>": composer.BodyHTML, // autolink: treated (and rejected) as HTML
	}
	for in, want := range cases {
		if got := composer.DetectBodyFormat(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestNormalize(t *testing.T) {
	m := composer.Normalize(composer.Message{
		SMSContent:   "  Merhaba {{first_name}}\r\n",
		EmailSubject: "\tKonu ",
		EmailBody:    "Satır 1\r\nSatır 2\rSatır 3\n\n",
	})
	if m.SMSContent != "Merhaba {{first_name}}" || m.EmailSubject != "Konu" || m.EmailBody != "Satır 1\nSatır 2\nSatır 3" {
		t.Fatalf("normalized = %#v", m)
	}
}

func TestNewVariablesValidation(t *testing.T) {
	val := func(domain.User) string { return "" }
	bad := [][]composer.Variable{
		{{Name: "First", EstimatedRunes: 1, Value: val}},
		{{Name: "a b", EstimatedRunes: 1, Value: val}},
		{{Name: "x", EstimatedRunes: 0, Value: val}},
		{{Name: "x", EstimatedRunes: 1}},
		{{Name: "x", EstimatedRunes: 1, Value: val}, {Name: "x", EstimatedRunes: 1, Value: val}},
	}
	for i, vs := range bad {
		if _, err := composer.NewVariables(vs...); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	if got := composer.DefaultVariables.Names(); strings.Join(got, ",") != "first_name,last_name" {
		t.Errorf("default whitelist = %v", got)
	}
}

func TestCampaignMessagesMapping(t *testing.T) {
	rows := validMsg().CampaignMessages(42)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	sms, email := rows[0], rows[1]
	if sms.Channel != domain.ChannelSMS || sms.Subject != "" || sms.Content != validMsg().SMSContent {
		t.Errorf("sms row = %+v", sms)
	}
	if email.Channel != domain.ChannelEmail || email.Subject != validMsg().EmailSubject || email.Content != validMsg().EmailBody {
		t.Errorf("email row = %+v", email)
	}
	for _, r := range rows {
		if r.CampaignID != 42 || r.Status != domain.MessageStatusDraft || !r.Channel.Valid() || !r.Status.Valid() {
			t.Errorf("row = %+v", r)
		}
	}
	if back := composer.MessageFromCampaign(rows); back != validMsg() {
		t.Errorf("round trip = %+v", back)
	}
}

// The validator's promise: a template it accepts renders within the channel
// limit for every user it can be rendered for. A user without a first name
// gets the fallback "Değerli müşterimiz" (18 runes), which is wider than the
// 10-rune estimate, so the estimate must cover the fallback.
func TestValidatorEstimateCoversFallback(t *testing.T) {
	noName := domain.User{ID: 1, FirstName: "", LastName: "Yılmaz"}
	fallback := utf8.RuneCountInString("Değerli müşterimiz")
	for pad := 120; pad <= 160; pad++ {
		tmpl := "{{first_name}}" + strings.Repeat("a", pad)
		if _, err := validate(withSMS(tmpl)); err == nil {
			if _, err := composer.DefaultRenderer.RenderSMS(tmpl, noName); err != nil {
				t.Fatalf("SMS with %d padding passed validation but cannot be rendered for a user without a name: %v", pad, err)
			}
		}
		if pad > 80 {
			continue
		}
		subj := "{{first_name}}" + strings.Repeat("b", pad-14)
		if _, err := validate(withSubject(subj)); err == nil {
			if _, _, _, err := composer.DefaultRenderer.RenderEmail(subj, "Gövde", noName); err != nil {
				t.Fatalf("subject of %d runes passed validation but cannot be rendered without a name: %v", utf8.RuneCountInString(subj), err)
			}
		}
	}
	rep, err := validate(withSMS("Merhaba {{first_name}}!"))
	if err != nil {
		t.Fatal(err)
	}
	if want := 23 - 14 + fallback; rep.SMS.EstimatedRunes != want {
		t.Fatalf("estimated runes = %d, want %d (fallback width)", rep.SMS.EstimatedRunes, want)
	}
}

// U+2028 / U+2029 end a line in many renderers (and in JavaScript source)
// without being a control character, and zero-width / format characters
// (U+200B, U+FEFF, U+00AD, U+2060, Unicode tag characters) are invisible:
// they hide text from the reviewer, smuggle instructions to other models and
// silently eat into the 160-character budget. Emoji joiners stay allowed.
func TestForbiddenInvisibleAndSeparatorCharacters(t *testing.T) {
	cases := []struct {
		name  string
		msg   composer.Message
		field composer.Field
	}{
		{"line separator in subject", withSubject("Konu\u2028Bcc: kurban@example.com"), composer.FieldEmailSubject},
		{"paragraph separator in subject", withSubject("Konu\u2029gizli"), composer.FieldEmailSubject},
		{"line separator in sms", withSMS("Merhaba\u2028dünya"), composer.FieldSMS},
		{"paragraph separator in body", withBody("Merhaba\u2029dünya"), composer.FieldEmailBody},
		{"zero width space in sms", withSMS("İndi\u200brim %30"), composer.FieldSMS},
		{"BOM in subject", withSubject("\ufeffKonu"), composer.FieldEmailSubject},
		{"soft hyphen in sms", withSMS("in\u00addirim"), composer.FieldSMS},
		{"word joiner in body", withBody("Merhaba\u2060dünya"), composer.FieldEmailBody},
		{"invisible tag characters", withBody("Merhaba \U000E0049\U000E0047\U000E004E\U000E004F\U000E0052\U000E0045"), composer.FieldEmailBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validate(tc.msg)
			wantIssue(t, err, tc.field, composer.ErrForbiddenCharacter)
		})
	}
	for _, ok := range []string{"Aile 👨\u200d👩\u200d👧 indirimi", "Sevgiler ❤️", "Bayrak 🇹🇷"} {
		if _, err := validate(withSMS(ok)); err != nil {
			t.Errorf("%q must stay valid (emoji joiners / selectors): %v", ok, err)
		}
	}
}
