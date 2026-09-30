package schedulertest

import (
	"os"
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
		Earlier: func(t *testing.T, site scheduler.Site, inst scheduler.Installation) scheduler.Ref {
			t.Helper()
			path := definitionPath(site, "model-earlier")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("job=model-earlier\nprogram=/opt/old/agent-archive\ndata="+inst.DataHome+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return "model-earlier"
		},
	})
}
