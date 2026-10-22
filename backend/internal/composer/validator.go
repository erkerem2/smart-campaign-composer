package composer

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/domain"
)

// Limits are the channel rules. Lengths are counted in Unicode code points
// (runes), so "ş" counts as one character, not two bytes.
type Limits struct {
	SMSMaxRunes          int
	EmailSubjectMaxRunes int
	// EmailBodyMaxBytes is a safety cap against runaway output, not a
	// business limit (the case sets none for the body).
	EmailBodyMaxBytes int
}

// DefaultLimits mirrors the limits in internal/domain (CASE_SPEC.md).
var DefaultLimits = Limits{
	SMSMaxRunes:          domain.SMSMaxLength,
	EmailSubjectMaxRunes: domain.EmailSubjectMaxLength,
	EmailBodyMaxBytes:    64 << 10,
}

func (l Limits) runeLimit(f Field) int {
	switch f {
	case FieldSMS:
		return l.SMSMaxRunes
	case FieldEmailSubject:
		return l.EmailSubjectMaxRunes
	}
	return 0
}

// Validator checks a Message against channel limits, the variable whitelist
// and markup rules. It is stateless and safe for concurrent use.
type Validator struct {
	Limits Limits
	Vars   *Variables
}

// NewValidator returns a validator; zero arguments fall back to defaults.
func NewValidator(l Limits, vars *Variables) *Validator {
	if l == (Limits{}) {
		l = DefaultLimits
	}
	if vars == nil {
		vars = DefaultVariables
	}
	return &Validator{Limits: l, Vars: vars}
}

// DefaultValidator uses DefaultLimits and DefaultVariables.
var DefaultValidator = NewValidator(DefaultLimits, DefaultVariables)

// Validate checks every field and returns all issues at once. The Report is
// filled even when validation fails, so the UI can show live counters next
// to the errors. Validate does not modify content; call Normalize first.
func (v *Validator) Validate(m Message) (Report, error) {
	var (
		rep    Report
		issues []Issue
	)
	rep.SMS, issues = v.checkTextField(FieldSMS, m.SMSContent, true, issues)
	rep.EmailSubject, issues = v.checkTextField(FieldEmailSubject, m.EmailSubject, false, issues)
	rep.EmailBody, rep.EmailBodyFormat, issues = v.checkBody(m.EmailBody, issues)
	if len(issues) > 0 {
		return rep, &ValidationError{Issues: issues}
	}
	return rep, nil
}

// checkTextField validates a length-limited plain-text field (SMS, subject).
func (v *Validator) checkTextField(f Field, s string, allowNewline bool, issues []Issue) (FieldStats, []Issue) {
	limit := v.Limits.runeLimit(f)
	st := FieldStats{Limit: limit}
	if strings.TrimSpace(s) == "" {
		return st, append(issues, newIssue(f, ErrEmptyField, "must not be empty"))
	}
	issues = checkCharacters(f, s, allowNewline, issues)
	st, issues = v.checkVariables(f, s, st, issues)
	if st.Runes > limit {
		issues = append(issues, newIssue(f, ErrTooLong,
			"%d characters, limit is %d (template counted as written)", st.Runes, limit))
	} else if st.EstimatedRunes > limit {
		issues = append(issues, newIssue(f, ErrEstimatedTooLong,
			"about %d characters after personalization, limit is %d", st.EstimatedRunes, limit))
	}
	return st, issues
}

func (v *Validator) checkBody(s string, issues []Issue) (FieldStats, BodyFormat, []Issue) {
	const f = FieldEmailBody
	var st FieldStats
	format := DetectBodyFormat(s)
	if strings.TrimSpace(s) == "" {
		return st, format, append(issues, newIssue(f, ErrEmptyField, "must not be empty"))
	}
	if len(s) > v.Limits.EmailBodyMaxBytes {
		// Do not scan a huge body further; one issue is enough.
		st.Runes = utf8.RuneCountInString(s)
		return st, format, append(issues, newIssue(f, ErrTooLarge,
			"%d bytes, safety cap is %d", len(s), v.Limits.EmailBodyMaxBytes))
	}
	issues = checkCharacters(f, s, true, issues)
	st, issues = v.checkVariables(f, s, st, issues)
	var problem string
	if format == BodyHTML {
		problem = checkHTML(s)
	} else {
		problem = checkMarkdown(s)
	}
	if problem != "" {
		issues = append(issues, newIssue(f, ErrUnsafeMarkup, "%s", problem))
	}
	return st, format, issues
}

// checkVariables validates placeholders and computes raw and estimated
// lengths. The estimate swaps each placeholder token for the variable's
// EstimatedRunes, so "{{first_name}}" (14 runes) counts as 10.
func (v *Validator) checkVariables(f Field, s string, st FieldStats, issues []Issue) (FieldStats, []Issue) {
	st.Runes = utf8.RuneCountInString(s)
	st.EstimatedRunes = st.Runes
	found, malformed := scanPlaceholders(s)
	for _, msg := range malformed {
		issues = append(issues, newIssue(f, ErrMalformedPlaceholder, "%s", msg))
	}
	seen := map[string]bool{}
	for _, p := range found {
		vr, ok := v.Vars.Lookup(p.Name)
		if !ok {
			issues = append(issues, newIssue(f, ErrUnknownVariable,
				"{{%s}} is not supported; allowed: %s", p.Name, braceList(v.Vars.Names())))
			continue
		}
		st.EstimatedRunes += vr.EstimatedRunes - utf8.RuneCountInString(s[p.Start:p.End])
		if !seen[p.Name] {
			seen[p.Name] = true
			st.Variables = append(st.Variables, p.Name)
		}
	}
	return st, issues
}

// checkCharacters rejects invalid UTF-8, control characters (newline only
// where allowed; tab only in the body) and bidirectional overrides, which
// can make a message display differently from what was approved.
func checkCharacters(f Field, s string, allowNewline bool, issues []Issue) []Issue {
	if !utf8.ValidString(s) {
		return append(issues, newIssue(f, ErrForbiddenCharacter, "invalid UTF-8"))
	}
	for _, r := range s {
		switch {
		case r == '\n' && allowNewline:
		case r == '\t' && f == FieldEmailBody:
		case (r == '\n' || r == '\r') && !allowNewline:
			return append(issues, newIssue(f, ErrForbiddenCharacter, "must be a single line"))
		case unicode.IsControl(r), isBidiControl(r):
			return append(issues, newIssue(f, ErrForbiddenCharacter, "contains control character %U", r))
		}
	}
	return issues
}

func isBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

func braceList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{{" + n + "}}"
	}
	return strings.Join(out, ", ")
}
