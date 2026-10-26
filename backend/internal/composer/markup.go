package composer

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// The e-mail body is Markdown by default. If it contains anything that looks
// like an HTML tag, comment or declaration it is treated as HTML and must
// pass a strict whitelist. The checker is a small purpose-built tokenizer
// (stdlib only): it does not try to "clean" input, it rejects anything it
// does not fully understand.

var htmlTagStart = regexp.MustCompile(`<[a-zA-Z/!?]`)

// DetectBodyFormat classifies an e-mail body.
func DetectBodyFormat(body string) BodyFormat {
	if htmlTagStart.MatchString(body) {
		return BodyHTML
	}
	return BodyMarkdown
}

// allowedTags maps each permitted element to its permitted attributes.
var allowedTags = map[string]map[string]bool{
	"p": {}, "br": {}, "hr": {}, "div": {}, "span": {},
	"strong": {}, "b": {}, "em": {}, "i": {}, "u": {},
	"h1": {}, "h2": {}, "h3": {}, "h4": {},
	"ul": {}, "ol": {}, "li": {}, "blockquote": {},
	"a": {"href": true, "title": true},
}

var voidTags = map[string]bool{"br": true, "hr": true}

const maxHTMLDepth = 32

// AllowedHTMLTags lists the whitelist for the system prompt.
func AllowedHTMLTags() []string {
	return []string{"p", "br", "hr", "div", "span", "strong", "b", "em", "i", "u",
		"h1", "h2", "h3", "h4", "ul", "ol", "li", "blockquote", "a(href,title)"}
}

// checkHTML validates body against the whitelist and returns a description
// of the first problem, or "" if the body is clean.
func checkHTML(body string) string {
	var stack []string
	i := 0
	for i < len(body) {
		lt := strings.IndexByte(body[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := body[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			return "HTML comments are not allowed"
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			return "declarations and processing instructions are not allowed"
		case strings.HasPrefix(rest, "</"):
			name, n, ok := readName(rest[2:])
			if !ok {
				return fmt.Sprintf("malformed end tag near %q", snippet(rest))
			}
			j := 2 + n
			j += countSpace(rest[j:])
			if j >= len(rest) || rest[j] != '>' {
				return fmt.Sprintf("malformed end tag </%s>", name)
			}
			if _, ok := allowedTags[name]; !ok {
				return fmt.Sprintf("tag <%s> is not allowed", name)
			}
			if voidTags[name] {
				return fmt.Sprintf("void tag <%s> cannot be closed", name)
			}
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return fmt.Sprintf("unbalanced end tag </%s>", name)
			}
			stack = stack[:len(stack)-1]
			i += j + 1
		case len(rest) > 1 && isASCIILetter(rest[1]):
			n, problem := checkStartTag(rest, &stack)
			if problem != "" {
				return problem
			}
			if len(stack) > maxHTMLDepth {
				return "HTML is nested too deeply"
			}
			i += n
		default:
			// A bare "<" (e.g. "a < b") is text.
			i++
		}
	}
	if len(stack) > 0 {
		return fmt.Sprintf("unclosed tag <%s>", stack[len(stack)-1])
	}
	return ""
}

// checkStartTag parses "<name attr="v" ...>" or "<name .../>" at the start of
// s and returns the number of bytes consumed.
func checkStartTag(s string, stack *[]string) (int, string) {
	name, n, _ := readName(s[1:])
	attrs, ok := allowedTags[name]
	if !ok {
		return 0, fmt.Sprintf("tag <%s> is not allowed", name)
	}
	j := 1 + n
	seen := map[string]bool{}
	for {
		sp := countSpace(s[j:])
		j += sp
		if j >= len(s) {
			return 0, fmt.Sprintf("unterminated tag <%s>", name)
		}
		switch {
		case s[j] == '>':
			if !voidTags[name] {
				*stack = append(*stack, name)
			}
			return j + 1, ""
		case strings.HasPrefix(s[j:], "/>"):
			if !voidTags[name] {
				return 0, fmt.Sprintf("self-closing <%s/> is only allowed for br and hr", name)
			}
			return j + 2, ""
		}
		if sp == 0 {
			return 0, fmt.Sprintf("malformed tag <%s>", name)
		}
		attr, an, ok := readName(s[j:])
		if !ok {
			return 0, fmt.Sprintf("malformed attribute in <%s>", name)
		}
		if !attrs[attr] {
			return 0, fmt.Sprintf("attribute %q is not allowed on <%s>", attr, name)
		}
		if seen[attr] {
			return 0, fmt.Sprintf("duplicate attribute %q on <%s>", attr, name)
		}
		seen[attr] = true
		j += an
		j += countSpace(s[j:])
		if j >= len(s) || s[j] != '=' {
			return 0, fmt.Sprintf("attribute %q on <%s> needs a quoted value", attr, name)
		}
		j++
		j += countSpace(s[j:])
		if j >= len(s) || (s[j] != '"' && s[j] != '\'') {
			return 0, fmt.Sprintf("attribute %q on <%s> must be quoted", attr, name)
		}
		q := s[j]
		end := strings.IndexByte(s[j+1:], q)
		if end < 0 {
			return 0, fmt.Sprintf("unterminated value for %q on <%s>", attr, name)
		}
		val := s[j+1 : j+1+end]
		if strings.ContainsAny(val, "<>") {
			return 0, fmt.Sprintf("attribute %q on <%s> contains '<' or '>'", attr, name)
		}
		if attr == "href" {
			if p := checkURL(html.UnescapeString(val)); p != "" {
				return 0, p
			}
		}
		j += end + 2
	}
}

// readName reads a lowercase-folded tag/attribute name.
func readName(s string) (string, int, bool) {
	n := 0
	for n < len(s) && (isASCIILetter(s[n]) || (n > 0 && (s[n] >= '0' && s[n] <= '9' || s[n] == '-'))) {
		n++
	}
	if n == 0 {
		return "", 0, false
	}
	return strings.ToLower(s[:n]), n, true
}

func countSpace(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t' || s[n] == '\n') {
		n++
	}
	return n
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// checkURL accepts absolute http(s) and mailto links only. Whitespace and
// control characters are stripped before the scheme check because browsers
// ignore them ("java\tscript:" is javascript:).
func checkURL(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	lower := strings.ToLower(cleaned)
	for _, ok := range []string{"https://", "http://", "mailto:"} {
		if strings.HasPrefix(lower, ok) {
			return checkURLVariables(cleaned, ok)
		}
	}
	return fmt.Sprintf("link %q must start with https://, http:// or mailto:", snippet(raw))
}

// checkURLVariables keeps personalization out of the part of a link that
// decides where it goes. A variable in the host ("https://{{first_name}}.x")
// or in a mailto address would let a recipient's own data choose the
// destination; in the path, query or fragment it is URL-encoded on render.
func checkURLVariables(u, scheme string) string {
	if !strings.Contains(u, "{{") {
		return ""
	}
	if scheme == "mailto:" {
		return "personalization variables are not allowed in mailto: links"
	}
	rest := u[len(scheme):]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if strings.Contains(rest[:end], "{{") {
		return fmt.Sprintf("link %q: a personalization variable cannot be part of the host", snippet(u))
	}
	return ""
}

// hrefValueRe finds href attribute values inside a start tag. Templates are
// validated by checkHTML first, so attribute values never contain < or >.
var (
	startTagRe  = regexp.MustCompile(`<[a-zA-Z][^<>]*>`)
	hrefValueRe = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// urlSpans returns the byte ranges of a body template that are link targets:
// href attribute values (HTML) or link destinations (Markdown). The renderer
// URL-encodes variables that fall inside them.
func urlSpans(body string, format BodyFormat) [][2]int {
	var spans [][2]int
	if format == BodyHTML {
		for _, tag := range startTagRe.FindAllStringIndex(body, -1) {
			for _, m := range hrefValueRe.FindAllStringSubmatchIndex(body[tag[0]:tag[1]], -1) {
				for g := 2; g+1 < len(m); g += 2 {
					if m[g] >= 0 {
						spans = append(spans, [2]int{tag[0] + m[g], tag[0] + m[g+1]})
					}
				}
			}
		}
		return spans
	}
	for _, re := range []*regexp.Regexp{mdInlineLink, mdRefLink} {
		for _, m := range re.FindAllStringSubmatchIndex(body, -1) {
			if m[2] >= 0 {
				spans = append(spans, [2]int{m[2], m[3]})
			}
		}
	}
	return spans
}

func inSpans(pos int, spans [][2]int) bool {
	for _, s := range spans {
		if pos >= s[0] && pos < s[1] {
			return true
		}
	}
	return false
}

// Markdown link targets: inline [text](url "title") and reference
// definitions "[id]: url".
var (
	mdInlineLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]*)`)
	mdRefLink    = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]]+\]:\s*<?(\S*?)>?(?:\s|$)`)
)

// checkMarkdown validates link targets in a Markdown body.
func checkMarkdown(body string) string {
	for _, re := range []*regexp.Regexp{mdInlineLink, mdRefLink} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if p := checkURL(m[1]); p != "" {
				return p
			}
		}
	}
	return ""
}
