package textguard

import (
	"strings"
	"testing"
)

func TestCleanBrief(t *testing.T) {
	zwsp, bom, wj, rlo := string(rune(0x200B)), string(rune(0xFEFF)), string(rune(0x2060)), string(rune(0x202E))
	tag := string(rune(0xE0049)) + string(rune(0xE0047)) // invisible "IG"
	cases := map[string]string{
		zwsp + zwsp:                    "",
		bom:                            "",
		" " + wj + " \n" + zwsp + "\t": "",
		string(rune(0x00A0)) + zwsp:    "", // NBSP is whitespace
		"İndi" + zwsp + "rim %30":      "İndirim %30",
		rlo + "kampanya":               "kampanya",
		"brief" + tag:                  "brief",
		"  Son 30 gün \xff":            "Son 30 gün",
		"Aile 👨‍👩‍👧":                   "Aile 👨‍👩‍👧",
	}
	for in, want := range cases {
		if got := CleanBrief(in); got != want {
			t.Errorf("CleanBrief(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ş", MaxBriefRunes) + strings.Repeat(zwsp, 10)
	if n := BriefRunes(CleanBrief(long)); n != MaxBriefRunes {
		t.Errorf("invisible padding must not count: %d", n)
	}
}

func TestIsInvisibleKeepsEmojiJoiners(t *testing.T) {
	for _, r := range []rune{0x200C, 0x200D, 0xFE0F, 'a', 'ş', 0x1F389} {
		if IsInvisible(r) {
			t.Errorf("%U must not be invisible", r)
		}
	}
	for _, r := range []rune{0x200B, 0xFEFF, 0x00AD, 0x2060, 0xE0041, 0x2061} {
		if !IsInvisible(r) {
			t.Errorf("%U must be invisible", r)
		}
	}
}
