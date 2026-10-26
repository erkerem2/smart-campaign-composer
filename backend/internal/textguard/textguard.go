// Package textguard holds the character rules shared by everything that
// accepts free text: the content validator (composer), the renderer, and the
// brief that is sent to the LLM (segment, composer, api).
package textguard

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxBriefRunes caps a campaign brief, counted in code points after
// CleanBrief. segment.MaxBriefRunes and composer.MaxBriefRunes alias it.
const MaxBriefRunes = 2000

// IsBidiControl reports bidirectional formatting characters, which can make
// text display differently from its logical order ("Trojan Source").
func IsBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// IsInvisible reports format characters (Unicode category Cf) that render as
// nothing: zero-width space U+200B, word joiner U+2060, BOM / zero-width
// no-break space U+FEFF, soft hyphen U+00AD, invisible operators, and the tag
// characters U+E0000–U+E007F, which can carry a whole hidden sentence (used
// to smuggle instructions to language models). They let text differ from
// what a reviewer sees and silently use up the SMS budget.
//
// The zero-width joiner and non-joiner (U+200D, U+200C) are allowed: emoji
// sequences such as 👨‍👩‍👧 and some scripts need them. Variation selectors
// (U+FE0F, ❤️) are category Mn, not Cf, and are not affected.
func IsInvisible(r rune) bool {
	if r == 0x200C || r == 0x200D {
		return false
	}
	return unicode.Is(unicode.Cf, r)
}

// CleanBrief normalises a campaign brief before it is validated, counted or
// sent to the model: invalid UTF-8 and invisible / bidi format characters are
// removed (a brief can otherwise hide instructions from the marketer who
// reads it) and surrounding whitespace is trimmed. A brief that consists
// only of such characters becomes "" and is rejected as empty.
func CleanBrief(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if IsInvisible(r) || IsBidiControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// BriefRunes is the length of a cleaned brief as the limit counts it.
func BriefRunes(cleaned string) int { return utf8.RuneCountInString(cleaned) }
