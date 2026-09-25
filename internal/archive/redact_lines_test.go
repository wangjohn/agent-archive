package archive

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// gatedPatterns are every line pattern the redaction runs, with the gate it
// runs under (see redactSensitiveOnce and the redact* functions).
func gatedPatterns() map[string]linePattern {
	patterns := map[string]linePattern{
		"shape":       credentialShape,
		"yaml key":    {yamlCredentialKey, vocabularyNeedles, ":"},
		"entry name":  {credentialNameEntry, vocabularyNeedles, ":"},
		"structure":   {credentialStructureStart, vocabularyNeedles, "{["},
		"assignment":  {credentialAssignment, vocabularyNeedles, assignmentSeparators},
		"flag":        {credentialFlag, vocabularyNeedles, "-"},
		"url userinf": {urlScheme, []string{"://"}, ""},
		"pgpass port": pgpassPortLine,
		"pgpass name": {pgpassNamedLine, nil, ":"},
	}
	for i, p := range credentialContextPatterns {
		patterns[fmt.Sprintf("context %d", i)] = p
	}
	return patterns
}

// matchKey is what a match redacts: its "value" group, or the whole match
// without a leading line break that stood in for a start anchor.
func matchKey(s string, p linePattern, match []int) [2]int {
	for i, name := range p.re.SubexpNames() {
		if name == "value" && match[2*i] >= 0 {
			return [2]int{match[2*i], match[2*i+1]}
		}
	}
	start := match[0]
	if start < match[1] && (s[start] == '\n' || s[start] == '\r') {
		start++
	}
	return [2]int{start, match[1]}
}

// checkLineMatchesCoverWholeString fails when running a pattern line by
// line, under its gate, misses something matching the whole string finds:
// the gates and the line split may only skip work, never a credential.
func checkLineMatchesCoverWholeString(t *testing.T, s string) {
	t.Helper()
	text := newNeedleText(s)
	for name, p := range gatedPatterns() {
		found := map[[2]int]bool{}
		for _, match := range lineMatches(p, text) {
			found[matchKey(s, p, match)] = true
		}
		for _, match := range p.re.FindAllStringSubmatchIndex(s, -1) {
			if key := matchKey(s, p, match); !found[key] {
				t.Fatalf("%s: line matching missed %q in %q", name, s[key[0]:key[1]], s)
			}
		}
	}
}

// The redaction runs its patterns line by line, only on the lines holding
// a needle (a word every match must contain), for speed; that must never
// miss what matching the whole string finds.
func TestLineMatchesAgreeWithWholeStringMatches(t *testing.T) {
	t.Parallel()
	for _, s := range gateSeeds() {
		checkLineMatchesCoverWholeString(t, s)
	}
}

func lineMatchSeeds() []string {
	seeds := []string{
		"a\nDB_PASSWORD=abc def\nb",
		"x\n--password abc\ny",
		"  1→password: |\n  2→    abc\n",
		"- name: DB_PASSWORD\n  value: abc\n",
		"\"secret\": {\"value\": \"abc\"}\nnext",
		"TOKEN=abc",
		"PAſſWORD=abc",
		"Authorization: Bearer abcdefghijklmnopqrstuv\nCookie: a=b",
		"mysql -uroot -pabc\ncurl -u a:b https://x.test",
		"https://u:p@h/x\nhttps://x.test/?key=abc&sig=def",
		"machine h login u password p\n<password>x</password>\n<add key=\"ApiKey\" value=\"v\"/>",
		"ghp_" + strings.Repeat("a", 24) + "\nsk-" + strings.Repeat("b", 16) + "\nAKIA" + strings.Repeat("C", 16),
		"hooks.slack.com/services/T0/B0/abcdefgh\n{\"name\": \"X-Api-Key\", \"value\": \"v\"}",
		"PASSWORD：abc\nAPI Key = abc\r\nset -gx TOKEN abc",
	}
	for _, template := range credentialTemplates {
		seeds = append(seeds, "line before\n"+strings.Replace(template, "%s", "S3cretValue9", 1)+"\nline after")
	}
	return seeds
}

// FuzzLineMatchesCoverWholeString checks the line-by-line, needle-gated
// search against the whole-string search for any text.
func FuzzLineMatchesCoverWholeString(f *testing.F) {
	for _, s := range gateSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			t.Skip()
		}
		checkLineMatchesCoverWholeString(t, s)
	})
}

// A pair set only ever rules a needle out when the text cannot hold it, so
// the needles found present are the same with it and without it, whatever
// the text.
func TestPairSetFindsTheSameNeedles(t *testing.T) {
	t.Parallel()
	var needles []string
	for _, p := range gatedPatterns() {
		needles = append(needles, p.needles...)
	}
	random := rand.New(rand.NewPCG(3, 4))
	alphabets := []string{"abcdefghijklmnopqrstuvwxyz", "etaoinsrhld_-:=. \n", "\x00\x01\xffAa:-_\u00e9"}
	for trial := range 2000 {
		alphabet := alphabets[trial%len(alphabets)]
		var b strings.Builder
		for b.Len() < pairSetMinLength+random.IntN(2048) {
			if random.IntN(20) == 0 {
				b.WriteString(needles[random.IntN(len(needles))])
				continue
			}
			b.WriteByte(alphabet[random.IntN(len(alphabet))])
		}
		text := newNeedleText(b.String())
		if text.pairs == nil {
			t.Fatalf("no pair set for %d bytes", len(text.s))
		}
		// Every substring of the text is one it may contain.
		for range 20 {
			start := random.IntN(len(text.lower))
			end := min(len(text.lower), start+1+random.IntN(12))
			if !text.pairs.mayContain(text.lower[start:end]) {
				t.Fatalf("the pair set rules out %q, which is in the text", text.lower[start:end])
			}
		}
		withPairs, _ := text.presentNeedles(needles)
		text.pairs = nil
		without, _ := text.presentNeedles(needles)
		if !slices.Equal(withPairs, without) {
			t.Fatalf("needles present with the pair set %v, without %v, in %q", withPairs, without, text.s)
		}
	}
}

// A long line with many assignments, as minified code has, costs time in
// proportion to its length. Each assignment used to rescan its line from the
// start, so the cost grew with the square of the line (0.9 MB took half a
// minute).
// It is timed, so it runs alone rather than alongside the parallel tests.
func TestManyAssignmentsOnOneLineStayLinear(t *testing.T) {
	if raceEnabled {
		t.Skip("timings under the race detector are meaningless")
	}
	const unit = "var a={password:e.password,token:t};"
	fastest := func(n int) time.Duration {
		in := strings.Repeat(unit, n)
		best := time.Duration(math.MaxInt64)
		for range 2 {
			start := time.Now()
			out, _ := redactSensitive(in)
			best = min(best, time.Since(start))
			// Each unquoted value runs to the next statement.
			if got := strings.Count(out, "[REDACTED]"); got != n {
				t.Fatalf("%d statements: %d values redacted", n, got)
			}
		}
		return best
	}
	// Eight times the input: linear is about 8x the time, quadratic 20x
	// or more.
	small, large := fastest(500), fastest(4000)
	if large > 14*small {
		t.Fatalf("8x the input took %.1fx as long (%v, then %v)", float64(large)/float64(small), small, large)
	}
}

// URLs glued into one long token (no whitespace or quote between them) cost
// time in proportion to the token's length. Each URL used to read the rest
// of the token again, to its end and for an `@` a host follows, so 1.6 MB
// of `http://x` took a minute.
// It is timed, so it runs alone rather than alongside the parallel tests.
func TestGluedURLsStayLinear(t *testing.T) {
	if raceEnabled {
		t.Skip("timings under the race detector are meaningless")
	}
	for _, unit := range []string{"http://x", "http://a:b/", "http://a:b/@"} {
		fastest := func(n int) time.Duration {
			in := strings.Repeat(unit, n)
			best := time.Duration(math.MaxInt64)
			for range 3 {
				start := time.Now()
				redactSensitive(in)
				best = min(best, time.Since(start))
			}
			return best
		}
		// Eight times the input: linear is about 8x the time, quadratic 64x.
		// Sized so the smaller run takes tens of milliseconds, long enough
		// to time while other tests run.
		small, large := fastest(32000), fastest(256000)
		if large > 20*small {
			t.Errorf("%q: 8x the input took %.1fx as long (%v, then %v)", unit, float64(large)/float64(small), small, large)
		}
	}
}
