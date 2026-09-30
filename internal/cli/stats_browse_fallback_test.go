package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// refusingKeys is a terminal whose modes cannot be changed.
type refusingKeys struct{ *fakeKeys }

func (refusingKeys) keys() error { return errors.New("no modes") }

// A terminal whose keys cannot be read (the platform has no key mode, or its
// modes cannot be set) gets the static screen.
func TestStatsInteractiveFallsBackWhenKeysCannotBeRead(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	static := mustRunStats(t, env, 100, "--prices", goldenPrices)
	for name, open := range map[string]func(io.Reader) (keyTerminal, bool){
		"no key terminal": func(io.Reader) (keyTerminal, bool) { return nil, false },
		"modes refused":   func(io.Reader) (keyTerminal, bool) { return refusingKeys{newFakeKeys()}, true },
	} {
		run := runStatsOnTerminal(t, fixedTerminal{100, 40}, func(e *Env) { e.openKeys = open }, nil)
		if run.code != 0 || !strings.HasSuffix(run.stdout, static) || len(run.frames) != 0 {
			t.Errorf("%s: code %d, output:\n%q", name, run.code, run.stdout)
		}
	}
}
