package schedulertest

import (
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// The model is a second implementation of the port with a vocabulary of its
// own, and it passes the suite: which shows the suite asks nothing that only
// launchd can answer, and that code written against the port can be tested
// over the model. It is not parallel: one of the suite's checks sets the
// process's environment.
func TestModelPassesTheConformanceSuite(t *testing.T) {
	RunConformance(t, Backend{
		New: func(t *testing.T) (scheduler.Scheduler, Manager) {
			t.Helper()
			m := NewModel()
			return m, m
		},
		Golden: func(t *testing.T, name string, got []byte) {
			t.Helper()
			golden.Check(t, filepath.Join("testdata", "model", name+".job"), got)
		},
	})
}
