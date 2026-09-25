package archive

import (
	"fmt"
	"strings"
	"testing"
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
