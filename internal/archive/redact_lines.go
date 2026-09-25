package archive

import (
	"regexp"
	"strings"
)

// The credential patterns are case-insensitive and have no literal prefix,
// so Go's regexp runs them through its NFA over the whole of a string, once
// per pattern: filter 11 made a large transcript about ten times slower to
// filter than filter 10. None of them matches across a line break (a
// pattern's match holds no newline, but for a leading character standing in
// for a start anchor), so they are run line by line instead, and only on the
// lines that hold one of the literal words (needles) every match of the
// pattern must contain. A short line also lets regexp use its much faster
// backtracker. The result is what matching the whole string would give:
// FuzzGatedRedactionMatchesWhole checks the whole redaction against an
// ungated run over the whole string (needleText.whole), and
// TestPatternNeedlesAreRequired proves from each parsed regexp that every
// match holds one of its needles and separators. A start anchor in a
// pattern must mean the same on a line as in the whole string: use `(?m)`
// or a leading `\s`, not `^` alone.

// linePattern is a single-line credential pattern and its needles: lower
// case literals one of which every match contains, in its ASCII-lowered
// text. No needles means every line is searched. When anyOf is set, every
// match also holds one of its characters (a separator), so a line without one is
// not searched either.
type linePattern struct {
	re      *regexp.Regexp
	needles []string
	anyOf   string
}

// vocabularyNeedles are the needles of a pattern built on credentialName:
// the first word of every credential term (a name holds its term, whose
// words may only be joined by a separator), and `auth` for npm's `_auth`.
var vocabularyNeedles = func() []string {
	seen := map[string]bool{"auth": true}
	needles := []string{"auth"}
	for _, term := range credentialVocabulary {
		first := strings.Fields(term.words)[0]
		if !seen[first] {
			seen[first] = true
			needles = append(needles, first)
		}
	}
	return needles
}()

// needleText is a string prepared for the needle checks: its bytes with
// ASCII letters lowered (so offsets are the string's own), and whether it
// holds a character outside ASCII that case-folds to an ASCII letter (ſ, the
// Kelvin sign), which a case-insensitive pattern matches but a needle check
// cannot see; such a string is never gated.
type needleText struct {
	s      string
	lower  string
	exotic bool
	// pairs, for a text long enough to be worth it, records the pairs of
	// adjacent bytes in lower, so a needle that holds a pair lower does not
	// is known absent without searching for it (see presentNeedles).
	pairs *pairSet
	// whole, set only by tests, runs every pattern over the whole string
	// with no needle, separator, or line gate: the reference the gated
	// search is checked against (FuzzGatedRedactionMatchesWhole).
	whole bool
}

func newNeedleText(s string) needleText {
	lower := []byte(s)
	for i, c := range lower {
		if 'A' <= c && c <= 'Z' {
			lower[i] = c + 'a' - 'A'
		}
	}
	t := needleText{s: s, lower: string(lower), exotic: strings.ContainsRune(s, '\u017f') || strings.ContainsRune(s, '\u212a')}
	if len(s) >= pairSetMinLength {
		t.pairs = newPairSet(t.lower)
	}
	return t
}

// presentNeedles returns the needles that occur anywhere in t, all of them
// when t is not gated (see needleText), or nil when needles is empty.
//
// Each redaction pass checks every string for a few hundred needles, and
// searching the whole string for each one was most of the time a large
// transcript took to filter. A needle one of whose byte pairs is not in
// t.pairs cannot be in the string, so only the rest are searched for; the
// answer is the same.
func (t needleText) presentNeedles(needles []string) (present []string, gated bool) {
	if len(needles) == 0 || t.exotic {
		return nil, false
	}
	for _, needle := range needles {
		if (t.pairs == nil || t.pairs.mayContain(needle)) && strings.Contains(t.lower, needle) {
			present = append(present, needle)
		}
	}
	return present, true
}

// pairSetMinLength is the length from which a needleText records its byte
// pairs: below it, searching for every needle is as cheap as recording them.
const pairSetMinLength = 256

// pairSet is a set of pairs of adjacent bytes, hashed into 8192 bits. A
// hash collision can only make a pair look present, never absent.
type pairSet [128]uint64

func pairBit(a, b byte) uint { return (uint(a)<<5 ^ uint(b)) & 8191 }

// newPairSet records every pair of adjacent bytes in s.
func newPairSet(s string) *pairSet {
	var set pairSet
	for i := 0; i+1 < len(s); i++ {
		bit := pairBit(s[i], s[i+1])
		set[bit>>6] |= 1 << (bit & 63)
	}
	return &set
}

// mayContain reports whether a string the set was built from can contain
// needle: false only when a pair of adjacent bytes in needle is in none of
// its positions, which every occurrence of needle would put there.
func (set *pairSet) mayContain(needle string) bool {
	for i := 0; i+1 < len(needle); i++ {
		bit := pairBit(needle[i], needle[i+1])
		if set[bit>>6]&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

// lineMatches returns every match of p in t.s, as FindAllStringSubmatchIndex
// on the whole string would, with offsets into t.s. It searches only the
// lines holding one of p's needles.
func lineMatches(p linePattern, t needleText) [][]int {
	if t.whole {
		return p.re.FindAllStringSubmatchIndex(t.s, -1)
	}
	present, gated := t.presentNeedles(p.needles)
	if gated && len(present) == 0 {
		return nil
	}
	var out [][]int
	for start := 0; start <= len(t.s); {
		end := strings.IndexByte(t.s[start:], '\n')
		if end < 0 {
			end = len(t.s)
		} else {
			end += start
		}
		if lineHasNeedle(t.lower[start:end], present, gated) && (p.anyOf == "" || strings.ContainsAny(t.s[start:end], p.anyOf)) {
			for _, match := range p.re.FindAllStringSubmatchIndex(t.s[start:end], -1) {
				for i := range match {
					if match[i] >= 0 {
						match[i] += start
					}
				}
				out = append(out, match)
			}
		}
		start = end + 1
	}
	return out
}

// assignmentSeparators holds a character of every credentialSeparator: `=`,
// `:`, a tab, and the full-width colon and equals sign.
const assignmentSeparators = "=:\t：＝"

func lineHasNeedle(line string, present []string, gated bool) bool {
	if !gated {
		return true
	}
	for _, needle := range present {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
