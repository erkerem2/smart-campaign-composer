package composer

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"smart-campaign-composer/backend/internal/domain"
)

// AverageFirstNameRunes is the width assumed for {{first_name}} when
// estimating the rendered length of a template.
const AverageFirstNameRunes = 10

// maxVariableNameLen bounds placeholder names so a hostile template cannot
// make the scanner do unbounded work per placeholder.
const maxVariableNameLen = 32

// Variable is a personalization variable that templates may reference as
// {{name}}. Only variables registered in a Variables set are accepted.
type Variable struct {
	Name        string
	Description string // shown to the LLM in the system prompt
	// EstimatedRunes is the assumed rendered width used by the validator.
	EstimatedRunes int
	// Fallback is used when the user's value is empty.
	Fallback string
	// Value extracts the raw value from a user. It must be side-effect free.
	Value func(domain.User) string
}

// Variables is an immutable whitelist of personalization variables.
type Variables struct {
	byName map[string]Variable
	names  []string // sorted
}

// NewVariables builds a whitelist. Names must be lowercase identifiers.
func NewVariables(vars ...Variable) (*Variables, error) {
	v := &Variables{byName: make(map[string]Variable, len(vars))}
	for _, x := range vars {
		if !isVariableName(x.Name) {
			return nil, fmt.Errorf("composer: invalid variable name %q", x.Name)
		}
		if _, dup := v.byName[x.Name]; dup {
			return nil, fmt.Errorf("composer: duplicate variable %q", x.Name)
		}
		if x.EstimatedRunes <= 0 || x.Value == nil {
			return nil, fmt.Errorf("composer: variable %q needs a positive estimate and a Value func", x.Name)
		}
		v.byName[x.Name] = x
		v.names = append(v.names, x.Name)
	}
	sort.Strings(v.names)
	return v, nil
}

// MustVariables is NewVariables that panics; for package-level defaults.
func MustVariables(vars ...Variable) *Variables {
	v, err := NewVariables(vars...)
	if err != nil {
		panic(err)
	}
	return v
}

// DefaultVariables is the production whitelist. Anything else — including
// sensitive-looking names such as {{credit_card}} or {{email}} — is rejected.
var DefaultVariables = MustVariables(
	Variable{
		Name:           "first_name",
		Description:    "Alıcının adı (ör. Ayşe).",
		EstimatedRunes: AverageFirstNameRunes,
		Fallback:       "Değerli müşterimiz",
		Value:          func(u domain.User) string { return u.FirstName },
	},
	Variable{
		Name:           "last_name",
		Description:    "Alıcının soyadı (ör. Yılmaz). Samimi tonda genellikle gerekmez.",
		EstimatedRunes: 10,
		Value:          func(u domain.User) string { return u.LastName },
	},
)

// Lookup returns the variable called name.
func (v *Variables) Lookup(name string) (Variable, bool) {
	x, ok := v.byName[name]
	return x, ok
}

// Names returns the sorted variable names.
func (v *Variables) Names() []string { return append([]string(nil), v.names...) }

// placeholder is one {{name}} occurrence; Start/End are byte offsets of the
// whole token (braces included).
type placeholder struct {
	Start, End int
	Name       string
}

// scanPlaceholders finds every {{name}} token. Any "{{" or "}}" that is not
// part of a well-formed {{identifier}} (optional inner spaces) is reported as
// malformed: "{{first_name", "first_name}}", "{{{first_name}}}", "{{ }}",
// "{{first name}}", "{{First_Name}}". Single braces are ordinary text.
func scanPlaceholders(s string) (found []placeholder, malformed []string) {
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "{{")
		closeAt := strings.Index(s[i:], "}}")
		if open < 0 && closeAt < 0 {
			break
		}
		if open < 0 || (closeAt >= 0 && closeAt < open) {
			malformed = append(malformed, fmt.Sprintf("unmatched %q at offset %d", "}}", i+closeAt))
			i += closeAt + 2
			continue
		}
		start := i + open
		end := strings.Index(s[start+2:], "}}")
		if end < 0 {
			malformed = append(malformed, fmt.Sprintf("unclosed %q at offset %d", "{{", start))
			break
		}
		end += start + 2 // index of the closing "}}"
		inner := strings.TrimSpace(s[start+2 : end])
		tokenEnd := end + 2
		switch {
		case !isVariableName(inner):
			malformed = append(malformed, fmt.Sprintf("invalid placeholder %q", snippet(s[start:tokenEnd])))
		case tokenEnd < len(s) && s[tokenEnd] == '}':
			malformed = append(malformed, fmt.Sprintf("extra %q after placeholder %q", "}", snippet(s[start:tokenEnd])))
			tokenEnd++
		default:
			found = append(found, placeholder{Start: start, End: tokenEnd, Name: inner})
		}
		i = tokenEnd
	}
	return found, malformed
}

func isVariableName(s string) bool {
	if s == "" || len(s) > maxVariableNameLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func snippet(s string) string {
	const max = 40
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
