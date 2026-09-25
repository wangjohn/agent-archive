package archive

import (
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// wholeText prepares a string for the ungated reference: every pattern over
// the whole string, with no needle, separator, or line gate.
func wholeText(s string) needleText {
	t := newNeedleText(s)
	t.whole = true
	return t
}

// redactWhole is redactSensitive run ungated.
func redactWhole(value string) (string, bool) {
	redacted := false
	for range maxRedactPasses {
		next, hit := redactOnce(value, wholeText)
		if !hit {
			break
		}
		redacted = true
		if next == value {
			break
		}
		value = next
	}
	return value, redacted
}

// checkGatedRedactionMatchesWhole fails when the redaction as it runs
// (line by line, gated by needles and separators) differs from the same
// redaction run over the whole string with no gate. The gates may only
// skip work; any difference is a credential the gated search missed or
// a behavior the gate changed.
func checkGatedRedactionMatchesWhole(t *testing.T, s string) {
	t.Helper()
	gated, gatedHit := redactSensitive(s)
	whole, wholeHit := redactWhole(s)
	if gated != whole || gatedHit != wholeHit {
		t.Fatalf("gated redaction differs from ungated\n in:    %q\n gated: %q (%v)\n whole: %q (%v)", s, gated, gatedHit, whole, wholeHit)
	}
}

// gateSeeds are inputs where a line or needle gate could plausibly lose a
// match: values next to line breaks (real, CR, CRLF, JSON-escaped), names
// in other cases or split across lines, Unicode case folds, full-width
// separators, NUL bytes, and several shapes on one line.
func gateSeeds() []string {
	seeds := lineMatchSeeds()
	seeds = append(seeds,
		"DB_PASSWORD=abc\\nnext=1",
		`{"cmd":"export TOKEN=abc\nmake","env":"PASSWORD=x\r\ny"}`,
		"pass\nword=abc\nPASS\r\nWORD: x",
		"TO\x00KEN=abc\x00PASSWORD=def",
		"PA\u017f\u017fWORD: abc\nTOKEN=x",
		"api\u212aey=abc",
		"ＰＡＳＳＷＯＲＤ=abc\npassword＝def",
		"password: |\r\n  abc\r\nnext: 1",
		"  1→secrets:\n  2→  db: abc\n  3→other: x",
		"a=1 b=2 password=abc token=def\nsecret: \"x\" apiKey: 'y'",
		"\"secret\": {\n  \"value\": \"abc\"\n}",
		"- name: DB_PASSWORD\r\n  value: abc\r\n",
		"curl -u me:abc\\nhttps://x.test",
		"https://x.test/?a=1&password=abc&key=def",
		"password%3Dabc&token%3Axyz",
		"\rpassword=abc\rnext",
	)
	return seeds
}

func TestGatedRedactionMatchesWhole(t *testing.T) {
	for _, s := range gateSeeds() {
		checkGatedRedactionMatchesWhole(t, s)
	}
}

// FuzzGatedRedactionMatchesWhole checks that the gated redaction equals the
// ungated one on any text: the line split, the needle gates, and the
// separator gates never change what is redacted.
func FuzzGatedRedactionMatchesWhole(f *testing.F) {
	for _, s := range gateSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			t.Skip()
		}
		checkGatedRedactionMatchesWhole(t, s)
	})
}

// Every gated pattern's needles (and separator characters) must be ones its
// regexp cannot match without: this proves it from the parsed regexp, so a
// pattern edited to accept a new spelling fails here rather than silently
// skipping lines.
func TestPatternNeedlesAreRequired(t *testing.T) {
	for name, p := range gatedPatterns() {
		re, err := syntax.Parse(p.re.String(), syntax.Perl)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		re = re.Simplify()
		if len(p.needles) > 0 && !requires(re, p.needles) {
			t.Errorf("%s: a match need not contain one of its needles %q", name, p.needles)
		}
		if p.anyOf != "" && !requires(re, strings.Split(p.anyOf, "")) {
			t.Errorf("%s: a match need not contain one of %q", name, p.anyOf)
		}
	}
	// And the checker itself rejects needles a pattern does not require.
	for _, bad := range []struct {
		re     string
		needle string
	}{
		{`(?i)pass(?:word)?|pwd`, "pass"},
		{`a[bc]d`, "abd"},
		{`secret\s*=`, "secret="},
		{`to.?ken`, "token"},
		{`x(?:token)*`, "token"},
	} {
		re, _ := syntax.Parse(bad.re, syntax.Perl)
		if requires(re.Simplify(), []string{bad.needle}) {
			t.Errorf("checker proved %q requires %q", bad.re, bad.needle)
		}
	}
}

// Needle checks lower only ASCII letters, so a case-insensitive pattern
// can match a string the checks cannot see only through a non-ASCII rune
// that case-folds to an ASCII letter. needleText treats those as exotic
// (never gated); this pins the list to Unicode's fold tables.
func TestExoticFoldsAreComplete(t *testing.T) {
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < 0x80 && unicode.IsLetter(f) && !newNeedleText(string(r)).exotic {
				t.Errorf("%U folds to %q but is not exotic", r, f)
			}
		}
	}
}

// maxExactSet bounds each set of strings the needle proof tracks; a set
// that would grow past it is given up (the proof only gets weaker).
const maxExactSet = 512

// requires reports whether every string re matches holds one of needles
// (lower case) in its ASCII-lowered form. It is conservative: false means
// "not proven". Like a trigram index's query analysis, it tracks for each
// sub-expression the exact strings it can match (when few), or else the
// prefixes and suffixes every match starts and ends with, and proves the
// property when every string of such a set, or of a concatenation's
// suffixes joined to the next part's prefixes, holds a needle.
func requires(re *syntax.Regexp, needles []string) bool {
	return analyze(re, needles).ok
}

// regexpInfo is what analyze knows about a sub-expression: exact is every
// string it matches (nil when unknown); prefix and suffix hold a string
// every match starts or ends with (a set holding "" says nothing); ok is
// whether every match holds a needle.
type regexpInfo struct {
	exact  []string
	prefix []string
	suffix []string
	ok     bool
}

func exactInfo(set, needles []string) regexpInfo {
	if set == nil || len(set) > maxExactSet {
		return regexpInfo{prefix: []string{""}, suffix: []string{""}}
	}
	return regexpInfo{exact: set, prefix: set, suffix: set, ok: holdsNeedle(set, needles)}
}

func holdsNeedle(set, needles []string) bool {
	if len(set) == 0 {
		return false
	}
	for _, s := range set {
		if !slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(s, n) }) {
			return false
		}
	}
	return true
}

func analyze(re *syntax.Regexp, needles []string) regexpInfo {
	unknown := regexpInfo{prefix: []string{""}, suffix: []string{""}}
	switch re.Op {
	case syntax.OpLiteral:
		set := []string{""}
		for _, r := range re.Rune {
			runes := []rune{r}
			if re.Flags&syntax.FoldCase != 0 {
				runes = foldOrbit(r)
			}
			if set = cross(set, lowered(runes)); set == nil {
				return unknown
			}
		}
		return exactInfo(set, needles)
	case syntax.OpCharClass:
		var runes []rune
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if re.Rune[i+1]-re.Rune[i] > 64 {
				return unknown
			}
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				runes = append(runes, r)
			}
		}
		if set := lowered(runes); len(set) <= 8 {
			return exactInfo(set, needles)
		}
		return unknown
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return exactInfo([]string{""}, needles)
	case syntax.OpCapture:
		return analyze(re.Sub[0], needles)
	case syntax.OpQuest:
		if sub := analyze(re.Sub[0], needles); sub.exact != nil {
			return exactInfo(union([]string{""}, sub.exact), needles)
		}
		return unknown
	case syntax.OpPlus:
		sub := analyze(re.Sub[0], needles)
		return regexpInfo{prefix: sub.prefix, suffix: sub.suffix, ok: sub.ok}
	case syntax.OpRepeat:
		if re.Min == 0 {
			return unknown
		}
		sub := analyze(re.Sub[0], needles)
		if re.Min == 1 && re.Max == 1 {
			return sub
		}
		return regexpInfo{prefix: sub.prefix, suffix: sub.suffix, ok: sub.ok}
	case syntax.OpAlternate:
		info := regexpInfo{ok: true}
		exact := true
		for _, sub := range re.Sub {
			s := analyze(sub, needles)
			info.ok = info.ok && s.ok
			if s.exact == nil {
				exact = false
			} else if exact {
				info.exact = union(info.exact, s.exact)
			}
			info.prefix = union(info.prefix, s.prefix)
			info.suffix = union(info.suffix, s.suffix)
		}
		if exact {
			merged := exactInfo(info.exact, needles)
			merged.ok = merged.ok || info.ok
			return merged
		}
		info.exact = nil
		info.ok = info.ok || holdsNeedle(info.prefix, needles) || holdsNeedle(info.suffix, needles)
		return info
	case syntax.OpConcat:
		acc := exactInfo([]string{""}, needles)
		for _, sub := range re.Sub {
			acc = concatInfo(acc, analyze(sub, needles), needles)
		}
		return acc
	case syntax.OpNoMatch, syntax.OpAnyCharNotNL, syntax.OpAnyChar, syntax.OpStar:
		return unknown
	default:
		return unknown
	}
}

// concatInfo is what is known of a match of a followed by a match of b.
func concatInfo(a, b regexpInfo, needles []string) regexpInfo {
	ok := a.ok || b.ok
	if a.exact != nil && b.exact != nil {
		if set := cross(a.exact, b.exact); set != nil {
			info := exactInfo(set, needles)
			info.ok = info.ok || ok
			return info
		}
	}
	if joined := cross(a.suffix, b.prefix); joined != nil && holdsNeedle(joined, needles) {
		ok = true
	}
	prefix, suffix := a.prefix, b.suffix
	if a.exact != nil {
		if joined := cross(a.exact, b.prefix); joined != nil {
			prefix = joined
		}
	}
	if b.exact != nil {
		if joined := cross(a.suffix, b.exact); joined != nil {
			suffix = joined
		}
	}
	info := regexpInfo{prefix: prefix, suffix: suffix, ok: ok}
	info.ok = info.ok || holdsNeedle(info.prefix, needles) || holdsNeedle(info.suffix, needles)
	return info
}

// foldOrbit returns r and every rune it case-folds to.
func foldOrbit(r rune) []rune {
	orbit := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f)
	}
	return orbit
}

// lowered returns runes as distinct one-rune strings in the form the needle
// checks see (newNeedleText): ASCII letters lowered, and the runes that
// fold to an ASCII letter (which make a string exotic, never gated) as that
// letter.
func lowered(runes []rune) []string {
	var set []string
	for _, r := range runes {
		switch {
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		case r == '\u017f':
			r = 's'
		case r == '\u212a':
			r = 'k'
		}
		if s := string(r); !slices.Contains(set, s) {
			set = append(set, s)
		}
	}
	return set
}

func cross(a, b []string) []string {
	if len(a)*len(b) > maxExactSet {
		return nil
	}
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			if s := x + y; !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
