package backfill

import "testing"

// Regression: 2026-09 review M-21. Backfill's own copy of harness-name
// canonicalization was case-sensitive: --harness Claude was refused, and a
// filter spelled that way matched nothing. It now uses
// archive.CanonicalHarness like every other package.
func TestHarnessFilterAcceptsEverySpellingOfAnApp(t *testing.T) {
	for _, spelling := range []string{"claude", "Claude", " CLAUDE", "claude-code", "Claude-Code"} {
		f := Filters{Harnesses: []string{spelling}}
		if err := f.Validate(); err != nil {
			t.Errorf("--harness %q refused: %v", spelling, err)
		}
		if !harnessMatches(f.Harnesses, string(harnessClaude)) {
			t.Errorf("--harness %q does not match Claude Code sessions", spelling)
		}
		if harnessMatches(f.Harnesses, string(harnessCodex)) {
			t.Errorf("--harness %q matches Codex sessions", spelling)
		}
	}
	for _, unknown := range []string{"gemini", "claude code", ""} {
		if err := (Filters{Harnesses: []string{unknown}}).Validate(); err == nil {
			t.Errorf("--harness %q accepted", unknown)
		}
	}
}
