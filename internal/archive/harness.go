package archive

import (
	"slices"
	"strings"
)

// The applications ("harnesses") agent-archive captures, by the canonical
// name the archive records and compares.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
	HarnessCursor = "cursor"
)

// knownHarnesses are the applications agent-archive captures.
var knownHarnesses = []string{HarnessClaude, HarnessCodex, HarnessCursor}

// harnessAliases maps every other accepted spelling to its canonical name.
// Claude Code's hooks and older configurations say "claude-code".
var harnessAliases = map[string]string{"claude-code": HarnessClaude}

// CanonicalHarness returns the name the archive uses for a harness name from
// a hook, a flag, a configuration, or stored metadata: trimmed, lower-cased,
// with an alias read as its canonical name ("Claude-Code" is "claude"). An
// unknown name comes back normalized the same way, not rejected; KnownHarness
// says whether it is one agent-archive captures. Every comparison of harness
// names goes through it, so two spellings of one application never disagree.
func CanonicalHarness(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := harnessAliases[name]; ok {
		return canonical
	}
	return name
}

// KnownHarness returns CanonicalHarness(name) and whether it is an
// application agent-archive captures.
func KnownHarness(name string) (string, bool) {
	name = CanonicalHarness(name)
	return name, slices.Contains(knownHarnesses, name)
}
