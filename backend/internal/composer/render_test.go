package composer_test

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/seed"
)

func user(first string) domain.User {
	return domain.User{ID: 7, FirstName: first, LastName: "Yılmaz", City: "İstanbul", OptInSMS: true, OptInEmail: true}
}

func TestRenderFirstName(t *testing.T) {
	r, err := composer.DefaultRenderer.Render(composer.ReferenceMessage, user("Ayşe"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.SMS, "Ayşe, sepetindeki ürünler") {
		t.Errorf("sms = %q", r.SMS)
	}
	if r.EmailSubject != "Ayşe, sepetindekiler %30 indirimle seni bekliyor" {
		t.Errorf("subject = %q", r.EmailSubject)
	}
	if !strings.HasPrefix(r.EmailBody, "Merhaba Ayşe,\n\n") || r.EmailBodyFormat != composer.BodyMarkdown {
		t.Errorf("body = %q (%s)", r.EmailBody, r.EmailBodyFormat)
	}
	for _, s := range []string{r.SMS, r.EmailSubject, r.EmailBody} {
		if strings.Contains(s, "{{") || strings.Contains(s, "}}") {
			t.Errorf("unrendered placeholder in %q", s)
		}
	}
}

func TestRenderSpacedPlaceholderAndMultipleVariables(t *testing.T) {
	got, err := composer.DefaultRenderer.RenderSMS("{{ first_name }} {{last_name}}, {{first_name}}!", user("Can"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Can Yılmaz, Can!" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderFallbackForEmptyName(t *testing.T) {
	for _, name := range []string{"", "   ", "‮"} {
		got, err := composer.DefaultRenderer.RenderSMS("{{first_name}}, indirim seni bekliyor", user(name))
		if err != nil {
			t.Fatal(err)
		}
		if got != "Değerli müşterimiz, indirim seni bekliyor" {
			t.Errorf("name %q: got %q", name, got)
		}
	}
}

// User data is inserted once and never interpreted as a template.
func TestRenderIsSinglePass(t *testing.T) {
	got, err := composer.DefaultRenderer.RenderSMS("Merhaba {{first_name}}", user("{{credit_card}}"))
	if err != nil {
		t.Fatalf("a value that looks like a placeholder is just text: %v", err)
	}
	if got != "Merhaba {{credit_card}}" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderEscapesHTMLBody(t *testing.T) {
	_, body, format, err := composer.DefaultRenderer.RenderEmail("Konu", "<p>Merhaba {{first_name}}</p>",
		user(`<script>alert("x")</script>`))
	if err != nil {
		t.Fatal(err)
	}
	if format != composer.BodyHTML || strings.Contains(body, "<script>") ||
		body != "<p>Merhaba &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;</p>" {
		t.Fatalf("body = %q", body)
	}
}

func TestRenderEscapesMarkdownBody(t *testing.T) {
	_, body, _, err := composer.DefaultRenderer.RenderEmail("Konu", "Merhaba {{first_name}}", user("[tıkla](javascript:alert(1)) *Ali*"))
	if err != nil {
		t.Fatal(err)
	}
	want := `Merhaba \[tıkla\]\(javascript:alert\(1\)\) \*Ali\*`
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// A hostile value cannot add a header line to the subject or break the SMS.
func TestRenderCleansValues(t *testing.T) {
	u := user("Ali\r\nBcc: kurban@example.com\x00\t")
	subject, _, _, err := composer.DefaultRenderer.RenderEmail("{{first_name}} için fırsat", "Gövde", u)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Ali Bcc: kurban@example.com için fırsat" {
		t.Fatalf("subject = %q", subject)
	}
	long := user(strings.Repeat("A", 500))
	sms, err := composer.DefaultRenderer.RenderSMS("{{first_name}}", long)
	if err != nil || utf8.RuneCountInString(sms) != 64 {
		t.Fatalf("values are capped at 64 runes: %d %v", utf8.RuneCountInString(sms), err)
	}
}

func TestRenderRejectsUnknownAndMalformed(t *testing.T) {
	_, err := composer.DefaultRenderer.RenderSMS("Kart: {{credit_card}}", user("Ayşe"))
	if !errors.Is(err, composer.ErrUnknownVariable) {
		t.Fatalf("want ErrUnknownVariable, got %v", err)
	}
	_, err = composer.DefaultRenderer.RenderSMS("Merhaba {{first_name", user("Ayşe"))
	if !errors.Is(err, composer.ErrMalformedPlaceholder) {
		t.Fatalf("want ErrMalformedPlaceholder, got %v", err)
	}
	_, _, _, err = composer.DefaultRenderer.RenderEmail("Konu", "Gövde {{email}}", user("Ayşe"))
	if !errors.Is(err, composer.ErrUnknownVariable) {
		t.Fatalf("body: want ErrUnknownVariable, got %v", err)
	}
}

// The validator works with an estimate; the renderer enforces the hard limit
// on the real value.
func TestRenderEnforcesHardLimits(t *testing.T) {
	tmpl := "{{first_name}}" + strings.Repeat("a", 146) // raw 160, estimated 156
	if _, err := composer.DefaultValidator.Validate(withSMS(tmpl)); err != nil {
		t.Fatalf("template passes validation: %v", err)
	}
	if _, err := composer.DefaultRenderer.RenderSMS(tmpl, user("Mehmetcan")); err != nil {
		t.Fatalf("9-letter name fits (155): %v", err)
	}
	_, err := composer.DefaultRenderer.RenderSMS(tmpl, user("Abdurrahmangazi")) // 15 → 161
	if !errors.Is(err, composer.ErrRenderedTooLong) {
		t.Fatalf("want ErrRenderedTooLong, got %v", err)
	}
	_, _, _, err = composer.DefaultRenderer.RenderEmail("{{first_name}}"+strings.Repeat("b", 66), "Gövde", user("Abdurrahmangazi"))
	if !errors.Is(err, composer.ErrRenderedTooLong) {
		t.Fatalf("subject: want ErrRenderedTooLong, got %v", err)
	}
}

// The validated reference content renders within limits for every user of
// the project's deterministic seed (same generator as cmd/seed).
func TestRenderReferenceForSeedUsers(t *testing.T) {
	users, err := seed.Generate(seed.Config{Count: seed.DefaultCount, Seed: seed.DefaultSeed,
		Now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := composer.NewComposer(composer.NewMockMessageLLMClient(), nil).Generate(t.Context(), composer.ReferenceBrief)
	if err != nil {
		t.Fatal(err)
	}
	maxSMS, maxSubject := 0, 0
	for _, u := range users {
		r, err := composer.DefaultRenderer.Render(draft.Message, u)
		if err != nil {
			t.Fatalf("user %d (%s): %v", u.ID, u.FirstName, err)
		}
		if !strings.HasPrefix(r.SMS, u.FirstName+", ") || !strings.HasPrefix(r.EmailBody, "Merhaba "+u.FirstName+",") {
			t.Fatalf("user %d: name not rendered: %q", u.ID, r.SMS)
		}
		maxSMS = max(maxSMS, utf8.RuneCountInString(r.SMS))
		maxSubject = max(maxSubject, utf8.RuneCountInString(r.EmailSubject))
	}
	if maxSMS > domain.SMSMaxLength || maxSubject > domain.EmailSubjectMaxLength {
		t.Fatalf("max rendered sms=%d subject=%d", maxSMS, maxSubject)
	}
	// The 10-rune estimate is conservative for this dataset.
	if est := draft.Report.SMS.EstimatedRunes; maxSMS > est {
		t.Errorf("longest real SMS %d exceeds the estimate %d", maxSMS, est)
	}
	t.Logf("%d users: longest SMS %d/160, longest subject %d/80", len(users), maxSMS, maxSubject)
}
