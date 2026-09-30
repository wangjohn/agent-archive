package archive

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// DisplayLine leaves nothing a terminal acts on and nothing a person cannot
// see: format characters (zero-width spaces and joiners, the byte-order mark,
// soft hyphens, the Arabic letter mark) and tag characters carry text a model
// can read.
func TestDisplayLineRemovesInvisibleCharacters(t *testing.T) {
	t.Parallel()
	tags := string([]rune{0xE0001, 0xE0041, 0xE0042, 0xE007F})
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"plain":                       {"widgets", "widgets"},
		"zero-width space":            {"a\u200bb", "ab"},
		"joiner and non-joiner":       {"a\u200cb\u200dc", "abc"},
		"direction marks":             {"a\u200eb\u200fc\u061cd", "abcd"},
		"byte-order mark":             {"\ufeffa", "a"},
		"word joiner":                 {"a\u2060b", "ab"},
		"soft hyphen":                 {"a\u00adb", "ab"},
		"tag characters":              {"a" + tags + "b", "ab"},
		"variation selectors":         {"a\ufe0fb\ufe00c\U000E0100d\U000E01EFe", "abcde"},
		"grapheme joiner":             {"a\u034fb", "ab"},
		"hangul fillers":              {"a\u115f\u1160\u3164\uffa0b", "ab"},
		"braille blank":               {"a\u2800b", "ab"},
		"mongolian selectors":         {"a\u180bb\u180fc", "abc"},
		"an emoji loses its selector": {"\u2764\ufe0f", "\u2764"},
		"an unassigned tag":           {"a\U000E0000b", "ab"},
		"only invisible":              {"\u200b\u200d" + tags, ""},
		"keeps text and space":        {"a  b\tc\nd é 字", "a b c d é 字"},
		"escape sequence":             {"a\x1b]52;c;eA==\x07b", "a]52;c;eA==b"},
	} {
		if got := DisplayLine(tc.in); got != tc.want {
			t.Errorf("%s: DisplayLine(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

// displayText, which shapes a handoff's own content, keeps what DisplayLine
// removes: a joined emoji or a Persian word is text there.
func TestDisplayTextKeepsJoiners(t *testing.T) {
	t.Parallel()
	in := "family \U0001F468\u200d\U0001F469 می\u200cخواهم"
	if got := displayText(in); got != in {
		t.Errorf("displayText(%q) = %q", in, got)
	}
}

func FuzzDisplayLine(f *testing.F) {
	for _, seed := range []string{"", "plain", "a\u200bb", "\x1b]52;c;x\x07", "\U000E0041tag", "line\nbreak\r\n", "\xff\xfe", "\u202e\u2066x", "a\ufe0f\u2800\u3164b", "\U000E0100"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := DisplayLine(in)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8 from %q: %q", in, got)
		}
		for _, r := range got {
			if r != ' ' && (unicode.IsControl(r) || isRemovedControl(r) || invisibleInTest(r) || r == '\u2028' || r == '\u2029') {
				t.Fatalf("%U survived in %q from %q", r, got, in)
			}
		}
		if strings.Contains(got, "  ") || got != strings.TrimSpace(got) {
			t.Fatalf("not one clean line: %q from %q", got, in)
		}
		if again := DisplayLine(got); again != got {
			t.Fatalf("not idempotent: %q then %q", got, again)
		}
	})
}

// invisibleInTest is DisplayLine's contract written out on its own, so the
// fuzz oracle does not use the function it checks.
func invisibleInTest(r rune) bool {
	return unicode.Is(unicode.Cf, r) || (r >= 0xE0000 && r <= 0xE007F) || (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF) ||
		(r >= 0x180B && r <= 0x180D) || r == 0x180F || r == 0x034F || r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0 || r == 0x2800
}
