package composer

import (
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/domain"
)

// maxValueRunes caps a single substituted value, so a corrupt or hostile
// user record cannot blow up a message.
const maxValueRunes = 64

// Renderer fills template variables with user data.
//
// Safety properties:
//   - single pass: a user value that itself looks like "{{credit_card}}" is
//     inserted literally and never expanded;
//   - only whitelisted variables are resolved, anything else is an error;
//   - values are cleaned (control/bidi characters removed, whitespace
//     collapsed, length capped) and escaped for the target format
//     (HTML-escaped for HTML bodies, backslash-escaped for Markdown) and,
//     inside a link target (href, Markdown link destination), URL-encoded
//     first, so a name cannot add query parameters, start a fragment or end
//     the link;
//   - the rendered SMS and subject are re-checked against the hard limits,
//     because a real name can be longer than the validator's estimate.
type Renderer struct {
	Limits Limits
	Vars   *Variables
}

// NewRenderer returns a renderer; zero arguments fall back to defaults.
func NewRenderer(l Limits, vars *Variables) *Renderer {
	if l == (Limits{}) {
		l = DefaultLimits
	}
	if vars == nil {
		vars = DefaultVariables
	}
	return &Renderer{Limits: l, Vars: vars}
}

// DefaultRenderer uses DefaultLimits and DefaultVariables.
var DefaultRenderer = NewRenderer(DefaultLimits, DefaultVariables)

// Rendered is a message personalised for one user.
type Rendered struct {
	SMS             string     `json:"sms"`
	EmailSubject    string     `json:"email_subject"`
	EmailBody       string     `json:"email_body"`
	EmailBodyFormat BodyFormat `json:"email_body_format"`
}

// Render personalises every field of m for u.
func (r *Renderer) Render(m Message, u domain.User) (Rendered, error) {
	sms, err := r.RenderSMS(m.SMSContent, u)
	if err != nil {
		return Rendered{}, err
	}
	subject, body, format, err := r.RenderEmail(m.EmailSubject, m.EmailBody, u)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{SMS: sms, EmailSubject: subject, EmailBody: body, EmailBodyFormat: format}, nil
}

// RenderSMS personalises an SMS template and enforces the SMS limit on the
// result.
func (r *Renderer) RenderSMS(tmpl string, u domain.User) (string, error) {
	out, err := r.renderField(FieldSMS, tmpl, u, plainText)
	if err != nil {
		return "", err
	}
	if n := utf8.RuneCountInString(out); n > r.Limits.SMSMaxRunes {
		return "", newIssue(FieldSMS, ErrRenderedTooLong, "%d characters for user %d, limit is %d", n, u.ID, r.Limits.SMSMaxRunes)
	}
	return out, nil
}

// RenderEmail personalises the subject and body. The body is escaped
// according to its detected format.
func (r *Renderer) RenderEmail(subjectTmpl, bodyTmpl string, u domain.User) (subject, body string, format BodyFormat, err error) {
	subject, err = r.renderField(FieldEmailSubject, subjectTmpl, u, plainText)
	if err != nil {
		return "", "", "", err
	}
	if n := utf8.RuneCountInString(subject); n > r.Limits.EmailSubjectMaxRunes {
		return "", "", "", newIssue(FieldEmailSubject, ErrRenderedTooLong, "%d characters for user %d, limit is %d", n, u.ID, r.Limits.EmailSubjectMaxRunes)
	}
	format = DetectBodyFormat(bodyTmpl)
	links := urlSpans(bodyTmpl, format)
	esc := func(p placeholder, v string) string {
		inLink := inSpans(p.Start, links)
		if format == BodyHTML {
			if inLink {
				v = urlComponent(v)
			}
			return html.EscapeString(v)
		}
		if inLink {
			return urlComponent(v) // only unreserved characters and %XX
		}
		return escapeMarkdown(v)
	}
	body, err = r.renderField(FieldEmailBody, bodyTmpl, u, esc)
	if err != nil {
		return "", "", "", err
	}
	return subject, body, format, nil
}

func (r *Renderer) renderField(f Field, tmpl string, u domain.User, escape func(placeholder, string) string) (string, error) {
	found, malformed := scanPlaceholders(tmpl)
	if len(malformed) > 0 {
		return "", newIssue(f, ErrMalformedPlaceholder, "%s", malformed[0])
	}
	var b strings.Builder
	b.Grow(len(tmpl))
	last := 0
	for _, p := range found {
		v, ok := r.Vars.Lookup(p.Name)
		if !ok {
			return "", newIssue(f, ErrUnknownVariable, "{{%s}} is not supported", p.Name)
		}
		val := cleanValue(v.Value(u))
		if val == "" {
			val = v.Fallback
		}
		b.WriteString(tmpl[last:p.Start])
		b.WriteString(escape(p, val))
		last = p.End
	}
	b.WriteString(tmpl[last:])
	return b.String(), nil
}

// cleanValue removes control, bidi and invisible characters, collapses whitespace
// (so a value can never add a line break to a subject) and caps the length.
func cleanValue(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError, isBidiControl(r), isInvisible(r):
			return -1
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxValueRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxValueRunes]))
	}
	return s
}

func plainText(_ placeholder, s string) string { return s }

// urlComponent percent-encodes a value for use anywhere in a URL after the
// host: & = ? # / + space and every non-ASCII byte are encoded, so the value
// stays one piece of data in a path segment, query value or fragment.
func urlComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// escapeMarkdown backslash-escapes ASCII punctuation that has meaning in
// CommonMark, so a value like "*Ali*" or "[x](javascript:..)" stays text.
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()<>#+-.!|~&", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
