package archive

import (
	"strings"
	"testing"
)

// A block nested inside a block of the same kind must extend the outer block,
// not end it early: the tail of the outer block is still injected text.
// Multiple blocks of different kinds in one string are all removed and the
// text between them is kept.
//
// Regression: filter v3 review, 2026-09 (1f977b0).
func TestFilterV3StripsNestedAndMultipleInstructionBlocks(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want string
	}{
		"nested same kind": {
			in:   "before <system-reminder>outer <system-reminder>inner</system-reminder> tail-of-outer</system-reminder> after",
			want: "before  after",
		},
		"multiple kinds": {
			in:   "a <system-reminder>x</system-reminder> b <user_instructions>y</user_instructions> c <environment_context><cwd>/w</cwd></environment_context> d",
			want: "a  b  c  d",
		},
		"different kind nested inside": {
			in:   "p <user_instructions>u <system-reminder>s</system-reminder> v</user_instructions> q",
			want: "p  q",
		},
		"nested and unterminated": {
			in:   "keep <system-reminder>outer <system-reminder>inner</system-reminder> never closed",
			want: "keep",
		},
		"attributes on the tag": {
			in:   "keep <system-reminder kind=\"memory\">m</system-reminder> too",
			want: "keep  too",
		},
	}
	for name, tc := range cases {
		hit, got := stripInjectedInstructions(tc.in)
		if !hit || got != tc.want {
			t.Errorf("%s: stripInjectedInstructions(%q) = (%v, %q), want %q", name, tc.in, hit, got, tc.want)
		}
		for _, leaked := range []string{"outer", "inner", "tail-of-outer", "never closed", "<system-reminder", "<user_instructions", "<environment_context"} {
			if strings.Contains(got, leaked) {
				t.Errorf("%s: injected text %q survived: %q", name, leaked, got)
			}
		}
	}
	if hit, got := stripInjectedInstructions("plain text with <b>markup</b>"); hit || got != "plain text with <b>markup</b>" {
		t.Errorf("unrelated markup was touched: (%v, %q)", hit, got)
	}
}
